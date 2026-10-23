package engine

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"idlifecycle/internal/model"
)

// ValidOps are the supported condition operators.
var ValidOps = map[string]bool{"eq": true, "ne": true, "in": true, "contains": true, "starts_with": true, "regex": true, "exists": true}

var (
	reCache sync.Map // pattern -> *regexp.Regexp (bounded)
	reCount atomic.Int64
)

func compile(p string) (*regexp.Regexp, error) {
	if v, ok := reCache.Load(p); ok {
		return v.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, err
	}
	if reCount.Add(1) <= maxCacheEntries {
		reCache.Store(p, re)
	}
	return re, nil
}

// ValidateRole checks a rule before it is saved.
func ValidateRole(r model.RoleRule) error {
	if strings.TrimSpace(r.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if r.Match != "all" && r.Match != "any" {
		return fmt.Errorf("match must be all or any")
	}
	for i, c := range r.Conditions {
		if strings.TrimSpace(c.Attr) == "" {
			return fmt.Errorf("conditions[%d].attr is required", i)
		}
		if !ValidOps[c.Op] {
			return fmt.Errorf("conditions[%d].op %q is not valid", i, c.Op)
		}
		if c.Op == "regex" {
			if _, err := compile(c.Value); err != nil {
				return fmt.Errorf("conditions[%d].value is not a valid regular expression: %v", i, err)
			}
		}
	}
	for i, g := range r.Grants.Groups {
		if strings.TrimSpace(g) == "" {
			return fmt.Errorf("grants.groups[%d] is empty", i)
		}
	}
	for i, a := range r.Grants.Attributes {
		if err := ValidateMapping(model.Mapping{Name: "grant", TargetAttr: a.TargetAttr, Template: a.Template, Mode: a.Mode, OnEmpty: a.OnEmpty, ValueType: a.ValueType}); err != nil {
			return fmt.Errorf("grants.attributes[%d]: %w", i, err)
		}
	}
	return nil
}

// ValidateMapping checks and normalizes nothing; it only reports problems.
func ValidateMapping(m model.Mapping) error {
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if strings.TrimSpace(m.TargetAttr) == "" {
		return fmt.Errorf("target_attr is required")
	}
	if strings.ContainsAny(m.TargetAttr, " ,;=()*\\") {
		return fmt.Errorf("target_attr %q is not a valid LDAP attribute name", m.TargetAttr)
	}
	lower := strings.ToLower(m.TargetAttr)
	switch lower {
	case "samaccountname", "userprincipalname", "cn", "name", "distinguishedname", "objectguid", "objectsid", "memberof", "unicodepwd", "userpassword", "useraccountcontrol", "primarygroupid",
		"altsecurityidentities", "serviceprincipalname", "scriptpath", "userworkstations", "admincount", "sidhistory", "ntsecuritydescriptor", "lockouttime", "pwdlastset":
		return fmt.Errorf("%s is security-sensitive or managed by the naming policy / lifecycle and cannot be mapped", m.TargetAttr)
	}
	if strings.HasPrefix(lower, "msds-") || strings.HasPrefix(lower, "msexch") {
		return fmt.Errorf("%s (msDS-*/msExch*) attributes cannot be mapped", m.TargetAttr)
	}
	switch m.Mode {
	case model.ModeCreateOnly, model.ModeSync, model.ModeImmutable:
	default:
		return fmt.Errorf("mode must be create_only, sync or immutable")
	}
	if m.OnEmpty != "skip" && m.OnEmpty != "clear" {
		return fmt.Errorf("on_empty must be skip or clear")
	}
	if m.ValueType != "string" && m.ValueType != "user_ref" {
		return fmt.Errorf("value_type must be string or user_ref")
	}
	return ValidateTemplate(m.Template)
}

func condMatch(c model.Condition, attrs map[string]string) bool {
	v := lookupAttr(attrs, c.Attr)
	switch c.Op {
	case "eq":
		return strings.EqualFold(strings.TrimSpace(v), strings.TrimSpace(c.Value))
	case "ne":
		return !strings.EqualFold(strings.TrimSpace(v), strings.TrimSpace(c.Value))
	case "in":
		for _, part := range strings.Split(c.Value, ",") {
			if strings.EqualFold(strings.TrimSpace(v), strings.TrimSpace(part)) {
				return true
			}
		}
		return false
	case "contains":
		return strings.Contains(strings.ToLower(v), strings.ToLower(c.Value))
	case "starts_with":
		return strings.HasPrefix(strings.ToLower(v), strings.ToLower(c.Value))
	case "regex":
		re, err := compile(c.Value)
		return err == nil && re.MatchString(v)
	case "exists":
		return strings.TrimSpace(v) != ""
	}
	return false
}

// lookupAttr finds a source attribute by exact key, then case-insensitively.
func lookupAttr(attrs map[string]string, name string) string {
	if v, ok := attrs[name]; ok {
		return v
	}
	for k, v := range attrs {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// RuleMatches reports whether an enabled rule applies. A rule with no conditions applies to everyone.
func RuleMatches(r model.RoleRule, attrs map[string]string) bool {
	if !r.Enabled {
		return false
	}
	if len(r.Conditions) == 0 {
		return true
	}
	if r.Match == "any" {
		for _, c := range r.Conditions {
			if condMatch(c, attrs) {
				return true
			}
		}
		return false
	}
	for _, c := range r.Conditions {
		if !condMatch(c, attrs) {
			return false
		}
	}
	return true
}

// Resolved is the union of everything granted by matching rules.
type Resolved struct {
	Matched []model.RoleRule
	Groups  []string // de-duplicated, order of first grant
	OU      string   // from the highest-priority matching rule that sets one
	Attrs   []model.AttrGrant
}

// ResolveRoles evaluates rules in ascending priority so that higher-priority rules apply last
// (and therefore win for OU and for attribute grants on the same target attribute).
func ResolveRoles(rules []model.RoleRule, attrs map[string]string) Resolved {
	sorted := append([]model.RoleRule(nil), rules...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Priority < sorted[j].Priority })

	var res Resolved
	seen := map[string]bool{}
	for _, r := range sorted {
		if !RuleMatches(r, attrs) {
			continue
		}
		res.Matched = append(res.Matched, r)
		for _, g := range r.Grants.Groups {
			g = strings.TrimSpace(g)
			if g != "" && !seen[strings.ToLower(g)] {
				seen[strings.ToLower(g)] = true
				res.Groups = append(res.Groups, g)
			}
		}
		if strings.TrimSpace(r.Grants.OU) != "" {
			res.OU = strings.TrimSpace(r.Grants.OU)
		}
		res.Attrs = append(res.Attrs, r.Grants.Attributes...)
	}
	return res
}

// AllRuleGroups returns every group named by any enabled rule (the set this service "manages").
func AllRuleGroups(rules []model.RoleRule) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		for _, g := range r.Grants.Groups {
			g = strings.TrimSpace(g)
			if g != "" && !seen[strings.ToLower(g)] {
				seen[strings.ToLower(g)] = true
				out = append(out, g)
			}
		}
	}
	return out
}
