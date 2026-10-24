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
	return e.decide(ctx, id, model.StatusPending, model.StatusApproved, by, note, "change.approve")
}

// Reject closes a pending change without applying it. A note is mandatory.
func (e *Engine) Reject(ctx context.Context, id, by, note string) (model.Change, error) {
	if strings.TrimSpace(note) == "" {
		return model.Change{}, fmt.Errorf("%w: a note is required when rejecting", ErrInvalid)
	}
	return e.decide(ctx, id, model.StatusPending, model.StatusRejected, by, note, "change.reject")
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

var executionOrder = []string{model.ChangeCreate, model.ChangeUpdate, model.ChangeEnable, model.ChangeDisable, model.ChangeDelete}

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
	switch typ {
	case model.ChangeCreate:
		items := make([]ad.CreateItem, 0, len(claimed))
		for _, c := range claimed {
			pw, perr := GeneratePassword(s.NewUser.PasswordLength)
			if perr != nil {
				e.releaseClaimed(claimed, perr)
				return perr
			}
			items = append(items, ad.CreateItem{
				ChangeID: c.ID, ExternalID: c.ExternalID, OU: c.Payload.OU, Candidates: c.Payload.Candidates,
				Attributes: c.Payload.Attributes, Groups: c.Payload.Groups, Password: pw,
				Enabled: s.NewUser.Enabled, ChangePasswordAtLogon: s.NewUser.ChangePasswordAtLogon,
			})
		}
		results, callErr = e.AD.Create(ctx, opts, items)
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
			items = append(items, ad.EnableItem{ChangeID: c.ID, ExternalID: c.ExternalID, ObjectGUID: c.Payload.ObjectGUID, MoveTo: c.Payload.MoveTo, AddGroups: c.Payload.AddGroups})
		}
		results, callErr = e.AD.Enable(ctx, opts, items)
	case model.ChangeDisable:
		items := make([]ad.DisableItem, 0, len(claimed))
		for _, c := range claimed {
			items = append(items, ad.DisableItem{ChangeID: c.ID, ExternalID: c.ExternalID, ObjectGUID: c.Payload.ObjectGUID,
				MoveTo: c.Payload.MoveTo, RemoveGroups: c.Payload.RemoveGroups,
				Description: "Disabled by IDLifecycle on " + e.Now().UTC().Format("2006-01-02") + " (no longer active in source)"})
		}
		results, callErr = e.AD.Disable(ctx, opts, items)
	case model.ChangeDelete:
		items := make([]ad.RemoveItem, 0, len(claimed))
		for _, c := range claimed {
			items = append(items, ad.RemoveItem{ChangeID: c.ID, ExternalID: c.ExternalID, ObjectGUID: c.Payload.ObjectGUID})
		}
		results, callErr = e.AD.Remove(ctx, opts, items)
	default:
		callErr = fmt.Errorf("unknown change type %q", typ)
	}

	byID := make(map[string]ad.Result, len(results))
	for _, r := range results {
		byID[r.ChangeID] = r
	}

	for _, c := range claimed {
		var r ad.Result
		switch {
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
		}
	case model.ChangeUpdate:
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

// GeneratePassword returns a random password that satisfies typical AD complexity rules
// (upper, lower, digit, symbol) using crypto/rand. It avoids visually ambiguous characters.
func GeneratePassword(n int) (string, error) {
	const (
		upper  = "ABCDEFGHJKLMNPQRSTUVWXYZ"
		lower  = "abcdefghijkmnopqrstuvwxyz"
		digits = "23456789"
		symbol = "!@#$%^&*-_=+?"
	)
	if n < 14 {
		n = 14
	}
	all := upper + lower + digits + symbol
	pick := func(set string) (byte, error) {
		i, err := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
		if err != nil {
			return 0, err
		}
		return set[i.Int64()], nil
	}

	out := make([]byte, 0, n)
	for _, set := range []string{upper, lower, digits, symbol} {
		c, err := pick(set)
		if err != nil {
			return "", err
		}
		out = append(out, c)
	}
	for len(out) < n {
		c, err := pick(all)
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
