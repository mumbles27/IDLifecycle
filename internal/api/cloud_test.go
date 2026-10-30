package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"idlifecycle/internal/cloud"
	"idlifecycle/internal/model"
	"idlifecycle/internal/secrets"
	"idlifecycle/internal/store"
)

const (
	tenantID = "72f988bf-86f1-41af-91ab-2d7cd011db47"
	appID    = "11111111-2222-3333-4444-555555555555"
	gSales   = "a0000000-0000-0000-0000-000000000002"
	gAdmins  = "a0000000-0000-0000-0000-000000000006"
)

func (e *tenv) cloudFake() *cloud.Fake {
	f := cloud.NewFake()
	f.AddGroup(gSales, "Sales Team", cloud.KindM365)
	f.AddGroup(gAdmins, "Helpdesk Admins", cloud.KindSecurity).RoleAssignable = true
	e.eng.Cloud = f
	return f
}

func secretCloud(secret string) model.CloudSettings {
	return model.CloudSettings{Enabled: true, Environment: model.CloudGlobal, TenantID: tenantID, ClientID: appID,
		Auth: model.CloudAuthSecret, ClientSecret: secret, UserMatch: model.CloudMatchSID}
}

func TestCloudSettingsSecretIsEncryptedMaskedAndKept(t *testing.T) {
	e := newTenv(t)
	e.cloudFake()
	admin := e.login("admin")
	const secret = "Gr4ph~Secret.Value_123"

	var s model.Settings
	admin.do("GET", "/api/settings", nil, &s)
	eq := func(what, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("%s: got %q, want %q", what, got, want)
		}
	}
	eq("defaults", s.Cloud.Auth+"/"+s.Cloud.UserMatch+"/"+s.Cloud.Environment, "certificate/sid/global")
	eq("approval default follows update", s.Approval.CloudGroups, s.Approval.Update)

	s.Cloud = secretCloud(secret)
	var saved model.Settings
	want(t, "save", admin.do("PUT", "/api/settings", s, &saved), 200)
	eq("masked in the response", saved.Cloud.ClientSecret, model.MaskedSecret)
	stored, _ := e.st.GetSettings(context.Background())
	if !secrets.IsEncrypted(stored.Cloud.ClientSecret) {
		t.Fatalf("the secret is encrypted at rest: %q", stored.Cloud.ClientSecret)
	}
	for _, who := range []string{"admin", "viewer"} {
		_, body := e.login(who).raw("GET", "/api/settings", nil, nil)
		if strings.Contains(string(body), secret) || strings.Contains(string(body), stored.Cloud.ClientSecret) {
			t.Errorf("%s sees the secret or its ciphertext", who)
		}
	}

	// The mask (or nothing) keeps it; another app registration needs it typed again.
	saved.Schedule.IntervalMinutes = 30
	want(t, "resave masked", admin.do("PUT", "/api/settings", saved, nil), 200)
	again, _ := e.st.GetSettings(context.Background())
	eq("kept", again.Cloud.ClientSecret, stored.Cloud.ClientSecret)
	other := saved
	other.Cloud.ClientID = "99999999-2222-3333-4444-555555555555"
	want(t, "new app without its secret", admin.do("PUT", "/api/settings", other, nil), 400)

	// A client that does not send "cloud" leaves it alone.
	var doc map[string]any
	b, _ := json.Marshal(saved)
	_ = json.Unmarshal(b, &doc)
	delete(doc, "cloud")
	want(t, "save without cloud", admin.do("PUT", "/api/settings", doc, nil), 200)
	again, _ = e.st.GetSettings(context.Background())
	if !again.Cloud.Enabled || again.Cloud.ClientSecret != stored.Cloud.ClientSecret {
		t.Errorf("omitted cloud block kept: %+v", again.Cloud)
	}

	// Validation.
	for name, mut := range map[string]func(*model.CloudSettings){
		"bad tenant":     func(c *model.CloudSettings) { c.TenantID = "not a tenant" },
		"bad client":     func(c *model.CloudSettings) { c.ClientID = "app" },
		"bad thumbprint": func(c *model.CloudSettings) { c.Auth, c.CertThumbprint = model.CloudAuthCertificate, "abc" },
		"exchange needs cert": func(c *model.CloudSettings) {
			c.Exchange = model.CloudExchange{Enabled: true, Organization: "contoso.onmicrosoft.com"}
		},
		"exchange needs org": func(c *model.CloudSettings) {
			c.Auth, c.CertThumbprint, c.Exchange.Enabled = model.CloudAuthCertificate, strings.Repeat("A1", 20), true
		},
		"unknown environment":  func(c *model.CloudSettings) { c.Environment = "mars" },
		"unknown match":        func(c *model.CloudSettings) { c.UserMatch = "email" },
		"ciphertext submitted": func(c *model.CloudSettings) { c.ClientSecret = stored.Cloud.ClientSecret },
	} {
		bad := saved
		mut(&bad.Cloud)
		want(t, name, admin.do("PUT", "/api/settings", bad, nil), 400)
	}
	good := saved
	good.Cloud.Auth, good.Cloud.CertThumbprint = model.CloudAuthCertificate, "ab cd ef 01 23 45 67 89 ab cd ef 01 23 45 67 89 ab cd ef 01"
	good.Cloud.Exchange = model.CloudExchange{Enabled: true, Organization: "Contoso.onmicrosoft.com"}
	want(t, "certificate + exchange", admin.do("PUT", "/api/settings", good, &saved), 200)
	eq("thumbprint normalized", saved.Cloud.CertThumbprint, "ABCDEF0123456789ABCDEF0123456789ABCDEF01")

	audit, _, _ := e.st.ListAudit(context.Background(), store.AuditFilter{}, store.Page{Limit: 500})
	for _, a := range audit {
		if strings.Contains(fmt.Sprint(a.Detail), secret) {
			t.Fatalf("secret in the audit log: %+v", a)
		}
	}
}

func TestCloudGroupSearchResolveAndTestOverHTTP(t *testing.T) {
	e := newTenv(t)
	f := e.cloudFake()
	admin, approver, viewer := e.login("admin"), e.login("approver"), e.login("viewer")

	var res struct {
		Groups []struct {
			cloud.Group
			Ref string `json:"ref"`
		}
		Truncated bool
		Error     string
	}
	// Connection off: an answer, not an error.
	want(t, "off", admin.do("GET", "/api/cloud/groups?q=sales", nil, &res), 200)
	if len(res.Groups) != 0 || !strings.Contains(res.Error, "not configured") {
		t.Fatalf("connection off: %+v", res)
	}

	const secret = "Gr4ph~Secret.Value_123"
	var s model.Settings
	admin.do("GET", "/api/settings", nil, &s)
	s.Cloud = secretCloud(secret)
	want(t, "save", admin.do("PUT", "/api/settings", s, nil), 200)

	for _, c := range []*struct {
		who  *client
		name string
	}{{approver, "approver"}, {viewer, "viewer"}} {
		want(t, c.name+" cannot search", c.who.do("GET", "/api/cloud/groups?q=sales", nil, nil), 403)
		want(t, c.name+" cannot resolve", c.who.do("GET", "/api/cloud/groups/resolve?ids="+gSales, nil, nil), 403)
		want(t, c.name+" cannot test", c.who.do("POST", "/api/cloud/test", map[string]any{}, nil), 403)
	}
	want(t, "too short", admin.do("GET", "/api/cloud/groups?q=s", nil, nil), 400)
	want(t, "bad max", admin.do("GET", "/api/cloud/groups?q=sales&max=0", nil, nil), 400)
	want(t, "search", admin.do("GET", "/api/cloud/groups?q=team", nil, &res), 200)
	if len(res.Groups) != 1 || res.Groups[0].Ref != "entra:"+gSales || res.Groups[0].Kind != cloud.KindM365 || !res.Groups[0].Manageable {
		t.Fatalf("search: %+v", res)
	}
	eqS := func(what, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("%s: got %q, want %q", what, got, want)
		}
	}
	eqS("the script got the decrypted secret", f.LastConn.ClientSecret, secret)
	want(t, "search admins", admin.do("GET", "/api/cloud/groups?q=helpdesk", nil, &res), 200)
	if len(res.Groups) != 1 || res.Groups[0].Manageable || res.Groups[0].Reason != cloud.ReasonRoleAssignable {
		t.Fatalf("role-assignable: %+v", res)
	}
	f.Err = errors.New("graph down")
	want(t, "graph error", admin.do("GET", "/api/cloud/groups?q=team", nil, &res), 200)
	if !strings.Contains(res.Error, "graph down") {
		t.Errorf("error: %+v", res)
	}
	f.Err = nil

	want(t, "resolve", admin.do("GET", "/api/cloud/groups/resolve?ids=entra:"+strings.ToUpper(gSales)+","+gAdmins+",a0000000-0000-0000-0000-0000000000ff", nil, &res), 200)
	if len(res.Groups) != 2 || res.Groups[0].Name != "Sales Team" || res.Groups[1].Name != "Helpdesk Admins" {
		t.Fatalf("resolve: %+v", res)
	}
	want(t, "resolve bad id", admin.do("GET", "/api/cloud/groups/resolve?ids=nope", nil, nil), 400)
	want(t, "resolve nothing", admin.do("GET", "/api/cloud/groups/resolve?ids=", nil, nil), 400)

	// Test: the saved connection, or an unsaved one whose masked secret means the saved one.
	var tr struct {
		OK     bool          `json:"ok"`
		Checks []cloud.Check `json:"checks"`
		Error  string        `json:"error"`
	}
	f.Checks = []cloud.Check{{Name: "token", OK: true, Detail: "ok"}, {Name: "graph_permissions", OK: false, Detail: "missing GroupMember.ReadWrite.All"}}
	want(t, "test saved", admin.do("POST", "/api/cloud/test", map[string]any{}, &tr), 200)
	if tr.OK || len(tr.Checks) != 2 {
		t.Errorf("a failing check fails the test: %+v", tr)
	}
	f.Checks = nil
	unsaved := secretCloud(model.MaskedSecret)
	unsaved.Enabled = false // testing works whether or not the connection is on
	want(t, "test unsaved", admin.do("POST", "/api/cloud/test", map[string]any{"cloud": unsaved}, &tr), 200)
	if !tr.OK {
		t.Errorf("test: %+v", tr)
	}
	eqS("masked secret = the saved one", f.LastConn.ClientSecret, secret)
	other := secretCloud(model.MaskedSecret)
	other.ClientID = "99999999-2222-3333-4444-555555555555"
	want(t, "another app without its secret", admin.do("POST", "/api/cloud/test", map[string]any{"cloud": other}, nil), 400)
	typed := secretCloud("typed-Secret-456")
	want(t, "test with a typed secret", admin.do("POST", "/api/cloud/test", map[string]any{"cloud": typed}, &tr), 200)
	eqS("typed secret used", f.LastConn.ClientSecret, "typed-Secret-456")
	stored, _ := e.st.GetSettings(context.Background())
	dec, _ := e.api.Cipher.Decrypt(stored.Cloud.ClientSecret)
	eqS("a test never saves", dec, secret)

	audit, _, _ := e.st.ListAudit(context.Background(), store.AuditFilter{Action: "cloud.test"}, store.Page{Limit: 50})
	if len(audit) == 0 {
		t.Fatal("cloud.test is audited")
	}
	all, _, _ := e.st.ListAudit(context.Background(), store.AuditFilter{}, store.Page{Limit: 500})
	for _, a := range all {
		if d := fmt.Sprint(a.Detail); strings.Contains(d, secret) || strings.Contains(d, "typed-Secret-456") {
			t.Fatalf("secret in the audit log: %+v", a)
		}
	}
}

func TestImpactPreviewIncludesCloudGroupsOverHTTP(t *testing.T) {
	e := newTenv(t)
	f := e.cloudFake()
	admin := e.login("admin")
	var s model.Settings
	admin.do("GET", "/api/settings", nil, &s)
	s.Cloud = secretCloud("Gr4ph~Secret.Value_123")
	want(t, "save", admin.do("PUT", "/api/settings", s, nil), 200)

	var res struct {
		Notes []string `json:"notes"`
	}
	roles := []model.RoleRule{{ID: "r1", Name: "Everyone", Enabled: true, Match: "all", Grants: model.Grants{Groups: []string{"entra:" + gSales}}}}
	want(t, "preview", admin.do("POST", "/api/preview/impact", map[string]any{"roles": roles}, &res), 200)
	snapped := false
	for _, c := range f.Calls {
		snapped = snapped || c == "snapshot"
	}
	if !snapped {
		t.Errorf("the preview reads the cloud groups: %v (notes %v)", f.Calls, res.Notes)
	}
	bad := []model.RoleRule{{ID: "r1", Name: "Everyone", Enabled: true, Match: "all", Grants: model.Grants{Groups: []string{"entra:nope"}}}}
	want(t, "bad cloud reference", admin.do("POST", "/api/preview/impact", map[string]any{"roles": bad}, nil), 400)
}
