package engine

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"idlifecycle/internal/model"
)

// Condition operators. Comparisons ignore case and surrounding spaces.
const (
	OpAllOf = "all_of" // group: every child matches
	OpAnyOf = "any_of" // group: at least one child matches
)

// ValidOps are the supported condition operators.
var ValidOps = map[string]bool{
	"eq": true, "ne": true, "in": true, "not_in": true, "contains": true, "not_contains": true,
	"starts_with": true, "ends_with": true, "regex": true, "not_regex": true, "exists": true, "empty": true,
	"gt": true, "gte": true, "lt": true, "lte": true, "older_than_days": true, "newer_than_days": true,
	"template": true, OpAllOf: true, OpAnyOf: true,
}

// Limits on a rule's conditions.
const (
	MaxConditionDepth = 3  // nested groups
	MaxConditions     = 50 // conditions and groups in one rule
)

func isGroupOp(op string) bool { return op == OpAllOf || op == OpAnyOf }

// validateConditions checks a condition list; path is the JSON path of the list ("conditions").
func validateConditions(list []model.Condition, path string, depth int, count *int) error {
	if depth > MaxConditionDepth {
		return fmt.Errorf("%s: groups can be nested at most %d deep", path, MaxConditionDepth)
	}
	for i, c := range list {
		p := fmt.Sprintf("%s[%d]", path, i)
		*count++
		if *count > MaxConditions {
			return fmt.Errorf("%s: a rule can have at most %d conditions", p, MaxConditions)
		}
		if !ValidOps[c.Op] {
			return fmt.Errorf("%s.op %q is not valid", p, c.Op)
		}
		if isGroupOp(c.Op) {
			if len(c.Conditions) == 0 {
				return fmt.Errorf("%s.conditions: a %s group needs at least one condition", p, c.Op)
			}
			if err := validateConditions(c.Conditions, p+".conditions", depth+1, count); err != nil {
				return err
			}
			continue
		}
		if len(c.Conditions) > 0 {
			return fmt.Errorf("%s.conditions: only all_of / any_of groups have child conditions", p)
		}
		if c.Op != "template" && strings.TrimSpace(c.Attr) == "" {
			return fmt.Errorf("%s.attr is required", p)
		}
		switch c.Op {
		case "regex", "not_regex":
			if _, err := compile(c.Value); err != nil {
				return fmt.Errorf("%s.value is not a valid regular expression: %v", p, err)
			}
		case "older_than_days", "newer_than_days":
			if n, err := strconv.Atoi(strings.TrimSpace(c.Value)); err != nil || n < 0 || n > 36500 {
				return fmt.Errorf("%s.value must be a number of days (0-36500)", p)
			}
		case "gt", "gte", "lt", "lte":
			if strings.TrimSpace(c.Value) == "" {
				return fmt.Errorf("%s.value is required (a number or a date such as 2026-01-31)", p)
			}
		case "template":
			if strings.TrimSpace(c.Value) == "" {
				return fmt.Errorf("%s.value is required (a template that renders true or false)", p)
			}
			if err := ValidateTemplate(c.Value); err != nil {
				return fmt.Errorf("%s.value: %v", p, err)
			}
		}
	}
	return nil
}

// dateLayouts are the date formats comparisons and day counts understand.
var dateLayouts = []string{time.DateOnly, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "1/2/2006", "01/02/2006"}

func parseDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, l := range dateLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// compareValues returns -1/0/1 comparing a with b as numbers, else as dates; ok is false when neither works.
func compareValues(a, b string) (int, bool) {
	fa, ea := strconv.ParseFloat(strings.TrimSpace(a), 64)
	fb, eb := strconv.ParseFloat(strings.TrimSpace(b), 64)
	if ea == nil && eb == nil {
		switch {
		case fa < fb:
			return -1, true
		case fa > fb:
			return 1, true
		}
		return 0, true
	}
	ta, oka := parseDate(a)
	tb, okb := parseDate(b)
	if oka && okb {
		return ta.Compare(tb), true
	}
	return 0, false
}

func inList(v, list string) bool {
	for _, part := range strings.Split(list, ",") {
		if strings.EqualFold(strings.TrimSpace(v), strings.TrimSpace(part)) {
			return true
		}
	}
	return false
}

// condMatch evaluates one condition (groups recursively) against a source record at time now.
func condMatch(c model.Condition, attrs map[string]string, now time.Time) bool {
	switch c.Op {
	case OpAllOf:
		for _, ch := range c.Conditions {
			if !condMatch(ch, attrs, now) {
				return false
			}
		}
		return len(c.Conditions) > 0
	case OpAnyOf:
		for _, ch := range c.Conditions {
			if condMatch(ch, attrs, now) {
				return true
			}
		}
		return false
	case "template":
		out, err := Render(c.Value, attrs)
		if err != nil {
			return false
		}
		switch strings.ToLower(strings.TrimSpace(out)) {
		case "true", "yes", "1":
			return true
		}
		return false
	}

	v := lookupAttr(attrs, c.Attr)
	lv, lc := strings.ToLower(strings.TrimSpace(v)), strings.ToLower(strings.TrimSpace(c.Value))
	switch c.Op {
	case "eq":
		return lv == lc
	case "ne":
		return lv != lc
	case "in":
		return inList(v, c.Value)
	case "not_in":
		return !inList(v, c.Value)
	case "contains":
		return strings.Contains(strings.ToLower(v), strings.ToLower(c.Value))
	case "not_contains":
		return !strings.Contains(strings.ToLower(v), strings.ToLower(c.Value))
	case "starts_with":
		return strings.HasPrefix(lv, lc)
	case "ends_with":
		return strings.HasSuffix(lv, lc)
	case "regex":
		re, err := compile(c.Value)
		return err == nil && re.MatchString(v)
	case "not_regex":
		re, err := compile(c.Value)
		return err == nil && !re.MatchString(v)
	case "exists":
		return lv != ""
	case "empty":
		return lv == ""
	case "gt", "gte", "lt", "lte":
		if lv == "" {
			return false
		}
		cmp, ok := compareValues(v, c.Value)
		if !ok {
			return false
		}
		switch c.Op {
		case "gt":
			return cmp > 0
		case "gte":
			return cmp >= 0
		case "lt":
			return cmp < 0
		}
		return cmp <= 0
	case "older_than_days", "newer_than_days":
		d, ok := parseDate(v)
		n, err := strconv.Atoi(strings.TrimSpace(c.Value))
		if !ok || err != nil {
			return false
		}
		age := now.Sub(d)
		limit := time.Duration(n) * 24 * time.Hour
		if c.Op == "older_than_days" {
			return age > limit
		}
		return age < limit
	}
	return false
}

// ExplainLine says why one condition of a rule matched or not (for the role editor's "Test a record").
type ExplainLine struct {
	Path    string `json:"path"`
	Attr    string `json:"attr"`
	Op      string `json:"op"`
	Value   string `json:"value"`
	Actual  string `json:"actual"`
	Matched bool   `json:"matched"`
}

// ExplainRule evaluates every condition of r (groups and their children) against a record. The rule's enabled flag is
// ignored: the editor tests unsaved and disabled rules too.
func ExplainRule(r model.RoleRule, attrs map[string]string, now time.Time) (bool, []ExplainLine) {
	out := []ExplainLine{}
	var walk func(list []model.Condition, path string)
	walk = func(list []model.Condition, path string) {
		for i, c := range list {
			p := fmt.Sprintf("%s[%d]", path, i)
			line := ExplainLine{Path: p, Attr: c.Attr, Op: c.Op, Value: c.Value, Matched: condMatch(c, attrs, now)}
			switch {
			case isGroupOp(c.Op):
				line.Actual = fmt.Sprintf("%d condition(s)", len(c.Conditions))
			case c.Op == "template":
				if v, err := Render(c.Value, attrs); err != nil {
					line.Actual = "error: " + err.Error()
				} else {
					line.Actual = v
				}
			default:
				line.Actual = lookupAttr(attrs, c.Attr)
			}
			out = append(out, line)
			if isGroupOp(c.Op) {
				walk(c.Conditions, p+".conditions")
			}
		}
	}
	walk(r.Conditions, "conditions")
	rr := r
	rr.Enabled = true
	return ruleMatchesAt(rr, attrs, now), out
}
