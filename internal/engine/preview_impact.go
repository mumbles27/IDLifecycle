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
	out.Properties = unionFold(a.Properties, b.Properties)
	out.Groups = unionFold(a.Groups, b.Groups)
	out.IncludeUnlinked = a.IncludeUnlinked || b.IncludeUnlinked
	return out
}

// ImpactRequest is what an impact preview compares: the proposed roles, mappings and settings (nil = the saved ones)
// against the saved configuration, plus filters for the returned page.
type ImpactRequest struct {
	Roles       *[]model.RoleRule
	Mappings    *[]model.Mapping
	Settings    *model.Settings
	Refresh     bool
	Q           string
	OnlyChanged bool
	Type        string
	Offset      int
	Limit       int
	By          string
}

// ImpactSide is one configuration's plan for a person.
type ImpactSide struct {
	Type    string   `json:"type"` // a change type, or "error"
	Summary []string `json:"summary"`
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
	base := planConfig{Settings: saved, Mappings: mappings, Rules: rules}
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
	now := e.Now()
	bp, err := planEveryone(base, snap, idents, now, map[string]int{}, nil)
	if err != nil {
		return nil, err
	}
	pp, err := planEveryone(prop, snap, idents, now, map[string]int{}, nil)
	if err != nil {
		return nil, err
	}

	res := buildImpact(bp, pp, idents)
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

// groupAdds / groupRemoves are the group DNs a plan adds or removes.
func groupAdds(p *Planned) map[string]bool {
	out := map[string]bool{}
	if p == nil {
		return out
	}
	for _, g := range append(append([]string{}, p.Payload.Groups...), p.Payload.AddGroups...) {
		out[strings.ToLower(g)] = true
	}
	return out
}

func groupRemoves(p *Planned) map[string]bool {
	out := map[string]bool{}
	if p == nil {
		return out
	}
	for _, g := range p.Payload.RemoveGroups {
		out[strings.ToLower(g)] = true
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

func buildImpact(bp, pp *planPass, idents []model.Identity) *ImpactResult {
	res := &ImpactResult{Summary: ImpactSummary{Baseline: map[string]int{}, Proposed: map[string]int{}, GroupsAdded: []ImpactCount{}, GroupsRemoved: []ImpactCount{}, Attributes: []ImpactCount{}}, Items: []ImpactItem{}}
	planIndex := func(pass *planPass) map[string]int {
		m := map[string]int{}
		for i := range pass.plans {
			m[pass.plans[i].id.ID] = i
		}
		return m
	}
	bIdx, pIdx := planIndex(bp), planIndex(pp)
	side := func(pass *planPass, idx map[string]int, id string) (*ImpactSide, *Planned) {
		if i, ok := idx[id]; ok {
			p := pass.plans[i].p
			return &ImpactSide{Type: p.Type, Summary: append([]string{}, p.Summary...)}, p
		}
		if msg, ok := pass.errs[id]; ok {
			return &ImpactSide{Type: "error", Summary: []string{msg}}, nil
		}
		return nil, nil
	}
	added, removed, attrs := map[string]int{}, map[string]int{}, map[string]int{}
	display := map[string]string{}
	for _, id := range idents {
		bs, bplan := side(bp, bIdx, id.ID)
		ps, pplan := side(pp, pIdx, id.ID)
		if bs == nil && ps == nil {
			continue
		}
		if bs != nil {
			res.Summary.Baseline[bs.Type]++
		}
		if ps != nil {
			res.Summary.Proposed[ps.Type]++
		}
		cur := pp.byID[id.ID]
		item := ImpactItem{IdentityID: id.ID, ExternalID: id.ExternalID, Display: id.Display, ADSam: cur.ADSam, State: cur.State, Baseline: bs, Proposed: ps, Lines: []ImpactLine{}}
		switch {
		case bs == nil:
			item.Difference = "new"
		case ps == nil:
			item.Difference = "gone"
		case bs.Type != ps.Type || strings.Join(bs.Summary, "\n") != strings.Join(ps.Summary, "\n") ||
			(bplan != nil && pplan != nil && PayloadHash(bplan.Type, bplan.Payload) != PayloadHash(pplan.Type, pplan.Payload)):
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
			ba, pa := groupAdds(bplan), groupAdds(pplan)
			for g := range pa {
				if !ba[g] {
					added[g]++
				}
			}
			br, pr := groupRemoves(bplan), groupRemoves(pplan)
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
		if req.Type != "" && !((it.Proposed != nil && it.Proposed.Type == req.Type) || (it.Baseline != nil && it.Baseline.Type == req.Type)) {
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
