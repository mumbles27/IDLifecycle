package engine

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/cloud"
	"idlifecycle/internal/model"
)

// cloudRemovedAllKey records in Identity.Applied that a disabled person's cloud memberships were all removed
// (lifecycle_groups.disabled_remove all_except). That removal cannot be observed in the snapshot (it only holds the
// groups the configuration names), so it is planned once per disable; the key is cleared when the person is active again.
const cloudRemovedAllKey = "~cloud_removed_all"

// ErrNoCloud: the service has no cloud client, or the Microsoft 365 connection is off.
var ErrNoCloud = errors.New("the Microsoft 365 connection is not configured")

// adGroupsOnly drops cloud group references (entra:<id>) from a group list.
func adGroupsOnly(list []string) []string {
	out := make([]string, 0, len(list))
	for _, g := range list {
		if !model.IsCloudGroupRef(g) {
			out = append(out, g)
		}
	}
	return out
}

// cloudIDs returns the group object ids of the cloud references in a group list (de-duplicated, in order).
func cloudIDs(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lists {
		for _, g := range l {
			if id, ok := model.CloudGroupID(g); ok && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

// cloudGroupIDs lists every cloud group a configuration names (enabled rules and the lifecycle lists).
func cloudGroupIDs(cfg planConfig) []string {
	return cloudIDs(AllRuleGroups(cfg.Rules), lifecycleGroupIDs(cfg.Settings))
}

// needsCloud: the configuration has the Microsoft 365 connection on and something for it to do.
func needsCloud(cfg planConfig) bool {
	s := cfg.Settings
	return s.Cloud.Enabled && (len(cloudGroupIDs(cfg)) > 0 || disabledRemoveMode(s) == model.DisabledRemoveAllExcept)
}

// cloudView is a cloud snapshot indexed for planning.
type cloudView struct {
	match  string
	users  map[string]*cloud.User // by match key
	groups map[string]cloud.SnapshotGroup
	member map[string]map[string]bool // group id -> user ids
}

func newCloudView(s *cloud.Snapshot, match string) *cloudView {
	cv := &cloudView{match: match, users: map[string]*cloud.User{}, groups: map[string]cloud.SnapshotGroup{}, member: map[string]map[string]bool{}}
	for i := range s.Users {
		u := &s.Users[i]
		if k := cloudUserKey(match, u.SID, u.ImmutableID, u.UPN); k != "" {
			cv.users[k] = u
		}
	}
	for _, g := range s.Groups {
		id := strings.ToLower(g.ID)
		cv.groups[id] = g
		m := map[string]bool{}
		for _, u := range g.Members {
			m[strings.ToLower(u)] = true
		}
		cv.member[id] = m
	}
	return cv
}

func cloudUserKey(match, sid, immutableID, upn string) string {
	switch match {
	case model.CloudMatchImmutableID:
		return immutableID // base64: case matters
	case model.CloudMatchUPN:
		return strings.ToLower(strings.TrimSpace(upn))
	}
	return strings.ToUpper(strings.TrimSpace(sid))
}

// immutableIDFor is the onPremisesImmutableId Entra Connect writes for an objectGUID: base64 of the GUID's bytes in
// Microsoft's (mixed-endian) order.
func immutableIDFor(guid string) string {
	u, err := uuid.Parse(guid)
	if err != nil {
		return ""
	}
	b := u[:]
	le := []byte{b[3], b[2], b[1], b[0], b[5], b[4], b[7], b[6]}
	le = append(le, b[8:]...)
	return base64.StdEncoding.EncodeToString(le)
}

// userFor finds the Entra ID user of an AD account (nil when it is not synced yet).
func (cv *cloudView) userFor(acct *ad.User) *cloud.User {
	var k string
	switch cv.match {
	case model.CloudMatchImmutableID:
		k = immutableIDFor(acct.ObjectGUID)
	case model.CloudMatchUPN:
		k = strings.ToLower(strings.TrimSpace(acct.UPN))
	default:
		k = strings.ToUpper(strings.TrimSpace(acct.SID))
	}
	if k == "" {
		return nil
	}
	return cv.users[k]
}

// usable: the group exists and its membership can be changed.
func (cv *cloudView) usable(id string) bool {
	g, ok := cv.groups[id]
	return ok && g.Found && g.Manageable
}

func (cv *cloudView) name(id string) string {
	if g, ok := cv.groups[id]; ok && g.Name != "" {
		return g.Name
	}
	return id
}

func (cv *cloudView) label(id string) string {
	g := cv.groups[id]
	return fmt.Sprintf("%s (%s)", cv.name(id), cloud.KindLabel(g.Kind))
}

// warnings for the cloud groups a configuration names that cannot be used.
func (cv *cloudView) warnings(ids []string) []string {
	var out []string
	for _, id := range ids {
		g, ok := cv.groups[id]
		switch {
		case !ok || !g.Found:
			out = append(out, fmt.Sprintf("Entra ID group %s not found; skipped", id))
		case !g.Manageable:
			out = append(out, fmt.Sprintf("Entra ID group %q is %s; skipped", cv.name(id), cloud.ReasonText(g.Reason)))
		}
	}
	return out
}

// planCloud decides one person's cloud group changes (nil when nothing must change). observed is bookkeeping for
// Identity.Applied; waiting reports an account whose Entra ID user does not exist yet (not synced).
func planCloud(pc *PlanContext, cv *cloudView, id model.Identity, acct *ad.User) (p *Planned, observed map[string]string, waiting bool) {
	if acct == nil {
		return nil, nil, false
	}
	u := cv.userFor(acct)
	if u == nil {
		return nil, nil, true
	}
	s := pc.Settings
	l := s.Lifecycle
	usable := func(ids []string) []string {
		var out []string
		for _, g := range ids {
			if cv.usable(g) {
				out = append(out, g)
			}
		}
		return out
	}
	held := func(g string) bool { return cv.member[g][strings.ToLower(u.ID)] }
	set := func(ids []string) map[string]bool {
		m := map[string]bool{}
		for _, g := range ids {
			m[g] = true
		}
		return m
	}
	managed := usable(cloudIDs(AllRuleGroups(pc.Rules), l.ActiveAdd, l.DisabledAdd))
	disabledAdd := usable(cloudIDs(l.DisabledAdd))
	keepIDs := cloudIDs(l.DisabledKeep, l.DisabledAdd)
	keep := set(keepIDs)

	var add, remove []string
	removeAll := false
	if id.Active {
		if id.Applied[cloudRemovedAllKey] != "" {
			observed = map[string]string{cloudRemovedAllKey: ""}
		}
		want := set(usable(cloudIDs(ResolveRolesAt(pc.Rules, id.Attrs, pc.now()).Groups, l.ActiveAdd)))
		for _, g := range sortedKeys(want) {
			if !held(g) {
				add = append(add, g)
			}
		}
		rm := map[string]bool{}
		if s.Engine.RemoveStaleGroups {
			for _, g := range managed {
				if held(g) && !want[g] {
					rm[g] = true
				}
			}
		}
		if !acct.Enabled { // rehire: groups only disabled accounts are in go
			for _, g := range disabledAdd {
				if held(g) {
					rm[g] = true
				}
			}
		}
		remove = sortedKeys(rm)
	} else {
		for _, g := range disabledAdd {
			if !held(g) {
				add = append(add, g)
			}
		}
		mode := disabledRemoveMode(s)
		switch {
		case mode == model.DisabledRemoveAllExcept && (acct.Enabled || id.Applied[cloudRemovedAllKey] != "1"):
			removeAll = true
		case mode != model.DisabledRemoveNone:
			for _, g := range managed {
				if held(g) && !keep[g] {
					remove = append(remove, g)
				}
			}
		}
		sort.Strings(add)
		sort.Strings(remove)
	}
	add, remove = dropHeldCloud(pc, id.ID, add, "add to", cv.label), dropHeldCloud(pc, id.ID, remove, "remove from", cv.label)
	if len(add) == 0 && len(remove) == 0 && !removeAll {
		return nil, observed, false
	}
	p = &Planned{Type: model.ChangeCloudGroups, Payload: model.ChangePayload{
		ObjectGUID: acct.ObjectGUID, DN: acct.DN, CloudUserID: strings.ToLower(u.ID), CloudAdd: add, CloudRemove: remove, CloudRemoveAll: removeAll,
	}}
	for _, g := range add {
		p.Summary = append(p.Summary, "Add to Entra group "+cv.label(g))
	}
	for _, g := range remove {
		p.Summary = append(p.Summary, "Remove from Entra group "+cv.label(g))
	}
	if removeAll {
		for _, h := range pc.Holds[id.ID] { // a held cloud group is kept by remove-all too
			if h.Kind == model.HoldCloudGroup && h.Active(pc.now()) && !keep[strings.ToLower(h.Key)] && held(strings.ToLower(h.Key)) {
				keepIDs = append(keepIDs, strings.ToLower(h.Key))
				keep[strings.ToLower(h.Key)] = true
				pc.OnHold++
				pc.suppress(h.ID, "would remove from Entra group "+cv.label(strings.ToLower(h.Key))+" (remove from all groups)")
			}
		}
		sort.Strings(keepIDs)
		p.Payload.CloudKeep = keepIDs
		names := make([]string, 0, len(keepIDs))
		for _, g := range keepIDs {
			names = append(names, cv.name(g))
		}
		line := "Remove from all cloud groups (role-assignable, dynamic and AD-synced groups are left alone)"
		if len(names) > 0 {
			line = "Remove from all cloud groups except " + strings.Join(names, ", ") + " (role-assignable, dynamic and AD-synced groups are left alone)"
		}
		p.Summary = append(p.Summary, line)
	}
	return p, observed, false
}

// cloudRemovals reports whether a cloud plan takes memberships away (for the bulk-destructive guard).
func cloudRemovals(p *Planned) bool {
	return p.Type == model.ChangeCloudGroups && (len(p.Payload.CloudRemove) > 0 || p.Payload.CloudRemoveAll)
}

// ---- snapshot -------------------------------------------------------------------

type cachedCloud struct {
	req  cloud.SnapshotRequest
	snap *cloud.Snapshot
	at   time.Time
}

// cloudConnection is the saved connection, ready for a script.
func (e *Engine) cloudConnection(s model.Settings) (cloud.Connection, error) {
	if e.Cloud == nil || !s.Cloud.Enabled {
		return cloud.Connection{}, ErrNoCloud
	}
	return cloud.ConnectionFrom(s.Cloud, e.Cipher)
}

// cloudSnapshot reads the groups ids names (and every synced user) with the saved connection. reuse allows a recent
// cached snapshot that covers the request (previews).
func (e *Engine) cloudSnapshot(ctx context.Context, s model.Settings, ids []string, reuse bool) (*cloudView, time.Time, error) {
	conn, err := e.cloudConnection(s)
	if err != nil {
		return nil, time.Time{}, err
	}
	req := cloud.SnapshotRequest{Groups: ids, Match: s.Cloud.UserMatch, IncludeUsers: true}
	if req.Groups == nil {
		req.Groups = []string{}
	}
	e.snapMu.Lock()
	c := e.cloudCache
	e.snapMu.Unlock()
	if reuse && c != nil && c.req.Match == req.Match && e.Now().Sub(c.at) < SnapshotMaxAge && subsetFold(req.Groups, c.req.Groups) {
		return newCloudView(c.snap, req.Match), c.at, nil
	}
	if reuse && c != nil && c.req.Match == req.Match {
		req.Groups = unionFold(req.Groups, c.req.Groups)
	}
	snap, err := e.Cloud.Snapshot(ctx, conn, req)
	if err != nil {
		return nil, time.Time{}, err
	}
	now := e.Now()
	e.snapMu.Lock()
	e.cloudCache = &cachedCloud{req: req, snap: snap, at: now}
	e.snapMu.Unlock()
	return newCloudView(snap, req.Match), now, nil
}

// CachedCloudGroups returns what the last cloud snapshot knows about these group ids (for showing names).
func (e *Engine) CachedCloudGroups(ids []string) map[string]cloud.Group {
	e.snapMu.Lock()
	c := e.cloudCache
	e.snapMu.Unlock()
	out := map[string]cloud.Group{}
	if c == nil {
		return out
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[strings.ToLower(id)] = true
	}
	for _, g := range c.snap.Groups {
		if id := strings.ToLower(g.ID); want[id] && g.Found {
			out[id] = g.Group
		}
	}
	return out
}

// markCloudRemovedAll records that a disabled person's cloud memberships were removed.
func (e *Engine) markCloudRemovedAll(ctx context.Context, identityID string) {
	id, err := e.Store.GetIdentity(ctx, identityID)
	if err != nil {
		e.Log.Error("load identity after cloud groups change", "identity", identityID, "error", err)
		return
	}
	if id.Applied == nil {
		id.Applied = map[string]string{}
	}
	id.Applied[cloudRemovedAllKey] = "1"
	if err := e.Store.UpdateIdentity(ctx, id); err != nil {
		e.Log.Error("update identity after cloud groups change", "identity", id.ID, "error", err)
	}
}
