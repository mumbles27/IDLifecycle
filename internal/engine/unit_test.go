package engine

import (
	"strings"
	"testing"

	"idlifecycle/internal/model"
)

func TestRenderFunctionsAndMissingKeys(t *testing.T) {
	data := map[string]string{"FirstName": "  José ", "LastName": "O'Neil-Smith", "Dept": "", "a.b": "dotted"}
	cases := map[string]string{
		`{{ .FirstName | trim | ascii | lower }}`:      "jose",
		`{{ first .FirstName 1 | lower }}`:             "", // first rune is a space; Render trims the result
		`{{ .LastName | alnum | lower }}`:              "oneilsmith",
		`{{ .Missing }}`:                               "",
		`{{ default "n/a" .Dept }}`:                    "n/a",
		`{{ coalesce .Dept .FirstName }}`:              "José",
		`{{ join " " .FirstName .Dept .LastName }}`:    "José  O'Neil-Smith",
		`{{ index . "a.b" }}`:                          "dotted",
		`{{ substr "abcdef" 2 3 }}`:                    "cde",
		`{{ title "mary-ann o'brien" }}`:               "Mary-Ann O'brien",
		`{{ replace "a-b" "-" "_" }}`:                  "a_b",
		`{{ digits "(555) 010-9999" }}`:                "5550109999",
		`{{ if .Dept }}has{{ else }}none{{ end }}`:     "none",
		`{{ last "abcdef" 2 }}`:                        "ef",
		`{{ with .FirstName }}{{ . | trim }}{{ end }}`: "José",
	}
	for src, want := range cases {
		got, err := Render(src, data)
		if err != nil {
			t.Errorf("%s: %v", src, err)
			continue
		}
		if got != want {
			t.Errorf("%s: got %q want %q", src, got, want)
		}
	}
	// join skips empty values.
	if got, _ := Render(`{{ join " " (trim .FirstName) .Dept (trim .LastName) }}`, data); got != "José O'Neil-Smith" {
		t.Errorf("join: %q", got)
	}
}

func TestRenderRejectsDangerousTemplates(t *testing.T) {
	bad := []string{
		`{{ range 1000000000 }}x{{ end }}`,
		`{{ define "x" }}{{ template "x" }}{{ end }}{{ template "x" }}`,
		`{{ template "x" }}`,
		`{{ .A `,
		`{{ nosuchfunc . }}`,
	}
	for _, src := range bad {
		if _, err := Render(src, map[string]string{}); err == nil {
			t.Errorf("expected rejection of %q", src)
		}
	}
	big := `{{ replace "aaaaaaaaaa" "a" "` + strings.Repeat("bbbbbbbbbb", 200) + `" }}`
	if _, err := Render(big, nil); err == nil {
		t.Error("oversized output must be rejected")
	}
}

func TestSanitizers(t *testing.T) {
	if got := SanitizeSam(`Jé"ane.Do/e [x]@y .`, 20); got != "Jeane.Doexy" {
		t.Errorf("sam: %q", got)
	}
	if got := SanitizeSam("abcdefghijklmnopqrstuvwxyz", 20); len(got) != 20 {
		t.Errorf("sam length: %q", got)
	}
	if got := SanitizeUPNLocal(" Jané Dö+e.. "); got != "JaneDoe" {
		t.Errorf("upn: %q", got)
	}
	if got := SanitizeCN("  Jane\x00 Doe\n "); got != "Jane Doe" {
		t.Errorf("cn: %q", got)
	}
}

func rule(name string, prio int, match string, conds []model.Condition, g model.Grants) model.RoleRule {
	return model.RoleRule{ID: name, Name: name, Enabled: true, Priority: prio, Match: match, Conditions: conds, Grants: g}
}

func TestRoleOperators(t *testing.T) {
	attrs := map[string]string{"Dept": "Marketing", "Title": "Sr. Designer", "Loc": "TX", "Empty": ""}
	cases := []struct {
		c    model.Condition
		want bool
	}{
		{model.Condition{Attr: "Dept", Op: "eq", Value: "marketing"}, true},
		{model.Condition{Attr: "dept", Op: "eq", Value: "Marketing"}, true}, // attribute name is case-insensitive
		{model.Condition{Attr: "Dept", Op: "ne", Value: "Sales"}, true},
		{model.Condition{Attr: "Loc", Op: "in", Value: "CA, TX ,NY"}, true},
		{model.Condition{Attr: "Loc", Op: "in", Value: "CA,NY"}, false},
		{model.Condition{Attr: "Title", Op: "contains", Value: "design"}, true},
		{model.Condition{Attr: "Title", Op: "starts_with", Value: "sr."}, true},
		{model.Condition{Attr: "Title", Op: "regex", Value: `^Sr\.\s+\w+$`}, true},
		{model.Condition{Attr: "Title", Op: "regex", Value: `[`}, false}, // invalid regex never matches
		{model.Condition{Attr: "Empty", Op: "exists"}, false},
		{model.Condition{Attr: "Dept", Op: "exists"}, true},
		{model.Condition{Attr: "Nope", Op: "eq", Value: ""}, true},
	}
	for _, tc := range cases {
		if got := condMatch(tc.c, attrs); got != tc.want {
			t.Errorf("%+v = %v, want %v", tc.c, got, tc.want)
		}
	}
}

func TestResolveRolesBaselineAdditiveAndPriority(t *testing.T) {
	rules := []model.RoleRule{
		rule("mkt", 20, "all", []model.Condition{{Attr: "Dept", Op: "eq", Value: "Marketing"}},
			model.Grants{Groups: []string{"GG-Mkt", "gg-all"}, OU: "OU=Mkt", Attributes: []model.AttrGrant{{TargetAttr: "extensionAttribute1", Template: "MKT"}}}),
		rule("all", 10, "all", nil, model.Grants{Groups: []string{"GG-All"}, OU: "OU=Base", Attributes: []model.AttrGrant{{TargetAttr: "extensionAttribute1", Template: "BASE"}}}),
		rule("any", 30, "any", []model.Condition{{Attr: "Dept", Op: "eq", Value: "Sales"}, {Attr: "Loc", Op: "eq", Value: "TX"}}, model.Grants{Groups: []string{"GG-Texas"}}),
		{ID: "off", Name: "off", Enabled: false, Priority: 99, Match: "all", Grants: model.Grants{Groups: []string{"GG-Disabled"}}},
	}
	res := ResolveRoles(rules, map[string]string{"Dept": "Marketing", "Loc": "TX"})

	if got := strings.Join(res.Groups, ","); got != "GG-All,GG-Mkt,GG-Texas" {
		t.Errorf("groups should be the de-duplicated union in priority order: %s", got)
	}
	if res.OU != "OU=Mkt" {
		t.Errorf("highest priority OU must win, got %q", res.OU)
	}
	if len(res.Attrs) != 2 || res.Attrs[1].Template != "MKT" {
		t.Errorf("attribute grants: %+v", res.Attrs)
	}
	eff := mergeMappings(nil, res.Attrs)
	if len(eff) != 1 || eff[0].Template != "MKT" {
		t.Errorf("higher priority grant must override the baseline: %+v", eff)
	}

	other := ResolveRoles(rules, map[string]string{"Dept": "Engineering", "Loc": "CA"})
	if strings.Join(other.Groups, ",") != "GG-All" || other.OU != "OU=Base" {
		t.Errorf("non-marketing gets only baseline: %+v", other)
	}
}

func TestValidateRoleAndMapping(t *testing.T) {
	good := rule("r", 1, "all", []model.Condition{{Attr: "a", Op: "regex", Value: "^x$"}}, model.Grants{Groups: []string{"g"}})
	if err := ValidateRole(good); err != nil {
		t.Fatal(err)
	}
	badRules := map[string]func(*model.RoleRule){
		"name":  func(r *model.RoleRule) { r.Name = "" },
		"match": func(r *model.RoleRule) { r.Match = "some" },
		"op":    func(r *model.RoleRule) { r.Conditions[0].Op = "like" },
		"regex": func(r *model.RoleRule) { r.Conditions[0].Value = "(" },
		"group": func(r *model.RoleRule) { r.Grants.Groups = []string{" "} },
		"grant": func(r *model.RoleRule) {
			r.Grants.Attributes = []model.AttrGrant{{TargetAttr: "samAccountName", Template: "x", Mode: "sync", OnEmpty: "skip", ValueType: "string"}}
		},
	}
	for name, mut := range badRules {
		r := good
		r.Conditions = append([]model.Condition(nil), good.Conditions...)
		mut(&r)
		if err := ValidateRole(r); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}

	m := model.Mapping{Name: "t", TargetAttr: "title", Template: "{{ .T }}", Mode: model.ModeSync, OnEmpty: "skip", ValueType: "string"}
	if err := ValidateMapping(m); err != nil {
		t.Fatal(err)
	}
	for _, attr := range []string{"sAMAccountName", "userPrincipalName", "memberOf", "unicodePwd", "userAccountControl", "altSecurityIdentities", "servicePrincipalName", "msDS-AllowedToDelegateTo", "msExchMailboxGuid", "adminCount", "bad attr"} {
		bad := m
		bad.TargetAttr = attr
		if err := ValidateMapping(bad); err == nil {
			t.Errorf("target %q must be refused", attr)
		}
	}
	bad := m
	bad.Template = "{{ range . }}"
	if err := ValidateMapping(bad); err == nil {
		t.Error("bad template must be refused")
	}
}

func TestGeneratePassword(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		pw, err := GeneratePassword(20)
		if err != nil {
			t.Fatal(err)
		}
		if len(pw) != 20 || seen[pw] {
			t.Fatalf("bad or repeated password %q", pw)
		}
		seen[pw] = true
		var up, lo, di, sy bool
		for _, r := range pw {
			switch {
			case r >= 'A' && r <= 'Z':
				up = true
			case r >= 'a' && r <= 'z':
				lo = true
			case r >= '0' && r <= '9':
				di = true
			default:
				sy = true
			}
		}
		if !(up && lo && di && sy) {
			t.Fatalf("password %q lacks a character class", pw)
		}
	}
	if pw, _ := GeneratePassword(3); len(pw) < 14 {
		t.Error("minimum length must be enforced")
	}
}

func TestNamingCandidates(t *testing.T) {
	s := model.DefaultSettings()
	s.AD.UPNSuffix = "corp.example.com"
	s.Naming.MaxCandidates = 3
	c, err := namingCandidates(s, map[string]string{"FirstName": "Jane", "LastName": "Doe"})
	if err != nil {
		t.Fatal(err)
	}
	if c[0].Sam != "jdoe" || c[0].UPN != "jane.doe@corp.example.com" || c[0].CN != "Jane Doe" {
		t.Errorf("first candidate: %+v", c[0])
	}
	if c[1].Sam != "jdoe2" || c[1].UPN != "jane.doe2@corp.example.com" || c[1].CN != "Jane Doe 2" || c[2].Sam != "jdoe3" {
		t.Errorf("collision candidates: %+v", c)
	}

	long, _ := namingCandidates(s, map[string]string{"FirstName": "Jane", "LastName": "Wolfeschlegelsteinhausenbergerdorff"})
	for _, x := range long {
		if len(x.Sam) > 20 {
			t.Errorf("sam too long: %q", x.Sam)
		}
	}
	if long[1].Sam[len(long[1].Sam)-1] != '2' {
		t.Errorf("suffix must survive truncation: %q", long[1].Sam)
	}

	if _, err := namingCandidates(s, map[string]string{"FirstName": "Jane"}); err == nil {
		t.Error("missing last name must produce a clear error rather than the account 'j'")
	}
}

func TestSettingsValidation(t *testing.T) {
	s := model.DefaultSettings()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	muts := map[string]func(*model.Settings){
		"interval":  func(s *model.Settings) { s.Schedule.IntervalMinutes = 0 },
		"kind":      func(s *model.Settings) { s.Schedule.Kind = "weekly" },
		"daily":     func(s *model.Settings) { s.Schedule.Kind, s.Schedule.DailyTime = "daily", "25:99" },
		"cron":      func(s *model.Settings) { s.Schedule.Kind, s.Schedule.CronExpr = "cron", "not cron" },
		"tz":        func(s *model.Settings) { s.Schedule.Timezone = "Mars/Base" },
		"approval":  func(s *model.Settings) { s.Approval.Create = "maybe" },
		"idattr":    func(s *model.Settings) { s.AD.IDAttribute = " " },
		"candidate": func(s *model.Settings) { s.Naming.MaxCandidates = 0 },
		"pwlen":     func(s *model.Settings) { s.NewUser.PasswordLength = 8 },
		"tmpl":      func(s *model.Settings) { s.Naming.SamTemplate = "" },
		"delete":    func(s *model.Settings) { s.Engine.DeleteAfterDays = -1 },
	}
	for name, mut := range muts {
		c := model.DefaultSettings()
		mut(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
	if model.DefaultSettings().ReadyForLive() == "" {
		t.Error("defaults must not be ready for live mode until AD settings are filled in")
	}
}
