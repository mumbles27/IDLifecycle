package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/model"
	"idlifecycle/internal/sources"
	"idlifecycle/internal/store"
)

// UpcomingItem is one leaver on the Leavers page: an account due for deletion (kind "delete") or a person whose
// termination date is coming up (kind "leaving").
type UpcomingItem struct {
	Kind       string     `json:"kind"` // delete | leaving
	IdentityID string     `json:"identity_id"`
	Display    string     `json:"display"`
	ExternalID string     `json:"external_id"`
	ADSam      string     `json:"ad_sam"`
	DisabledAt *time.Time `json:"disabled_at,omitempty"`
	DeleteDue  *time.Time `json:"delete_due,omitempty"`
	LeaveDate  string     `json:"leave_date,omitempty"`
	PolicyID   string     `json:"policy_id"`
	PolicyName string     `json:"policy_name"`
	Reason     string     `json:"reason"`
	AssignedBy string     `json:"assigned_by,omitempty"`
	ManualHold bool       `json:"manual_hold"` // a Manual pin (whole person or state) stops the delete
	sortKey    time.Time
}

// RetentionUpcoming lists disabled accounts due for deletion within days (overdue ones too) and active people whose
// termination date falls within days, soonest first.
func (e *Engine) RetentionUpcoming(ctx context.Context, days int) ([]UpcomingItem, error) {
	if days <= 0 {
		days = 30
	}
	s, err := e.Store.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	resolved, err := e.Store.ListIdentityRetention(ctx)
	if err != nil {
		return nil, err
	}
	holds, err := e.Store.ActiveHolds(ctx, e.Now())
	if err != nil {
		return nil, err
	}
	srcs, err := e.Store.ListSources(ctx)
	if err != nil {
		return nil, err
	}
	loc := scheduleLocation(s)
	now := e.Now()
	horizon := now.Add(time.Duration(days) * 24 * time.Hour)
	l := now.In(loc)
	today := time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.UTC)
	def := DefaultRetention(s)

	out := []UpcomingItem{}
	for _, src := range srcs {
		list, err := e.Store.ListIdentitiesBySource(ctx, src.ID)
		if err != nil {
			return nil, err
		}
		for _, id := range list {
			manual := false
			for _, h := range holds[id.ID] {
				if h.Kind == model.HoldAll || h.Kind == model.HoldState {
					manual = true
				}
			}
			it := UpcomingItem{IdentityID: id.ID, Display: id.Display, ExternalID: id.ExternalID, ADSam: id.ADSam, ManualHold: manual}
			r, ok := resolved[id.ID]
			if !ok {
				r = model.IdentityRetention{PolicyID: def.ID, PolicyName: def.Name, Reason: model.RetentionReasonDefault, DeleteDue: deleteDue(def, id.DisabledAt)}
			}
			it.PolicyID, it.PolicyName, it.Reason = r.PolicyID, r.PolicyName, r.Reason
			if r.Reason == model.RetentionReasonAssigned {
				it.AssignedBy = id.RetentionAssignedBy
			}
			if id.State == model.StateDisabled && id.DisabledAt != nil {
				if r.DeleteDue != nil && !r.DeleteDue.After(horizon) {
					it.Kind, it.DisabledAt, it.DeleteDue, it.sortKey = "delete", id.DisabledAt, r.DeleteDue, *r.DeleteDue
					out = append(out, it)
				}
				continue
			}
			if !id.Active || src.Options.TerminationDateField == "" {
				continue
			}
			ld := sources.LeaveDate(src.Options, lookupAttr(id.Attrs, src.Options.TerminationDateField))
			d, err := time.Parse(time.DateOnly, ld)
			if ld == "" || err != nil || d.Before(today) || d.After(today.AddDate(0, 0, days)) {
				continue
			}
			it.Kind, it.LeaveDate, it.sortKey = "leaving", ld, d
			out = append(out, it)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].sortKey.Equal(out[j].sortKey) {
			return out[i].sortKey.Before(out[j].sortKey)
		}
		return out[i].Display < out[j].Display
	})
	return out, nil
}

func scheduleLocation(s model.Settings) *time.Location {
	if loc, err := time.LoadLocation(s.Schedule.Timezone); err == nil && s.Schedule.Timezone != "" {
		return loc
	}
	return time.UTC
}

// RetentionTest is the result of "Test this policy" for one person.
type RetentionTest struct {
	Matched bool          `json:"matched"`
	Explain []ExplainLine `json:"explain"`
	// Note says when the account's AD groups were not known (no recent AD snapshot).
	Note string `json:"note,omitempty"`
}

// TestRetentionPolicy evaluates an (unsaved) policy against a stored person, or against a typed record when
// identityID is empty. The account's groups come from the last AD snapshot when one is cached.
func (e *Engine) TestRetentionPolicy(ctx context.Context, p model.RetentionPolicy, identityID string, record map[string]string) (RetentionTest, error) {
	NormalizeRetentionPolicy(&p)
	if p.Name == "" {
		p.Name = "test"
	}
	if err := ValidateRetentionPolicy(p); err != nil {
		return RetentionTest{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	id := model.Identity{Attrs: record}
	srcName, origin := "", OriginSource
	if identityID != "" {
		var err error
		if id, err = e.Store.GetIdentity(ctx, identityID); err != nil {
			return RetentionTest{}, err
		}
		srcName = id.SourceName
		if src, err := e.Store.GetSource(ctx, id.SourceID); err == nil {
			origin = originOf(src.Type)
		}
	}
	if id.Attrs == nil {
		id.Attrs = map[string]string{}
	}
	acct := e.cachedAccount(id)
	out := RetentionTest{}
	if identityID != "" && acct == nil && id.ADGUID != "" {
		out.Note = "The account's AD groups are not known (no recent AD snapshot): ad.memberOf conditions see no groups. Run Preview impact or a sync first."
	}
	out.Matched, out.Explain = ExplainRetention(p, id, acct, srcName, origin, e.Now())
	return out, nil
}

// cachedAccount finds a person's account in the cached AD snapshot (nil when there is none).
func (e *Engine) cachedAccount(id model.Identity) *ad.User {
	if id.ExternalID == "" {
		return nil
	}
	e.snapMu.Lock()
	c := e.snapCache
	e.snapMu.Unlock()
	if c == nil {
		return nil
	}
	for i := range c.snap.Users {
		u := &c.snap.Users[i]
		if strings.EqualFold(u.ExternalID, id.ExternalID) && (id.ADGUID == "" || strings.EqualFold(u.ObjectGUID, id.ADGUID)) {
			return u
		}
	}
	return nil
}

// SetIdentityRetention assigns a retention policy to a person ("" = automatic, "default" = the default policy, or a
// policy id). It takes effect at the next sync. reason is required (10+ characters).
func (e *Engine) SetIdentityRetention(ctx context.Context, identityID, policyID, reason, by string) (model.Identity, error) {
	reason = strings.TrimSpace(reason)
	if len([]rune(reason)) < 10 {
		return model.Identity{}, fmt.Errorf("%w: reason must be at least 10 characters", ErrInvalid)
	}
	id, err := e.Store.GetIdentity(ctx, identityID)
	if err != nil {
		return id, err
	}
	name := "Automatic"
	switch policyID {
	case "":
	case model.RetentionDefaultID:
		name = "Default"
	default:
		p, err := e.Store.GetRetentionPolicy(ctx, policyID)
		if errors.Is(err, store.ErrNotFound) {
			return id, fmt.Errorf("%w: retention policy %q does not exist", ErrInvalid, policyID)
		}
		if err != nil {
			return id, err
		}
		name = p.Name
	}
	if err := e.Store.SetIdentityRetention(ctx, identityID, policyID, by); err != nil {
		return id, err
	}
	e.identityAudit(ctx, by, ActRetentionSet, id, map[string]any{"from": id.RetentionPolicyID, "to": policyID, "to_name": name, "reason": reason})
	return e.Store.GetIdentity(ctx, identityID)
}

// IdentityRetentionInfo is what the identity page shows: the policy the last sync resolved (or what applies now when
// nothing was resolved yet), who assigned it, the delete date and the termination date from the source record.
func (e *Engine) IdentityRetentionInfo(ctx context.Context, id model.Identity) (model.IdentityRetention, error) {
	r, err := e.Store.GetIdentityRetention(ctx, id.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return r, err
	}
	s, serr := e.Store.GetSettings(ctx)
	if serr != nil {
		return r, serr
	}
	if errors.Is(err, store.ErrNotFound) {
		def := DefaultRetention(s)
		r = model.IdentityRetention{PolicyID: def.ID, PolicyName: def.Name, Reason: model.RetentionReasonDefault, DeleteDue: deleteDue(def, id.DisabledAt)}
	}
	// A person's assignment wins from the next sync on: show it right away.
	if id.RetentionPolicyID != "" && r.Reason != model.RetentionReasonAssigned {
		switch id.RetentionPolicyID {
		case model.RetentionDefaultID:
			def := DefaultRetention(s)
			r.PolicyID, r.PolicyName, r.Reason, r.DeleteDue = def.ID, def.Name, model.RetentionReasonAssigned, deleteDue(def, id.DisabledAt)
		default:
			if p, err := e.Store.GetRetentionPolicy(ctx, id.RetentionPolicyID); err == nil && p.Enabled {
				r.PolicyID, r.PolicyName, r.Reason, r.DeleteDue = p.ID, p.Name, model.RetentionReasonAssigned, deleteDue(p, id.DisabledAt)
			}
		}
	}
	if r.Reason == model.RetentionReasonAssigned {
		r.AssignedBy = id.RetentionAssignedBy
	}
	if r.PolicyID == model.RetentionDefaultID {
		r.DeleteAfterDays = DefaultRetention(s).DeleteAfterDays
	} else if p, err := e.Store.GetRetentionPolicy(ctx, r.PolicyID); err == nil {
		r.DeleteAfterDays = p.DeleteAfterDays
	}
	if id.State != model.StateDisabled {
		r.DeleteDue = nil
	}
	if src, err := e.Store.GetSource(ctx, id.SourceID); err == nil && src.Options.TerminationDateField != "" {
		r.LeaveDate = sources.LeaveDate(src.Options, lookupAttr(id.Attrs, src.Options.TerminationDateField))
	}
	return r, nil
}
