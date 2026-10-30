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
)

// ErrPreviewBusy: another impact preview is running.
var ErrPreviewBusy = errors.New("an impact preview is already running")

// SnapshotMaxAge is how long a snapshot taken by a sync or a preview is reused by the next preview.
var SnapshotMaxAge = 15 * time.Minute

type cachedSnapshot struct {
	req  ad.SnapshotRequest
	snap *ad.Snapshot
	at   time.Time
}

func (e *Engine) cacheSnapshot(req ad.SnapshotRequest, snap *ad.Snapshot) {
	e.snapMu.Lock()
	defer e.snapMu.Unlock()
	e.snapCache = &cachedSnapshot{req: req, snap: snap, at: e.Now()}
}

// sameScope: both requests read the same directory scope.
func sameScope(a, b ad.SnapshotRequest) bool {
	return strings.EqualFold(a.Server, b.Server) && strings.EqualFold(a.IDAttribute, b.IDAttribute) && sameFold(a.SearchBases, b.SearchBases)
}

// covers reports whether a snapshot taken with have answers want (same directory scope; at least the properties,
// groups and unlinked accounts asked for).
func covers(have, want ad.SnapshotRequest) bool {
	if !sameScope(have, want) || (want.IncludeUnlinked && !have.IncludeUnlinked) {
		return false
	}
	return subsetFold(want.Properties, have.Properties) && subsetFold(want.Groups, have.Groups)
}

func subsetFold(small, big []string) bool {
	set := map[string]bool{}
	for _, v := range big {
		set[strings.ToLower(v)] = true
	}
	for _, v := range small {
		if !set[strings.ToLower(v)] {
			return false
		}
	}
	return true
}

func sameFold(a, b []string) bool { return subsetFold(a, b) && subsetFold(b, a) }

// mergeRequests is a snapshot request that answers both.
func mergeRequests(a, b ad.SnapshotRequest) ad.SnapshotRequest {
	out := a
	out.SearchBases = unionFold(a.SearchBases, b.SearchBases) // a proposed retention policy may name another disabled OU
	out.Properties = unionFold(a.Properties, b.Properties)
	out.Groups = unionFold(a.Groups, b.Groups)
	out.IncludeUnlinked = a.IncludeUnlinked || b.IncludeUnlinked
	return out
}

// ImpactRequest is what an impact preview compares: the proposed roles, mappings and settings (nil = the saved ones)
// against the saved configuration, plus filters for the returned page.
type ImpactRequest struct {
	Roles    *[]model.RoleRule
	Mappings *[]model.Mapping
	// RetentionPolicies is a proposed full list of retention policies (nil = the saved ones).
	RetentionPolicies *[]model.RetentionPolicy
	Settings          *model.Settings
	Refresh           bool
	Q                 string
	OnlyChanged       bool
	Type              string
	Offset            int
	Limit             int
	By                string
}

// ImpactSide is one configuration's plan for a person.
type ImpactSide struct {
	Type    string   `json:"type"` // a change type, or "error"
	Summary []string `json:"summary"`
	// Also lists the other change types of this side (cloud_groups next to an AD change); Summary covers all of them.
	Also []string `json:"also,omitempty"`
}

// has reports whether the side plans a change of this type.
func (s *ImpactSide) has(typ string) bool {
	if s == nil {
		return false
	}
	if s.Type == typ {
		return true
	}
	for _, t := range s.Also {
		if t == typ {
			return true
		}
	}
	return false
}

// ImpactLine is one difference between the baseline and the proposed plan.
type ImpactLine struct {
	Op   string `json:"op"` // + (only in the proposed plan) or - (only in the baseline)
	Text string `json:"text"`
}

// ImpactItem is one person whose plan exists in either configuration.
type ImpactItem struct {
	IdentityID string       `json:"identity_id"`
	ExternalID string       `json:"external_id"`
	Display    string       `json:"display"`
	ADSam      string       `json:"ad_sam"`
	State      string       `json:"state"`
	Difference string       `json:"difference"` // new | changed | gone | same
	Baseline   *ImpactSide  `json:"baseline"`
	Proposed   *ImpactSide  `json:"proposed"`
	Lines      []ImpactLine `json:"lines"`
}

// ImpactCount is a group or attribute and how many people the difference touches.
type ImpactCount struct {
	Group  string `json:"group,omitempty"`
	Attr   string `json:"attr,omitempty"`
	People int    `json:"people"`
}

// ImpactSummary covers everyone, not just the returned page.
type ImpactSummary struct {
	ChangedPeople int            `json:"changed_people"`
	Baseline      map[string]int `json:"baseline"`
	Proposed      map[string]int `json:"proposed"`
	GroupsAdded   []ImpactCount  `json:"groups_added"`
	GroupsRemoved []ImpactCount  `json:"groups_removed"`
	Attributes    []ImpactCount  `json:"attributes"`
}

// ImpactResult is the answer of an impact preview.
type ImpactResult struct {
	GeneratedAt  time.Time     `json:"generated_at"`
	SnapshotAt   time.Time     `json:"snapshot_at"`
	People       int           `json:"people"`
	NamesChecked bool          `json:"names_checked"`
	Notes        []string      `json:"notes"`
	Summary      ImpactSummary `json:"summary"`
	Items        []ImpactItem  `json:"items"`
	Total        int           `json:"total"`
}

// PreviewImpact plans every person with the saved configuration (baseline) and with the proposed one against the same
// AD snapshot and the stored source records, and reports the differences. It writes nothing (store or AD), does not
// fetch sources and does not check account names in AD. It refuses to run during a sync (ErrRunning) or while another
// preview runs (ErrPreviewBusy).
func (e *Engine) PreviewImpact(ctx context.Context, req ImpactRequest) (*ImpactResult, error) {
	if !e.previewing.CompareAndSwap(false, true) {
		return nil, ErrPreviewBusy
	}
	defer e.previewing.Store(false)
	if !e.runMu.TryLock() {
		return nil, ErrRunning
	}
	defer e.runMu.Unlock()

	st := e.Store
	saved, err := st.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	mappings, err := st.ListMappings(ctx)
	if err != nil {
		return nil, err
	}
	rules, err := st.ListRoles(ctx)
	if err != nil {
		return nil, err
	}
	holds, err := st.ActiveHolds(ctx, e.Now())
	if err != nil {
		return nil, err
	}
	retention, err := st.ListRetentionPolicies(ctx)
	if err != nil {
		return nil, err
	}
	srcList, err := st.ListSources(ctx)
	if err != nil {
		return nil, err
	}
	base := planConfig{Settings: saved, Mappings: mappings, Rules: rules, Holds: holds, Retention: retention, Sources: srcList}
	prop := base
	var notes []string
	if req.Settings != nil {
		s := *req.Settings
		if err := s.Validate(); err != nil {
			return nil, fmt.Errorf("%w: settings: %v", ErrInvalid, err)
		}
		if !strings.EqualFold(s.AD.Server, saved.AD.Server) || !sameFold(s.AD.SearchBases, saved.AD.SearchBases) ||
			!strings.EqualFold(s.AD.IDAttribute, saved.AD.IDAttribute) || !strings.EqualFold(s.AD.DisabledOU, saved.AD.DisabledOU) {
			notes = append(notes, "the AD server, search bases, id attribute and disabled OU are previewed with the saved values")
			s.AD.Server, s.AD.SearchBases, s.AD.IDAttribute = saved.AD.Server, saved.AD.SearchBases, saved.AD.IDAttribute
			s.AD.DisabledOU = saved.AD.DisabledOU
		}
		// The cloud connection is previewed as saved too (switching it on or off is previewed).
		if c := s.Cloud; c.Environment != saved.Cloud.Environment || c.TenantID != saved.Cloud.TenantID || c.ClientID != saved.Cloud.ClientID ||
			c.Auth != saved.Cloud.Auth || c.CertThumbprint != saved.Cloud.CertThumbprint || c.UserMatch != saved.Cloud.UserMatch || c.Exchange != saved.Cloud.Exchange {
			notes = append(notes, "the Microsoft 365 connection is previewed with the saved values")
		}
		enabled := s.Cloud.Enabled
		s.Cloud = saved.Cloud
		s.Cloud.Enabled = enabled
		prop.Settings = s
	}
	if req.Roles != nil {
		for i, r := range *req.Roles {
			if err := ValidateRole(r); err != nil {
				return nil, fmt.Errorf("%w: roles[%d] (%s): %v", ErrInvalid, i, r.Name, err)
			}
		}
		prop.Rules = *req.Roles
	}
	if req.Mappings != nil {
		for i, m := range *req.Mappings {
			if err := ValidateMapping(m); err != nil {
				return nil, fmt.Errorf("%w: mappings[%d] (%s): %v", ErrInvalid, i, m.Name, err)
			}
		}
		prop.Mappings = *req.Mappings
	}
	if req.RetentionPolicies != nil {
		list := make([]model.RetentionPolicy, len(*req.RetentionPolicies))
		for i, p := range *req.RetentionPolicies {
			NormalizeRetentionPolicy(&p)
			if err := ValidateRetentionPolicy(p); err != nil {
				return nil, fmt.Errorf("%w: retention_policies[%d] (%s): %v", ErrInvalid, i, p.Name, err)
			}
			list[i] = p
		}
		prop.Retention = list
	}
	if len(saved.AD.SearchBases) == 0 {
		return nil, fmt.Errorf("%w: ad.search_bases is not configured", ErrInvalid)
	}

	// The people a sync would plan: the stored records of every enabled source.
	srcs, err := st.ListSources(ctx)
	if err != nil {
		return nil, err
	}
	var idents []model.Identity
	for _, s := range srcs {
		if !s.Enabled {
			continue
		}
		list, err := st.ListIdentitiesBySource(ctx, s.ID)
		if err != nil {
			return nil, err
		}
		idents = append(idents, list...)
	}

	snap, snapAt, err := e.previewSnapshot(ctx, mergeRequests(snapshotRequestFor(base), snapshotRequestFor(prop)), req.Refresh)
	if err != nil {
		return nil, fmt.Errorf("AD snapshot: %w", err)
	}
	var cv *cloudView
	switch {
	case needsCloud(base) || needsCloud(prop):
		if !saved.Cloud.Enabled {
			notes = append(notes, "cloud groups are not previewed: switch the Microsoft 365 connection on and save it first")
		} else if cv, _, err = e.cloudSnapshot(ctx, saved, unionFold(cloudGroupIDs(base), cloudGroupIDs(prop)), !req.Refresh); err != nil {
			cv = nil
			notes = append(notes, "cloud groups are not previewed: "+err.Error())
		}
	case len(cloudGroupIDs(prop)) > 0:
		notes = append(notes, "cloud groups are configured but the Microsoft 365 connection is off; they are not previewed")
	}
	now := e.Now()
	bp, err := planEveryone(base, snap, cv, idents, now, map[string]int{}, nil)
	if err != nil {
		return nil, err
	}
	pp, err := planEveryone(prop, snap, cv, idents, now, map[string]int{}, nil)
	if err != nil {
		return nil, err
	}

	res := buildImpact(bp, pp, idents, cv)
	res.GeneratedAt, res.SnapshotAt, res.NamesChecked = now, snapAt, false
	res.Notes = append(notes, "account names are not checked against AD in a preview (a sync does that)")
	res.People = len(idents)
	filterImpact(res, req)
	e.audit(ctx, orStr(req.By, "system"), "preview.impact", "preview", "", map[string]any{
		"people": res.People, "changed_people": res.Summary.ChangedPeople, "roles": req.Roles != nil, "mappings": req.Mappings != nil, "settings": req.Settings != nil,
	})
	return res, nil
}

// previewSnapshot reuses the last snapshot (sync or preview) when it is recent and covers want, else takes one.
func (e *Engine) previewSnapshot(ctx context.Context, want ad.SnapshotRequest, refresh bool) (*ad.Snapshot, time.Time, error) {
	e.snapMu.Lock()
	c := e.snapCache
	e.snapMu.Unlock()
	if !refresh && c != nil && e.Now().Sub(c.at) < SnapshotMaxAge && covers(c.req, want) {
		return c.snap, c.at, nil
	}
	if c != nil && sameScope(c.req, want) {
		want = mergeRequests(want, c.req) // keep what the cache answered too
	}
	snap, err := e.AD.Snapshot(ctx, want)
	if err != nil {
		return nil, time.Time{}, err
	}
	e.cacheSnapshot(want, snap)
	return snap, e.Now(), nil
}

// cloudAllKey stands for "every cloud group" in the impact counts (a disable that removes all cloud memberships).
const cloudAllKey = model.CloudGroupPrefix + "*"

// groupAdds / groupRemoves are the groups plans add or remove: AD DNs, and entra:<id> for cloud groups.
func groupAdds(plans ...*Planned) map[string]bool {
	out := map[string]bool{}
	for _, p := range plans {
		if p == nil {
			continue
		}
		for _, g := range append(append([]string{}, p.Payload.Groups...), p.Payload.AddGroups...) {
			out[strings.ToLower(g)] = true
		}
		for _, g := range p.Payload.CloudAdd {
			out[model.CloudGroupPrefix+g] = true
		}
	}
	return out
}

func groupRemoves(plans ...*Planned) map[string]bool {
	out := map[string]bool{}
	for _, p := range plans {
		if p == nil {
			continue
		}
		for _, g := range p.Payload.RemoveGroups {
			out[strings.ToLower(g)] = true
		}
		for _, g := range p.Payload.CloudRemove {
			out[model.CloudGroupPrefix+g] = true
		}
		if p.Payload.CloudRemoveAll {
			out[cloudAllKey] = true
		}
	}
	return out
}

// attrValues are the attribute values a plan writes.
func attrValues(p *Planned) map[string]string {
	out := map[string]string{}
	if p == nil {
		return out
	}
	for k, v := range p.Payload.Attributes {
		out[strings.ToLower(k)] = v
	}
	for k, d := range p.Payload.Set {
		out[strings.ToLower(k)] = d.To
	}
	for _, k := range p.Payload.Clear {
		out[strings.ToLower(k)] = ""
	}
	return out
}

// samePlan: both plans exist and carry the same payload, or neither exists.
func samePlan(a, b *Planned) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return PayloadHash(a.Type, a.Payload) == PayloadHash(b.Type, b.Payload)
}

func buildImpact(bp, pp *planPass, idents []model.Identity, cv *cloudView) *ImpactResult {
	res := &ImpactResult{Summary: ImpactSummary{Baseline: map[string]int{}, Proposed: map[string]int{}, GroupsAdded: []ImpactCount{}, GroupsRemoved: []ImpactCount{}, Attributes: []ImpactCount{}}, Items: []ImpactItem{}}
	planIndex := func(list []plannedFor) map[string]*Planned {
		m := map[string]*Planned{}
		for i := range list {
			m[list[i].id.ID] = list[i].p
		}
		return m
	}
	bIdx, pIdx, bCloud, pCloud := planIndex(bp.plans), planIndex(pp.plans), planIndex(bp.cloud), planIndex(pp.cloud)
	// side is one configuration's view of a person: the AD plan and the cloud plan in one summary.
	side := func(pass *planPass, idx, cidx map[string]*Planned, id string) (*ImpactSide, *Planned, *Planned) {
		p, cp := idx[id], cidx[id]
		switch {
		case p != nil:
			s := &ImpactSide{Type: p.Type, Summary: append([]string{}, p.Summary...)}
			if cp != nil {
				s.Also = []string{cp.Type}
				s.Summary = append(s.Summary, cp.Summary...)
			}
			return s, p, cp
		case pass.errs[id] != "":
			return &ImpactSide{Type: "error", Summary: []string{pass.errs[id]}}, nil, nil
		case cp != nil:
			return &ImpactSide{Type: cp.Type, Summary: append([]string{}, cp.Summary...)}, nil, cp
		}
		return nil, nil, nil
	}
	added, removed, attrs := map[string]int{}, map[string]int{}, map[string]int{}
	display := map[string]string{}
	for _, id := range idents {
		bs, bplan, bcloud := side(bp, bIdx, bCloud, id.ID)
		ps, pplan, pcloud := side(pp, pIdx, pCloud, id.ID)
		if bs == nil && ps == nil {
			continue
		}
		if bs != nil {
			for _, t := range append([]string{bs.Type}, bs.Also...) {
				res.Summary.Baseline[t]++
			}
		}
		if ps != nil {
			for _, t := range append([]string{ps.Type}, ps.Also...) {
				res.Summary.Proposed[t]++
			}
		}
		cur := pp.byID[id.ID]
		item := ImpactItem{IdentityID: id.ID, ExternalID: id.ExternalID, Display: id.Display, ADSam: cur.ADSam, State: cur.State, Baseline: bs, Proposed: ps, Lines: []ImpactLine{}}
		switch {
		case bs == nil:
			item.Difference = "new"
		case ps == nil:
			item.Difference = "gone"
		case bs.Type != ps.Type || strings.Join(bs.Summary, "\n") != strings.Join(ps.Summary, "\n") ||
			(bplan != nil && pplan != nil && !samePlan(bplan, pplan)) || !samePlan(bcloud, pcloud):
			item.Difference = "changed"
		default:
			item.Difference = "same"
		}
		if item.Difference != "same" {
			res.Summary.ChangedPeople++
			inB, inP := map[string]bool{}, map[string]bool{}
			if bs != nil {
				for _, l := range bs.Summary {
					inB[l] = true
				}
			}
			if ps != nil {
				for _, l := range ps.Summary {
					inP[l] = true
					if !inB[l] {
						item.Lines = append(item.Lines, ImpactLine{Op: "+", Text: l})
					}
				}
			}
			if bs != nil {
				for _, l := range bs.Summary {
					if !inP[l] {
						item.Lines = append(item.Lines, ImpactLine{Op: "-", Text: l})
					}
				}
			}
			ba, pa := groupAdds(bplan, bcloud), groupAdds(pplan, pcloud)
			for g := range pa {
				if !ba[g] {
					added[g]++
				}
			}
			br, pr := groupRemoves(bplan, bcloud), groupRemoves(pplan, pcloud)
			for g := range pr {
				if !br[g] {
					removed[g]++
				}
			}
			bv, pv := attrValues(bplan), attrValues(pplan)
			for k, v := range pv {
				if old, ok := bv[k]; !ok || old != v {
					attrs[k]++
				}
			}
		}
		res.Items = append(res.Items, item)
	}
	// Group names as AD spells them.
	for _, pass := range []*planPass{pp, bp} {
		for _, dn := range pass.pc.GroupDN {
			display[strings.ToLower(dn)] = dn
		}
	}
	if cv != nil {
		for id := range cv.groups {
			display[model.CloudGroupPrefix+id] = "Entra: " + cv.name(id)
		}
	}
	display[cloudAllKey] = "Entra: all cloud groups (disable)"
	toCounts := func(m map[string]int, group bool) []ImpactCount {
		out := []ImpactCount{}
		for k, n := range m {
			c := ImpactCount{People: n}
			if group {
				c.Group = orStr(display[k], k)
			} else {
				c.Attr = k
			}
			out = append(out, c)
		}
		sort.Slice(out, func(i, j int) bool {
			if out[i].People != out[j].People {
				return out[i].People > out[j].People
			}
			return out[i].Group+out[i].Attr < out[j].Group+out[j].Attr
		})
		return out
	}
	res.Summary.GroupsAdded, res.Summary.GroupsRemoved, res.Summary.Attributes = toCounts(added, true), toCounts(removed, true), toCounts(attrs, false)
	sort.SliceStable(res.Items, func(i, j int) bool {
		a, b := strings.ToLower(res.Items[i].Display), strings.ToLower(res.Items[j].Display)
		if a != b {
			return a < b
		}
		return res.Items[i].ExternalID < res.Items[j].ExternalID
	})
	return res
}

// MaxImpactPage caps the items one preview response returns.
const MaxImpactPage = 500

func filterImpact(res *ImpactResult, req ImpactRequest) {
	q := strings.ToLower(strings.TrimSpace(req.Q))
	var keep []ImpactItem
	for _, it := range res.Items {
		if req.OnlyChanged && it.Difference == "same" {
			continue
		}
		if req.Type != "" && !it.Proposed.has(req.Type) && !it.Baseline.has(req.Type) {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(it.Display), q) && !strings.Contains(strings.ToLower(it.ExternalID), q) &&
			!strings.Contains(strings.ToLower(it.ADSam), q) {
			continue
		}
		keep = append(keep, it)
	}
	res.Total = len(keep)
	limit := req.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit > MaxImpactPage {
		limit = MaxImpactPage
	}
	off := min(max(req.Offset, 0), len(keep))
	end := min(off+limit, len(keep))
	res.Items = append([]ImpactItem{}, keep[off:end]...)
}
