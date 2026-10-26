package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/engine"
	"idlifecycle/internal/model"
	"idlifecycle/internal/secrets"
	"idlifecycle/internal/store"
	"idlifecycle/internal/store/storetest"
)

const pw = "correct-horse-battery"

type fakeSched struct{ reloads int32 }

func (f *fakeSched) Reload()                 { atomic.AddInt32(&f.reloads, 1) }
func (f *fakeSched) Next() (time.Time, bool) { return time.Now().Add(time.Hour), true }

type tenv struct {
	t     *testing.T
	srv   *httptest.Server
	st    *store.Store
	fake  *ad.Fake
	eng   *engine.Engine
	sched *fakeSched
	csv   string
	api   *Server
}

func newTenv(t *testing.T) *tenv {
	t.Helper()
	BcryptCost = 4
	st := storetest.New(t)
	c, _ := secrets.New(make([]byte, 32))
	fake := ad.NewFake()
	fake.AddGroup("GG-All", "CN=GG-All,OU=Groups,DC=x")
	eng := engine.New(st, fake, c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	sched := &fakeSched{}
	api := &Server{Store: st, Engine: eng, Cipher: c, Scheduler: sched, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Version: "test", BaseCtx: context.Background(),
		Static: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "<html>spa</html>") })}
	srv := httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)

	e := &tenv{t: t, srv: srv, st: st, fake: fake, eng: eng, sched: sched, csv: filepath.Join(t.TempDir(), "hr.csv"), api: api}
	e.addUser("admin", model.RoleAdmin)
	e.addUser("approver", model.RoleApprover)
	e.addUser("viewer", model.RoleViewer)

	s := model.DefaultSettings()
	s.Engine.DryRun = false
	s.AD.SearchBases = []string{"OU=Staff,DC=x"}
	s.AD.DefaultOU = "OU=Staff,DC=x"
	s.AD.UPNSuffix = "x.test"
	if err := st.SaveSettings(context.Background(), s, "test"); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *tenv) addUser(name, role string) model.User {
	h, err := HashPassword(pw)
	if err != nil {
		e.t.Fatal(err)
	}
	u := model.User{ID: uuid.NewString(), Username: name, PasswordHash: h, Role: role, CreatedAt: time.Now()}
	if err := e.st.CreateUser(context.Background(), u); err != nil {
		e.t.Fatal(err)
	}
	return u
}

type client struct {
	t    *testing.T
	base string
	http *http.Client
	csrf string
}

func (e *tenv) client() *client {
	jar, _ := cookiejar.New(nil)
	return &client{t: e.t, base: e.srv.URL, http: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (e *tenv) login(user string) *client {
	c := e.client()
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	if st := c.do("POST", "/api/auth/login", map[string]string{"username": user, "password": pw}, &out); st != 200 {
		e.t.Fatalf("login %s: %d", user, st)
	}
	c.csrf = out.CSRF
	return c
}

func (c *client) raw(method, path string, body any, hdr map[string]string) (*http.Response, []byte) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.csrf != "" && method != "GET" {
		req.Header.Set("X-CSRF-Token", c.csrf)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (c *client) do(method, path string, body any, out any) int {
	c.t.Helper()
	resp, b := c.raw(method, path, body, nil)
	if out != nil && len(b) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			c.t.Fatalf("%s %s: bad JSON %q: %v", method, path, b, err)
		}
	}
	return resp.StatusCode
}

func want(t *testing.T, what string, got, exp int) {
	t.Helper()
	if got != exp {
		t.Errorf("%s: status %d, want %d", what, got, exp)
	}
}

// ---- auth -------------------------------------------------------------------------

func TestUnauthenticatedAccessIsRefused(t *testing.T) {
	e := newTenv(t)
	c := e.client()
	for _, p := range []string{"/api/status", "/api/changes", "/api/settings", "/api/users", "/api/tables/changes"} {
		want(t, p, c.do("GET", p, nil, nil), 401)
	}
	want(t, "login required for writes", c.do("POST", "/api/runs", nil, nil), 401)
	want(t, "unknown endpoint", c.do("GET", "/api/nope", nil, nil), 404)

	resp, body := c.raw("GET", "/", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "spa") {
		t.Errorf("SPA must be public: %d %s", resp.StatusCode, body)
	}
	csp := resp.Header.Get("Content-Security-Policy")
	for _, must := range []string{"default-src 'self'", "script-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, must) {
			t.Errorf("CSP missing %q: %s", must, csp)
		}
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Error("security headers missing")
	}
}

func TestLoginSessionAndLogout(t *testing.T) {
	e := newTenv(t)
	c := e.client()

	resp, body := c.raw("POST", "/api/auth/login", map[string]string{"username": "admin", "password": pw}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("login: %d %s", resp.StatusCode, body)
	}
	var ck *http.Cookie
	for _, x := range resp.Cookies() {
		if x.Name == "idl_session" {
			ck = x
		}
	}
	if ck == nil || !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode || ck.Path != "/" {
		t.Fatalf("session cookie must be HttpOnly + SameSite=Strict: %+v", ck)
	}
	if strings.Contains(string(body), "password") || strings.Contains(string(body), "hash") {
		t.Errorf("login response leaks credentials: %s", body)
	}

	// The raw token is never stored; only its hash.
	rows, _ := e.st.BrowseTable(context.Background(), "audit_log", "", store.Page{})
	_ = rows
	if _, err := e.st.GetSession(context.Background(), ck.Value); err == nil {
		t.Error("session lookup by raw token must not work (token is stored hashed)")
	}

	var me struct {
		User userView `json:"user"`
		CSRF string   `json:"csrf_token"`
	}
	want(t, "me", c.do("GET", "/api/auth/me", nil, &me), 200)
	if me.User.Username != "admin" || me.CSRF == "" {
		t.Errorf("me: %+v", me)
	}

	c.csrf = me.CSRF
	want(t, "logout", c.do("POST", "/api/auth/logout", nil, nil), 204)
	want(t, "session dead after logout", c.do("GET", "/api/auth/me", nil, nil), 401)
}

func TestBadCredentialsAreGenericAndLockAccount(t *testing.T) {
	e := newTenv(t)
	c := e.client()

	var unknown, wrong apiError
	want(t, "unknown user", c.do("POST", "/api/auth/login", map[string]string{"username": "ghost", "password": "whatever-whatever"}, &unknown), 401)
	want(t, "wrong password", c.do("POST", "/api/auth/login", map[string]string{"username": "viewer", "password": "wrong-wrong-wrong"}, &wrong), 401)
	if unknown.Error.Message != wrong.Error.Message {
		t.Errorf("responses must not reveal whether the user exists: %q vs %q", unknown.Error.Message, wrong.Error.Message)
	}

	for i := 0; i < 5; i++ {
		c.do("POST", "/api/auth/login", map[string]string{"username": "approver", "password": "nope-nope-nope"}, nil)
	}
	// Even the right password is refused while locked.
	var lockedResp apiError
	want(t, "locked out looks like any other failure", c.do("POST", "/api/auth/login", map[string]string{"username": "approver", "password": pw}, &lockedResp), 401)
	if lockedResp.Error.Code != "bad_credentials" {
		t.Errorf("lockout must not be distinguishable from a bad password: %+v", lockedResp)
	}
	for _, ev := range mustAudit(t, e, "auth.login_failed") {
		if ev.EntityID == "ghost" || strings.Contains(ev.EntityID, "whatever") {
			t.Errorf("what was typed for an unknown user must not be stored: %q", ev.EntityID)
		}
	}
	events, _, _ := e.st.ListAudit(context.Background(), store.AuditFilter{Action: "auth.login_failed"}, store.Page{Limit: 100})
	if len(events) < 6 {
		t.Errorf("failed logins must be audited, got %d", len(events))
	}
}

func TestCSRFAndOriginProtection(t *testing.T) {
	e := newTenv(t)
	c := e.login("admin")
	body := map[string]any{"name": "x", "target_attr": "title", "template": "{{ .T }}", "mode": "sync"}

	token := c.csrf
	c.csrf = ""
	want(t, "no CSRF token", c.do("POST", "/api/mappings", body, nil), 403)
	c.csrf = "wrong-token"
	want(t, "wrong CSRF token", c.do("POST", "/api/mappings", body, nil), 403)
	c.csrf = token
	resp, _ := c.raw("POST", "/api/mappings", body, map[string]string{"Origin": "https://evil.example"})
	want(t, "cross-origin", resp.StatusCode, 403)
	resp, _ = c.raw("POST", "/api/mappings", body, map[string]string{"Content-Type": "text/plain"})
	want(t, "non-JSON content type", resp.StatusCode, 415)
	want(t, "valid request", c.do("POST", "/api/mappings", body, nil), 201)
}

func TestRoleBasedAccess(t *testing.T) {
	e := newTenv(t)
	viewer, approver, admin := e.login("viewer"), e.login("approver"), e.login("admin")

	want(t, "viewer reads", viewer.do("GET", "/api/mappings", nil, nil), 200)
	want(t, "viewer cannot write", viewer.do("POST", "/api/mappings", map[string]any{}, nil), 403)
	want(t, "viewer cannot sync", viewer.do("POST", "/api/runs", nil, nil), 403)
	want(t, "viewer cannot approve", viewer.do("POST", "/api/changes/x/approve", nil, nil), 403)
	want(t, "viewer cannot manage users", viewer.do("GET", "/api/users", nil, nil), 403)

	want(t, "approver reads", approver.do("GET", "/api/changes", nil, nil), 200)
	want(t, "approver cannot edit settings", approver.do("PUT", "/api/settings", model.DefaultSettings(), nil), 403)
	want(t, "approver cannot run sync", approver.do("POST", "/api/runs", nil, nil), 403)
	want(t, "approver may decide (404 = authorized)", approver.do("POST", "/api/changes/missing/approve", nil, nil), 404)

	want(t, "admin lists users", admin.do("GET", "/api/users", nil, nil), 200)
}

func TestPasswordChangeEndsOtherSessions(t *testing.T) {
	e := newTenv(t)
	a, b := e.login("admin"), e.login("admin")
	want(t, "short password", a.do("POST", "/api/auth/password", map[string]string{"current": pw, "new": "short"}, nil), 400)
	want(t, "wrong current", a.do("POST", "/api/auth/password", map[string]string{"current": "nope-nope-nope", "new": "a-brand-new-password"}, nil), 403)
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	want(t, "change", a.do("POST", "/api/auth/password", map[string]string{"current": pw, "new": "a-brand-new-password"}, &out), 200)
	a.csrf = out.CSRF
	want(t, "current session continues", a.do("GET", "/api/auth/me", nil, nil), 200)
	want(t, "other session revoked", b.do("GET", "/api/auth/me", nil, nil), 401)
}

// ---- users ------------------------------------------------------------------------

func TestUserAdministrationGuards(t *testing.T) {
	e := newTenv(t)
	admin := e.login("admin")

	var created model.User
	want(t, "weak password", admin.do("POST", "/api/users", map[string]any{"username": "n", "role": "viewer", "password": "short"}, nil), 400)
	want(t, "bad role", admin.do("POST", "/api/users", map[string]any{"username": "n", "role": "root", "password": pw}, nil), 400)
	want(t, "create", admin.do("POST", "/api/users", map[string]any{"username": "newbie", "role": "viewer", "password": pw}, &created), 201)
	want(t, "duplicate", admin.do("POST", "/api/users", map[string]any{"username": "newbie", "role": "viewer", "password": pw}, nil), 409)

	var list struct{ Items []map[string]any }
	admin.do("GET", "/api/users", nil, &list)
	for _, u := range list.Items {
		if _, leaked := u["password_hash"]; leaked {
			t.Fatal("password hashes must never be serialized")
		}
	}

	var self model.User
	for _, u := range mustUsers(t, e) {
		if u.Username == "admin" {
			self = u
		}
	}
	want(t, "cannot delete self", admin.do("DELETE", "/api/users/"+self.ID, nil, nil), 409)
	want(t, "cannot demote the last admin", admin.do("PUT", "/api/users/"+self.ID, map[string]any{"role": "viewer"}, nil), 409)
	want(t, "cannot disable the last admin", admin.do("PUT", "/api/users/"+self.ID, map[string]any{"role": "admin", "disabled": true}, nil), 409)

	// A second admin makes demotion legal.
	var second model.User
	want(t, "second admin", admin.do("POST", "/api/users", map[string]any{"username": "admin2", "role": "admin", "password": pw}, &second), 201)
	want(t, "demote admin after adding another", admin.do("PUT", "/api/users/"+self.ID, map[string]any{"role": "viewer"}, nil), 200)
	want(t, "demoted session revoked", admin.do("GET", "/api/users", nil, nil), 401)
}

func mustUsers(t *testing.T, e *tenv) []model.User {
	u, err := e.st.ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// ---- configuration resources ------------------------------------------------------------

func TestSourceSecretsNeverLeave(t *testing.T) {
	e := newTenv(t)
	admin := e.login("admin")

	body := map[string]any{
		"name": "Paylocity", "type": "paylocity", "enabled": true,
		"options": map[string]any{"external_id_field": "employeeId", "status_field": "currentStatus.statusType", "terminated_values": []string{"T"}},
		"config":  map[string]any{"company_id": "C1", "client_id": "cid", "client_secret": "top-secret-value"},
	}
	var created model.Source
	want(t, "create", admin.do("POST", "/api/sources", body, &created), 201)
	if created.Config["client_secret"] != model.MaskedSecret {
		t.Errorf("secret must be masked in responses: %v", created.Config)
	}

	stored, _ := e.st.GetSource(context.Background(), created.ID)
	if v, _ := stored.Config["client_secret"].(string); !secrets.IsEncrypted(v) || strings.Contains(v, "top-secret") {
		t.Fatalf("secret must be encrypted at rest: %v", stored.Config["client_secret"])
	}

	// Round trip with the mask keeps the stored secret.
	var got model.Source
	admin.do("GET", "/api/sources/"+created.ID, nil, &got)
	got.Name = "Paylocity renamed"
	want(t, "update with masked secret", admin.do("PUT", "/api/sources/"+created.ID, got, nil), 200)
	after, _ := e.st.GetSource(context.Background(), created.ID)
	if after.Config["client_secret"] != stored.Config["client_secret"] || after.Name != "Paylocity renamed" {
		t.Error("masked secret must keep the stored ciphertext")
	}

	var all struct{ Items []model.Source }
	admin.do("GET", "/api/sources", nil, &all)
	raw, _ := json.Marshal(all)
	if strings.Contains(string(raw), "top-secret") || strings.Contains(string(raw), "enc:v1:") {
		t.Fatal("list leaks secret material")
	}

	// And it is not in the browsable tables either.
	for _, tbl := range []string{"sources", "users", "sessions", "settings"} {
		want(t, "table "+tbl+" must not be browsable", admin.do("GET", "/api/tables/"+tbl, nil, nil), 404)
	}

	want(t, "unknown type", admin.do("POST", "/api/sources", map[string]any{"name": "x", "type": "ldap", "options": map[string]any{"external_id_field": "id"}}, nil), 400)
	want(t, "missing required config", admin.do("POST", "/api/sources", map[string]any{"name": "y", "type": "paylocity", "options": map[string]any{"external_id_field": "id"}, "config": map[string]any{}}, nil), 400)
	body["name"] = "Paylocity renamed"
	want(t, "duplicate name", admin.do("POST", "/api/sources", body, nil), 409)
}

func TestSourceTestEndpointAndTypes(t *testing.T) {
	e := newTenv(t)
	admin := e.login("admin")

	var types []map[string]any
	want(t, "types", admin.do("GET", "/api/source-types", nil, &types), 200)
	if len(types) < 2 || types[0]["fields"] == nil {
		t.Fatalf("source types must describe their fields: %v", types)
	}

	os.WriteFile(e.csv, []byte("EmployeeID,FirstName\nE1,Jane\nE2,Bob\n"), 0o600)
	var s model.Source
	admin.do("POST", "/api/sources", map[string]any{"name": "csv", "type": "csv", "enabled": true,
		"options": map[string]any{"external_id_field": "EmployeeID"}, "config": map[string]any{"path": e.csv}}, &s)

	var res struct {
		OK         bool
		Count      int
		Attributes []string
		Sample     []map[string]string
		Error      string
	}
	want(t, "test", admin.do("POST", "/api/sources/"+s.ID+"/test", nil, &res), 200)
	if !res.OK || res.Count != 2 || strings.Join(res.Attributes, ",") != "EmployeeID,FirstName" || res.Sample[0]["FirstName"] != "Jane" {
		t.Errorf("test result: %+v", res)
	}

	os.Remove(e.csv)
	admin.do("POST", "/api/sources/"+s.ID+"/test", nil, &res)
	if res.OK || res.Error == "" {
		t.Errorf("failure must be reported, not swallowed: %+v", res)
	}
}

func TestMappingsAndTemplatePreview(t *testing.T) {
	e := newTenv(t)
	admin := e.login("admin")

	var m model.Mapping
	want(t, "create", admin.do("POST", "/api/mappings", map[string]any{"name": "Title", "target_attr": "title", "template": "{{ .Title | trim }}", "mode": "immutable", "enabled": true}, &m), 201)
	if m.OnEmpty != "skip" || m.ValueType != "string" || m.ID == "" {
		t.Errorf("defaults not applied: %+v", m)
	}
	want(t, "protected attribute", admin.do("POST", "/api/mappings", map[string]any{"name": "sam", "target_attr": "sAMAccountName", "template": "x", "mode": "sync"}, nil), 400)
	want(t, "bad template", admin.do("POST", "/api/mappings", map[string]any{"name": "bad", "target_attr": "department", "template": "{{ .A", "mode": "sync"}, nil), 400)
	want(t, "bad mode", admin.do("POST", "/api/mappings", map[string]any{"name": "bad", "target_attr": "department", "template": "x", "mode": "forever"}, nil), 400)

	m.Mode = model.ModeSync
	want(t, "update", admin.do("PUT", "/api/mappings/"+m.ID, m, nil), 200)
	var got model.Mapping
	admin.do("GET", "/api/mappings/"+m.ID, nil, &got)
	if got.Mode != model.ModeSync {
		t.Errorf("update not applied: %+v", got)
	}

	var prev struct {
		OK     bool
		Output string
		Error  string
	}
	want(t, "preview", admin.do("POST", "/api/templates/preview", map[string]any{"template": "{{ .A | upper }}-{{ index . \"b c\" }}", "record": map[string]string{"A": "x", "b c": "y"}}, &prev), 200)
	if !prev.OK || prev.Output != "X-y" {
		t.Errorf("preview: %+v", prev)
	}
	admin.do("POST", "/api/templates/preview", map[string]any{"template": "{{ range . }}", "record": map[string]string{}}, &prev)
	if prev.OK || prev.Error == "" {
		t.Errorf("preview of a bad template must report the error: %+v", prev)
	}

	// Initial-password preview: the password functions plus an AD complexity check.
	var pw struct {
		OK     bool
		Output string
		Error  string
		Check  *string
	}
	want(t, "password preview", admin.do("POST", "/api/templates/preview", map[string]any{"kind": "password", "template": "{{ upper .A }}{{ .B }}!{{ random 4 }}", "record": map[string]string{"A": "x", "B": "2026"}}, &pw), 200)
	if !pw.OK || len(pw.Output) != 10 || pw.Check == nil || *pw.Check != "" {
		t.Errorf("password preview: %+v", pw)
	}
	pw.Check = nil
	admin.do("POST", "/api/templates/preview", map[string]any{"kind": "password", "template": "{{ .A }}", "record": map[string]string{"A": "short"}}, &pw)
	if !pw.OK || pw.Check == nil || !strings.Contains(*pw.Check, "at least 8") {
		t.Errorf("a weak preview must say why: %+v", pw)
	}
	admin.do("POST", "/api/templates/preview", map[string]any{"template": "{{ random 4 }}", "record": map[string]string{}}, &prev)
	if prev.OK {
		t.Errorf("random must not work in an ordinary template preview: %+v", prev)
	}

	want(t, "delete", admin.do("DELETE", "/api/mappings/"+m.ID, nil, nil), 204)
	want(t, "gone", admin.do("GET", "/api/mappings/"+m.ID, nil, nil), 404)
}

func TestRolesAndRolePreview(t *testing.T) {
	e := newTenv(t)
	admin := e.login("admin")
	admin.do("POST", "/api/mappings", map[string]any{"name": "Title", "target_attr": "title", "template": "{{ .Title }}", "mode": "sync", "enabled": true}, nil)

	var base, mkt model.RoleRule
	want(t, "baseline", admin.do("POST", "/api/roles", map[string]any{"name": "Everyone", "enabled": true, "priority": 10, "grants": map[string]any{"groups": []string{"GG-All"}}}, &base), 201)
	if base.Match != "all" {
		t.Errorf("match should default to all: %+v", base)
	}
	want(t, "marketing", admin.do("POST", "/api/roles", map[string]any{
		"name": "Marketing", "enabled": true, "priority": 20, "match": "all",
		"conditions": []map[string]string{{"attr": "Dept", "op": "eq", "value": "Marketing"}},
		"grants":     map[string]any{"groups": []string{"GG-Mkt"}, "attributes": []map[string]string{{"target_attr": "extensionAttribute1", "template": "MKT"}}},
	}, &mkt), 201)
	want(t, "bad operator", admin.do("POST", "/api/roles", map[string]any{"name": "x", "conditions": []map[string]string{{"attr": "a", "op": "sounds_like", "value": "b"}}}, nil), 400)
	want(t, "bad regex", admin.do("POST", "/api/roles", map[string]any{"name": "y", "conditions": []map[string]string{{"attr": "a", "op": "regex", "value": "("}}}, nil), 400)

	var prev struct {
		Matched    []map[string]string
		Groups     []string
		Attributes map[string]string
	}
	want(t, "preview", admin.do("POST", "/api/roles/preview", map[string]any{"record": map[string]string{"Dept": "Marketing", "Title": "Designer"}}, &prev), 200)
	if len(prev.Matched) != 2 || strings.Join(prev.Groups, ",") != "GG-All,GG-Mkt" || prev.Attributes["extensionAttribute1"] != "MKT" || prev.Attributes["title"] != "Designer" {
		t.Errorf("preview: %+v", prev)
	}
	admin.do("POST", "/api/roles/preview", map[string]any{"record": map[string]string{"Dept": "Eng"}}, &prev)
	if len(prev.Matched) != 1 || strings.Join(prev.Groups, ",") != "GG-All" {
		t.Errorf("non-marketing preview: %+v", prev)
	}
}

func TestSettingsValidationAndAudit(t *testing.T) {
	e := newTenv(t)
	admin := e.login("admin")

	var s model.Settings
	want(t, "get", admin.do("GET", "/api/settings", nil, &s), 200)

	bad := s
	bad.Schedule.IntervalMinutes = 0
	want(t, "invalid schedule", admin.do("PUT", "/api/settings", bad, nil), 400)
	bad = s
	bad.Naming.SamTemplate = "{{ range . }}"
	want(t, "unsafe template", admin.do("PUT", "/api/settings", bad, nil), 400)
	bad = s
	bad.Engine.DryRun, bad.AD.UPNSuffix = false, ""
	want(t, "cannot go live without AD settings", admin.do("PUT", "/api/settings", bad, nil), 400)
	bad = s
	bad.NewUser.PasswordMode, bad.NewUser.PasswordTemplate = model.PasswordModeTemplate, "{{ range . }}"
	want(t, "unsafe password template", admin.do("PUT", "/api/settings", bad, nil), 400)
	bad = s
	bad.Naming.MailTemplate, bad.Naming.MailDomain = "{{ random 3 }}", "corp.example.com"
	want(t, "random is not allowed in the mail template", admin.do("PUT", "/api/settings", bad, nil), 400)

	s.Approval.Update = "auto"
	s.Schedule = model.ScheduleSettings{Enabled: true, Kind: "daily", DailyTime: "03:30", Timezone: "America/Chicago", IntervalMinutes: 60, CronExpr: "0 * * * *"}
	before := atomic.LoadInt32(&e.sched.reloads)
	want(t, "valid update", admin.do("PUT", "/api/settings", s, nil), 200)
	if atomic.LoadInt32(&e.sched.reloads) != before+1 {
		t.Error("saving settings must reload the scheduler")
	}
	events, _, _ := e.st.ListAudit(context.Background(), store.AuditFilter{Action: "settings.update"}, store.Page{})
	detail, _ := json.Marshal(events[0].Detail)
	if !strings.Contains(string(detail), "approval.update") || !strings.Contains(string(detail), "schedule.kind") {
		t.Errorf("audit should list changed settings: %s", detail)
	}

	var status map[string]any
	want(t, "status", admin.do("GET", "/api/status", nil, &status), 200)
	if status["mode"] != "live" || status["db_driver"] != "sqlite" {
		t.Errorf("status: %v", status)
	}
	if status["scheduler"].(map[string]any)["next_run"] == nil {
		t.Error("status should expose the next scheduled run")
	}
}

// ---- end-to-end approval flow through the API ---------------------------------------------

func (e *tenv) waitRun(c *client, id string) model.Run {
	e.t.Helper()
	for i := 0; i < 200; i++ {
		var r model.Run
		c.do("GET", "/api/runs/"+id, nil, &r)
		if r.Status != model.RunRunning {
			return r
		}
		time.Sleep(25 * time.Millisecond)
	}
	e.t.Fatal("run did not finish")
	return model.Run{}
}

func (e *tenv) seedSource(admin *client) {
	os.WriteFile(e.csv, []byte("EmployeeID,FirstName,LastName,Dept\nE1,Jane,Doe,Marketing\nE2,Bob,Smith,Eng\n"), 0o600)
	want(e.t, "source", admin.do("POST", "/api/sources", map[string]any{"name": "csv", "type": "csv", "enabled": true,
		"options": map[string]any{"external_id_field": "EmployeeID"}, "config": map[string]any{"path": e.csv}}, nil), 201)
	admin.do("POST", "/api/mappings", map[string]any{"name": "givenName", "target_attr": "givenName", "template": "{{ .FirstName }}", "mode": "sync", "enabled": true}, nil)
	admin.do("POST", "/api/roles", map[string]any{"name": "Everyone", "enabled": true, "priority": 10, "grants": map[string]any{"groups": []string{"GG-All"}}}, nil)
}

func TestApprovalFlowOverHTTP(t *testing.T) {
	e := newTenv(t)
	admin, approver, viewer := e.login("admin"), e.login("approver"), e.login("viewer")
	e.seedSource(admin)

	var started struct{ ID string }
	want(t, "start run", admin.do("POST", "/api/runs", nil, &started), 202)
	run := e.waitRun(admin, started.ID)
	if run.Status != model.RunOK || run.Counters["pending"] != 2 {
		t.Fatalf("run: %+v", run)
	}
	var pend struct {
		Items []model.Change
		Total int
	}
	want(t, "pending list", viewer.do("GET", "/api/changes?status=pending&type=create", nil, &pend), 200)
	if pend.Total != 2 || len(pend.Items[0].Summary) == 0 {
		t.Fatalf("pending: %+v", pend)
	}
	raw, _ := json.Marshal(pend)
	if strings.Contains(strings.ToLower(string(raw)), "password") {
		t.Error("API must never expose passwords")
	}
	if len(e.fake.Users) != 0 {
		t.Fatal("nothing may be created before approval")
	}

	a, b := pend.Items[0], pend.Items[1]
	want(t, "viewer cannot approve", viewer.do("POST", "/api/changes/"+a.ID+"/approve", nil, nil), 403)
	want(t, "reject needs a note", approver.do("POST", "/api/changes/"+a.ID+"/reject", map[string]string{"note": ""}, nil), 400)
	var rejected model.Change
	want(t, "reject", approver.do("POST", "/api/changes/"+a.ID+"/reject", map[string]string{"note": "duplicate hire"}, &rejected), 200)
	if rejected.Status != model.StatusRejected || rejected.DecidedBy != "approver" {
		t.Errorf("rejected: %+v", rejected)
	}
	want(t, "rejected cannot be approved", approver.do("POST", "/api/changes/"+a.ID+"/approve", nil, nil), 409)

	var approved model.Change
	want(t, "approve", approver.do("POST", "/api/changes/"+b.ID+"/approve", map[string]string{"note": "welcome"}, &approved), 200)
	want(t, "double approve", approver.do("POST", "/api/changes/"+b.ID+"/approve", nil, nil), 409)

	if _, err := e.eng.Execute(context.Background()); err != nil { // the background worker does this in production
		t.Fatal(err)
	}
	var done model.Change
	want(t, "get change", viewer.do("GET", "/api/changes/"+b.ID, nil, &done), 200)
	if done.Status != model.StatusApplied || len(e.fake.Users) != 1 {
		t.Fatalf("approved change should be applied: %+v users=%d", done, len(e.fake.Users))
	}

	var bulk struct {
		Results []struct {
			ID    string
			OK    bool
			Error string
		}
	}
	want(t, "bulk validation", approver.do("POST", "/api/changes/bulk", map[string]any{"ids": []string{}, "action": "approve"}, nil), 400)
	want(t, "bulk bad action", approver.do("POST", "/api/changes/bulk", map[string]any{"ids": []string{"x"}, "action": "delete"}, nil), 400)
	want(t, "bulk", approver.do("POST", "/api/changes/bulk", map[string]any{"ids": []string{b.ID, "missing"}, "action": "approve"}, &bulk), 200)
	if len(bulk.Results) != 2 || bulk.Results[0].OK || bulk.Results[1].Error != "not found" {
		t.Errorf("bulk reports per-item outcomes: %+v", bulk)
	}

	// history: runs, identities, audit chain, row browser
	var runs struct{ Total int }
	viewer.do("GET", "/api/runs", nil, &runs)
	var ids struct {
		Items []model.Identity
		Total int
	}
	viewer.do("GET", "/api/identities?q=jane", nil, &ids)
	if runs.Total != 1 || ids.Total != 1 || ids.Items[0].State != model.StateLinked {
		t.Errorf("runs=%d identities=%+v", runs.Total, ids)
	}
	var verify store.AuditVerifyResult
	want(t, "verify", viewer.do("GET", "/api/audit/verify", nil, &verify), 200)
	if !verify.OK || verify.Checked < 5 {
		t.Errorf("audit verify: %+v", verify)
	}
	var tbl store.TableView
	want(t, "table", viewer.do("GET", "/api/tables/changes?q=jane&limit=10", nil, &tbl), 200)
	if tbl.Total != 1 || len(tbl.Columns) == 0 {
		t.Errorf("table: %+v", tbl)
	}
	var audit struct {
		Items []model.AuditEntry
	}
	viewer.do("GET", "/api/audit?action=change.approve", nil, &audit)
	if len(audit.Items) != 1 || audit.Items[0].Actor != "approver" {
		t.Errorf("approvals must be audited with the approver's name: %+v", audit.Items)
	}
}

func TestSourceCannotBeDeletedWhileInUse(t *testing.T) {
	e := newTenv(t)
	admin := e.login("admin")
	e.seedSource(admin)
	var started struct{ ID string }
	admin.do("POST", "/api/runs", nil, &started)
	e.waitRun(admin, started.ID)

	var list struct{ Items []model.Source }
	admin.do("GET", "/api/sources", nil, &list)
	want(t, "in use", admin.do("DELETE", "/api/sources/"+list.Items[0].ID, nil, nil), 409)
}

func TestMalformedRequests(t *testing.T) {
	e := newTenv(t)
	admin := e.login("admin")
	resp, _ := admin.raw("POST", "/api/mappings", nil, map[string]string{"Content-Type": "application/json"})
	want(t, "empty body", resp.StatusCode, 400)
	resp, _ = admin.raw("POST", "/api/mappings", nil, nil)
	want(t, "no content type", resp.StatusCode, 415)
	req, _ := http.NewRequest("POST", admin.base+"/api/mappings", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", admin.csrf)
	r, _ := admin.http.Do(req)
	r.Body.Close()
	want(t, "malformed JSON", r.StatusCode, 400)

	big := strings.Repeat("a", 2<<20)
	req, _ = http.NewRequest("POST", admin.base+"/api/templates/preview", strings.NewReader(`{"template":"`+big+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", admin.csrf)
	r, _ = admin.http.Do(req)
	r.Body.Close()
	want(t, "oversized body", r.StatusCode, 400)

	want(t, "unknown api path", admin.do("GET", "/api/definitely-not-real", nil, nil), 404)
}
