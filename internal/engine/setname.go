package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/model"
)

// ErrNameTaken: a name a person chose is already in use (the conflicts say where).
var ErrNameTaken = errors.New("name already in use")

// NameTakenError lists the conflicts of a chosen name.
type NameTakenError struct{ Conflicts []model.NameConflict }

func (e *NameTakenError) Error() string { return conflictText(e.Conflicts) }
func (e *NameTakenError) Unwrap() error { return ErrNameTaken }

// openCreate loads a pending (or simulated) create.
func (e *Engine) openChange(ctx context.Context, changeID, typ, what string) (model.Change, error) {
	c, err := e.Store.GetChange(ctx, changeID)
	if err != nil {
		return c, err
	}
	if c.Type != typ {
		return c, fmt.Errorf("%w: only a %s change has %s", ErrInvalid, typ, what)
	}
	if c.Status != model.StatusPending && c.Status != model.StatusSimulated {
		return c, fmt.Errorf("%w: %s can only be changed while the change is pending (it is %s)", ErrInvalid, what, c.Status)
	}
	return c, nil
}

// CheckNames asks AD (and the other open creates) whether the candidate's names are free. exclude is the change
// to leave out of the open-create comparison; skipGUID an AD object that does not count (the account being renamed).
func (e *Engine) CheckNames(ctx context.Context, s model.Settings, c model.Candidate, ou, excludeChange, skipGUID string) ([]model.NameConflict, error) {
	keys := CandidateQueries(c, ou)
	req := ad.NameCheckRequest{Server: s.AD.Server, Exclude: []string{}}
	if skipGUID != "" {
		req.Exclude = append(req.Exclude, skipGUID)
	}
	for _, k := range keys {
		req.Names = append(req.Names, ad.NameQuery{Field: k.field, Value: k.value, OU: k.ou})
	}
	taken, err := e.AD.CheckNames(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("checking the names in AD: %w", err)
	}
	var out []model.NameConflict
	for _, t := range taken {
		if t.Field == ad.NameCN && !strings.EqualFold(parentDN(t.DN), ou) {
			continue
		}
		out = append(out, model.NameConflict{Field: t.Field, Value: t.Value, DN: t.DN})
	}
	open, err := e.Store.ListChangesWithStatus(ctx, model.StatusPending, model.StatusSimulated, model.StatusApproved, model.StatusApplying)
	if err != nil {
		return nil, err
	}
	for _, oc := range open {
		if oc.ID == excludeChange {
			continue
		}
		var other *model.Candidate
		switch {
		case oc.Type == model.ChangeCreate && !oc.Payload.NeedsName && len(oc.Payload.Candidates) > 0:
			other = &oc.Payload.Candidates[0]
		case oc.Type == model.ChangeLink && oc.Payload.Link != nil && oc.Payload.Link.Rename != nil:
			other = oc.Payload.Link.Rename
		default:
			continue
		}
		who := fmt.Sprintf("%s (%s)", oc.Display, oc.ExternalID)
		for _, pair := range [][3]string{{"sam", c.Sam, other.Sam}, {"upn", c.UPN, other.UPN}, {"mail", c.Mail, other.Mail}} {
			if pair[1] != "" && strings.EqualFold(pair[1], pair[2]) {
				out = append(out, model.NameConflict{Field: pair[0], Value: pair[1], With: who})
			}
		}
	}
	return out, nil
}

// ChangeNames returns what a person needs to choose a name for a create: the current names, why they are taken, and
// free suggestions from every automatic mode.
func (e *Engine) ChangeNames(ctx context.Context, changeID string) (*model.Candidate, []model.NameConflict, []NameSuggestion, error) {
	c, err := e.openChange(ctx, changeID, model.ChangeCreate, "account names")
	if err != nil {
		return nil, nil, nil, err
	}
	s, err := e.Store.GetSettings(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	id, err := e.Store.GetIdentity(ctx, c.IdentityID)
	if err != nil {
		return nil, nil, nil, err
	}
	var cur *model.Candidate
	if len(c.Payload.Candidates) > 0 {
		x := c.Payload.Candidates[0]
		cur = &x
	}
	withMail := cur != nil && cur.Mail != ""
	ou := c.Payload.OU
	if c.Payload.NeedsOU {
		ou = ""
	}
	var free []NameSuggestion
	for _, sug := range NameSuggestions(s, id, withMail) {
		if len(free) == 6 {
			break
		}
		conflicts, err := e.CheckNames(ctx, s, sug.Candidate, ou, c.ID, "")
		if err != nil {
			return cur, c.Payload.NameConflicts, nil, err
		}
		if len(conflicts) == 0 {
			free = append(free, sug)
		}
	}
	return cur, c.Payload.NameConflicts, free, nil
}

// SetChangeName puts names a person chose on a pending create (after checking AD and the other open creates) and
// remembers them for the person, so the next sync plans the same create.
func (e *Engine) SetChangeName(ctx context.Context, changeID string, cand model.Candidate, by string) (model.Change, error) {
	c, err := e.openChange(ctx, changeID, model.ChangeCreate, "account names")
	if err != nil {
		return c, err
	}
	s, err := e.Store.GetSettings(ctx)
	if err != nil {
		return c, err
	}
	cand.Sam, cand.UPN, cand.CN, cand.Mail = strings.TrimSpace(cand.Sam), strings.TrimSpace(cand.UPN), strings.TrimSpace(cand.CN), strings.TrimSpace(cand.Mail)
	needMail := len(c.Payload.Candidates) > 0 && c.Payload.Candidates[0].Mail != ""
	if !needMail {
		cand.Mail = "" // a mapping writes mail (or no mail template): the name dialog does not set it
	}
	if err := ValidateCandidate(cand, s.AD.UPNSuffix, needMail); err != nil {
		return c, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	ou := c.Payload.OU
	if c.Payload.NeedsOU {
		ou = ""
	}
	conflicts, err := e.CheckNames(ctx, s, cand, ou, c.ID, "")
	if err != nil {
		return c, err
	}
	if len(conflicts) > 0 {
		return c, &NameTakenError{Conflicts: conflicts}
	}

	p := c.Payload
	prev := model.Candidate{}
	if len(p.Candidates) > 0 {
		prev = p.Candidates[0]
	}
	p.Candidates, p.NeedsName, p.NameConflicts = []model.Candidate{cand}, false, nil
	summary := []string{fmt.Sprintf("Username chosen by %s: %s", by, cand.Sam)}
	for _, line := range c.Summary {
		switch {
		case strings.HasPrefix(line, "Choose a username:"), strings.HasPrefix(line, "WAITING for a username"), strings.HasPrefix(line, "Username chosen by "):
		case strings.HasPrefix(line, "Create account"):
			where := "in " + p.OU
			if p.NeedsOU {
				where = "in the OU chosen at approval"
			}
			who := fmt.Sprintf("account %q (%s)", cand.Sam, cand.CN)
			if cand.Mail != "" {
				who += ", mail " + cand.Mail
			}
			summary = append(summary, "Create "+who+" "+where)
		default:
			summary = append(summary, line)
		}
	}
	if err := e.Store.SetIdentityNameOverride(ctx, c.IdentityID, &cand); err != nil {
		return c, err
	}
	if err := e.Store.UpdateChangePayload(ctx, c.ID, c.Status, p, summary, PayloadHash(c.Type, p)); err != nil {
		return c, err
	}
	e.audit(ctx, by, "change.set_name", "change", c.ID, map[string]any{
		"external_id": c.ExternalID, "sam": cand.Sam, "upn": cand.UPN, "cn": cand.CN, "mail": cand.Mail, "previous_sam": prev.Sam,
	})
	return e.Store.GetChange(ctx, c.ID)
}

// SetChangeLink records a person's decision on a pending link: guid picks one of the candidate accounts (remembered
// for the person, so the next sync proposes the same link); createNew means none of them is this person - the change
// is rejected and the next sync plans a new account.
func (e *Engine) SetChangeLink(ctx context.Context, changeID, guid string, createNew bool, by string) (model.Change, error) {
	c, err := e.openChange(ctx, changeID, model.ChangeLink, "an account choice")
	if err != nil {
		return c, err
	}
	if c.Payload.Link == nil {
		return c, fmt.Errorf("%w: the change has no account candidates", ErrInvalid)
	}
	if createNew {
		if err := e.Store.SetIdentityLinkDecision(ctx, c.IdentityID, model.LinkDecisionNew); err != nil {
			return c, err
		}
		if err := e.Store.TransitionChange(ctx, c.ID, c.Status, model.StatusRejected, by, "none of the accounts is this person: create a new account"); err != nil {
			return c, err
		}
		e.audit(ctx, by, "change.set_link", "change", c.ID, map[string]any{"external_id": c.ExternalID, "create_new": true})
		return e.Store.GetChange(ctx, c.ID)
	}

	var chosen *model.LinkCandidate
	for i := range c.Payload.Link.Candidates {
		if strings.EqualFold(c.Payload.Link.Candidates[i].ObjectGUID, guid) {
			chosen = &c.Payload.Link.Candidates[i]
		}
	}
	if chosen == nil {
		return c, fmt.Errorf("%w: %q is not one of this change's candidate accounts", ErrInvalid, guid)
	}
	s, err := e.Store.GetSettings(ctx)
	if err != nil {
		return c, err
	}
	id, err := e.Store.GetIdentity(ctx, c.IdentityID)
	if err != nil {
		return c, err
	}
	mappings, err := e.Store.ListMappings(ctx)
	if err != nil {
		return c, err
	}
	rules, err := e.Store.ListRoles(ctx)
	if err != nil {
		return c, err
	}
	// Plan exactly what the next sync plans for a person-chosen account, so the change is not replaced.
	cand := *chosen
	cand.Manual = true
	pc := &PlanContext{Settings: s}
	maps := mergeMappings(mappings, ResolveRoles(rules, id.Attrs).Attrs)
	p, err := planLink(pc, id, maps, &model.LinkPayload{Candidates: []model.LinkCandidate{cand}, Chosen: cand.ObjectGUID})
	if err != nil {
		return c, err
	}
	for _, w := range e.checkNames(ctx, s, []plannedFor{{id, p}}) {
		e.Log.Warn("name check", "warning", w)
	}
	summary := append([]string{fmt.Sprintf("Account chosen by %s: %s", by, cand.Sam)}, p.Summary...)
	if err := e.Store.SetIdentityLinkDecision(ctx, id.ID, model.LinkDecisionGUID+cand.ObjectGUID); err != nil {
		return c, err
	}
	if err := e.Store.UpdateChangePayload(ctx, c.ID, c.Status, p.Payload, summary, PayloadHash(c.Type, p.Payload)); err != nil {
		return c, err
	}
	e.audit(ctx, by, "change.set_link", "change", c.ID, map[string]any{"external_id": c.ExternalID, "object_guid": cand.ObjectGUID, "sam": cand.Sam})
	return e.Store.GetChange(ctx, c.ID)
}

// SetIdentityLink sets (guid) or clears a person's link decision for an identity without an account: the next sync
// proposes linking that account (when it is in the search bases and nobody else's).
func (e *Engine) SetIdentityLink(ctx context.Context, identityID, guid string, clear bool, by string) (model.Identity, error) {
	id, err := e.Store.GetIdentity(ctx, identityID)
	if err != nil {
		return id, err
	}
	decision := ""
	if !clear {
		guid = strings.TrimSpace(guid)
		if guid == "" || len(guid) > 64 || strings.Trim(guid, "0123456789abcdefABCDEF-gui") != "" {
			return id, fmt.Errorf("%w: %q is not an objectGUID", ErrInvalid, guid)
		}
		if id.ADGUID != "" {
			return id, fmt.Errorf("%w: %s already has an AD account (%s)", ErrInvalid, id.Display, id.ADSam)
		}
		if id.State == model.StateMerged {
			return id, fmt.Errorf("%w: this record was merged into another one", ErrInvalid)
		}
		decision = model.LinkDecisionGUID + guid
	}
	if err := e.Store.SetIdentityLinkDecision(ctx, id.ID, decision); err != nil {
		return id, err
	}
	e.audit(ctx, by, "identity.link_decision", "identity", id.ID, map[string]any{"external_id": id.ExternalID, "decision": decision, "previous": id.LinkDecision})
	return e.Store.GetIdentity(ctx, id.ID)
}

// PersonPreview is what a sync would plan for a new person typed into the console.
type PersonPreview struct {
	Names     *model.Candidate     `json:"names"`
	NeedsName bool                 `json:"needs_name"`
	OU        string               `json:"ou"`
	OUReason  string               `json:"ou_reason"`
	NeedsOU   bool                 `json:"needs_ou"`
	Conflicts []model.NameConflict `json:"conflicts"`
	Warnings  []string             `json:"warnings"`
}

// PreviewPerson renders the account names and OU for attrs (a full source row) and checks the names in AD. Nothing
// is written.
func (e *Engine) PreviewPerson(ctx context.Context, attrs map[string]string) (PersonPreview, error) {
	out := PersonPreview{Conflicts: []model.NameConflict{}, Warnings: []string{}}
	s, err := e.Store.GetSettings(ctx)
	if err != nil {
		return out, err
	}
	mappings, err := e.Store.ListMappings(ctx)
	if err != nil {
		return out, err
	}
	rules, err := e.Store.ListRoles(ctx)
	if err != nil {
		return out, err
	}
	res := ResolveRoles(rules, attrs)
	mailMapped := false
	for _, m := range mergeMappings(mappings, res.Attrs) {
		if strings.EqualFold(m.Attr, "mail") {
			mailMapped = true
		}
	}
	id := model.Identity{ExternalID: attrs["ExternalID"], Attrs: attrs, Active: true}
	place := ResolvePlacement(s, id, res.OU)
	out.OU, out.OUReason, out.NeedsOU = place.OU, place.Reason, place.Needs
	cands, err := identityCandidates(s, id, !mailMapped)
	if err != nil {
		out.Warnings = append(out.Warnings, err.Error())
		return out, nil
	}
	ou := place.OU
	if place.Needs {
		ou = ""
	}
	for i, c := range cands {
		conflicts, err := e.CheckNames(ctx, s, c, ou, "", "")
		if err != nil {
			out.Warnings = append(out.Warnings, err.Error())
			x := cands[0]
			out.Names = &x
			return out, nil
		}
		if i == 0 && conflicts != nil {
			out.Conflicts = conflicts
		}
		if len(conflicts) == 0 {
			x := c
			out.Names = &x
			return out, nil
		}
	}
	x := cands[0]
	out.Names, out.NeedsName = &x, true
	return out, nil
}
