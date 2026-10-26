package engine

import (
	"fmt"
	"regexp"
	"strings"

	"idlifecycle/internal/model"
)

// placementKey is where Identity.Applied remembers the placement value (e.g. the department) the account's OU was
// last set for; a different value later is a department change. It cannot clash with an attribute name.
const placementKey = "~placement"

// Placement is where an identity's account belongs.
type Placement struct {
	OU     string `json:"ou"`
	Source string `json:"source"` // override, role, rule, default, none (placement off: role or default)
	Value  string `json:"value"`  // the placement field's value
	Needs  bool   `json:"needs"`  // no OU: a person must choose one
	Reason string `json:"reason,omitempty"`
}

// MatchPlacementRule returns the OU for value: an exact match (ignoring case and surrounding spaces) first, then
// the first wildcard rule ('*' = any characters) in list order.
func MatchPlacementRule(rules []model.PlacementRule, value string) (string, bool) {
	v := strings.TrimSpace(value)
	if v == "" {
		return "", false
	}
	for _, r := range rules {
		if !strings.Contains(r.Match, "*") && strings.EqualFold(strings.TrimSpace(r.Match), v) {
			return r.OU, true
		}
	}
	for _, r := range rules {
		if strings.Contains(r.Match, "*") && wildcard(r.Match).MatchString(v) {
			return r.OU, true
		}
	}
	return "", false
}

func wildcard(pattern string) *regexp.Regexp {
	parts := strings.Split(strings.TrimSpace(pattern), "*")
	for i := range parts {
		parts[i] = regexp.QuoteMeta(parts[i])
	}
	return regexp.MustCompile(`(?is)^` + strings.Join(parts, ".*") + `$`)
}

// fieldValue reads a source field by name, ignoring case (source columns are written by people).
func fieldValue(attrs map[string]string, field string) string {
	if v, ok := attrs[field]; ok {
		return strings.TrimSpace(v)
	}
	for k, v := range attrs {
		if strings.EqualFold(k, field) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// ResolvePlacement decides the OU for an identity: a person's choice for it (while the field still has the value it
// was made for), a role rule's OU, the placement rules, then placement.unmapped. With placement off it is the role
// rule's OU or ad.default_ou, as before.
func ResolvePlacement(s model.Settings, id model.Identity, roleOU string) Placement {
	pl := s.Placement
	if !pl.Enabled {
		if id.OUOverride != "" { // chosen with Change OU; without a placement field it does not expire
			return Placement{OU: id.OUOverride, Source: "override"}
		}
		if roleOU != "" {
			return Placement{OU: roleOU, Source: "role"}
		}
		return Placement{OU: s.AD.DefaultOU, Source: "default"}
	}
	value := fieldValue(id.Attrs, pl.Field)
	p := Placement{Value: value}
	switch {
	case id.OUOverride != "" && strings.EqualFold(strings.TrimSpace(id.OUOverrideValue), value):
		p.OU, p.Source = id.OUOverride, "override"
	case roleOU != "":
		p.OU, p.Source = roleOU, "role"
	default:
		if ou, ok := MatchPlacementRule(pl.Rules, value); ok {
			p.OU, p.Source = ou, "rule"
			return p
		}
		reason := fmt.Sprintf("no OU is mapped for %s %q", pl.Field, value)
		if value == "" {
			reason = fmt.Sprintf("the source has no %s for this person", pl.Field)
		}
		if pl.Unmapped == model.UnmappedDefault && s.AD.DefaultOU != "" {
			p.OU, p.Source, p.Reason = s.AD.DefaultOU, "default", reason+"; using the default OU"
			return p
		}
		p.Source, p.Needs, p.Reason = "none", true, reason
	}
	return p
}
