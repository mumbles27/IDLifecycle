package engine

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/model"
	"idlifecycle/internal/store"
)

// ---- approvals -----------------------------------------------------------------

// Approve moves a pending change to approved and wakes the executor.
func (e *Engine) Approve(ctx context.Context, id, by, note string) (model.Change, error) {
	if c, err := e.Store.GetChange(ctx, id); err == nil {
		switch {
		case c.Payload.NeedsOU:
			return c, fmt.Errorf("%w: choose an OU for this change first (%s)", ErrInvalid, c.Payload.OUReason)
		case c.Payload.NeedsName:
			return c, fmt.Errorf("%w: choose a username for this change first (%s)", ErrInvalid, conflictText(c.Payload.NameConflicts))
		case c.Type == model.ChangeLink && (c.Payload.Link == nil || c.Payload.Link.Chosen == ""):
			return c, fmt.Errorf("%w: choose the matching account first, or create a new account instead", ErrInvalid)
		}
	}
	return e.decide(ctx, id, model.StatusPending, model.StatusApproved, by, note, "change.approve")
}

// Reject closes a pending change without applying it. A note is mandatory.
func (e *Engine) Reject(ctx context.Context, id, by, note string) (model.Change, error) {
	if strings.TrimSpace(note) == "" {
		return model.Change{}, fmt.Errorf("%w: a note is required when rejecting", ErrInvalid)
	}
	c, err := e.decide(ctx, id, model.StatusPending, model.StatusRejected, by, note, "change.reject")
	if err == nil && c.Type == model.ChangeLink && c.Payload.Link != nil {
		// "Not this person": never propose these accounts for this identity again.
		var guids []string
		for _, cand := range c.Payload.Link.Candidates {
			if c.Payload.Link.Chosen == "" || strings.EqualFold(cand.ObjectGUID, c.Payload.Link.Chosen) {
				guids = append(guids, cand.ObjectGUID)
			}
		}
		if rerr := e.Store.AddIdentityLinkRejects(ctx, c.IdentityID, guids...); rerr != nil {
			e.Log.Error("record rejected link", "change", c.ID, "error", rerr)
		} else {
			e.audit(ctx, by, "identity.link_reject", "identity", c.IdentityID, map[string]any{"change": c.ID, "accounts": guids})
		}
	}
	return c, err
}

// Retry re-queues a failed change.
func (e *Engine) Retry(ctx context.Context, id, by, note string) (model.Change, error) {
	return e.decide(ctx, id, model.StatusFailed, model.StatusApproved, by, note, "change.retry")
}

func (e *Engine) decide(ctx context.Context, id, from, to, by, note, action string) (model.Change, error) {
	if err := e.Store.TransitionChange(ctx, id, from, to, by, note); err != nil {
		if errors.Is(err, store.ErrConflict) {
			c, gerr := e.Store.GetChange(ctx, id)
			if gerr != nil {
				return model.Change{}, gerr
			}
			return c, fmt.Errorf("%w: change is %s, expected %s", store.ErrConflict, c.Status, from)
		}
		return model.Change{}, err
	}
	e.audit(ctx, by, action, "change", id, map[string]any{"note": note})
	if to == model.StatusApproved {
		e.Kick()
	}
	return e.Store.GetChange(ctx, id)
}

// ---- execution -------------------------------------------------------------------

var executionOrder = []string{model.ChangeLink, model.ChangeCreate, model.ChangeUpdate, model.ChangeEnable, model.ChangeDisable, model.ChangeDelete}

// ExecuteChunk caps the items sent to one PowerShell invocation, so a large batch (the first load of a
// whole HR population) never runs into the script timeout as a single all-or-nothing call.
var ExecuteChunk = 50

// execResult summarises one executor pass.
type execResult struct {
	applied, failed, deferred int
	failures                  []string // "type display (external id): error" for notifications
}

// Execute applies every approved change. It returns how many were applied and failed.
func (e *Engine) Execute(ctx context.Context) (int, error) {
	r, err := e.executeCounted(ctx)
	return r.applied + r.failed, err
}

func (e *Engine) executeCounted(ctx context.Context) (res execResult, err error) {
	e.execMu.Lock()
	defer e.execMu.Unlock()
	defer func() { e.notifyExecution(ctx, res) }()

	settings, err := e.Store.GetSettings(ctx)
	if err != nil {
		return res, err
	}
	if settings.Engine.DryRun {
		return res, nil // never write to AD while in dry-run, even if something was approved earlier
	}

	approved, err := e.Store.ListChangesWithStatus(ctx, model.StatusApproved)
	if err != nil {
		return res, err
	}
	byType := map[string][]model.Change{}
	for _, c := range approved {
		byType[c.Type] = append(byType[c.Type], c)
	}

	if dels := byType[model.ChangeDelete]; len(dels) > 0 && settings.Engine.MaxDeletesPerDay > 0 {
		done, err := e.Store.CountAppliedSince(ctx, model.ChangeDelete, e.Now().Add(-24*time.Hour))
		if err != nil {
			return res, err
		}
		allowed := max(settings.Engine.MaxDeletesPerDay-done, 0)
		if len(dels) > allowed {
			// Oldest approvals go first; the rest stay approved and are picked up when the window allows.
			res.deferred = len(dels) - allowed
			byType[model.ChangeDelete] = dels[:allowed]
		}
	}

	for _, typ := range executionOrder {
		batch := byType[typ]
		for start := 0; start < len(batch); start += ExecuteChunk {
			chunk := batch[start:min(start+ExecuteChunk, len(batch))]
			if err := e.applyBatch(ctx, settings, typ, chunk, &res); err != nil {
				return res, err
			}
		}
	}
	return res, nil
}

func (e *Engine) applyBatch(ctx context.Context, s model.Settings, typ string, batch []model.Change, res *execResult) error {
	// Claim each change (approved -> applying). Anyone who lost a race is skipped.
	var claimed []model.Change
	for _, c := range batch {
		if err := e.Store.TransitionChange(ctx, c.ID, model.StatusApproved, model.StatusApplying, "system", ""); err != nil {
			if errors.Is(err, store.ErrConflict) {
				continue
			}
			e.releaseClaimed(claimed, err)
			return err
		}
		claimed = append(claimed, c)
	}
	if len(claimed) == 0 {
		return nil
	}

	opts := ad.ActionOpts{Server: s.AD.Server, IDAttribute: s.AD.IDAttribute}
	ids := make([]string, len(claimed))
	for i, c := range claimed {
		ids[i] = c.ID
	}

	var results []ad.Result
	var callErr error
	refused := map[string]ad.Result{} // changes failed before the AD call (their own reason wins over callErr)
	switch typ {
	case model.ChangeCreate:
		items := make([]ad.CreateItem, 0, len(claimed))
		for _, c := range claimed {
			pw, mustChange, msg, perr := e.initialPassword(ctx, s, c)
			if perr != nil {
				e.releaseClaimed(claimed, perr)
				return perr
			}
			if msg != "" {
				refused[c.ID] = ad.Result{ChangeID: c.ID, Status: ad.StatusFailed, Error: msg}
				continue
			}
			items = append(items, ad.CreateItem{
				ChangeID: c.ID, ExternalID: c.ExternalID, OU: c.Payload.OU, Candidates: c.Payload.Candidates,
				Attributes: c.Payload.Attributes, Groups: c.Payload.Groups, Password: pw,
				Enabled: s.NewUser.Enabled, ChangePasswordAtLogon: mustChange,
			})
		}
		if len(items) > 0 {
			results, callErr = e.AD.Create(ctx, opts, items)
		}
	case model.ChangeUpdate:
		items := make([]ad.UpdateItem, 0, len(claimed))
		for _, c := range claimed {
			set := make(map[string]string, len(c.Payload.Set))
			for k, d := range c.Payload.Set {
				set[k] = d.To
			}
			items = append(items, ad.UpdateItem{ChangeID: c.ID, ExternalID: c.ExternalID, ObjectGUID: c.Payload.ObjectGUID,
				Set: set, Clear: c.Payload.Clear, AddGroups: c.Payload.AddGroups, RemoveGroups: c.Payload.RemoveGroups, MoveTo: c.Payload.MoveTo})
		}
		results, callErr = e.AD.Update(ctx, opts, items)
	case model.ChangeEnable:
		items := make([]ad.EnableItem, 0, len(claimed))
		for _, c := range claimed {
			items = append(items, ad.EnableItem{ChangeID: c.ID, ExternalID: c.ExternalID, ObjectGUID: c.Payload.ObjectGUID, MoveTo: c.Payload.MoveTo, AddGroups: c.Payload.AddGroups, RemoveGroups: c.Payload.RemoveGroups})
		}
		results, callErr = e.AD.Enable(ctx, opts, items)
	case model.ChangeDisable:
		items := make([]ad.DisableItem, 0, len(claimed))
		for _, c := range claimed {
			items = append(items, ad.DisableItem{ChangeID: c.ID, ExternalID: c.ExternalID, ObjectGUID: c.Payload.ObjectGUID,
				MoveTo: c.Payload.MoveTo, RemoveGroups: c.Payload.RemoveGroups, AddGroups: c.Payload.AddGroups,
				Description: "Disabled by IDLifecycle on " + e.Now().UTC().Format("2006-01-02") + " (no longer active in source)"})
		}
		results, callErr = e.AD.Disable(ctx, opts, items)
	case model.ChangeDelete:
		items := make([]ad.RemoveItem, 0, len(claimed))
		for _, c := range claimed {
			items = append(items, ad.RemoveItem{ChangeID: c.ID, ExternalID: c.ExternalID, ObjectGUID: c.Payload.ObjectGUID})
		}
		results, callErr = e.AD.Remove(ctx, opts, items)
	case model.ChangeLink:
		items := make([]ad.LinkItem, 0, len(claimed))
		for _, c := range claimed {
			ch, ok := c.Payload.Link.ChosenCandidate()
			if !ok {
				refused[c.ID] = ad.Result{ChangeID: c.ID, Status: ad.StatusFailed, Error: "no account chosen"}
				continue
			}
			items = append(items, ad.LinkItem{ChangeID: c.ID, ObjectGUID: ch.ObjectGUID, ExternalID: c.ExternalID, ExpectedExternalID: ch.ExternalID, Rename: c.Payload.Link.Rename})
		}
		if len(items) > 0 {
			results, callErr = e.AD.Link(ctx, opts, items)
		}
	default:
		callErr = fmt.Errorf("unknown change type %q", typ)
	}

	byID := make(map[string]ad.Result, len(results))
	for _, r := range results {
		byID[r.ChangeID] = r
	}

	for _, c := range claimed {
		r, pre := refused[c.ID]
		switch {
		case pre:
		case callErr != nil:
			r = ad.Result{ChangeID: c.ID, Status: ad.StatusFailed, Error: callErr.Error()}
		default:
			var ok bool
			if r, ok = byID[c.ID]; !ok {
				r = ad.Result{ChangeID: c.ID, Status: ad.StatusFailed, Error: "no result returned"}
			}
		}
		e.recordResult(ctx, c, r)
		if r.OK() {
			res.applied++
		} else {
			res.failed++
			res.failures = append(res.failures, fmt.Sprintf("%s %s (%s): %s", c.Type, c.Display, c.ExternalID, firstNonEmpty(r.Error, r.Message, "failed")))
		}
	}

	if callErr != nil {
		e.Log.Error("AD action failed", "type", typ, "count", len(claimed), "error", callErr)
	}
	return nil
}

// initialPassword makes the password for one create: random, or new_user.password_template rendered from the
// identity's source record (then change-at-logon is forced). A template that fails or produces a password AD would
// refuse returns msg (the change fails on its own; the reason never contains the password); err is returned only
// for failures that should stop the batch (no randomness).
func (e *Engine) initialPassword(ctx context.Context, s model.Settings, c model.Change) (pw string, mustChange bool, msg string, err error) {
	if s.NewUser.PasswordMode != model.PasswordModeTemplate {
		pw, err = GeneratePassword(s.NewUser.PasswordLength)
		return pw, s.NewUser.ChangePasswordAtLogon, "", err
	}
	id, err := e.Store.GetIdentity(ctx, c.IdentityID)
	if err != nil {
		return "", true, "initial password: cannot load the identity record: " + err.Error(), nil
	}
	pw, err = RenderPassword(s.NewUser.PasswordTemplate, id.Attrs)
	if err != nil {
		return "", true, "initial password: new_user.password_template: " + strings.TrimPrefix(err.Error(), "template: "), nil
	}
	sam := ""
	if len(c.Payload.Candidates) > 0 {
		sam = c.Payload.Candidates[0].Sam
	}
	if cerr := CheckInitialPassword(pw, sam); cerr != nil {
		return "", true, cerr.Error() + " (fix new_user.password_template)", nil
	}
	return pw, true, "", nil
}

// releaseClaimed fails changes that were claimed but never sent to AD so they do not sit in
// "applying" until the next restart. It uses a fresh context because the caller may be shutting down.
func (e *Engine) releaseClaimed(claimed []model.Change, cause error) {
	for _, c := range claimed {
		if err := e.Store.FinishChange(context.Background(), c.ID, model.StatusFailed, "not attempted: "+cause.Error()); err != nil {
			e.Log.Error("release claimed change", "change", c.ID, "error", err)
		}
	}
}

// recordResult finishes the change and updates the identity link.
func (e *Engine) recordResult(ctx context.Context, c model.Change, r ad.Result) {
	status, msg := model.StatusApplied, ""
	if !r.OK() {
		status = model.StatusFailed
		msg = firstNonEmpty(r.Error, r.Message, "failed")
	}
	if err := e.Store.FinishChange(ctx, c.ID, status, msg); err != nil {
		e.Log.Error("finish change", "change", c.ID, "error", err)
	}
	action := "change.applied"
	if status == model.StatusFailed {
		action = "change.failed"
	}
	e.audit(ctx, "system", action, "change", c.ID, map[string]any{
		"type": c.Type, "external_id": c.ExternalID, "result": r.Status, "message": r.Message, "error": msg, "dn": r.DN,
	})

	// Link whenever AD gave us an object, even on partial failure, so the next run does not try to re-create it.
	if r.ObjectGUID == "" && !(r.OK() && c.Type == model.ChangeDelete) {
		return
	}
	if c.Type == model.ChangeLink && !r.OK() {
		return // the account was not handed over (wrong id attribute, name taken...): leave both records as they are
	}
	id, err := e.Store.GetIdentity(ctx, c.IdentityID)
	if err != nil {
		e.Log.Error("load identity after apply", "identity", c.IdentityID, "error", err)
		return
	}
	now := e.Now()
	if r.ObjectGUID != "" {
		id.ADGUID = r.ObjectGUID
	}
	if r.DN != "" {
		id.ADDN = r.DN
	}
	if r.Sam != "" {
		id.ADSam = r.Sam
	}
	if id.Applied == nil {
		id.Applied = map[string]string{}
	}

	switch c.Type {
	case model.ChangeCreate:
		id.State = model.StateLinked
		if r.OK() {
			for k, v := range c.Payload.Attributes {
				setApplied(id.Applied, k, v)
			}
			if c.Payload.Placement != "" {
				setApplied(id.Applied, placementKey, c.Payload.Placement)
			}
		}
	case model.ChangeUpdate:
		if r.OK() && c.Payload.MoveTo != "" && (c.Payload.Placement != "" || c.Payload.NeedsOU) {
			setApplied(id.Applied, placementKey, c.Payload.Placement)
		}
		if r.OK() {
			for k, d := range c.Payload.Set {
				setApplied(id.Applied, k, d.To)
			}
			for _, k := range c.Payload.Clear {
				setApplied(id.Applied, k, "")
			}
		}
	case model.ChangeDisable:
		if r.OK() {
			id.State, id.DisabledAt = model.StateDisabled, &now
		}
	case model.ChangeEnable:
		if r.OK() {
			id.State, id.DisabledAt = model.StateLinked, nil
		}
	case model.ChangeDelete:
		if r.OK() {
			id.State, id.ADGUID, id.ADDN, id.ADSam, id.DisabledAt = model.StateDeleted, "", "", "", nil
			id.Applied = map[string]string{}
		}
	}
	if c.Type == model.ChangeLink {
		ch, _ := c.Payload.Link.ChosenCandidate()
		id.State, id.DisabledAt = model.StateLinked, nil
		if !ch.Enabled {
			id.State = model.StateDisabled
		}
		id.Applied = map[string]string{} // nothing was written by this service for this record yet
		if err := e.Store.SetIdentityLinkDecision(ctx, id.ID, ""); err != nil {
			e.Log.Error("clear link decision", "identity", id.ID, "error", err)
		}
		if ch.IdentityID != "" {
			e.retire(ctx, ch.IdentityID, id)
		}
	}
	if err := e.Store.UpdateIdentity(ctx, id); err != nil {
		e.Log.Error("update identity after apply", "identity", id.ID, "error", err)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Character sets for generated passwords (no visually ambiguous characters).
const (
	pwUpper  = "ABCDEFGHJKLMNPQRSTUVWXYZ"
	pwLower  = "abcdefghijkmnopqrstuvwxyz"
	pwDigits = "23456789"
	pwSymbol = "!@#$%^&*-_=+?"
)

func pickChar(set string) (byte, error) {
	i, err := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
	if err != nil {
		return 0, err
	}
	return set[i.Int64()], nil
}

// randomChars returns n characters from set (crypto/rand); n must be 1-64 (password templates call it).
func randomChars(set string, n int) (string, error) {
	if n < 1 || n > 64 {
		return "", fmt.Errorf("random length must be between 1 and 64, got %d", n)
	}
	out := make([]byte, n)
	for i := range out {
		c, err := pickChar(set)
		if err != nil {
			return "", err
		}
		out[i] = c
	}
	return string(out), nil
}

// GeneratePassword returns a random password that satisfies typical AD complexity rules
// (upper, lower, digit, symbol) using crypto/rand. It avoids visually ambiguous characters.
func GeneratePassword(n int) (string, error) {
	if n < 14 {
		n = 14
	}
	all := pwUpper + pwLower + pwDigits + pwSymbol

	out := make([]byte, 0, n)
	for _, set := range []string{pwUpper, pwLower, pwDigits, pwSymbol} {
		c, err := pickChar(set)
		if err != nil {
			return "", err
		}
		out = append(out, c)
	}
	for len(out) < n {
		c, err := pickChar(all)
		if err != nil {
			return "", err
		}
		out = append(out, c)
	}
	// Fisher-Yates shuffle so the guaranteed characters are not always first.
	for i := len(out) - 1; i > 0; i-- {
		j, err := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		if err != nil {
			return "", err
		}
		out[i], out[j.Int64()] = out[j.Int64()], out[i]
	}
	return string(out), nil
}

// retire marks the record whose account was handed to newOwner as merged: the planner skips it from now on, so
// the old record (a contractor entry, a former employee id) can never disable or delete the account.
func (e *Engine) retire(ctx context.Context, oldID string, newOwner model.Identity) {
	old, err := e.Store.GetIdentity(ctx, oldID)
	if err != nil {
		e.Log.Error("load the record that gave up its account", "identity", oldID, "error", err)
		return
	}
	old.State, old.MergedInto = model.StateMerged, newOwner.ID
	old.ADGUID, old.ADDN, old.ADSam, old.DisabledAt = "", "", "", nil
	old.Applied = map[string]string{}
	if err := e.Store.UpdateIdentity(ctx, old); err != nil {
		e.Log.Error("retire merged record", "identity", oldID, "error", err)
		return
	}
	e.audit(ctx, "system", "identity.merge", "identity", old.ID, map[string]any{
		"external_id": old.ExternalID, "source": old.SourceName, "merged_into": newOwner.ID, "new_external_id": newOwner.ExternalID,
	})
}
