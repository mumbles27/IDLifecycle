package engine

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/model"
	"idlifecycle/internal/sources"
)

// Fields a retention condition can test besides the person's source record.
const (
	RetFieldMemberOf = "ad.memberOf" // the account's groups (DN or name); matches when any group matches
	RetFieldOU       = "ad.ou"       // the account's parent OU (DN)
	RetFieldSam      = "ad.sam"
	RetFieldSource   = "source" // the source's name
	RetFieldOrigin   = "origin" // source | manual
)

// MaxRetentionDays caps delete_after_days (about 100 years).
const MaxRetentionDays = 36500

// DefaultRetention is the built-in policy every person falls back to: engine.delete_after_days and ad.disabled_ou.
func DefaultRetention(s model.Settings) model.RetentionPolicy {
	return model.RetentionPolicy{
		ID: model.RetentionDefaultID, Name: "Default", Enabled: true, Priority: 1 << 30, Match: "all",
		Conditions: []model.Condition{}, DeleteAfterDays: s.Engine.DeleteAfterDays, DisabledOU: s.AD.DisabledOU,
		ExtraKeepGroups: []string{},
	}
}

var ldapDNish = regexp.MustCompile(`(?i)^(ou|cn|dc)=`)

// ValidateRetentionPolicy checks a policy before it is stored or previewed.
func ValidateRetentionPolicy(p model.RetentionPolicy) error {
	if strings.TrimSpace(p.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if strings.EqualFold(strings.TrimSpace(p.Name), "default") {
		return fmt.Errorf("name: \"Default\" is the built-in policy (edit it under its own row)")
	}
	if p.Match != "all" && p.Match != "any" {
		return fmt.Errorf("match_mode must be all or any")
	}
	count := 0
	if err := validateConditions(p.Conditions, "conditions", 1, &count); err != nil {
		return err
	}
	if p.DeleteAfterDays < 0 || p.DeleteAfterDays > MaxRetentionDays {
		return fmt.Errorf("delete_after_days must be between 0 (never delete automatically) and %d", MaxRetentionDays)
	}
	if ou := strings.TrimSpace(p.DisabledOU); ou != "" && !ldapDNish.MatchString(ou) {
		return fmt.Errorf("disabled_ou %q is not a distinguished name (OU=...,DC=...)", ou)
	}
	for i, g := range p.ExtraKeepGroups {
		if strings.TrimSpace(g) == "" {
			return fmt.Errorf("extra_keep_groups[%d] is empty", i)
		}
		if model.IsCloudGroupRef(g) {
			return fmt.Errorf("extra_keep_groups[%d]: only Active Directory groups can be kept on disable", i)
		}
	}
	if p.Approval != "" && p.Approval != model.RetentionApprovalManual {
		return fmt.Errorf("approval must be \"\" or manual")
	}
	return nil
}

// NormalizeRetentionPolicy fills defaults (match all, empty lists) and trims names.
func NormalizeRetentionPolicy(p *model.RetentionPolicy) {
	p.Name, p.DisabledOU = strings.TrimSpace(p.Name), strings.TrimSpace(p.DisabledOU)
	if p.Match == "" {
		p.Match = "all"
	}
	if p.Conditions == nil {
		p.Conditions = []model.Condition{}
	}
	keep := []string{}
	for _, g := range p.ExtraKeepGroups {
		keep = append(keep, strings.TrimSpace(g))
	}
	p.ExtraKeepGroups = keep
}

// retentionFacts is what a retention condition sees: the source record plus the account and source facts.
type retentionFacts struct {
	attrs  map[string]string
	groups []string // DNs of the account's groups
}

func newRetentionFacts(id model.Identity, acct *ad.User, sourceName, origin string) retentionFacts {
	attrs := make(map[string]string, len(id.Attrs)+5)
	for k, v := range id.Attrs {
		attrs[k] = v
	}
	attrs[RetFieldSource], attrs[RetFieldOrigin] = sourceName, origin
	f := retentionFacts{attrs: attrs}
	dn, sam := id.ADDN, id.ADSam
	if acct != nil {
		dn, sam, f.groups = acct.DN, acct.Sam, acct.MemberOf
	}
	attrs[RetFieldOU], attrs[RetFieldSam] = parentDN(dn), sam
	return f
}

// retentionMatch evaluates one condition; ad.memberOf is multi-valued (a group matches by DN or by its name).
func retentionMatch(c model.Condition, f retentionFacts, now time.Time) bool {
	switch c.Op {
	case OpAllOf:
		for _, ch := range c.Conditions {
			if !retentionMatch(ch, f, now) {
				return false
			}
		}
		return len(c.Conditions) > 0
	case OpAnyOf:
		for _, ch := range c.Conditions {
			if retentionMatch(ch, f, now) {
				return true
			}
		}
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(c.Attr), RetFieldMemberOf) {
		return condMatch(c, f.attrs, now)
	}
	switch c.Op {
	case "exists":
		return len(f.groups) > 0
	case "empty":
		return len(f.groups) == 0
	}
	positive, negate := c, false
	switch c.Op {
	case "ne":
		positive.Op, negate = "eq", true
	case "not_in":
		positive.Op, negate = "in", true
	case "not_contains":
		positive.Op, negate = "contains", true
	case "not_regex":
		positive.Op, negate = "regex", true
	}
	hit := false
	for _, g := range f.groups {
		for _, v := range []string{g, shortCN(g)} {
			if condMatch(positive, map[string]string{c.Attr: v}, now) {
				hit = true
			}
		}
	}
	return hit != negate
}

// retentionPolicyMatches: a policy without conditions matches nobody by itself (only people assigned to it).
func retentionPolicyMatches(p model.RetentionPolicy, f retentionFacts, now time.Time) bool {
	if len(p.Conditions) == 0 {
		return false
	}
	if p.Match == "any" {
		for _, c := range p.Conditions {
			if retentionMatch(c, f, now) {
				return true
			}
		}
		return false
	}
	for _, c := range p.Conditions {
		if !retentionMatch(c, f, now) {
			return false
		}
	}
	return true
}

// ExplainRetention evaluates every condition of a policy against a person (for "Test this policy").
func ExplainRetention(p model.RetentionPolicy, id model.Identity, acct *ad.User, sourceName, origin string, now time.Time) (bool, []ExplainLine) {
	f := newRetentionFacts(id, acct, sourceName, origin)
	out := []ExplainLine{}
	var walk func(list []model.Condition, path string)
	walk = func(list []model.Condition, path string) {
		for i, c := range list {
			p := fmt.Sprintf("%s[%d]", path, i)
			line := ExplainLine{Path: p, Attr: c.Attr, Op: c.Op, Value: c.Value, Matched: retentionMatch(c, f, now)}
			switch {
			case isGroupOp(c.Op):
				line.Actual = fmt.Sprintf("%d condition(s)", len(c.Conditions))
			case strings.EqualFold(c.Attr, RetFieldMemberOf):
				names := make([]string, 0, len(f.groups))
				for _, g := range f.groups {
					names = append(names, shortCN(g))
				}
				line.Actual = strings.Join(names, ", ")
			case c.Op == "template":
				if v, err := Render(c.Value, f.attrs); err != nil {
					line.Actual = "error: " + err.Error()
				} else {
					line.Actual = v
				}
			default:
				line.Actual = lookupAttr(f.attrs, c.Attr)
			}
			out = append(out, line)
			if isGroupOp(c.Op) {
				walk(c.Conditions, p+".conditions")
			}
		}
	}
	walk(p.Conditions, "conditions")
	return retentionPolicyMatches(p, f, now), out
}

// ResolvedRetention is the policy that applies to one person and why.
type ResolvedRetention struct {
	Policy model.RetentionPolicy
	Reason string // model.RetentionReasonAssigned | Rule | Default
}

// resolveRetention picks a person's policy: one a person assigned (if it still exists and is enabled), else the first
// enabled policy whose conditions match, else the default. policies must be in priority order.
func resolveRetention(policies []model.RetentionPolicy, def model.RetentionPolicy, id model.Identity, acct *ad.User,
	sourceName, origin string, now time.Time) ResolvedRetention {
	switch a := id.RetentionPolicyID; {
	case a == model.RetentionDefaultID:
		return ResolvedRetention{def, model.RetentionReasonAssigned}
	case a != "":
		for _, p := range policies {
			if p.ID == a && p.Enabled {
				return ResolvedRetention{p, model.RetentionReasonAssigned}
			}
		}
	}
	f := newRetentionFacts(id, acct, sourceName, origin)
	for _, p := range policies {
		if p.Enabled && retentionPolicyMatches(p, f, now) {
			return ResolvedRetention{p, model.RetentionReasonRule}
		}
	}
	return ResolvedRetention{def, model.RetentionReasonDefault}
}

// deleteDue is when a disabled account falls due for deletion under a policy (nil = never, or not disabled).
func deleteDue(p model.RetentionPolicy, disabledAt *time.Time) *time.Time {
	if disabledAt == nil || p.DeleteAfterDays <= 0 {
		return nil
	}
	t := disabledAt.Add(time.Duration(p.DeleteAfterDays) * 24 * time.Hour)
	return &t
}

// retentionFor returns (and remembers) the policy for one person in this planning pass.
func (pc *PlanContext) retentionFor(id model.Identity, acct *ad.User) ResolvedRetention {
	if r, ok := pc.Retention[id.ID]; ok {
		return r
	}
	def := DefaultRetention(pc.Settings)
	r := resolveRetention(pc.RetentionPolicies, def, id, acct, pc.SourceNames[id.SourceID], originOf(pc.SourceTypes[id.SourceID]), pc.now())
	if pc.Retention == nil {
		pc.Retention = map[string]ResolvedRetention{}
	}
	pc.Retention[id.ID] = r
	return r
}

func originOf(sourceType string) string {
	if sourceType == sources.TypeManual {
		return OriginManual
	}
	return OriginSource
}

// isDisabledOU reports whether dn is the disabled OU of the settings or of any retention policy.
func (pc *PlanContext) isDisabledOU(dn string) bool {
	if dn == "" {
		return false
	}
	if pc.Settings.AD.DisabledOU != "" && strings.EqualFold(dn, pc.Settings.AD.DisabledOU) {
		return true
	}
	for _, p := range pc.RetentionPolicies {
		if p.DisabledOU != "" && strings.EqualFold(dn, p.DisabledOU) {
			return true
		}
	}
	return false
}

// sortRetention orders policies like the store does (priority, then name).
func sortRetention(list []model.RetentionPolicy) []model.RetentionPolicy {
	out := append([]model.RetentionPolicy{}, list...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && (out[j].Priority < out[j-1].Priority || (out[j].Priority == out[j-1].Priority && out[j].Name < out[j-1].Name)); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// retentionGroupIDs lists the AD groups retention policies keep on disable (for the AD snapshot request).
func retentionGroupIDs(list []model.RetentionPolicy) []string {
	var out []string
	for _, p := range list {
		out = append(out, p.ExtraKeepGroups...)
	}
	return adGroupsOnly(out)
}

// recordRetention stores the policy each planned person resolved to and their delete date, only when that changed,
// and writes an identity.retention_changed history row when an already recorded policy changes.
func (e *Engine) recordRetention(ctx context.Context, pc *PlanContext, byID map[string]model.Identity) {
	if len(pc.Retention) == 0 {
		return
	}
	stored, err := e.Store.ListIdentityRetention(ctx)
	if err != nil {
		e.Log.Error("read retention", "error", err)
		return
	}
	now := e.Now()
	for idID, r := range pc.Retention {
		id, ok := byID[idID]
		if !ok {
			continue
		}
		row := model.IdentityRetention{IdentityID: idID, PolicyID: r.Policy.ID, PolicyName: r.Policy.Name, Reason: r.Reason,
			DeleteDue: deleteDue(r.Policy, id.DisabledAt), ResolvedAt: now}
		old, had := stored[idID]
		if had && old.PolicyID == row.PolicyID && old.PolicyName == row.PolicyName && old.Reason == row.Reason && sameTimeP(old.DeleteDue, row.DeleteDue) {
			continue
		}
		if err := e.Store.PutIdentityRetention(ctx, row); err != nil {
			e.Log.Error("store retention", "identity", id.ExternalID, "error", err)
			continue
		}
		if had && old.PolicyID != row.PolicyID {
			d := map[string]any{"from": old.PolicyID, "from_name": old.PolicyName, "to": row.PolicyID, "to_name": row.PolicyName, "reason": row.Reason}
			if row.DeleteDue != nil {
				d["delete_due"] = row.DeleteDue.UTC().Format(time.RFC3339)
			}
			e.identityAudit(ctx, "system", ActRetentionChanged, id, d)
		}
	}
}

func sameTimeP(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Unix() == b.Unix()
}

// retentionOUs lists the disabled OUs retention policies move accounts to (disabled policies too: accounts may
// still be there).
func retentionOUs(list []model.RetentionPolicy) []string {
	var out []string
	for _, p := range list {
		if p.DisabledOU != "" {
			out = append(out, p.DisabledOU)
		}
	}
	return out
}
