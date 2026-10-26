package engine

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/model"
)

func TestTemplateFields(t *testing.T) {
	cases := []struct {
		tpl     string
		want    []string
		dynamic bool
	}{
		{`{{ .FirstName }} {{ .LastName }}`, []string{"FirstName", "LastName"}, false},
		{`{{ first .FirstName 1 | lower }}{{ .LastName | lower }}`, []string{"FirstName", "LastName"}, false},
		{`{{ index . "currentStatus.statusType" }}`, []string{"currentStatus.statusType"}, false},
		{`{{ coalesce .Preferred .FirstName }}`, []string{"FirstName", "Preferred"}, false},
		{`{{ if .Dept }}{{ .Dept }}{{ else }}{{ default "none" .Division }}{{ end }}`, []string{"Dept", "Division"}, false},
		{`{{ with .Manager }}{{ . }} {{ .Ignored }} {{ $.Site }}{{ end }}`, []string{"Manager", "Site"}, false},
		{`{{ index $ "Cost Center" }}`, []string{"Cost Center"}, false},
		{`{{ index . .KeyField }}`, []string{"KeyField"}, true},
		{`{{ (upper .A) }}-{{ join "," .B .C }}`, []string{"A", "B", "C"}, false},
		{`static text`, []string{}, false},
		{`{{ .A }}{{ .A }}`, []string{"A"}, false},
	}
	for _, c := range cases {
		got, dyn, err := TemplateFields(c.tpl)
		if err != nil {
			t.Errorf("%s: %v", c.tpl, err)
			continue
		}
		if got == nil {
			got = []string{}
		}
		if !reflect.DeepEqual(got, c.want) || dyn != c.dynamic {
			t.Errorf("%s: got %v dynamic=%v, want %v dynamic=%v", c.tpl, got, dyn, c.want, c.dynamic)
		}
	}
	if _, _, err := TemplateFields(`{{ .A `); err == nil {
		t.Error("a broken template must be an error")
	}
}

func TestFieldReferences(t *testing.T) {
	maps := []model.Mapping{
		{ID: "m1", Name: "Given", TargetAttr: "givenName", Template: `{{ .FirstName }}`, Enabled: true},
		{ID: "m2", Name: "Broken", TargetAttr: "title", Template: `{{ .X `},
	}
	roles := []model.RoleRule{{ID: "r1", Name: "Sales", Enabled: true,
		Conditions: []model.Condition{{Attr: "Dept", Op: "eq", Value: "Sales"}, {Attr: " ", Op: "exists"}},
		Grants:     model.Grants{Attributes: []model.AttrGrant{{TargetAttr: "company", Template: `{{ .Company }} {{ .Dept }}`}}}}}
	refs := FieldReferences(maps, roles)
	if len(refs) != 3 {
		t.Fatalf("refs: %+v", refs)
	}
	if refs[0].Kind != "mapping" || !reflect.DeepEqual(refs[0].Fields, []string{"FirstName"}) || refs[0].TargetAttr != "givenName" {
		t.Errorf("mapping ref: %+v", refs[0])
	}
	if refs[1].Error == "" || refs[1].Fields == nil {
		t.Errorf("broken template must report an error and an empty (not null) field list: %+v", refs[1])
	}
	if refs[2].Kind != "role" || !reflect.DeepEqual(refs[2].Fields, []string{"Company", "Dept"}) {
		t.Errorf("role ref must merge conditions and grant templates: %+v", refs[2])
	}
	b, _ := json.Marshal(refs)
	if strings.Contains(string(b), "null") {
		t.Errorf("JSON must not contain null lists: %s", b)
	}
}

func TestClassifyAttribute(t *testing.T) {
	type c struct {
		attr     ad.SchemaAttribute
		mappable bool
		reason   string
		hint     string
	}
	s := func(name string) ad.SchemaAttribute {
		return ad.SchemaAttribute{Name: name, Syntax: "string", SingleValued: true}
	}
	withSyntax := func(a ad.SchemaAttribute, syn string) ad.SchemaAttribute { a.Syntax = syn; return a }
	cases := []c{
		{s("title"), true, "", ""},
		{s("employeeID"), true, "", "link key"},
		{ad.SchemaAttribute{Name: "manager", Syntax: "dn", SingleValued: true, LinkID: 42}, true, "", "user_ref"},
		{withSyntax(s("countryCode"), "integer"), true, "", ""},
		{s("sAMAccountName"), false, "managed by IDLifecycle", ""},
		{s("msDS-cloudExtensionAttribute1"), false, "managed by IDLifecycle", ""},
		{ad.SchemaAttribute{Name: "proxyAddresses", Syntax: "string"}, false, "managed by IDLifecycle", ""},
		{ad.SchemaAttribute{Name: "otherMobile", Syntax: "string"}, false, "multi-valued", ""},
		{ad.SchemaAttribute{Name: "directReports", Syntax: "dn", LinkID: 43, SystemOnly: true}, false, "back-link", ""},
		{ad.SchemaAttribute{Name: "whenCreated", Syntax: "time", SingleValued: true, SystemOnly: true}, false, "only AD", ""},
		{ad.SchemaAttribute{Name: "tokenGroups", Syntax: "binary", Constructed: true}, false, "computed", ""},
		{withSyntax(s("thumbnailPhoto"), "binary"), false, "not a text value", ""},
		{s("bad_name"), false, "not a valid", ""},
	}
	for _, x := range cases {
		ok, reason, hint := ClassifyAttribute(x.attr)
		if ok != x.mappable || !strings.Contains(reason, x.reason) || !strings.Contains(hint, x.hint) {
			t.Errorf("%s: got %v %q %q, want %v %q %q", x.attr.Name, ok, reason, hint, x.mappable, x.reason, x.hint)
		}
	}
}

// Every built-in attribute is classified the way ValidateMapping would treat it: a mappable one can be
// saved as a mapping, an unmappable one cannot (or is not single-valued text).
func TestBuiltinAttributesAgreeWithValidateMapping(t *testing.T) {
	seen := map[string]bool{}
	mappable := 0
	for _, a := range ad.BuiltinUserAttributes() {
		key := strings.ToLower(a.Name)
		if seen[key] {
			t.Errorf("%s listed twice", a.Name)
		}
		seen[key] = true
		ok, _, _ := ClassifyAttribute(a)
		vt := "string"
		if a.Syntax == "dn" {
			vt = "user_ref"
		}
		err := ValidateMapping(model.Mapping{Name: "x", TargetAttr: a.Name, Template: "{{ .A }}", Mode: model.ModeSync, OnEmpty: "skip", ValueType: vt})
		if ok && err != nil {
			t.Errorf("%s is shown as mappable but ValidateMapping refuses it: %v", a.Name, err)
		}
		if ok {
			mappable++
		}
	}
	if mappable < 40 {
		t.Errorf("only %d mappable built-in attributes", mappable)
	}
}

func TestRealSchemaScriptThroughClient(t *testing.T) {
	p := newPwshEnv(t)
	state := map[string]any{"schema": map[string]any{
		"classes": []any{
			map[string]any{"lDAPDisplayName": "top", "subClassOf": "top", "systemMayContain": []string{"cn"}},
			map[string]any{"lDAPDisplayName": "person", "subClassOf": "top", "mayContain": []string{"sn"}},
			map[string]any{"lDAPDisplayName": "user", "subClassOf": "person", "mayContain": []string{"manager", "otherMobile", "corpCostCenter"}},
		},
		"attributes": []any{
			map[string]any{"lDAPDisplayName": "cn", "attributeSyntax": "2.5.5.12", "isSingleValued": true, "systemOnly": true},
			map[string]any{"lDAPDisplayName": "sn", "attributeSyntax": "2.5.5.12", "isSingleValued": true, "rangeUpper": 64},
			map[string]any{"lDAPDisplayName": "manager", "attributeSyntax": "2.5.5.1", "isSingleValued": true, "linkID": 42},
			map[string]any{"lDAPDisplayName": "otherMobile", "attributeSyntax": "2.5.5.12", "isSingleValued": false},
			map[string]any{"lDAPDisplayName": "corpCostCenter", "attributeSyntax": "2.5.5.12", "isSingleValued": true, "adminDescription": "Cost center"},
		},
	}}
	b, _ := json.Marshal(state)
	if err := os.WriteFile(p.state, b, 0o600); err != nil {
		t.Fatal(err)
	}
	reader, ok := p.eng.AD.(ad.SchemaReader)
	if !ok {
		t.Fatal("PowerShellClient must implement SchemaReader")
	}
	attrs, err := reader.Schema(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]ad.SchemaAttribute{}
	for _, a := range attrs {
		got[a.Name] = a
	}
	if len(got) != 5 || got["sn"].RangeUpper != 64 || got["manager"].Syntax != "dn" || got["corpCostCenter"].Description != "Cost center" {
		t.Fatalf("schema: %+v", attrs)
	}
	for name, want := range map[string]bool{"sn": true, "manager": true, "corpCostCenter": true, "otherMobile": false, "cn": false} {
		if ok, reason, _ := ClassifyAttribute(got[name]); ok != want {
			t.Errorf("%s mappable=%v (%s), want %v", name, ok, reason, want)
		}
	}
}
