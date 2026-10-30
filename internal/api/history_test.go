package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/model"
)

// auditRows returns the audit rows of one action, newest first.
func auditRows(t *testing.T, c *client, action string) []model.AuditEntry {
	t.Helper()
	var out struct{ Items []model.AuditEntry }
	want(t, "audit "+action, c.do("GET", "/api/audit?action="+action+"&limit=50", nil, &out), 200)
	return out.Items
}

func changeOf(t *testing.T, e model.AuditEntry, path string) map[string]any {
	t.Helper()
	ch, _ := e.Detail["changes"].(map[string]any)
	v, ok := ch[path].(map[string]any)
	if !ok {
		t.Fatalf("%s: no change for %q in %+v", e.Action, path, e.Detail["changes"])
	}
	return v
}

// noSecrets fails when any of the values (or any ciphertext) appears in the audit log.
func noSecrets(t *testing.T, c *client, secrets ...string) {
	t.Helper()
	var all struct{ Items []model.AuditEntry }
	c.do("GET", "/api/audit?limit=500", nil, &all)
	raw, _ := json.Marshal(all)
	for _, s := range secrets {
		if strings.Contains(string(raw), s) {
			t.Errorf("the audit log contains the secret %q", s)
		}
	}
	if strings.Contains(string(raw), "enc:v1:") {
		t.Error("the audit log contains ciphertext")
	}
}

func TestConsoleChangesAreAuditedWithBeforeAndAfter(t *testing.T) {
	e := newTenv(t)
	admin := e.login("admin")
	hr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"1"}]}`))
	}))
	defer hr.Close()

	// Source: create (secret hidden), update (secret changed = flag only, other fields from/to), delete (snapshot).
	var src model.Source
	want(t, "create source", admin.do("POST", "/api/sources", map[string]any{"name": "HR API", "type": "rest", "enabled": true,
		"options": map[string]any{"external_id_field": "id"},
		"config":  map[string]any{"base_url": hr.URL, "records_path": "data", "auth": "bearer", "bearer_token": "tok-FIRST-secret"}}, &src), 201)
	created := auditRows(t, admin, "source.create")[0]
	fields, _ := created.Detail["fields"].(map[string]any)
	if fields["config.bearer_token"] != hiddenValue || fields["name"] != "HR API" || fields["config.base_url"] != hr.URL {
		t.Fatalf("create snapshot: %+v", fields)
	}
	want(t, "update source", admin.do("PUT", "/api/sources/"+src.ID, map[string]any{"name": "HR API (prod)", "type": "rest", "enabled": false,
		"options": map[string]any{"external_id_field": "id"},
		"config":  map[string]any{"base_url": hr.URL, "records_path": "data", "auth": "bearer", "bearer_token": "tok-SECOND-secret"}}, nil), 200)
	upd := auditRows(t, admin, "source.update")[0]
	if v := changeOf(t, upd, "config.bearer_token"); v["changed"] != true || v["from"] != nil || v["to"] != nil {
		t.Fatalf("a changed secret is only flagged: %+v", v)
	}
	if v := changeOf(t, upd, "name"); v["from"] != "HR API" || v["to"] != "HR API (prod)" {
		t.Fatalf("name from/to: %+v", v)
	}
	if v := changeOf(t, upd, "enabled"); v["from"] != true || v["to"] != false {
		t.Fatalf("enabled from/to: %+v", v)
	}
	if ch, _ := upd.Detail["changes"].(map[string]any); ch["config.base_url"] != nil || ch["updated_at"] != nil {
		t.Fatalf("unchanged and bookkeeping fields are left out: %+v", ch)
	}
	want(t, "delete source", admin.do("DELETE", "/api/sources/"+src.ID, nil, nil), 204)
	del := auditRows(t, admin, "source.delete")[0]
	if f, _ := del.Detail["fields"].(map[string]any); f["name"] != "HR API (prod)" || f["config.bearer_token"] != hiddenValue {
		t.Fatalf("delete snapshot: %+v", del.Detail)
	}

	// Mapping and role: every changed field from/to; deletes keep what was deleted.
	var m model.Mapping
	want(t, "create mapping", admin.do("POST", "/api/mappings", map[string]any{"name": "title", "target_attr": "title", "template": "{{ .Title }}", "mode": "sync", "enabled": true}, &m), 201)
	want(t, "update mapping", admin.do("PUT", "/api/mappings/"+m.ID, map[string]any{"name": "title", "target_attr": "title", "template": "{{ .JobTitle }}", "mode": "create_only", "enabled": true}, nil), 200)
	mu := auditRows(t, admin, "mapping.update")[0]
	if v := changeOf(t, mu, "template"); v["from"] != "{{ .Title }}" || v["to"] != "{{ .JobTitle }}" {
		t.Fatalf("template from/to: %+v", v)
	}
	changeOf(t, mu, "mode")
	want(t, "delete mapping", admin.do("DELETE", "/api/mappings/"+m.ID, nil, nil), 204)
	if f, _ := auditRows(t, admin, "mapping.delete")[0].Detail["fields"].(map[string]any); f["template"] != "{{ .JobTitle }}" {
		t.Fatalf("mapping delete snapshot: %+v", f)
	}
	var role model.RoleRule
	want(t, "create role", admin.do("POST", "/api/roles", map[string]any{"name": "Staff", "enabled": true, "priority": 10, "grants": map[string]any{"groups": []string{"GG-All"}}}, &role), 201)
	want(t, "update role", admin.do("PUT", "/api/roles/"+role.ID, map[string]any{"name": "Staff", "enabled": true, "priority": 20, "grants": map[string]any{"groups": []string{"GG-All", "GG-Staff"}}}, nil), 200)
	ru := auditRows(t, admin, "role.update")[0]
	if v := changeOf(t, ru, "grants.groups"); len(v["to"].([]any)) != 2 {
		t.Fatalf("role groups from/to: %+v", v)
	}
	if v := changeOf(t, ru, "priority"); v["from"] != float64(10) || v["to"] != float64(20) {
		t.Fatalf("priority from/to: %+v", v)
	}
	want(t, "delete role", admin.do("DELETE", "/api/roles/"+role.ID, nil, nil), 204)
	if f, _ := auditRows(t, admin, "role.delete")[0].Detail["fields"].(map[string]any); f["name"] != "Staff" {
		t.Fatalf("role delete snapshot: %+v", f)
	}

	// Console accounts: a password change is a flag, never a value or a hash.
	var u struct{ ID string }
	want(t, "create user", admin.do("POST", "/api/users", map[string]any{"username": "carol", "role": "viewer", "password": "Carol-Passw0rd-123"}, &u), 201)
	want(t, "update user", admin.do("PUT", "/api/users/"+u.ID, map[string]any{"role": "approver", "password": "Carol-Passw0rd-456"}, nil), 200)
	uu := auditRows(t, admin, "user.update")[0]
	if v := changeOf(t, uu, "role"); v["from"] != "viewer" || v["to"] != "approver" {
		t.Fatalf("role from/to: %+v", v)
	}
	if v := changeOf(t, uu, "password"); v["changed"] != true {
		t.Fatalf("password flagged: %+v", v)
	}
	want(t, "delete user", admin.do("DELETE", "/api/users/"+u.ID, nil, nil), 204)
	if f, _ := auditRows(t, admin, "user.delete")[0].Detail["fields"].(map[string]any); f["username"] != "carol" {
		t.Fatalf("user delete snapshot: %+v", f)
	}

	noSecrets(t, admin, "tok-FIRST-secret", "tok-SECOND-secret", "Carol-Passw0rd-123", "Carol-Passw0rd-456", "$2a$")
}

func TestSettingsAuditNeverHoldsSecrets(t *testing.T) {
	e := newTenv(t)
	admin := e.login("admin")
	var doc map[string]any
	want(t, "get settings", admin.do("GET", "/api/settings", nil, &doc), 200)
	notifications := doc["notifications"].(map[string]any)
	smtp := notifications["smtp"].(map[string]any)
	smtp["host"], smtp["port"], smtp["security"], smtp["auth"], smtp["username"], smtp["password"] = "smtp.corp.example", 587, "starttls", "login", "idl", "Smtp-Secret-111"
	doc["auth"] = map[string]any{"local_login": "all", "oidc": map[string]any{"enabled": false, "tenant_id": tTenant, "client_id": tClient,
		"client_secret": "Oidc-Secret-222", "redirect_url": "https://idl.corp.example/api/auth/oidc/callback"}}
	cl := doc["cloud"].(map[string]any)
	cl["tenant_id"], cl["client_id"], cl["auth"], cl["client_secret"] = tTenant, tClient, "secret", "Cloud-Secret-333"
	bk := doc["backup"].(map[string]any)
	bk["passphrase"] = "Backup-Secret-444-long"
	doc["engine"].(map[string]any)["max_destructive_per_run"] = 7
	if st := admin.do("PUT", "/api/settings", doc, nil); st != 200 {
		var errBody map[string]any
		admin.do("PUT", "/api/settings", doc, &errBody)
		t.Fatalf("put settings: %d %v", st, errBody)
	}
	row := auditRows(t, admin, "settings.update")[0]
	for _, p := range []string{"notifications.smtp.password", "auth.oidc.client_secret", "cloud.client_secret", "backup.passphrase"} {
		if v := changeOf(t, row, p); v["changed"] != true || len(v) != 1 {
			t.Errorf("%s must only be flagged: %+v", p, v)
		}
	}
	if v := changeOf(t, row, "engine.max_destructive_per_run"); v["to"] != float64(7) {
		t.Fatalf("plain setting from/to: %+v", v)
	}
	if v := changeOf(t, row, "notifications.smtp.host"); v["to"] != "smtp.corp.example" {
		t.Fatalf("smtp host from/to: %+v", v)
	}
	if paths, _ := row.Detail["changed"].([]any); len(paths) == 0 {
		t.Fatalf("the changed path list is kept: %+v", row.Detail)
	}

	// Saving again with the masked values changes nothing secret.
	want(t, "get masked", admin.do("GET", "/api/settings", nil, &doc), 200)
	doc["engine"].(map[string]any)["max_destructive_per_run"] = 8
	want(t, "put again", admin.do("PUT", "/api/settings", doc, nil), 200)
	again := auditRows(t, admin, "settings.update")[0]
	if ch, _ := again.Detail["changes"].(map[string]any); len(ch) != 1 || ch["engine.max_destructive_per_run"] == nil {
		t.Fatalf("only the one real change: %+v", ch)
	}
	noSecrets(t, admin, "Smtp-Secret-111", "Oidc-Secret-222", "Cloud-Secret-333", "Backup-Secret-444-long")
}

func TestManualPersonAuditCarriesChanges(t *testing.T) {
	e := newTenv(t)
	admin, approver := e.login("admin"), e.login("approver")
	var src model.Source
	want(t, "manual source", admin.do("POST", "/api/sources", map[string]any{"name": "Contractors", "type": "manual", "enabled": true,
		"options": map[string]any{}, "config": map[string]any{}}, &src), 201)
	base := "/api/manual/sources/" + src.ID
	var p model.ManualPerson
	want(t, "add", approver.do("POST", base+"/people", map[string]any{"attrs": map[string]string{"FirstName": "Carl", "LastName": "Contract", "Department": "IT"}}, &p), 201)
	want(t, "edit", approver.do("PUT", base+"/people/"+p.ID, map[string]any{"attrs": map[string]string{"FirstName": "Carl", "LastName": "Contract", "Department": "Sales"}}, nil), 200)
	row := auditRows(t, admin, "manual.update")[0]
	if v := changeOf(t, row, "Department"); v["from"] != "IT" || v["to"] != "Sales" || row.Actor != "approver" {
		t.Fatalf("manual edit from/to: %+v (%s)", v, row.Actor)
	}
	if row.Detail["source_id"] != src.ID {
		t.Fatalf("source id recorded: %+v", row.Detail)
	}
}

func TestIdentityHistoryOverHTTP(t *testing.T) {
	e := newTenv(t)
	admin, approver, viewer := e.login("admin"), e.login("approver"), e.login("viewer")
	e.seedSource(admin)
	e.runOnce(admin)
	var pend struct{ Items []model.Change }
	approver.do("GET", "/api/changes?status=pending&type=create", nil, &pend)
	for _, c := range pend.Items {
		want(t, "approve", approver.do("POST", "/api/changes/"+c.ID+"/approve", map[string]string{"note": "welcome"}, nil), 200)
	}
	if _, err := e.eng.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	var ids struct{ Items []model.Identity }
	viewer.do("GET", "/api/identities?q=E1", nil, &ids)
	if len(ids.Items) != 1 {
		t.Fatalf("identities: %+v", ids)
	}
	jane := ids.Items[0]

	// A change row written before change rows carried the identity is found through the change.
	var janeChange string
	for _, c := range pend.Items {
		if c.ExternalID == "E1" {
			janeChange = c.ID
		}
	}
	if err := e.st.AppendAudit(context.Background(), "legacy", "change.set_ou", "change", janeChange, map[string]any{"ou": "OU=Old,DC=x"}); err != nil {
		t.Fatal(err)
	}

	type item struct {
		Seq        int64    `json:"seq"`
		Kind       string   `json:"kind"`
		Action     string   `json:"action"`
		Title      string   `json:"title"`
		Lines      []string `json:"lines"`
		ChangeID   string   `json:"change_id"`
		ChangeType string   `json:"change_type"`
		Actor      string   `json:"actor"`
	}
	var hist struct {
		Items []item `json:"items"`
		Next  int64  `json:"next_before_seq"`
	}
	want(t, "unauthenticated", e.client().do("GET", "/api/identities/"+jane.ID+"/history", nil, nil), 401)
	want(t, "unknown identity", viewer.do("GET", "/api/identities/nope/history", nil, nil), 404)
	want(t, "viewer reads history", viewer.do("GET", "/api/identities/"+jane.ID+"/history", nil, &hist), 200)
	titles := map[string]item{}
	for _, it := range hist.Items {
		titles[it.Title] = it
	}
	for _, w := range []string{"First seen in csv", "Account creation planned, waiting for approval", "Account creation approved by approver",
		"Account created in AD (jdoe)", "Linked to AD account jdoe", "OU chosen by legacy"} {
		if _, ok := titles[w]; !ok {
			t.Errorf("history lacks %q: %v", w, keys(titles))
		}
	}
	if it := titles["Account created in AD (jdoe)"]; it.Kind != "change" || it.ChangeType != model.ChangeCreate || it.ChangeID != janeChange || len(it.Lines) == 0 {
		t.Errorf("created row: %+v", it)
	}
	if it := titles["Account creation approved by approver"]; len(it.Lines) != 1 || it.Lines[0] != "Note: welcome" {
		t.Errorf("approval note: %+v", it)
	}
	for _, it := range hist.Items {
		if strings.Contains(it.Title, "Bob") || strings.Contains(strings.Join(it.Lines, " "), "bsmith") {
			t.Errorf("someone else's row: %+v", it)
		}
	}
	for i := 1; i < len(hist.Items); i++ {
		if hist.Items[i].Seq >= hist.Items[i-1].Seq {
			t.Fatalf("newest first: %+v", hist.Items)
		}
	}

	// Paging.
	var p1, p2 struct {
		Items []item `json:"items"`
		Next  int64  `json:"next_before_seq"`
	}
	viewer.do("GET", "/api/identities/"+jane.ID+"/history?limit=2", nil, &p1)
	if len(p1.Items) != 2 || p1.Next != p1.Items[1].Seq {
		t.Fatalf("page 1: %+v", p1)
	}
	viewer.do("GET", "/api/identities/"+jane.ID+"/history?limit=2&before_seq="+itoa(p1.Next), nil, &p2)
	if len(p2.Items) == 0 || p2.Items[0].Seq >= p1.Next {
		t.Fatalf("page 2: %+v", p2)
	}
	var all struct {
		Next int64 `json:"next_before_seq"`
	}
	viewer.do("GET", "/api/identities/"+jane.ID+"/history?limit=200", nil, &all)
	if all.Next != 0 {
		t.Fatalf("no more pages: %+v", all)
	}

	var detail struct {
		Origin    string `json:"origin"`
		CreatedBy string `json:"created_by"`
		Last      any    `json:"ad_last_known"`
	}
	viewer.do("GET", "/api/identities/"+jane.ID, nil, &detail)
	if detail.Origin != "source" || detail.CreatedBy != "" || detail.Last != nil {
		t.Fatalf("identity detail: %+v", detail)
	}
}

func TestIdentityHistoryIncludesConsoleRecordAndLastKnownAccount(t *testing.T) {
	e := newTenv(t)
	admin, approver, viewer := e.login("admin"), e.login("approver"), e.login("viewer")
	var src model.Source
	want(t, "manual source", admin.do("POST", "/api/sources", map[string]any{"name": "Contractors", "type": "manual", "enabled": true,
		"options": map[string]any{}, "config": map[string]any{}}, &src), 201)
	base := "/api/manual/sources/" + src.ID
	var p model.ManualPerson
	want(t, "add", approver.do("POST", base+"/people", map[string]any{"attrs": map[string]string{"FirstName": "Carl", "LastName": "Contract", "Department": "IT"}}, &p), 201)
	want(t, "edit", approver.do("PUT", base+"/people/"+p.ID, map[string]any{"attrs": map[string]string{"FirstName": "Carl", "LastName": "Contract", "Department": "Sales"}}, nil), 200)
	e.runOnce(admin)
	var ids struct{ Items []model.Identity }
	viewer.do("GET", "/api/identities?q="+p.ExternalID, nil, &ids)
	if len(ids.Items) != 1 {
		t.Fatalf("identities: %+v", ids)
	}
	carl := ids.Items[0]
	var hist struct {
		Items []struct {
			Kind  string   `json:"kind"`
			Title string   `json:"title"`
			Lines []string `json:"lines"`
		} `json:"items"`
	}
	viewer.do("GET", "/api/identities/"+carl.ID+"/history", nil, &hist)
	got := map[string][]string{}
	for _, it := range hist.Items {
		got[it.Title] = it.Lines
	}
	if _, ok := got["Added in the console by approver (Contractors)"]; !ok {
		t.Errorf("first seen names who added them: %v", got)
	}
	if lines, ok := got["Edited in the console by approver"]; !ok || len(lines) != 1 || lines[0] != "Department: IT → Sales" {
		t.Errorf("console edit with from/to: %v", got)
	}
	if _, ok := got["Added in the console by approver"]; !ok {
		t.Errorf("the console record's own row: %v", got)
	}
	var detail struct {
		Origin    string `json:"origin"`
		CreatedBy string `json:"created_by"`
	}
	viewer.do("GET", "/api/identities/"+carl.ID, nil, &detail)
	if detail.Origin != "manual" || detail.CreatedBy != "approver" {
		t.Fatalf("identity detail: %+v", detail)
	}

	// An account removed outside IDLifecycle: the detail keeps what it was.
	var pend struct{ Items []model.Change }
	approver.do("GET", "/api/changes?status=pending&type=create", nil, &pend)
	for _, c := range pend.Items {
		approver.do("POST", "/api/changes/"+c.ID+"/approve", nil, nil)
	}
	if _, err := e.eng.Execute(context.Background()); err != nil {
		t.Fatal(err)
	}
	u, ok := e.fake.ByExternal(p.ExternalID)
	if !ok {
		t.Fatal("no account created")
	}
	// Another account stays, or the empty-snapshot guard would (rightly) stop the run.
	e.fake.AddUser(ad.FakeUser{DN: "CN=Other,OU=Staff,DC=x", Sam: "other", UPN: "other@x.test", Enabled: true, External: "X-OTHER"})
	delete(e.fake.Users, u.GUID)
	e.runOnce(admin)
	var gone struct {
		Last *struct {
			GUID, Sam, Reason string
		} `json:"ad_last_known"`
	}
	viewer.do("GET", "/api/identities/"+carl.ID, nil, &gone)
	if gone.Last == nil || gone.Last.GUID != u.GUID || gone.Last.Sam != u.Sam || gone.Last.Reason != "missing" {
		t.Fatalf("last known account: %+v", gone.Last)
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
