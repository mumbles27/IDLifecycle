package engine

import (
	"strings"
	"testing"
	"time"

	"idlifecycle/internal/model"
)

func TestConditionOperators(t *testing.T) {
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	rec := map[string]string{"Dept": " Finance ", "Title": "Senior Analyst", "Grade": "7", "HireDate": "2026-02-20",
		"Start": "01/15/2020", "Empty": "  ", "Email": "jane@corp.example.com"}
	cases := []struct {
		c    model.Condition
		want bool
	}{
		{model.Condition{Attr: "Dept", Op: "eq", Value: "finance"}, true},
		{model.Condition{Attr: "Dept", Op: "ne", Value: "finance"}, false},
		{model.Condition{Attr: "Dept", Op: "in", Value: "Sales, Finance"}, true},
		{model.Condition{Attr: "Dept", Op: "not_in", Value: "Sales, Finance"}, false},
		{model.Condition{Attr: "Dept", Op: "not_in", Value: "Sales,IT"}, true},
		{model.Condition{Attr: "Title", Op: "contains", Value: "analyst"}, true},
		{model.Condition{Attr: "Title", Op: "not_contains", Value: "manager"}, true},
		{model.Condition{Attr: "Title", Op: "starts_with", Value: "senior"}, true},
		{model.Condition{Attr: "Email", Op: "ends_with", Value: "@CORP.example.com"}, true},
		{model.Condition{Attr: "Title", Op: "regex", Value: `^Senior\b`}, true},
		{model.Condition{Attr: "Title", Op: "not_regex", Value: `^Junior`}, true},
		{model.Condition{Attr: "Dept", Op: "exists"}, true},
		{model.Condition{Attr: "Empty", Op: "exists"}, false},
		{model.Condition{Attr: "Empty", Op: "empty"}, true},
		{model.Condition{Attr: "Missing", Op: "empty"}, true},
		{model.Condition{Attr: "Grade", Op: "gt", Value: "6"}, true},
		{model.Condition{Attr: "Grade", Op: "gte", Value: "7"}, true},
		{model.Condition{Attr: "Grade", Op: "lt", Value: "10"}, true}, // numbers, not text ("7" > "10" as text)
		{model.Condition{Attr: "Grade", Op: "lte", Value: "6.5"}, false},
		{model.Condition{Attr: "HireDate", Op: "gt", Value: "2026-01-01"}, true},
		{model.Condition{Attr: "Start", Op: "lt", Value: "2021-01-01"}, true}, // M/D/YYYY
		{model.Condition{Attr: "Title", Op: "gt", Value: "5"}, false},         // neither number nor date: no match
		{model.Condition{Attr: "Missing", Op: "lt", Value: "5"}, false},
		{model.Condition{Attr: "HireDate", Op: "newer_than_days", Value: "30"}, true},
		{model.Condition{Attr: "HireDate", Op: "older_than_days", Value: "30"}, false},
		{model.Condition{Attr: "Start", Op: "older_than_days", Value: "365"}, true},
		{model.Condition{Attr: "Title", Op: "older_than_days", Value: "1"}, false},
		{model.Condition{Attr: "*", Op: "template", Value: `{{ if eq (lower (trim .Dept)) "finance" }}true{{ end }}`}, true},
		{model.Condition{Attr: "*", Op: "template", Value: `{{ .Dept }}`}, false},
		{model.Condition{Op: "any_of", Conditions: []model.Condition{
			{Attr: "Dept", Op: "eq", Value: "IT"},
			{Op: "all_of", Conditions: []model.Condition{{Attr: "Grade", Op: "gte", Value: "7"}, {Attr: "Title", Op: "contains", Value: "senior"}}},
		}}, true},
		{model.Condition{Op: "all_of", Conditions: []model.Condition{{Attr: "Dept", Op: "eq", Value: "Finance"}, {Attr: "Grade", Op: "gt", Value: "9"}}}, false},
	}
	for i, tc := range cases {
		if got := condMatch(tc.c, rec, now); got != tc.want {
			t.Errorf("case %d %s %s %q: got %v", i, tc.c.Attr, tc.c.Op, tc.c.Value, got)
		}
	}
}

func TestValidateConditionsNamesThePath(t *testing.T) {
	rule := func(cs ...model.Condition) model.RoleRule {
		return model.RoleRule{Name: "r", Match: "all", Enabled: true, Conditions: cs}
	}
	deep := model.Condition{Op: "any_of", Conditions: []model.Condition{{Op: "all_of", Conditions: []model.Condition{
		{Op: "any_of", Conditions: []model.Condition{{Op: "all_of", Conditions: []model.Condition{{Attr: "a", Op: "exists"}}}}}}}}}
	many := make([]model.Condition, MaxConditions+1)
	for i := range many {
		many[i] = model.Condition{Attr: "a", Op: "exists"}
	}
	for _, tc := range []struct {
		r    model.RoleRule
		want string
	}{
		{rule(model.Condition{Attr: "a", Op: "bogus"}), `conditions[0].op "bogus" is not valid`},
		{rule(model.Condition{Attr: "a", Op: "exists"}, model.Condition{Op: "any_of", Conditions: []model.Condition{{Attr: "b", Op: "regex", Value: "("}}}), "conditions[1].conditions[0].value is not a valid regular expression"},
		{rule(model.Condition{Attr: "a", Op: "older_than_days", Value: "soon"}), "conditions[0].value must be a number of days"},
		{rule(model.Condition{Op: "all_of"}), "conditions[0].conditions: a all_of group needs at least one condition"},
		{rule(model.Condition{Attr: "a", Op: "eq", Conditions: []model.Condition{{Attr: "b", Op: "exists"}}}), "only all_of / any_of groups"},
		{rule(model.Condition{Op: "template", Attr: "*", Value: "{{ .A "}), "conditions[0].value:"},
		{rule(model.Condition{Op: "eq", Value: "x"}), "conditions[0].attr is required"},
		{rule(model.Condition{Attr: "a", Op: "gt"}), "conditions[0].value is required"},
		{rule(deep), "nested at most 3 deep"},
		{rule(many...), "at most 50 conditions"},
	} {
		err := ValidateRole(tc.r)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("want %q, got %v", tc.want, err)
		}
	}
	if err := ValidateRole(rule(model.Condition{Op: "template", Value: `{{ if .A }}true{{ end }}`})); err != nil {
		t.Errorf("a template condition needs no attr: %v", err)
	}
}

func TestExplainRule(t *testing.T) {
	r := model.RoleRule{Name: "Finance seniors", Match: "all", Enabled: false, Conditions: []model.Condition{
		{Attr: "Dept", Op: "eq", Value: "Finance"},
		{Op: "any_of", Conditions: []model.Condition{{Attr: "Grade", Op: "gte", Value: "8"}, {Attr: "Title", Op: "contains", Value: "senior"}}},
	}}
	matched, lines := ExplainRule(r, map[string]string{"Dept": "Finance", "Grade": "7", "Title": "Senior Analyst"}, time.Now())
	if !matched {
		t.Error("an unsaved / disabled rule is explained as if enabled")
	}
	got := []string{}
	for _, l := range lines {
		got = append(got, l.Path+"="+map[bool]string{true: "y", false: "n"}[l.Matched]+":"+l.Actual)
	}
	want := "conditions[0]=y:Finance|conditions[1]=y:2 condition(s)|conditions[1].conditions[0]=n:7|conditions[1].conditions[1]=y:Senior Analyst"
	if strings.Join(got, "|") != want {
		t.Errorf("explain:\n got %s\nwant %s", strings.Join(got, "|"), want)
	}
}
