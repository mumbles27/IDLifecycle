package api

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"idlifecycle/internal/ad"
)

type mapperField struct {
	Name    string   `json:"name"`
	Filled  int      `json:"filled"`
	Samples []string `json:"samples"`
	Role    string   `json:"role"`
	UsedBy  []struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"used_by"`
}

type mapperRef struct {
	Kind        string            `json:"kind"`
	Name        string            `json:"name"`
	TargetAttr  string            `json:"target_attr"`
	Fields      []string          `json:"fields"`
	Missing     []string          `json:"missing"`
	Suggestions map[string]string `json:"suggestions"`
	Error       string            `json:"error"`
}

type mapperFields struct {
	OK         bool          `json:"ok"`
	Error      string        `json:"error"`
	Count      int           `json:"count"`
	IDField    string        `json:"id_field"`
	Fields     []mapperField `json:"fields"`
	References []mapperRef   `json:"references"`
}

type mapperAttrs struct {
	Source     string `json:"source"`
	LiveError  string `json:"live_error"`
	Attributes []struct {
		Name     string `json:"name"`
		Mappable bool   `json:"mappable"`
		Reason   string `json:"reason"`
		Hint     string `json:"hint"`
		Desc     string `json:"description"`
	} `json:"attributes"`
}

func firstSourceID(t *testing.T, c *client) string {
	t.Helper()
	var list struct {
		Items []struct{ ID string }
	}
	c.do("GET", "/api/sources", nil, &list)
	if len(list.Items) == 0 {
		t.Fatal("no source")
	}
	return list.Items[0].ID
}

func TestMapperIsAdminOnly(t *testing.T) {
	e := newTenv(t)
	admin, approver, viewer := e.login("admin"), e.login("approver"), e.login("viewer")
	e.seedSource(admin)
	id := firstSourceID(t, admin)
	for _, c := range []*client{approver, viewer} {
		want(t, "ad attributes", c.do("GET", "/api/mapper/ad-attributes", nil, nil), 403)
		want(t, "source fields", c.do("POST", "/api/mapper/sources/"+id+"/fields", map[string]any{}, nil), 403)
	}
	want(t, "no request body needed", e.login("admin").do("POST", "/api/mapper/sources/"+id+"/fields", nil, nil), 200)
	noCSRF := e.login("admin")
	noCSRF.csrf = ""
	want(t, "missing CSRF", noCSRF.do("POST", "/api/mapper/sources/"+id+"/fields", map[string]any{}, nil), 403)
	want(t, "unknown source", admin.do("POST", "/api/mapper/sources/nope/fields", map[string]any{}, nil), 404)
}

func TestMapperSourceFields(t *testing.T) {
	e := newTenv(t)
	admin := e.login("admin")
	e.seedSource(admin) // EmployeeID,FirstName,LastName,Dept + mapping givenName={{ .FirstName }} + role "Everyone"
	os.WriteFile(e.csv, []byte("EmployeeID,FirstName,LastName,Dept,Notes\nE1,Jane,Doe,Marketing,\nE2,Bob,Smith,Eng,\nE3,Ann,Lee,Eng,x\n"), 0o600)
	want(t, "mapping with wrong case", admin.do("POST", "/api/mappings", map[string]any{"name": "Surname", "target_attr": "sn", "template": "{{ .lastname }}", "mode": "sync", "enabled": true}, nil), 201)
	want(t, "mapping with gone field", admin.do("POST", "/api/mappings", map[string]any{"name": "Cost", "target_attr": "extensionAttribute1", "template": `{{ index . "Cost Center" }}`, "mode": "sync", "enabled": false}, nil), 201)
	want(t, "role", admin.do("POST", "/api/roles", map[string]any{"name": "Eng", "enabled": true, "priority": 20,
		"conditions": []map[string]string{{"attr": "Dept", "op": "eq", "value": "Eng"}},
		"grants":     map[string]any{"attributes": []map[string]any{{"target_attr": "department", "template": "{{ .Dept }}", "mode": "sync", "on_empty": "skip", "value_type": "string"}}}}, nil), 201)

	var res mapperFields
	want(t, "fields", admin.do("POST", "/api/mapper/sources/"+firstSourceID(t, admin)+"/fields", map[string]any{}, &res), 200)
	if !res.OK || res.Count != 3 || res.IDField != "EmployeeID" {
		t.Fatalf("result: %+v", res)
	}
	names := []string{}
	byName := map[string]mapperField{}
	for _, f := range res.Fields {
		names = append(names, f.Name)
		byName[f.Name] = f
	}
	if !reflect.DeepEqual(names, []string{"Dept", "EmployeeID", "FirstName", "LastName", "Notes"}) {
		t.Fatalf("fields: %v", names)
	}
	if f := byName["Dept"]; f.Filled != 3 || !reflect.DeepEqual(f.Samples, []string{"Marketing", "Eng"}) || len(f.UsedBy) != 1 || f.UsedBy[0].Name != "Eng" {
		t.Errorf("Dept: %+v (samples are distinct; used by the role's condition and grant once)", f)
	}
	if f := byName["Notes"]; f.Filled != 1 || len(f.UsedBy) != 0 {
		t.Errorf("Notes: %+v", f)
	}
	if byName["EmployeeID"].Role != "external_id" {
		t.Errorf("EmployeeID must be marked as the external id: %+v", byName["EmployeeID"])
	}
	if f := byName["FirstName"]; len(f.UsedBy) != 1 || f.UsedBy[0].Kind != "mapping" || f.UsedBy[0].Name != "givenName" {
		t.Errorf("FirstName: %+v", f)
	}
	if len(byName["LastName"].UsedBy) != 0 {
		t.Error("LastName is only referenced in the wrong case, so nothing uses it")
	}

	refs := map[string]mapperRef{}
	for _, r := range res.References {
		refs[r.Name] = r
	}
	if r := refs["Surname"]; !reflect.DeepEqual(r.Missing, []string{"lastname"}) || r.Suggestions["lastname"] != "LastName" {
		t.Errorf("wrong case must be missing with a suggestion: %+v", r)
	}
	if r := refs["Cost"]; !reflect.DeepEqual(r.Missing, []string{"Cost Center"}) || len(r.Suggestions) != 0 {
		t.Errorf("gone field: %+v", r)
	}
	if r := refs["givenName"]; len(r.Missing) != 0 || r.Missing == nil {
		t.Errorf("a complete mapping has an empty (not null) missing list: %+v", r)
	}
	if r := refs["Eng"]; r.Kind != "role" || !reflect.DeepEqual(r.Fields, []string{"Dept"}) || len(r.Missing) != 0 {
		t.Errorf("role ref: %+v", r)
	}
}

func TestMapperSourceFieldsClipsSamplesAndReportsFetchErrors(t *testing.T) {
	e := newTenv(t)
	admin := e.login("admin")
	e.seedSource(admin)
	long := strings.Repeat("é", 100)
	os.WriteFile(e.csv, []byte("EmployeeID,Bio\nE1,"+long+"\n"), 0o600)
	var res mapperFields
	admin.do("POST", "/api/mapper/sources/"+firstSourceID(t, admin)+"/fields", map[string]any{}, &res)
	for _, f := range res.Fields {
		if f.Name == "Bio" && (len([]rune(f.Samples[0])) != maxSampleLen || !strings.HasSuffix(f.Samples[0], "…")) {
			t.Errorf("sample not clipped to %d runes: %q", maxSampleLen, f.Samples[0])
		}
	}

	os.Remove(e.csv)
	res = mapperFields{}
	want(t, "fetch error", admin.do("POST", "/api/mapper/sources/"+firstSourceID(t, admin)+"/fields", map[string]any{}, &res), 200)
	if res.OK || res.Error == "" || len(res.Fields) != 0 || res.Fields == nil {
		t.Errorf("a failing source must report ok=false with an error and empty lists: %+v", res)
	}
}

func TestMapperADAttributesLiveFallbackAndCache(t *testing.T) {
	e := newTenv(t)
	admin := e.login("admin")
	e.fake.SchemaAttrs = []ad.SchemaAttribute{
		{Name: "title", Description: "schema text", Syntax: "string", SingleValued: true},
		{Name: "corpCostCenter", Description: "Cost center", Syntax: "string", SingleValued: true},
		{Name: "otherMobile", Syntax: "string"},
		{Name: "manager", Syntax: "dn", SingleValued: true, LinkID: 42},
	}
	var res mapperAttrs
	want(t, "live", admin.do("GET", "/api/mapper/ad-attributes", nil, &res), 200)
	if res.Source != "live" || res.LiveError != "" || len(res.Attributes) != 4 {
		t.Fatalf("live: %+v", res)
	}
	by := map[string]int{}
	for i, a := range res.Attributes {
		by[a.Name] = i
	}
	if a := res.Attributes[by["title"]]; !a.Mappable || a.Desc != "Job title" {
		t.Errorf("title must be mappable with the friendly description: %+v", a)
	}
	if a := res.Attributes[by["corpCostCenter"]]; !a.Mappable || a.Desc != "Cost center" {
		t.Errorf("a custom attribute keeps the schema description: %+v", a)
	}
	if a := res.Attributes[by["otherMobile"]]; a.Mappable || a.Reason != "multi-valued" {
		t.Errorf("otherMobile: %+v", a)
	}
	if a := res.Attributes[by["manager"]]; !a.Mappable || !strings.Contains(a.Hint, "user_ref") {
		t.Errorf("manager: %+v", a)
	}
	if res.Attributes[0].Name != "corpCostCenter" {
		t.Errorf("attributes must be sorted case-insensitively: %+v", res.Attributes)
	}

	// Cached: a schema change is not seen until refresh=1.
	e.fake.SchemaErr = errors.New("Get-IDLSchema.ps1 is not installed")
	calls := func() int {
		n := 0
		for _, c := range e.fake.Calls {
			if c == "schema" {
				n++
			}
		}
		return n
	}
	before := calls()
	res = mapperAttrs{}
	admin.do("GET", "/api/mapper/ad-attributes", nil, &res)
	if res.Source != "live" || calls() != before {
		t.Errorf("second call must come from the cache: %s, %d schema reads", res.Source, calls()-before)
	}

	res = mapperAttrs{}
	admin.do("GET", "/api/mapper/ad-attributes?refresh=1", nil, &res)
	if res.Source != "builtin" || !strings.Contains(res.LiveError, "not installed") || len(res.Attributes) < 40 {
		t.Fatalf("fallback: source=%s error=%q n=%d", res.Source, res.LiveError, len(res.Attributes))
	}
	for _, a := range res.Attributes {
		if a.Name == "sAMAccountName" && a.Mappable {
			t.Error("sAMAccountName must be listed as not mappable")
		}
	}
}

func TestMapperReferencesWithoutFetchingTheSource(t *testing.T) {
	e := newTenv(t)
	admin := e.login("admin")
	e.seedSource(admin)
	os.Remove(e.csv) // the references endpoint must not need the source
	var res struct {
		References []mapperRef `json:"references"`
	}
	want(t, "references", admin.do("GET", "/api/mapper/references", nil, &res), 200)
	if len(res.References) != 2 || res.References[0].Kind != "mapping" || !reflect.DeepEqual(res.References[0].Fields, []string{"FirstName"}) ||
		res.References[1].Kind != "role" || len(res.References[1].Fields) != 0 {
		t.Fatalf("references: %+v", res.References)
	}
	want(t, "viewer", e.login("viewer").do("GET", "/api/mapper/references", nil, nil), 403)
}
