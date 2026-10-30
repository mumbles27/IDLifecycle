package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/cloud"
	"idlifecycle/internal/model"
	"idlifecycle/internal/store"
)

// Undo rolls back one applied change. PlanUndo works out the inverse from the state the change found (recorded when it
// was applied), PreviewUndo runs that inverse through the real script in dry-run, and RequestUndo queues it as an undo
// change that always waits for a person (ApproveUndo). The scripts refuse to overwrite anything that changed since
// (Expect), unless the request said to (Force).

// UndoHoldOption is a hold offered with an undo, so the next sync does not simply redo the change.
type UndoHoldOption struct {
	Kind    string `json:"kind"`
	Key     string `json:"key"`
	Label   string `json:"label"`
	Default bool   `json:"default"`
}

// UndoPlan is the inverse of one applied change. When Undoable is false, Reason says why and nothing else is set.
type UndoPlan struct {
	Undoable    bool
	Reason      string
	Change      model.Change
	Script      string
	Payload     model.ChangePayload // the inverse; Payload.Undo holds Of/OfType/Script/Expect/link fields/ConfirmText
	Steps       []string
	Holds       []UndoHoldOption
	ConfirmText string
	Warnings    []string
	Hash        string // PayloadHash of the inverse: a request must name the plan the person saw
	// Items are every part of an update / cloud_groups change (with what is undone, waiting or Manual); Selected the
	// parts this plan undoes. WholeOnly: the change type is only undone as a whole.
	Items     []UndoItem
	Selected  []string
	WholeOnly bool
}

// UndoDryRun is the outcome of running an inverse with -DryRun.
type UndoDryRun struct {
	Status  string `json:"status"` // would | failed (| skipped)
	Message string `json:"message"`
	Error   string `json:"error"`
}

// UndoPreview is PlanUndo plus a live dry run.
type UndoPreview struct {
	UndoPlan
	DryRun   UndoDryRun
	Drift    []string
	Approval string // manual | second_person
}

// UndoRequest is what a person sends to queue an undo.
type UndoRequest struct {
	Reason      string           `json:"reason"`
	Confirm     string           `json:"confirm"`
	Force       bool             `json:"force"`
	Holds       []model.HoldSpec `json:"holds"`
	PreviewHash string           `json:"preview_hash"`
	// Items: the parts to undo (UndoItem keys); empty = every part not undone yet.
	Items []string `json:"items"`
}

// MinUndoReason is the shortest reason an undo request accepts.
const MinUndoReason = 10

func notUndoable(c model.Change, why string) UndoPlan {
	return UndoPlan{Change: c, Reason: why}
}

// scriptType maps an undo script to the change type whose rules give its own inverse (redo).
func scriptType(script string) string {
	switch script {
	case model.UndoScriptSet:
		return model.ChangeUpdate
	case model.UndoScriptEnable:
		return model.ChangeEnable
	case model.UndoScriptDisable:
		return model.ChangeDisable
	case model.UndoScriptLink:
		return model.ChangeLink
	case model.UndoScriptCloud:
		return model.ChangeCloudGroups
	}
	return ""
}

func valueOf(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func lookupFold(m map[string]*string, k string) (*string, bool) {
	if v, ok := m[k]; ok {
		return v, true
	}
	for kk, v := range m {
		if strings.EqualFold(kk, k) {
			return v, true
		}
	}
	return nil, false
}

// wroteBy is what a change writes to attributes (nil = cleared): its Set and Clear.
func wroteBy(p model.ChangePayload) map[string]*string {
	if len(p.Set) == 0 && len(p.Clear) == 0 {
		return nil
	}
	out := make(map[string]*string, len(p.Set)+len(p.Clear))
	for k, d := range p.Set {
		v := d.To
		out[k] = &v
	}
	for _, k := range p.Clear {
		out[k] = nil
	}
	return out
}

// PlanUndo works out how to roll back change id. A change that cannot be undone gives Undoable false and a reason.
func (e *Engine) PlanUndo(ctx context.Context, id string) (UndoPlan, error) {
	return e.PlanUndoItems(ctx, id, nil)
}

// PlanUndoItems is PlanUndo for some parts of an update or cloud_groups change (UndoItem keys); none = every part not
// undone yet. Naming parts of a change that is only undone as a whole, or an unknown or undone part, is ErrInvalid.
func (e *Engine) PlanUndoItems(ctx context.Context, id string, want []string) (UndoPlan, error) {
	c, err := e.Store.GetChange(ctx, id)
	if err != nil {
		return UndoPlan{}, err
	}
	s, err := e.Store.GetSettings(ctx)
	if err != nil {
		return UndoPlan{}, err
	}
	switch {
	case !s.Undo.Enabled:
		return notUndoable(c, "Undo is turned off in Settings."), nil
	case s.Engine.DryRun:
		return notUndoable(c, "Turn off dry run first: nothing is applied in dry run."), nil
	case c.Status != model.StatusApplied:
		return notUndoable(c, fmt.Sprintf("Only applied changes can be undone; this one is %s.", c.Status)), nil
	case c.Type == model.ChangeDelete:
		return notUndoable(c, "Deleted accounts can only be restored from the AD Recycle Bin (Restore-ADObject); IDLifecycle does not restore them."), nil
	case c.UndoneBy != "":
		return notUndoable(c, "This change was already undone (change "+c.UndoneBy+")."), nil
	case c.UndoPending != "":
		return notUndoable(c, "An undo of this change is already waiting or running (change "+c.UndoPending+")."), nil
	case c.AppliedState == nil:
		return notUndoable(c, "This change was applied before undo was available (or changed nothing); there is no record of the previous values."), nil
	}
	ident, err := e.Store.GetIdentity(ctx, c.IdentityID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return notUndoable(c, "The person's record no longer exists."), nil
		}
		return UndoPlan{}, err
	}
	switch ident.State {
	case model.StateMerged:
		return notUndoable(c, "The person's record was merged into another record; undo the change from that record's history instead."), nil
	case model.StateDeleted:
		return notUndoable(c, "The person's account was deleted."), nil
	}
	if c.Type == model.ChangeLink {
		if ch, ok := c.Payload.Link.ChosenCandidate(); ok && ch.IdentityID != "" {
			return notUndoable(c, "This link took the account over from another person record ("+firstNonEmpty(ch.IdentityDisplay, ch.ExternalID)+"); undoing a merge is not supported."), nil
		}
	}

	st := c.AppliedState
	b := st.Before
	typ, ofScript := c.Type, ""
	if c.Type == model.ChangeUndo {
		if c.Payload.Undo == nil {
			return notUndoable(c, "This undo change has no undo details."), nil
		}
		ofScript = c.Payload.Undo.Script
		typ = scriptType(ofScript)
	}
	// Parts: an update or cloud_groups change can be undone piece by piece; the other types only as a whole.
	selectable := typ == model.ChangeUpdate || typ == model.ChangeCloudGroups
	items := itemsOf(c, typ)
	if holdsNow, herr := e.Store.ListHolds(ctx, ident.ID); herr == nil {
		markManual(items, holdsNow, e.Now())
	}
	sel, whole, err := selectItems(items, selectable, want)
	if err != nil {
		return UndoPlan{}, err
	}
	if selectable && len(items) > 0 && len(sel) == 0 {
		pl := notUndoable(c, "Every part of this change was already undone or is waiting in another undo.")
		pl.Items = items
		return pl, nil
	}
	in := func(key string) bool { return !selectable || sel[key] }

	guid := firstNonEmpty(st.ObjectGUID, c.Payload.ObjectGUID, ident.ADGUID)
	u := &model.UndoPayload{Of: c.ID, OfType: c.Type}
	p := model.ChangePayload{ObjectGUID: guid, DN: st.DN, Undo: u}
	exp := &model.Expect{}
	plan := UndoPlan{Change: c}
	sam := firstNonEmpty(st.Sam, ident.ADSam)

	// restoreAccount fills the attribute, group and OU parts of the inverse from what the change found.
	restoreAccount := func(holds bool) {
		for _, k := range sortedKeys(b.Attributes) {
			was := b.Attributes[k]
			now, known := lookupFold(st.Wrote, k)
			if known && valueOf(was) == valueOf(now) && (was == nil) == (now == nil) {
				continue // the change did not alter it
			}
			if !in(itemAttribute + k) {
				continue
			}
			if known {
				if exp.Attributes == nil {
					exp.Attributes = map[string]*string{}
				}
				exp.Attributes[k] = now
			}
			if was != nil {
				if p.Set == nil {
					p.Set = map[string]model.AttrDiff{}
				}
				p.Set[k] = model.AttrDiff{From: valueOf(now), To: *was, Mode: "undo"}
			} else {
				p.Clear = append(p.Clear, k)
			}
			if holds {
				plan.Holds = append(plan.Holds, UndoHoldOption{Kind: model.HoldAttribute, Key: k, Label: "Don't change " + k + " for this person", Default: true})
			}
		}
		var heldGroups []string
		for _, g := range b.GroupsAdded {
			if in(itemGroupAdd + g) {
				p.RemoveGroups = append(p.RemoveGroups, g)
				exp.MemberOf = append(exp.MemberOf, g)
				heldGroups = append(heldGroups, g)
			}
		}
		for _, g := range b.GroupsRemoved {
			if in(itemGroupRemove + g) {
				p.AddGroups = append(p.AddGroups, g)
				exp.NotMemberOf = append(exp.NotMemberOf, g)
				heldGroups = append(heldGroups, g)
			}
		}
		if holds {
			for _, g := range heldGroups {
				plan.Holds = append(plan.Holds, UndoHoldOption{Kind: model.HoldGroup, Key: g, Label: "Don't add or remove " + shortCN(g) + " for this person", Default: true})
			}
		}
		if b.DN != "" && st.DN != "" && !strings.EqualFold(parentDN(b.DN), parentDN(st.DN)) && in(itemOU) {
			p.MoveTo = parentDN(b.DN)
			if holds {
				plan.Holds = append(plan.Holds, UndoHoldOption{Kind: model.HoldOU, Label: "Don't move this person's account to another OU", Default: true})
			}
		}
		// The account's DN is only checked when the OU move is undone (or the whole account state): undoing one
		// attribute must not be blocked by a later move.
		if st.DN != "" && (!selectable || in(itemOU)) {
			exp.DN = st.DN
		}
	}
	stateHold := UndoHoldOption{Kind: model.HoldState, Label: "Don't create, link, disable, enable or delete this person's account", Default: true}

	switch typ {
	case model.ChangeUpdate:
		u.Script = model.UndoScriptSet
		restoreAccount(true)
	case model.ChangeDisable:
		u.Script = model.UndoScriptEnable
		restoreAccount(false)
		exp.Enabled = boolp(false)
		plan.Holds = append(plan.Holds, stateHold)
	case model.ChangeEnable:
		u.Script = model.UndoScriptDisable
		restoreAccount(false)
		exp.Enabled = boolp(true)
		plan.Holds = append(plan.Holds, stateHold)
	case model.ChangeCreate:
		u.Script = model.UndoScriptDisable
		exp = nil
		plan.Holds = append(plan.Holds, stateHold)
		plan.Warnings = append(plan.Warnings, "The account is disabled, not deleted. Without the hold, the next sync enables it again while the person is active in the source.")
	case model.ChangeLink:
		u.Script = model.UndoScriptLink
		after := c.ExternalID
		if c.Type == model.ChangeUndo {
			after = valueOf(c.Payload.Undo.LinkTo)
		}
		to := valueOf(b.IDValue)
		u.LinkFrom, u.LinkTo = after, &to
		exp.IDValue = &after
		if n := b.Names; n != nil {
			u.Rename = &model.Candidate{Sam: n.Sam, UPN: n.UPN, CN: n.CN, Mail: n.Mail}
			if len(n.ProxyAddresses) > 0 {
				u.ProxyAddresses = append([]string(nil), n.ProxyAddresses...)
			}
			exp.Names = &model.AccountNames{Sam: st.Sam, UPN: st.UPN, CN: rdn(st.DN)}
		}
		plan.Holds = append(plan.Holds, stateHold)
		if to != ident.ExternalID {
			plan.Warnings = append(plan.Warnings, "The person will have no account afterwards. Without the hold, the next sync creates a new account for them or proposes an account again.")
		}
	case model.ChangeCloudGroups:
		u.Script = model.UndoScriptCloud
		exp = nil
		p.CloudUserID = c.Payload.CloudUserID
		var heldCloud []string
		for _, g := range b.CloudAdded {
			if in(itemCloudAdd + g) {
				p.CloudRemove = append(p.CloudRemove, g)
				heldCloud = append(heldCloud, g)
			}
		}
		for _, g := range b.CloudRemoved {
			if in(itemCloudRemove + g) {
				p.CloudAdd = append(p.CloudAdd, g)
				heldCloud = append(heldCloud, g)
			}
		}
		for _, g := range heldCloud {
			plan.Holds = append(plan.Holds, UndoHoldOption{Kind: model.HoldCloudGroup, Key: g, Label: "Don't add or remove Entra group " + g + " for this person", Default: true})
		}
	default:
		return notUndoable(c, fmt.Sprintf("Changes of type %s cannot be undone.", c.Type)), nil
	}
	if (u.Script == model.UndoScriptSet || u.Script == model.UndoScriptCloud) &&
		len(p.Set) == 0 && len(p.Clear) == 0 && len(p.AddGroups) == 0 && len(p.RemoveGroups) == 0 && p.MoveTo == "" &&
		len(p.CloudAdd) == 0 && len(p.CloudRemove) == 0 {
		return notUndoable(c, "Nothing to roll back: the change did not alter anything that can be restored."), nil
	}
	sort.Strings(p.Clear)
	if exp != nil && (exp.Enabled != nil || exp.DN != "" || len(exp.Attributes) > 0 || len(exp.MemberOf) > 0 || len(exp.NotMemberOf) > 0 ||
		exp.IDValue != nil || exp.Names != nil) {
		u.Expect = exp
	}
	u.ConfirmText = firstNonEmpty(sam, ident.Display, c.Display, c.ExternalID)
	plan.Items, plan.WholeOnly = items, !selectable
	if selectable {
		plan.Selected = sortedKeys(sel)
		if !whole {
			u.Items = plan.Selected
		}
	}

	plan.Undoable, plan.Script, plan.Payload, plan.ConfirmText = true, u.Script, p, u.ConfirmText
	plan.Steps = undoSteps(p, sam, s.AD.IDAttribute)
	plan.Hash = PayloadHash(model.ChangeUndo, p)
	what := c.Type
	if c.Type == model.ChangeUndo {
		what = "undo"
	}
	if mode := s.Approval.For(typ); c.Type != model.ChangeUndo && mode == "auto" {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("Without a hold, the next sync can plan this %s again, and because approval for %s is automatic it would be applied without asking.", what, typ))
	} else {
		plan.Warnings = append(plan.Warnings, "Without a hold, the next sync can plan this change again (it would then wait for approval).")
	}
	if s.Undo.SecondPerson {
		plan.Warnings = append(plan.Warnings, "Someone other than you must approve this undo (undo.second_person).")
	}
	return plan, nil
}

func boolp(b bool) *bool { return &b }

func rdn(dn string) string {
	first := strings.SplitN(dn, ",", 2)[0]
	if i := strings.Index(first, "="); i >= 0 {
		return first[i+1:]
	}
	return first
}

// undoSteps says in plain words what an inverse does.
func undoSteps(p model.ChangePayload, sam, idAttr string) []string {
	u := p.Undo
	acct := "the account"
	if sam != "" {
		acct = "the account " + sam
	}
	var out []string
	switch u.Script {
	case model.UndoScriptEnable:
		out = append(out, "Enable "+acct)
	case model.UndoScriptDisable:
		if u.OfType == model.ChangeCreate {
			out = append(out, "Disable "+acct+" that the change created (it is not deleted)")
		} else {
			out = append(out, "Disable "+acct)
		}
	case model.UndoScriptLink:
		if valueOf(u.LinkTo) == "" {
			out = append(out, fmt.Sprintf("Clear %s on %s (it is no longer this person's account)", idAttr, acct))
		} else {
			out = append(out, fmt.Sprintf("Set %s on %s back to '%s'", idAttr, acct, valueOf(u.LinkTo)))
		}
		if r := u.Rename; r != nil {
			out = append(out, fmt.Sprintf("Rename back to %s / %s / %s", r.Sam, r.UPN, r.CN))
			if r.Mail != "" {
				out = append(out, "Set mail back to "+r.Mail)
			}
		}
		if len(u.ProxyAddresses) > 0 {
			out = append(out, fmt.Sprintf("Set proxyAddresses back to %d address(es)", len(u.ProxyAddresses)))
		}
	}
	for _, k := range sortedKeys(p.Set) {
		d := p.Set[k]
		if d.From == "" {
			out = append(out, fmt.Sprintf("Set %s back to '%s' (it is empty now)", k, d.To))
		} else {
			out = append(out, fmt.Sprintf("Set %s back to '%s' (now '%s')", k, d.To, d.From))
		}
	}
	for _, k := range p.Clear {
		out = append(out, "Clear "+k+" (it was empty before)")
	}
	if p.MoveTo != "" {
		out = append(out, "Move back to "+p.MoveTo)
	}
	for _, g := range p.AddGroups {
		out = append(out, "Add back to group "+shortCN(g))
	}
	for _, g := range p.RemoveGroups {
		out = append(out, "Remove from group "+shortCN(g))
	}
	for _, g := range p.CloudAdd {
		out = append(out, "Add back to Entra group "+g)
	}
	for _, g := range p.CloudRemove {
		out = append(out, "Remove from Entra group "+g)
	}
	return out
}

// PreviewUndo plans the undo of change id and runs it through the real script with -DryRun (reads AD or the tenant
// live, writes nothing). Drift (changes made since) is reported, not an error.
func (e *Engine) PreviewUndo(ctx context.Context, id string) (UndoPreview, error) {
	return e.PreviewUndoItems(ctx, id, nil)
}

// PreviewUndoItems is PreviewUndo for some parts of a change (see PlanUndoItems).
func (e *Engine) PreviewUndoItems(ctx context.Context, id string, items []string) (UndoPreview, error) {
	plan, err := e.PlanUndoItems(ctx, id, items)
	if err != nil {
		return UndoPreview{}, err
	}
	pv := UndoPreview{UndoPlan: plan, Approval: "manual"}
	s, err := e.Store.GetSettings(ctx)
	if err != nil {
		return pv, err
	}
	if s.Undo.SecondPerson {
		pv.Approval = "second_person"
	}
	if !plan.Undoable {
		return pv, nil
	}
	probe := model.Change{ID: "undo-preview-" + plan.Change.ID, IdentityID: plan.Change.IdentityID, ExternalID: plan.Change.ExternalID, Display: plan.Change.Display,
		Type: model.ChangeUndo, Payload: plan.Payload}
	pv.DryRun, pv.Drift = e.dryRunUndo(ctx, s, probe)
	if len(pv.Drift) > 0 {
		pv.Warnings = append(pv.Warnings, "Later changes would be overwritten: "+strings.Join(pv.Drift, "; "))
	}
	return pv, nil
}

func (e *Engine) dryRunUndo(ctx context.Context, s model.Settings, c model.Change) (UndoDryRun, []string) {
	r := e.runUndo(ctx, s, []model.Change{c}, true)[0]
	dr := UndoDryRun{Status: r.Status, Message: r.Message, Error: r.Error}
	var drift []string
	if msg, ok := strings.CutPrefix(r.Error, "drift: "); ok {
		for _, d := range strings.Split(msg, "; ") {
			if d = strings.TrimSpace(d); d != "" {
				drift = append(drift, d)
			}
		}
	}
	return dr, drift
}

// RequestUndo queues the undo of change id for approval. The person must have seen the same plan (PreviewHash), typed
// the account name, given a reason and, when later changes would be overwritten, said so (Force).
func (e *Engine) RequestUndo(ctx context.Context, id, by string, in UndoRequest) (model.Change, error) {
	pv, err := e.PreviewUndoItems(ctx, id, in.Items)
	if err != nil {
		return model.Change{}, err
	}
	if !pv.Undoable {
		return model.Change{}, fmt.Errorf("%w: %s", store.ErrConflict, pv.Reason)
	}
	if in.PreviewHash != pv.Hash {
		return model.Change{}, fmt.Errorf("%w: the undo changed since you previewed it; preview again", store.ErrConflict)
	}
	if !strings.EqualFold(strings.TrimSpace(in.Confirm), pv.ConfirmText) {
		return model.Change{}, fmt.Errorf("%w: type %s to confirm the undo", ErrInvalid, pv.ConfirmText)
	}
	reason := strings.TrimSpace(in.Reason)
	if len([]rune(reason)) < MinUndoReason {
		return model.Change{}, fmt.Errorf("%w: give a reason of at least %d characters", ErrInvalid, MinUndoReason)
	}
	var holds []model.HoldSpec
	for _, h := range in.Holds {
		ok := false
		for _, o := range pv.Holds {
			if o.Kind == h.Kind && strings.EqualFold(o.Key, h.Key) {
				ok, h.Key = true, o.Key
			}
		}
		if !ok {
			return model.Change{}, fmt.Errorf("%w: hold %s %q was not offered for this undo", ErrInvalid, h.Kind, h.Key)
		}
		if h.Until != nil && !h.Until.After(e.Now()) {
			return model.Change{}, fmt.Errorf("%w: a hold's end date must be in the future", ErrInvalid)
		}
		holds = append(holds, h)
	}
	switch {
	case len(pv.Drift) > 0 && !in.Force:
		return model.Change{}, fmt.Errorf("%w: later changes would be overwritten (%s); preview again and confirm overwriting them", store.ErrConflict, strings.Join(pv.Drift, "; "))
	case len(pv.Drift) == 0 && pv.DryRun.Status == ad.StatusFailed:
		return model.Change{}, fmt.Errorf("%w: the dry run failed: %s; fix the cause and preview again", store.ErrConflict, firstNonEmpty(pv.DryRun.Error, pv.DryRun.Message, "unknown error"))
	}

	return e.queueUndo(ctx, pv, by, reason, in.Force && len(pv.Drift) > 0, holds, "")
}

// queueUndo stores a previewed undo as a pending undo change (batch groups the undos of one run request).
func (e *Engine) queueUndo(ctx context.Context, pv UndoPreview, by, reason string, force bool, holds []model.HoldSpec, batch string) (model.Change, error) {
	orig := pv.Change
	p := pv.Payload
	u := *p.Undo
	u.Force, u.Reason, u.Holds, u.RequestedBy, u.Batch = force, reason, holds, by, batch
	p.Undo = &u
	// An earlier failed undo of the same parts is replaced by this one (retrying it would run the old plan).
	if failed, err := e.Store.ListChangesWithStatus(ctx, model.StatusFailed); err == nil {
		for _, f := range failed {
			if f.Type == model.ChangeUndo && f.Payload.Undo != nil && f.Payload.Undo.Of == orig.ID && overlaps(f.Payload.Undo.Items, u.Items) {
				e.supersede(ctx, f, map[string]int{})
			}
		}
	}
	applied := ""
	if orig.AppliedAt != nil {
		applied = " applied " + orig.AppliedAt.UTC().Format("2006-01-02 15:04") + " UTC"
	}
	summary := []string{fmt.Sprintf("UNDO of %s%s (requested by %s): %s", orig.Type, applied, by, reason)}
	if u.Force {
		summary = append(summary, "OVERWRITES later changes: "+strings.Join(pv.Drift, "; "))
	}
	if len(u.Items) > 0 {
		summary = append(summary, fmt.Sprintf("Undoes %d of %d part(s) of the change", len(u.Items), len(pv.Items)))
	}
	summary = append(summary, pv.Steps...)
	for _, h := range holds {
		summary = append(summary, "Then set to Manual: "+holdText(h))
	}
	c := model.Change{
		ID: uuid.NewString(), IdentityID: orig.IdentityID, ExternalID: orig.ExternalID, Display: orig.Display,
		Type: model.ChangeUndo, Status: model.StatusPending, Payload: p, Summary: summary,
		PayloadHash: PayloadHash(model.ChangeUndo, p), CreatedAt: e.Now(),
	}
	if err := e.Store.InsertChange(ctx, c); err != nil {
		return model.Change{}, err
	}
	e.audit(ctx, by, "change.undo_requested", "change", c.ID, map[string]any{
		"of": orig.ID, "of_type": orig.Type, "external_id": orig.ExternalID, "reason": reason, "force": u.Force, "holds": holds,
		"items": u.Items, "batch": batch, "identity_id": orig.IdentityID,
	})
	return e.Store.GetChange(ctx, c.ID)
}

func holdText(h model.HoldSpec) string {
	t := h.Kind
	if h.Key != "" {
		t += " " + h.Key
	}
	if h.Until != nil {
		t += " until " + h.Until.UTC().Format("2006-01-02")
	}
	return t
}

// UndoCheck re-runs an undo change's inverse with -DryRun (for the approval dialog).
func (e *Engine) UndoCheck(ctx context.Context, id string) (UndoDryRun, []string, []string, string, error) {
	c, err := e.Store.GetChange(ctx, id)
	if err != nil {
		return UndoDryRun{}, nil, nil, "", err
	}
	if c.Type != model.ChangeUndo || c.Payload.Undo == nil {
		return UndoDryRun{}, nil, nil, "", fmt.Errorf("%w: not an undo change", ErrInvalid)
	}
	s, err := e.Store.GetSettings(ctx)
	if err != nil {
		return UndoDryRun{}, nil, nil, "", err
	}
	ident, _ := e.Store.GetIdentity(ctx, c.IdentityID)
	dr, drift := e.dryRunUndo(ctx, s, c)
	sam := ident.ADSam
	if c.Payload.Undo.ConfirmText != "" {
		sam = c.Payload.Undo.ConfirmText
	}
	return dr, drift, undoSteps(c.Payload, sam, s.AD.IDAttribute), c.Payload.Undo.ConfirmText, nil
}

// runUndo runs undo changes through their scripts (grouped by script) and returns one result per change, in order.
func (e *Engine) runUndo(ctx context.Context, s model.Settings, changes []model.Change, dryRun bool) []ad.Result {
	opts := ad.ActionOpts{Server: s.AD.Server, IDAttribute: s.AD.IDAttribute, DryRun: dryRun}
	byScript := map[string][]model.Change{}
	results := map[string]ad.Result{}
	for _, c := range changes {
		if c.Payload.Undo == nil {
			results[c.ID] = ad.Result{ChangeID: c.ID, Status: ad.StatusFailed, Error: "undo change without undo details"}
			continue
		}
		byScript[c.Payload.Undo.Script] = append(byScript[c.Payload.Undo.Script], c)
	}
	for _, script := range []string{model.UndoScriptLink, model.UndoScriptSet, model.UndoScriptEnable, model.UndoScriptDisable, model.UndoScriptCloud} {
		batch := byScript[script]
		if len(batch) == 0 {
			continue
		}
		var got []ad.Result
		var err error
		switch script {
		case model.UndoScriptSet:
			items := make([]ad.UpdateItem, 0, len(batch))
			for _, c := range batch {
				u := c.Payload.Undo
				items = append(items, ad.UpdateItem{ChangeID: c.ID, ExternalID: c.ExternalID, ObjectGUID: c.Payload.ObjectGUID, Set: undoSet(c.Payload),
					Clear: c.Payload.Clear, AddGroups: c.Payload.AddGroups, RemoveGroups: c.Payload.RemoveGroups, MoveTo: c.Payload.MoveTo, Expect: u.Expect, Force: u.Force})
			}
			got, err = e.AD.Update(ctx, opts, items)
		case model.UndoScriptEnable:
			items := make([]ad.EnableItem, 0, len(batch))
			for _, c := range batch {
				u := c.Payload.Undo
				items = append(items, ad.EnableItem{ChangeID: c.ID, ExternalID: c.ExternalID, ObjectGUID: c.Payload.ObjectGUID, MoveTo: c.Payload.MoveTo,
					AddGroups: c.Payload.AddGroups, RemoveGroups: c.Payload.RemoveGroups, Set: undoSet(c.Payload), Clear: c.Payload.Clear, Expect: u.Expect, Force: u.Force})
			}
			got, err = e.AD.Enable(ctx, opts, items)
		case model.UndoScriptDisable:
			items := make([]ad.DisableItem, 0, len(batch))
			for _, c := range batch {
				u := c.Payload.Undo
				items = append(items, ad.DisableItem{ChangeID: c.ID, ExternalID: c.ExternalID, ObjectGUID: c.Payload.ObjectGUID, MoveTo: c.Payload.MoveTo,
					AddGroups: c.Payload.AddGroups, RemoveGroups: c.Payload.RemoveGroups, Set: undoSet(c.Payload), Clear: c.Payload.Clear, Expect: u.Expect, Force: u.Force})
			}
			got, err = e.AD.Disable(ctx, opts, items)
		case model.UndoScriptLink:
			items := make([]ad.LinkItem, 0, len(batch))
			for _, c := range batch {
				u := c.Payload.Undo
				items = append(items, ad.LinkItem{ChangeID: c.ID, ObjectGUID: c.Payload.ObjectGUID, ExternalID: valueOf(u.LinkTo), ExpectedExternalID: u.LinkFrom,
					Rename: u.Rename, ProxyAddresses: u.ProxyAddresses, Expect: u.Expect, Force: u.Force})
			}
			got, err = e.AD.Link(ctx, opts, items)
		case model.UndoScriptCloud:
			var conn cloud.Connection
			if conn, err = e.cloudConnection(s); err == nil {
				items := make([]cloud.SetItem, 0, len(batch))
				for _, c := range batch {
					items = append(items, cloud.SetItem{ChangeID: c.ID, ExternalID: c.ExternalID, UserID: c.Payload.CloudUserID, Add: c.Payload.CloudAdd, Remove: c.Payload.CloudRemove})
				}
				got, err = e.Cloud.SetGroups(ctx, conn, dryRun, items)
			}
		}
		byID := map[string]ad.Result{}
		for _, r := range got {
			byID[r.ChangeID] = r
		}
		for _, c := range batch {
			r, ok := byID[c.ID]
			switch {
			case err != nil:
				r = ad.Result{ChangeID: c.ID, Status: ad.StatusFailed, Error: err.Error()}
			case !ok:
				r = ad.Result{ChangeID: c.ID, Status: ad.StatusFailed, Error: "no result returned"}
			}
			results[c.ID] = r
		}
		if err != nil {
			e.Log.Error("undo action failed", "script", script, "count", len(batch), "dry_run", dryRun, "error", err)
		}
	}
	out := make([]ad.Result, len(changes))
	for i, c := range changes {
		out[i] = results[c.ID]
	}
	return out
}

func undoSet(p model.ChangePayload) map[string]string {
	if len(p.Set) == 0 {
		return nil
	}
	out := make(map[string]string, len(p.Set))
	for k, d := range p.Set {
		out[k] = d.To
	}
	return out
}

// finishUndo records an applied undo: the original change is marked undone, the person's record follows the account,
// and the holds the request asked for start.
func (e *Engine) finishUndo(ctx context.Context, c model.Change, r ad.Result) {
	u := c.Payload.Undo
	e.markUndone(ctx, u.Of, c.ID, u.Items)
	if id, err := e.Store.GetIdentity(ctx, c.IdentityID); err != nil {
		e.Log.Error("load identity after undo", "identity", c.IdentityID, "error", err)
	} else {
		prev := id
		now := e.Now()
		switch u.Script {
		case model.UndoScriptEnable:
			id.State, id.DisabledAt = model.StateLinked, nil
		case model.UndoScriptDisable:
			id.State, id.DisabledAt = model.StateDisabled, &now
		case model.UndoScriptLink:
			if valueOf(u.LinkTo) == id.ExternalID && valueOf(u.LinkTo) != "" {
				id.ADGUID, id.State, id.DisabledAt = firstNonEmpty(r.ObjectGUID, c.Payload.ObjectGUID), model.StateLinked, nil
				id.Applied = map[string]string{}
			} else {
				guid := firstNonEmpty(r.ObjectGUID, c.Payload.ObjectGUID)
				id.ADGUID, id.ADDN, id.ADSam, id.State, id.DisabledAt = "", "", "", model.StateNew, nil
				id.Applied = map[string]string{}
				if err := e.Store.AddIdentityLinkRejects(ctx, id.ID, guid); err != nil {
					e.Log.Error("record unlinked account", "identity", id.ID, "error", err)
				}
			}
		}
		if id.ADGUID != "" && strings.EqualFold(id.ADGUID, r.ObjectGUID) {
			if r.DN != "" {
				id.ADDN = r.DN
			}
			if r.Sam != "" {
				id.ADSam = r.Sam
			}
		}
		if err := e.Store.UpdateIdentity(ctx, id); err != nil {
			e.Log.Error("update identity after undo", "identity", id.ID, "error", err)
		} else {
			e.noteState(ctx, prev, id, c.ID, c.Type)
		}
	}
	for _, h := range u.Holds {
		hold := model.Hold{ID: uuid.NewString(), IdentityID: c.IdentityID, Kind: h.Kind, Key: h.Key, Until: h.Until, Reason: u.Reason,
			CreatedBy: u.RequestedBy, CreatedAt: e.Now(), ChangeID: c.ID}
		if err := e.Store.InsertHold(ctx, hold); err != nil {
			e.Log.Error("add hold", "identity", c.IdentityID, "error", err)
			continue
		}
		e.audit(ctx, "system", "identity.hold_added", "identity", c.IdentityID, map[string]any{
			"hold": hold.ID, "kind": hold.Kind, "key": hold.Key, "until": hold.Until, "change": c.ID, "by": u.RequestedBy,
		})
	}
	e.audit(ctx, "system", "change.undone", "change", u.Of, map[string]any{"undo": c.ID, "script": u.Script, "external_id": c.ExternalID, "items": u.Items, "identity_id": c.IdentityID})
}

// ReleaseHold ends one hold of an identity (sets that item back to Auto).
func (e *Engine) ReleaseHold(ctx context.Context, identityID, holdID, by string) (model.Hold, error) {
	return e.ReleaseHoldNote(ctx, identityID, holdID, by, "")
}

// ReleaseHoldNote is ReleaseHold with a note for the history.
func (e *Engine) ReleaseHoldNote(ctx context.Context, identityID, holdID, by, note string) (model.Hold, error) {
	h, err := e.Store.DeleteHold(ctx, identityID, holdID)
	if err != nil {
		return h, err
	}
	detail := map[string]any{"hold": h.ID, "kind": h.Kind, "key": h.Key, "change": h.ChangeID}
	if note = strings.TrimSpace(note); note != "" {
		detail["note"] = note
	}
	e.audit(ctx, by, "identity.hold_released", "identity", identityID, detail)
	return h, nil
}
