package engine

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/model"
)

// psArray extracts a single-quoted string array assigned to $script:<name> in the common module.
func psArray(t *testing.T, src, name string) []string {
	t.Helper()
	m := regexp.MustCompile(`(?s)\$script:` + regexp.QuoteMeta(name) + `\s*=\s*@\((.*?)\)`).FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("$script:%s not found in IDLifecycle.Common.psm1", name)
	}
	var out []string
	for _, q := range regexp.MustCompile(`'([^']*)'`).FindAllStringSubmatch(m[1], -1) {
		out = append(out, strings.ToLower(q[1]))
	}
	sort.Strings(out)
	return out
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func TestProtectedAttributesMatchTheScripts(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "scripts", "IDLifecycle.Common.psm1"))
	if err != nil {
		t.Skip("scripts not present:", err)
	}
	src := string(b)
	if got, want := psArray(t, src, "IDLProtectedAttributes"), sorted(ProtectedAttributes); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("protected attributes differ between Go and PowerShell:\n ps: %v\n go: %v", got, want)
	}
	if got, want := psArray(t, src, "IDLProtectedAttributePrefixes"), sorted(ProtectedPrefixes); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("protected prefixes differ:\n ps: %v\n go: %v", got, want)
	}
}

func TestValidateMappingRejectsProtectedAndMalformedNames(t *testing.T) {
	base := model.Mapping{Name: "m", Template: "{{ .X }}", Mode: model.ModeSync, OnEmpty: "skip", ValueType: "string"}
	for _, attr := range []string{"homeDirectory", "ProfilePath", "scriptPath", "proxyAddresses", "accountExpires", "msDS-AllowedToActOnBehalfOfOtherIdentity",
		"ms-DS-ConsistencyGuid", "msExchHideFromAddressLists", "msPKI-Foo", "userAccountControl", "member", "cn"} {
		m := base
		m.TargetAttr = attr
		if err := ValidateMapping(m); err == nil {
			t.Errorf("%s must be refused", attr)
		}
	}
	for _, attr := range []string{"title", "department", "extensionAttribute1", "manager", "mail", "telephoneNumber"} {
		m := base
		m.TargetAttr = attr
		if err := ValidateMapping(m); err != nil {
			t.Errorf("%s must be allowed: %v", attr, err)
		}
	}
	for _, attr := range []string{"1title", "ti tle", "title;binary", "a" + strings.Repeat("b", 64), "x_y"} {
		m := base
		m.TargetAttr = attr
		if err := ValidateMapping(m); err == nil {
			t.Errorf("malformed name %q must be refused", attr)
		}
	}
}

func TestValidateRoleRejectsDuplicateGrant(t *testing.T) {
	r := model.RoleRule{Name: "r", Match: "all", Grants: model.Grants{Attributes: []model.AttrGrant{
		{TargetAttr: "title", Template: "A", Mode: model.ModeSync, OnEmpty: "skip", ValueType: "string"},
		{TargetAttr: "Title", Template: "B", Mode: model.ModeSync, OnEmpty: "skip", ValueType: "string"},
	}}}
	if err := ValidateRole(r); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("duplicate grant must be refused, got %v", err)
	}
}

func TestAppliedLookupIgnoresAttributeCase(t *testing.T) {
	applied := map[string]string{"Title": "Designer"}
	if v, ok := appliedValue(applied, "title"); !ok || v != "Designer" {
		t.Fatalf("appliedValue = %q %v", v, ok)
	}
	setApplied(applied, "title", "Lead")
	if len(applied) != 1 || applied["title"] != "Lead" {
		t.Fatalf("setApplied must replace other spellings: %v", applied)
	}
}

// A role grant spelling an attribute differently from the global mapping must still respect a manual AD
// edit in sync mode (the last-written value is found regardless of case).
func TestSyncModeHonoursManualEditAcrossSpellings(t *testing.T) {
	e := newEnv(t)
	e.provision()
	// Marketing now grants "Title" (capitalised) with the same source template as the global "title" mapping.
	r, err := e.st.GetRole(e.ctx, "r-mkt")
	e.must(err)
	r.Grants.Attributes = append(r.Grants.Attributes, model.AttrGrant{TargetAttr: "Title", Template: "{{ .Title }}", Mode: model.ModeSync, OnEmpty: "skip", ValueType: "string"})
	e.must(e.st.UpdateRole(e.ctx, r))

	// Someone edits Jane's title by hand in AD; HR data is unchanged.
	e.fake.Mutate("E1", func(u *ad.FakeUser) { u.Attrs["title"] = "Hand-edited" })
	run := e.run()
	eq(t, "manual edit survives (no update proposed)", run.Counters["planned_update"], 0)
}

func TestPlannerIgnoresMappingOfTheLinkKey(t *testing.T) {
	e := newEnv(t)
	e.must(e.st.CreateMapping(e.ctx, model.Mapping{ID: "bad", Name: "bad", TargetAttr: "employeeID", Template: "X{{ .EmployeeID }}",
		Mode: model.ModeImmutable, OnEmpty: "skip", ValueType: "string", Enabled: true, SortOrder: 99}))
	e.provision()
	u := e.user("E1")
	eq(t, "link key untouched", u.External, "E1")
	eq(t, "mapped value never written", u.Attrs["employeeID"] == "XE1", false)
	run := e.run()
	eq(t, "no update to the link key", run.Counters["planned_update"], 0)
}
