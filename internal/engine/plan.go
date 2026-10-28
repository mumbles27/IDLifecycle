package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/model"
)

// effective is one attribute rule after merging global mappings with role grants.
type effective struct {
	Attr      string
	Template  string
	Mode      string
	OnEmpty   string
	ValueType string
}

// caseInsensitiveAttrs are compared ignoring case; everything else must match exactly so that
// immutable mappings also enforce capitalization.
var caseInsensitiveAttrs = map[string]bool{"userprincipalname": true, "samaccountname": true, "mail": true}

func attrEqual(attr, a, b string) bool {
	if caseInsensitiveAttrs[strings.ToLower(attr)] {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// mergeMappings overlays role attribute grants (already in ascending priority) on global mappings.
func mergeMappings(base []model.Mapping, grants []model.AttrGrant) []effective {
	sorted := append([]model.Mapping(nil), base...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].SortOrder < sorted[j].SortOrder })

	idx := map[string]int{}
	var out []effective
	put := func(e effective) {
		key := strings.ToLower(e.Attr)
		if i, ok := idx[key]; ok {
			out[i] = e
			return
		}
		idx[key] = len(out)
		out = append(out, e)
	}
	for _, m := range sorted {
		if m.Enabled {
			put(effective{m.TargetAttr, m.Template, m.Mode, m.OnEmpty, m.ValueType})
		}
	}
	for _, g := range grants {
		put(effective{g.TargetAttr, g.Template, orStr(g.Mode, model.ModeSync), orStr(g.OnEmpty, "skip"), orStr(g.ValueType, "string")})
	}
	return out
}

// appliedValue looks up the last value written for attr. LDAP names are case-insensitive, and a mapping
// and a role grant may spell the same attribute differently ("Title" / "title").
func appliedValue(applied map[string]string, attr string) (string, bool) {
	if v, ok := applied[attr]; ok {
		return v, true
	}
	for k, v := range applied {
		if strings.EqualFold(k, attr) {
			return v, true
		}
	}
	return "", false
}

// setApplied records a written value under attr, dropping other spellings of the same attribute.
func setApplied(applied map[string]string, attr, v string) {
	for k := range applied {
		if k != attr && strings.EqualFold(k, attr) {
			delete(applied, k)
		}
	}
	applied[attr] = v
}

func orStr(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// PlanContext is the world the planner needs for one run.
type PlanContext struct {
	Settings     model.Settings
	Mappings     []model.Mapping
	Rules        []model.RoleRule
	GroupDN      map[string]string // lower(identity as typed) -> DN, only for groups that exist
	ManagedDNs   map[string]bool   // lower(DN) of every group named by an enabled rule
	UserDNByExt  map[string]string // external id -> DN (for user_ref attributes)
	Now          time.Time
	MappingProps []string // properties to fetch from AD
	// Links are correlation proposals by identity id (people without an account by id attribute).
	Links map[string]*model.LinkPayload
}

// Planned is the planner's output for one identity.
type Planned struct {
	// Observed lists sync attributes whose AD value already equals the source value; recording them as
	// "applied" keeps a later revert of the source from being mistaken for a manual AD edit.
	Observed map[string]string
	Type     string
	Payload  model.ChangePayload
	Summary  []string
	Warnings []string

	place   Placement         // create: where the OU came from (for the summary)
	tail    []string          // create: summary lines after the "Create account" line
	renames []model.Candidate // link: names to try when the account is renamed (the name check picks the first free one)
}

// PlanIdentity decides what (if anything) must change for one person. acct is nil when no AD account is linked.
func PlanIdentity(pc *PlanContext, id model.Identity, acct *ad.User) (*Planned, []string, error) {
	var warnings []string
	res := ResolveRolesAt(pc.Rules, id.Attrs, pc.now())
	maps := mergeMappings(pc.Mappings, res.Attrs)
	// Rules saved before validation tightened may still target the link key or a protected attribute; the
	// scripts would refuse the whole item, so drop just that attribute and say so.
	kept := maps[:0]
	for _, m := range maps {
		switch {
		case strings.EqualFold(m.Attr, pc.Settings.AD.IDAttribute):
			warnings = append(warnings, fmt.Sprintf("%s is the link key (ad.id_attribute) and is set by the service; mapping ignored", m.Attr))
		case IsProtectedAttribute(m.Attr):
			warnings = append(warnings, fmt.Sprintf("%s is a protected attribute; mapping ignored", m.Attr))
		default:
			kept = append(kept, m)
		}
	}
	maps = kept

	desired, w, err := renderAll(pc, id, maps)
	warnings = append(warnings, w...)
	if err != nil {
		return nil, warnings, err
	}

	// Lifecycle: every active account is in lifecycle_groups.active_add, like a role that matches everyone.
	groupDNs, w := resolveGroups(pc, append(append([]string{}, res.Groups...), pc.Settings.Lifecycle.ActiveAdd...))
	warnings = append(warnings, w...)
	place := ResolvePlacement(pc.Settings, id, res.OU)
	if place.Reason != "" && !place.Needs && (acct == nil || !acct.Enabled) {
		warnings = append(warnings, place.Reason)
	}

	if acct == nil {
		if !id.Active {
			return nil, warnings, nil
		}
		if lp := pc.Links[id.ID]; lp != nil {
			p, err := planLink(pc, id, maps, lp)
			return p, warnings, err
		}
		p, err := planCreate(pc, id, maps, desired, groupDNs, place)
		return p, warnings, err
	}

	if !id.Active {
		return planDeactivate(pc, id, acct), warnings, nil
	}
	if !acct.Enabled {
		return planEnable(pc, acct, groupDNs, place.OU), warnings, nil
	}
	return planUpdate(pc, id, acct, maps, desired, groupDNs, place), warnings, nil
}

type rendered struct {
	Value string
	OK    bool // false when the value could not be produced (unresolved user_ref)
}

func renderAll(pc *PlanContext, id model.Identity, maps []effective) (map[string]rendered, []string, error) {
	out := make(map[string]rendered, len(maps))
	var warns []string
	for _, m := range maps {
		v, err := Render(m.Template, id.Attrs)
		if err != nil {
			return nil, warns, fmt.Errorf("attribute %s: %w", m.Attr, err)
		}
		if m.ValueType == "user_ref" && v != "" {
			dn, ok := pc.UserDNByExt[v]
			if !ok {
				warns = append(warns, fmt.Sprintf("%s: referenced user %q has no AD account yet; skipped", m.Attr, v))
				out[strings.ToLower(m.Attr)] = rendered{OK: false}
				continue
			}
			v = dn
		}
		out[strings.ToLower(m.Attr)] = rendered{Value: v, OK: true}
	}
	return out, warns, nil
}

func resolveGroups(pc *PlanContext, identities []string) ([]string, []string) {
	var dns, warns []string
	seen := map[string]bool{}
	for _, g := range identities {
		dn, ok := pc.GroupDN[strings.ToLower(g)]
		if !ok {
			warns = append(warns, fmt.Sprintf("group %q not found in AD; skipped", g))
			continue
		}
		if !seen[strings.ToLower(dn)] {
			seen[strings.ToLower(dn)] = true
			dns = append(dns, dn)
		}
	}
	return dns, warns
}

func shortCN(dn string) string {
	first := strings.SplitN(dn, ",", 2)[0]
	return strings.TrimPrefix(strings.TrimPrefix(first, "CN="), "cn=")
}

func parentDN(dn string) string {
	if i := strings.Index(dn, ","); i >= 0 {
		return dn[i+1:]
	}
	return ""
}

func planCreate(pc *PlanContext, id model.Identity, maps []effective, desired map[string]rendered, groupDNs []string, place Placement) (*Planned, error) {
	ou := place.OU
	if ou == "" && !place.Needs {
		return nil, fmt.Errorf("no target OU: set ad.default_ou, a role rule OU or an OU placement rule")
	}
	attrs := map[string]string{}
	mailMapped := false
	for _, m := range maps {
		r := desired[strings.ToLower(m.Attr)]
		if r.OK && r.Value != "" {
			attrs[m.Attr] = r.Value
		}
		if strings.EqualFold(m.Attr, "mail") {
			mailMapped = true // a mapping owns mail (and keeps it in sync); the naming template is not used
		}
	}

	cands, err := identityCandidates(pc.Settings, id, !mailMapped)
	if err != nil {
		return nil, err
	}

	p := &Planned{Type: model.ChangeCreate, Payload: model.ChangePayload{OU: ou, Candidates: cands, Attributes: attrs, Groups: groupDNs}, place: place}
	if pc.Settings.Placement.Enabled {
		p.Payload.Placement = place.Value
	}
	if place.Needs {
		p.Payload.NeedsOU, p.Payload.OUReason = true, place.Reason
	}
	for _, k := range sortedKeys(attrs) {
		p.tail = append(p.tail, fmt.Sprintf("Set %s = %q", k, attrs[k]))
	}
	for _, g := range groupDNs {
		p.tail = append(p.tail, "Add to group "+shortCN(g))
	}
	p.summarizeCreate()
	return p, nil
}

// summarizeCreate (re)builds a create's summary; the name check calls it again after dropping taken names.
func (p *Planned) summarizeCreate() {
	pl := p.Payload
	var s []string
	if pl.NeedsName {
		s = append(s, "Choose a username: "+conflictText(pl.NameConflicts))
	}
	if pl.NeedsOU {
		s = append(s, "Choose an OU: "+pl.OUReason)
	}
	who := "account (username chosen at approval)"
	if !pl.NeedsName && len(pl.Candidates) > 0 {
		who = fmt.Sprintf("account %q (%s)", pl.Candidates[0].Sam, pl.Candidates[0].CN)
		if pl.Candidates[0].Mail != "" {
			who += ", mail " + pl.Candidates[0].Mail
		}
	}
	if pl.NeedsOU {
		s = append(s, "Create "+who+" in the OU chosen at approval")
	} else {
		s = append(s, fmt.Sprintf("Create %s in %s%s", who, pl.OU, placementNote(p.place)))
	}
	p.Summary = append(s, p.tail...)
}

// conflictText describes taken names for a summary line.
func conflictText(cs []model.NameConflict) string {
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		by := c.DN
		if c.With != "" {
			by = "also planned for " + c.With
		}
		parts = append(parts, fmt.Sprintf("%s %q is taken (%s)", nameFieldLabel(c.Field), c.Value, by))
	}
	if len(parts) == 0 {
		return "every generated name is taken"
	}
	return strings.Join(parts, "; ")
}

func nameFieldLabel(f string) string {
	switch f {
	case "sam":
		return "sAMAccountName"
	case "upn":
		return "UPN"
	case "mail":
		return "mail"
	case "cn":
		return "name (CN)"
	}
	return f
}

func planDeactivate(pc *PlanContext, id model.Identity, acct *ad.User) *Planned {
	s := pc.Settings
	if acct.Enabled {
		p := &Planned{Type: model.ChangeDisable, Payload: model.ChangePayload{
			ObjectGUID: acct.ObjectGUID,
			DN:         acct.DN,
		}}
		p.Summary = append(p.Summary, fmt.Sprintf("Disable account %s", acct.Sam))
		if s.AD.DisabledOU != "" && !strings.EqualFold(parentDN(acct.DN), s.AD.DisabledOU) {
			p.Payload.MoveTo = s.AD.DisabledOU
			p.Summary = append(p.Summary, "Move to "+s.AD.DisabledOU)
		}
		p.Payload.RemoveGroups, p.Payload.AddGroups = disableGroups(pc, acct)
		if n := len(p.Payload.RemoveGroups); n > 0 {
			p.Summary = append(p.Summary, fmt.Sprintf("Remove from %d group(s)", n))
		}
		for _, g := range p.Payload.AddGroups {
			p.Summary = append(p.Summary, "Add to group "+shortCN(g)+" (disabled accounts)")
		}
		return p
	}

	days := s.Engine.DeleteAfterDays
	if days <= 0 || id.DisabledAt == nil {
		return nil
	}
	if pc.Now.Sub(*id.DisabledAt) < time.Duration(days)*24*time.Hour {
		return nil
	}
	p := &Planned{Type: model.ChangeDelete, Payload: model.ChangePayload{ObjectGUID: acct.ObjectGUID, DN: acct.DN}}
	p.Summary = append(p.Summary, fmt.Sprintf("Delete account %s (disabled since %s, retention %d days)", acct.Sam, id.DisabledAt.UTC().Format("2006-01-02"), days))
	return p
}

func planEnable(pc *PlanContext, acct *ad.User, groupDNs []string, ou string) *Planned {
	p := &Planned{Type: model.ChangeEnable, Payload: model.ChangePayload{ObjectGUID: acct.ObjectGUID, DN: acct.DN, AddGroups: groupDNs}}
	p.Summary = append(p.Summary, fmt.Sprintf("Re-enable account %s (active again in source)", acct.Sam))
	if ou != "" && !strings.EqualFold(parentDN(acct.DN), ou) &&
		(pc.Settings.Engine.EnforceOU || (pc.Settings.AD.DisabledOU != "" && strings.EqualFold(parentDN(acct.DN), pc.Settings.AD.DisabledOU))) {
		p.Payload.MoveTo = ou
		p.Summary = append(p.Summary, "Move to "+ou)
	}
	for _, g := range groupDNs {
		p.Summary = append(p.Summary, "Add to group "+shortCN(g))
	}
	// Groups only disabled accounts are in (lifecycle_groups.disabled_add) are left on re-enable.
	for _, g := range lifecycleDNs(pc, pc.Settings.Lifecycle.DisabledAdd) {
		if inGroups(acct.MemberOf, g) {
			p.Payload.RemoveGroups = append(p.Payload.RemoveGroups, g)
			p.Summary = append(p.Summary, "Remove from group "+shortCN(g)+" (disabled accounts)")
		}
	}
	sort.Strings(p.Payload.RemoveGroups)
	return p
}

func planUpdate(pc *PlanContext, id model.Identity, acct *ad.User, maps []effective, desired map[string]rendered, groupDNs []string, place Placement) *Planned {
	ou := place.OU
	p := &Planned{Type: model.ChangeUpdate, Payload: model.ChangePayload{ObjectGUID: acct.ObjectGUID, DN: acct.DN, Set: map[string]model.AttrDiff{}}}
	var observed map[string]string

	for _, m := range maps {
		if m.Mode == model.ModeCreateOnly {
			continue
		}
		r := desired[strings.ToLower(m.Attr)]
		if !r.OK {
			continue
		}
		current := acct.Attr(m.Attr)
		if attrEqual(m.Attr, current, r.Value) {
			if last, ok := appliedValue(id.Applied, m.Attr); m.Mode == model.ModeSync && (!ok || last != r.Value) {
				if observed == nil {
					observed = map[string]string{}
				}
				observed[m.Attr] = r.Value
			}
			continue
		}
		if m.Mode == model.ModeSync {
			// Only push when the *source-derived* value changed since we last wrote it, so a manual
			// AD edit survives until HR data changes.
			if last, ok := appliedValue(id.Applied, m.Attr); ok && attrEqual(m.Attr, last, r.Value) {
				continue
			}
		}
		if r.Value == "" {
			if m.OnEmpty != "clear" || current == "" {
				continue
			}
			p.Payload.Clear = append(p.Payload.Clear, m.Attr)
			p.Summary = append(p.Summary, fmt.Sprintf("Clear %s (was %q) [%s]", m.Attr, current, m.Mode))
			continue
		}
		p.Payload.Set[m.Attr] = model.AttrDiff{From: current, To: r.Value, Mode: m.Mode}
		p.Summary = append(p.Summary, fmt.Sprintf("Set %s: %q -> %q [%s]", m.Attr, current, r.Value, m.Mode))
	}
	if len(p.Payload.Set) == 0 {
		p.Payload.Set = nil
	}
	sort.Strings(p.Payload.Clear)

	have := map[string]bool{}
	for _, g := range acct.MemberOf {
		have[strings.ToLower(g)] = true
	}
	want := map[string]bool{}
	for _, g := range groupDNs {
		want[strings.ToLower(g)] = true
		if !have[strings.ToLower(g)] {
			p.Payload.AddGroups = append(p.Payload.AddGroups, g)
			p.Summary = append(p.Summary, "Add to group "+shortCN(g))
		}
	}
	if pc.Settings.Engine.RemoveStaleGroups {
		for _, g := range acct.MemberOf {
			l := strings.ToLower(g)
			if pc.ManagedDNs[l] && !want[l] {
				p.Payload.RemoveGroups = append(p.Payload.RemoveGroups, g)
				p.Summary = append(p.Summary, "Remove from group "+shortCN(g))
			}
		}
	}
	sort.Strings(p.Payload.AddGroups)
	sort.Strings(p.Payload.RemoveGroups)

	if pc.Settings.Engine.EnforceOU && ou != "" && !strings.EqualFold(parentDN(acct.DN), ou) {
		p.Payload.MoveTo = ou
		p.Summary = append(p.Summary, fmt.Sprintf("Move %s -> %s", parentDN(acct.DN), ou))
	}

	// OU placement: move the account when the placement field (department) changed - and only then, so an OU
	// someone set by hand in AD is left alone otherwise. The first time an account is seen, its value is recorded.
	if pl := pc.Settings.Placement; pl.Enabled && pl.MoveOnChange {
		last, seen := appliedValue(id.Applied, placementKey)
		switch {
		case !seen:
			observed = withObserved(observed, placementKey, place.Value)
		case strings.EqualFold(strings.TrimSpace(last), place.Value):
		case place.Needs:
			p.Payload.NeedsOU, p.Payload.OUReason, p.Payload.Placement = true, place.Reason, place.Value
			p.Summary = append(p.Summary, fmt.Sprintf("Choose an OU: %s changed from %q to %q and %s", pl.Field, last, place.Value, place.Reason))
		case !strings.EqualFold(parentDN(acct.DN), ou):
			if p.Payload.MoveTo == "" { // enforce_ou may already have planned this move
				p.Summary = append(p.Summary, fmt.Sprintf("Move %s -> %s (%s changed from %q to %q)%s", parentDN(acct.DN), ou, pl.Field, last, place.Value, placementNote(place)))
			}
			p.Payload.MoveTo, p.Payload.Placement = ou, place.Value
		default:
			observed = withObserved(observed, placementKey, place.Value) // already in the right OU
		}
	}

	if len(p.Summary) == 0 {
		if len(observed) == 0 {
			return nil
		}
		return &Planned{Observed: observed} // bookkeeping only; no change to propose
	}
	p.Observed = observed
	p.Type = model.ChangeUpdate
	return p
}

// PayloadHash is stable for equal plans (map keys are sorted by encoding/json).
func PayloadHash(changeType string, p model.ChangePayload) string {
	b, _ := json.Marshal(struct {
		T string
		P model.ChangePayload
	}{changeType, p})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func withObserved(m map[string]string, k, v string) map[string]string {
	if m == nil {
		m = map[string]string{}
	}
	m[k] = v
	return m
}

// placementNote says where an OU came from, for the change summary.
func placementNote(p Placement) string {
	switch p.Source {
	case "override":
		return " (OU chosen for this person)"
	case "rule":
		return fmt.Sprintf(" (OU placement rule for %q)", p.Value)
	case "default":
		if p.Reason != "" {
			return " (" + p.Reason + ")"
		}
	}
	return ""
}

func (pc *PlanContext) now() time.Time {
	if pc.Now.IsZero() {
		return time.Now()
	}
	return pc.Now
}

// lifecycleDNs resolves lifecycle group identities to DNs (groups AD does not know are left out; the run already
// warned about them when the snapshot resolved them).
func lifecycleDNs(pc *PlanContext, identities []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, g := range identities {
		if dn, ok := pc.GroupDN[strings.ToLower(strings.TrimSpace(g))]; ok && !seen[strings.ToLower(dn)] {
			seen[strings.ToLower(dn)] = true
			out = append(out, dn)
		}
	}
	return out
}

func inGroups(memberOf []string, dn string) bool {
	for _, m := range memberOf {
		if strings.EqualFold(m, dn) {
			return true
		}
	}
	return false
}

// disabledRemoveMode is lifecycle_groups.disabled_remove, derived from ad.strip_groups_on_disable when unset.
func disabledRemoveMode(s model.Settings) string {
	if m := s.Lifecycle.DisabledRemove; m != "" {
		return m
	}
	if s.AD.StripGroupsOnDisable {
		return model.DisabledRemoveAllExcept
	}
	return model.DisabledRemoveNone
}

// disableGroups decides group changes when an account is disabled: which memberships to remove (by
// lifecycle_groups.disabled_remove, never disabled_keep or disabled_add) and which disabled_add groups to add.
// Privileged groups are still listed for removal; the script keeps them and says so.
func disableGroups(pc *PlanContext, acct *ad.User) (remove, add []string) {
	keep := map[string]bool{}
	for _, g := range lifecycleDNs(pc, pc.Settings.Lifecycle.DisabledKeep) {
		keep[strings.ToLower(g)] = true
	}
	addDNs := lifecycleDNs(pc, pc.Settings.Lifecycle.DisabledAdd)
	for _, g := range addDNs {
		keep[strings.ToLower(g)] = true
	}
	mode := disabledRemoveMode(pc.Settings)
	if mode != model.DisabledRemoveNone {
		for _, g := range acct.MemberOf {
			l := strings.ToLower(g)
			if keep[l] || (mode == model.DisabledRemoveManaged && !pc.ManagedDNs[l]) {
				continue
			}
			remove = append(remove, g)
		}
	}
	for _, g := range addDNs {
		if !inGroups(acct.MemberOf, g) {
			add = append(add, g)
		}
	}
	sort.Strings(remove)
	sort.Strings(add)
	return remove, add
}
