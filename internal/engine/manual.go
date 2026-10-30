package engine

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/model"
	"idlifecycle/internal/store"
)

// Manual pins: a person marks an attribute, a group, the OU, the account state or the whole person as Manual, so the
// planner leaves it alone until someone sets it back to Auto. They are holds (identity_holds); an undo creates them
// too. Setting back to Auto is ReleaseHold.

var (
	ldapNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,63}$`)
	guidRe     = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)
)

// PinRequest asks for one Manual pin without an undo (e.g. someone fixed AD by hand and wants it kept).
type PinRequest struct {
	Kind   string     `json:"kind"`
	Key    string     `json:"key"`
	Reason string     `json:"reason"`
	Until  *time.Time `json:"until"`
}

// PinManual marks one item of a person as Manual.
func (e *Engine) PinManual(ctx context.Context, identityID, by string, in PinRequest) (model.Hold, error) {
	id, err := e.Store.GetIdentity(ctx, identityID)
	if err != nil {
		return model.Hold{}, err
	}
	if id.State == model.StateMerged {
		return model.Hold{}, fmt.Errorf("%w: this record was merged into another record; pin items on that record", store.ErrConflict)
	}
	kind, key := strings.TrimSpace(in.Kind), strings.TrimSpace(in.Key)
	switch kind {
	case model.HoldAttribute:
		if !ldapNameRe.MatchString(key) {
			return model.Hold{}, fmt.Errorf("%w: %q is not an LDAP attribute name", ErrInvalid, key)
		}
	case model.HoldGroup:
		if key == "" || len(key) > 255 || strings.ContainsAny(key, "\r\n") {
			return model.Hold{}, fmt.Errorf("%w: name the group (its DN, as shown for the person)", ErrInvalid)
		}
	case model.HoldCloudGroup:
		key = strings.ToLower(strings.TrimPrefix(key, model.CloudGroupPrefix))
		if !guidRe.MatchString(key) {
			return model.Hold{}, fmt.Errorf("%w: %q is not an Entra group object id", ErrInvalid, in.Key)
		}
	case model.HoldOU, model.HoldState, model.HoldAll:
		if key != "" {
			return model.Hold{}, fmt.Errorf("%w: a %s pin has no key", ErrInvalid, kind)
		}
	default:
		return model.Hold{}, fmt.Errorf("%w: unknown kind %q (attribute, group, cloud_group, ou, state or all)", ErrInvalid, in.Kind)
	}
	reason := strings.TrimSpace(in.Reason)
	if len([]rune(reason)) < MinUndoReason {
		return model.Hold{}, fmt.Errorf("%w: give a reason of at least %d characters", ErrInvalid, MinUndoReason)
	}
	if in.Until != nil && !in.Until.After(e.Now()) {
		return model.Hold{}, fmt.Errorf("%w: the end date must be in the future", ErrInvalid)
	}
	existing, err := e.Store.ListHolds(ctx, identityID)
	if err != nil {
		return model.Hold{}, err
	}
	for _, h := range existing {
		if h.Kind == kind && strings.EqualFold(h.Key, key) && h.Active(e.Now()) {
			return model.Hold{}, fmt.Errorf("%w: this is already Manual (since %s, by %s)", store.ErrConflict, h.CreatedAt.UTC().Format("2006-01-02"), h.CreatedBy)
		}
	}
	h := model.Hold{ID: uuid.NewString(), IdentityID: identityID, Kind: kind, Key: key, Until: in.Until, Reason: reason, CreatedBy: by, CreatedAt: e.Now()}
	if err := e.Store.InsertHold(ctx, h); err != nil {
		return model.Hold{}, err
	}
	e.audit(ctx, by, "identity.hold_added", "identity", identityID, map[string]any{
		"hold": h.ID, "kind": kind, "key": key, "until": in.Until, "by": by, "reason": reason, "manual": true, "external_id": id.ExternalID,
		"inactive": !id.Active,
	})
	return h, nil
}

// ManagedAttr is one attribute IDLifecycle manages for a person.
type ManagedAttr struct {
	Name    string      `json:"name"`
	ADValue *string     `json:"ad_value"` // nil = empty in AD (or no account)
	Desired *string     `json:"desired"`  // what the mappings / role rules would set now (nil = nothing / unresolved)
	Mode    string      `json:"mode"`     // auto | manual
	Sync    string      `json:"sync"`     // the mapping mode: create_only | sync | immutable
	Hold    *model.Hold `json:"hold,omitempty"`
}

// ManagedGroup is one group IDLifecycle manages that the person is in or should be in.
type ManagedGroup struct {
	Group   string      `json:"group"` // DN
	Name    string      `json:"name"`
	Member  *bool       `json:"member"` // nil = no account
	Desired bool        `json:"desired"`
	Mode    string      `json:"mode"`
	Hold    *model.Hold `json:"hold,omitempty"`
}

// ManagedOU is the account's OU and the OU the rules would place it in.
type ManagedOU struct {
	Current string      `json:"current"`
	Desired string      `json:"desired"`
	Mode    string      `json:"mode"`
	Hold    *model.Hold `json:"hold,omitempty"`
}

// ManagedPerson is the person-level mode: auto, state (no create / link / disable / enable / delete) or manual (all).
type ManagedPerson struct {
	Mode   string      `json:"mode"`
	Hold   *model.Hold `json:"hold,omitempty"`
	Active bool        `json:"active"`
}

// ManagedView is what IDLifecycle manages for one person and which parts are Manual.
type ManagedView struct {
	IdentityID string         `json:"identity_id"`
	SnapshotAt time.Time      `json:"snapshot_at"`
	Account    bool           `json:"account"`
	Attributes []ManagedAttr  `json:"attributes"`
	Groups     []ManagedGroup `json:"groups"`
	OU         *ManagedOU     `json:"ou"`
	Person     ManagedPerson  `json:"person"`
	Holds      []model.Hold   `json:"holds"`
	Warnings   []string       `json:"warnings"`
}

// Managed lists the attributes, groups and OU IDLifecycle manages for one person: the value in AD (from the cached AD
// snapshot, at most SnapshotMaxAge old; refresh takes a new one), what the rules would set now, and whether it is Auto
// or Manual. It writes nothing.
func (e *Engine) Managed(ctx context.Context, identityID string, refresh bool) (ManagedView, error) {
	v := ManagedView{IdentityID: identityID, Attributes: []ManagedAttr{}, Groups: []ManagedGroup{}, Holds: []model.Hold{}, Warnings: []string{}}
	id, err := e.Store.GetIdentity(ctx, identityID)
	if err != nil {
		return v, err
	}
	s, err := e.Store.GetSettings(ctx)
	if err != nil {
		return v, err
	}
	mappings, err := e.Store.ListMappings(ctx)
	if err != nil {
		return v, err
	}
	rules, err := e.Store.ListRoles(ctx)
	if err != nil {
		return v, err
	}
	all, err := e.Store.ListHolds(ctx, identityID)
	if err != nil {
		return v, err
	}
	now := e.Now()
	var holds []model.Hold
	for _, h := range all {
		if h.Active(now) {
			holds = append(holds, h)
		}
	}
	v.Holds = append(v.Holds, holds...)
	find := func(kind, key string) *model.Hold {
		for i := range holds {
			if holds[i].Kind == kind && (kind == model.HoldOU || kind == model.HoldState || kind == model.HoldAll || strings.EqualFold(holds[i].Key, key)) {
				return &holds[i]
			}
		}
		return nil
	}
	mode := func(h *model.Hold) string {
		if h != nil {
			return "manual"
		}
		return "auto"
	}
	v.Person = ManagedPerson{Mode: "auto", Active: id.Active}
	if h := find(model.HoldAll, ""); h != nil {
		v.Person.Mode, v.Person.Hold = "manual", h
	} else if h := find(model.HoldState, ""); h != nil {
		v.Person.Mode, v.Person.Hold = "state", h
	}
	if v.Person.Mode != "auto" && !id.Active {
		v.Warnings = append(v.Warnings, "This person is no longer active in the source but is Manual: IDLifecycle will not disable or delete the account until it is set back to Auto.")
	}

	retention, err := e.Store.ListRetentionPolicies(ctx)
	if err != nil {
		return v, err
	}
	cfg := planConfig{Settings: s, Mappings: mappings, Rules: rules, Retention: retention}
	snap, at, err := e.previewSnapshot(ctx, snapshotRequestFor(cfg), refresh)
	if err != nil {
		return v, fmt.Errorf("AD snapshot: %w", err)
	}
	v.SnapshotAt = at
	pc := &PlanContext{Settings: s, Mappings: mappings, Rules: rules, Now: now,
		GroupDN: map[string]string{}, ManagedDNs: map[string]bool{}, UserDNByExt: map[string]string{}}
	managed := managedGroupIDs(cfg)
	for _, g := range snap.Groups {
		if g.Found {
			pc.GroupDN[strings.ToLower(g.Identity)] = g.DN
			if managed[strings.ToLower(strings.TrimSpace(g.Identity))] {
				pc.ManagedDNs[strings.ToLower(g.DN)] = true
			}
		}
	}
	var acct *ad.User
	for ext, list := range linkedByExt(snap) {
		if len(list) == 1 {
			pc.UserDNByExt[ext] = list[0].DN
		}
		if strings.EqualFold(ext, id.ExternalID) {
			for _, u := range list {
				if id.ADGUID == "" || strings.EqualFold(u.ObjectGUID, id.ADGUID) {
					acct = u
				}
			}
		}
	}
	v.Account = acct != nil

	res := ResolveRolesAt(rules, id.Attrs, now)
	maps := mergeMappings(mappings, res.Attrs)
	desired, warns, err := renderAll(pc, id, maps)
	if err != nil {
		v.Warnings = append(v.Warnings, err.Error())
	}
	v.Warnings = append(v.Warnings, warns...)
	for _, m := range maps {
		if strings.EqualFold(m.Attr, s.AD.IDAttribute) || IsProtectedAttribute(m.Attr) {
			continue
		}
		a := ManagedAttr{Name: m.Attr, Sync: m.Mode, Hold: find(model.HoldAttribute, m.Attr)}
		a.Mode = mode(a.Hold)
		if r, ok := desired[strings.ToLower(m.Attr)]; ok && r.OK && r.Value != "" {
			a.Desired = strp(r.Value)
		}
		if acct != nil {
			if val := acct.Attr(m.Attr); val != "" {
				a.ADValue = strp(val)
			}
		}
		v.Attributes = append(v.Attributes, a)
	}
	sort.SliceStable(v.Attributes, func(i, j int) bool {
		return strings.ToLower(v.Attributes[i].Name) < strings.ToLower(v.Attributes[j].Name)
	})

	groupDNs, gw := resolveGroups(pc, adGroupsOnly(append(append([]string{}, res.Groups...), s.Lifecycle.ActiveAdd...)))
	v.Warnings = append(v.Warnings, gw...)
	want := map[string]bool{}
	if id.Active {
		for _, g := range groupDNs {
			want[strings.ToLower(g)] = true
		}
	}
	seen := map[string]bool{}
	add := func(dn string) {
		l := strings.ToLower(dn)
		if seen[l] {
			return
		}
		seen[l] = true
		g := ManagedGroup{Group: dn, Name: shortCN(dn), Desired: want[l], Hold: find(model.HoldGroup, dn)}
		g.Mode = mode(g.Hold)
		if acct != nil {
			m := inGroups(acct.MemberOf, dn)
			g.Member = &m
		}
		v.Groups = append(v.Groups, g)
	}
	for _, g := range groupDNs {
		add(g)
	}
	if acct != nil {
		for _, g := range acct.MemberOf {
			if pc.ManagedDNs[strings.ToLower(g)] {
				add(g)
			}
		}
	}
	for _, h := range holds { // a pinned group the rules no longer name still shows, so it can be set back to Auto
		if h.Kind == model.HoldGroup {
			add(h.Key)
		}
	}
	sort.SliceStable(v.Groups, func(i, j int) bool { return strings.ToLower(v.Groups[i].Name) < strings.ToLower(v.Groups[j].Name) })

	place := ResolvePlacement(s, id, res.OU)
	ou := &ManagedOU{Desired: place.OU, Hold: find(model.HoldOU, "")}
	ou.Mode = mode(ou.Hold)
	if acct != nil {
		ou.Current = parentDN(acct.DN)
	}
	if ou.Current != "" || ou.Desired != "" || ou.Hold != nil {
		v.OU = ou
	}
	return v, nil
}
