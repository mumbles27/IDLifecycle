package engine

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"idlifecycle/internal/model"
)

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
	count := 0
	if err := validateConditions(r.Conditions, "conditions", 1, &count); err != nil {
		return err
	}
	for i, g := range r.Grants.Groups {
		if strings.TrimSpace(g) == "" {
			return fmt.Errorf("grants.groups[%d] is empty", i)
		}
		if _, ok := model.CloudGroupID(g); model.IsCloudGroupRef(g) && !ok {
			return fmt.Errorf("grants.groups[%d]: %q is not a cloud group reference (entra:<group object id>)", i, g)
		}
	}
	seen := map[string]bool{}
	for i, a := range r.Grants.Attributes {
		key := strings.ToLower(strings.TrimSpace(a.TargetAttr))
		if seen[key] {
			return fmt.Errorf("grants.attributes[%d]: %s is granted twice in this rule", i, a.TargetAttr)
		}
		seen[key] = true
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
	if !attrNameRe.MatchString(m.TargetAttr) {
		return fmt.Errorf("target_attr %q is not a valid LDAP attribute name (a letter, then letters, digits or '-', at most 64)", m.TargetAttr)
	}
	if IsProtectedAttribute(m.TargetAttr) {
		return fmt.Errorf("%s is security-sensitive, multi-valued or managed by the naming policy / lifecycle and cannot be mapped", m.TargetAttr)
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

// attrNameRe is the LDAP display-name shape the scripts accept (Test-IDLAttributeName).
var attrNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,63}$`)

// ProtectedAttributes and ProtectedPrefixes may never be written through mappings or role grants. They MUST
// match $script:IDLProtectedAttributes / $script:IDLProtectedAttributePrefixes in scripts\IDLifecycle.Common.psm1
// (a test compares them): identity and naming attributes (the lifecycle sets those), credentials and
// account control, logon scripts/home paths (a UNC path there harvests NTLM hashes), certificate mappings,
// and multi-valued attributes this single-value plumbing would overwrite.
var ProtectedAttributes = []string{
	"samaccountname", "userprincipalname", "cn", "name", "distinguishedname", "objectguid", "objectsid", "objectclass",
	"objectcategory", "memberof", "member", "unicodepwd", "userpassword", "unixuserpassword", "useraccountcontrol",
	"primarygroupid", "pwdlastset", "lockouttime", "accountexpires", "logonhours", "userworkstations",
	"altsecurityidentities", "serviceprincipalname", "scriptpath", "homedirectory", "homedrive", "profilepath",
	"admincount", "sidhistory", "ntsecuritydescriptor", "usercertificate", "usersmimecertificate", "proxyaddresses",
	"msnpallowdialin",
}

var ProtectedPrefixes = []string{"msds-", "ms-ds-", "msexch", "mspki-"}

var protectedSet = func() map[string]bool {
	m := make(map[string]bool, len(ProtectedAttributes))
	for _, a := range ProtectedAttributes {
		m[a] = true
	}
	return m
}()

// IsProtectedAttribute reports whether an attribute may never be mapped (case-insensitive).
func IsProtectedAttribute(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if protectedSet[lower] {
		return true
	}
	for _, p := range ProtectedPrefixes {
		if strings.HasPrefix(lower, p) {
			return true
		}
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

// RuleMatches reports whether an enabled rule applies now. A rule with no conditions applies to everyone.
func RuleMatches(r model.RoleRule, attrs map[string]string) bool {
	return ruleMatchesAt(r, attrs, time.Now())
}

func ruleMatchesAt(r model.RoleRule, attrs map[string]string, now time.Time) bool {
	if !r.Enabled {
		return false
	}
	if len(r.Conditions) == 0 {
		return true
	}
	if r.Match == "any" {
		for _, c := range r.Conditions {
			if condMatch(c, attrs, now) {
				return true
			}
		}
		return false
	}
	for _, c := range r.Conditions {
		if !condMatch(c, attrs, now) {
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
	return ResolveRolesAt(rules, attrs, time.Now())
}

// ResolveRolesAt is ResolveRoles at a given time (day-count conditions).
func ResolveRolesAt(rules []model.RoleRule, attrs map[string]string, now time.Time) Resolved {
	sorted := append([]model.RoleRule(nil), rules...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Priority < sorted[j].Priority })

	var res Resolved
	seen := map[string]bool{}
	for _, r := range sorted {
		if !ruleMatchesAt(r, attrs, now) {
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
