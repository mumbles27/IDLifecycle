package api

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"idlifecycle/internal/model"
	"idlifecycle/internal/secrets"
	"idlifecycle/internal/store"
)

const (
	tTenant = "11111111-2222-3333-4444-555555555555"
	tClient = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	tSecret = "s3cret-value~from.entra"
	tRedir  = "http://localhost/api/auth/oidc/callback"
)

// fakeEntra is a minimal OpenID provider shaped like Entra ID v2.0: discovery per tenant, a JWKS, and a token
// endpoint that enforces the client secret, the redirect URI and the PKCE verifier.
type fakeEntra struct {
	t      *testing.T
	srv    *httptest.Server
	key    *rsa.PrivateKey
	kid    string
	mu     sync.Mutex
	codes  map[string]fakeCode
	tokens int
}

type fakeCode struct {
	claims    map[string]any
	challenge string
}

func newFakeEntra(t *testing.T) *fakeEntra {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeEntra{t: t, key: key, kid: "k1", codes: map[string]fakeCode{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{tenant}/v2.0/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		base := f.srv.URL + "/" + r.PathValue("tenant")
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": base + "/v2.0", "authorization_endpoint": base + "/oauth2/v2.0/authorize",
			"token_endpoint": base + "/oauth2/v2.0/token", "jwks_uri": f.srv.URL + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"}, "response_types_supported": []string{"code"},
			"subject_types_supported": []string{"pairwise"},
		})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: f.kid, Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("POST /{tenant}/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = r.ParseForm()
		id, sec, ok := r.BasicAuth()
		if !ok {
			id, sec = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
		}
		if id != tClient || sec != tSecret {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client", "error_description": "AADSTS7000215: Invalid client secret provided.\r\nTrace ID: x"})
			return
		}
		f.mu.Lock()
		f.tokens++
		f.mu.Unlock()
		if r.PostForm.Get("grant_type") == "client_credentials" {
			json.NewEncoder(w).Encode(map[string]any{"access_token": "app-token", "token_type": "Bearer", "expires_in": 3600})
			return
		}
		f.mu.Lock()
		c, found := f.codes[r.PostForm.Get("code")]
		delete(f.codes, r.PostForm.Get("code"))
		f.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !found || r.PostForm.Get("redirect_uri") != tRedir || base64.RawURLEncoding.EncodeToString(sum[:]) != c.challenge {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "user-token", "token_type": "Bearer", "expires_in": 3600, "id_token": f.sign(c.claims)})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeEntra) sign(claims map[string]any) string {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: f.key, KeyID: f.kid}}, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	b, _ := json.Marshal(claims)
	obj, err := signer.Sign(b)
	if err != nil {
		f.t.Fatal(err)
	}
	s, _ := obj.CompactSerialize()
	return s
}

// claims returns a valid ID token body for a user; tweak the map to test rejections.
func (f *fakeEntra) claims(nonce, oid, upn string, roles ...string) map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": f.srv.URL + "/" + tTenant + "/v2.0", "aud": tClient, "sub": "pairwise-" + oid, "iat": now.Unix(), "nbf": now.Unix(),
		"exp": now.Add(time.Hour).Unix(), "nonce": nonce, "tid": tTenant, "oid": oid, "preferred_username": upn, "name": upn, "roles": roles,
	}
}

func enableOIDC(t *testing.T, e *tenv, admin *client, f *fakeEntra, mutate func(a map[string]any)) {
	t.Helper()
	e.api.OIDCAuthority = f.srv.URL
	var doc map[string]any
	admin.do("GET", "/api/settings", nil, &doc)
	auth := map[string]any{"local_login": "all", "oidc": map[string]any{
		"enabled": true, "tenant_id": tTenant, "client_id": tClient, "client_secret": tSecret, "redirect_url": tRedir,
		"admin_values": []string{"IDLifecycle.Admin"}, "approver_values": []string{"IDLifecycle.Approver", "99999999-0000-0000-0000-000000000001"},
		"viewer_values": []string{"IDLifecycle.Viewer"},
	}}
	if mutate != nil {
		mutate(auth)
	}
	doc["auth"] = auth
	resp, body := admin.raw("PUT", "/api/settings", doc, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("enable oidc: %d %s", resp.StatusCode, body)
	}
}

// startFlow calls /start like a browser and returns the state, nonce and PKCE challenge it sent to Entra ID.
func startFlow(t *testing.T, c *client) (state, nonce, challenge string) {
	t.Helper()
	resp, body := c.raw("GET", "/api/auth/oidc/start", nil, nil)
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("start: %d %s", resp.StatusCode, body)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := loc.Query()
	if !strings.HasSuffix(loc.Path, "/"+tTenant+"/oauth2/v2.0/authorize") || q.Get("client_id") != tClient || q.Get("redirect_uri") != tRedir ||
		q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || !strings.Contains(q.Get("scope"), "openid") {
		t.Fatalf("authorize URL: %s", loc)
	}
	var ck *http.Cookie
	for _, x := range resp.Cookies() {
		if x.Name == oidcCookie {
			ck = x
		}
	}
	if ck == nil || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode || ck.Value != q.Get("state") {
		t.Fatalf("state cookie must be HttpOnly, SameSite=Lax and carry the state: %+v", ck)
	}
	return q.Get("state"), q.Get("nonce"), q.Get("code_challenge")
}

// finish redeems a code at the callback and returns the response status and page body.
func finish(t *testing.T, f *fakeEntra, c *client, state, challenge string, claims map[string]any) (int, string) {
	t.Helper()
	code := "code-" + state[:8]
	f.mu.Lock()
	f.codes[code] = fakeCode{claims: claims, challenge: challenge}
	f.mu.Unlock()
	resp, body := c.raw("GET", "/api/auth/oidc/callback?code="+code+"&state="+url.QueryEscape(state), nil, nil)
	return resp.StatusCode, string(body)
}

func signIn(t *testing.T, f *fakeEntra, c *client, oid, upn string, roles ...string) (int, string) {
	t.Helper()
	state, nonce, ch := startFlow(t, c)
	return finish(t, f, c, state, ch, f.claims(nonce, oid, upn, roles...))
}

func me(c *client) (int, map[string]any) {
	var out struct {
		User map[string]any `json:"user"`
		CSRF string         `json:"csrf_token"`
	}
	st := c.do("GET", "/api/auth/me", nil, &out)
	c.csrf = out.CSRF
	return st, out.User
}

func TestOIDCIsOffByDefault(t *testing.T) {
	e := newTenv(t)
	var p struct {
		LocalLogin string `json:"local_login"`
		OIDC       struct {
			Enabled bool   `json:"enabled"`
			Label   string `json:"label"`
		} `json:"oidc"`
	}
	want(t, "providers is public", e.client().do("GET", "/api/auth/providers", nil, &p), 200)
	if p.OIDC.Enabled || p.LocalLogin != "all" {
		t.Fatalf("defaults: %+v", p)
	}
	resp, body := e.client().raw("GET", "/api/auth/oidc/start", nil, nil)
	if resp.StatusCode != 404 || !strings.Contains(string(body), "not enabled") {
		t.Errorf("start while disabled: %d %s", resp.StatusCode, body)
	}
}

func TestOIDCSignInCreatesAccountWithRoleFromEntra(t *testing.T) {
	e := newTenv(t)
	f := newFakeEntra(t)
	enableOIDC(t, e, e.login("admin"), f, nil)

	var p struct {
		OIDC struct {
			Enabled bool   `json:"enabled"`
			Label   string `json:"label"`
		} `json:"oidc"`
	}
	e.client().do("GET", "/api/auth/providers", nil, &p)
	if !p.OIDC.Enabled || p.OIDC.Label != "Sign in with Microsoft" {
		t.Fatalf("providers: %+v", p)
	}

	c := e.client()
	st, body := signIn(t, f, c, "OID-1", "jane@corp.test", "IDLifecycle.Approver")
	if st != 200 || !strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatalf("callback: %d %s", st, body)
	}
	code, u := me(c)
	if code != 200 || u["username"] != "jane@corp.test" || u["role"] != model.RoleApprover || u["auth_source"] != model.AuthOIDC {
		t.Fatalf("session after sign-in: %d %+v", code, u)
	}
	want(t, "approver can read", c.do("GET", "/api/changes", nil, nil), 200)
	want(t, "approver cannot change settings", c.do("PUT", "/api/settings", map[string]any{}, nil), 403)

	stored, err := e.st.GetUserByExternalID(t.Context(), strings.ToLower(tTenant)+":oid-1")
	if err != nil || stored.AuthSource != model.AuthOIDC || stored.PasswordHash != "" {
		t.Fatalf("stored account: %+v %v", stored, err)
	}
	audit, _, _ := e.st.ListAudit(t.Context(), store.AuditFilter{Action: "auth.login"}, store.Page{Limit: 50})
	found := false
	for _, a := range audit {
		if a.Actor == "jane@corp.test" && a.Detail["method"] == "oidc" {
			found = true
		}
	}
	if !found {
		t.Errorf("an oidc sign-in must be audited with method=oidc: %+v", audit)
	}

	// Next sign-in: Entra now says admin. The role follows and the old session is signed out.
	c2 := e.client()
	if st, body := signIn(t, f, c2, "OID-1", "jane@corp.test", "IDLifecycle.Admin", "IDLifecycle.Viewer"); st != 200 {
		t.Fatalf("second sign-in: %d %s", st, body)
	}
	if _, u := me(c2); u["role"] != model.RoleAdmin {
		t.Errorf("role must follow Entra ID (highest wins): %+v", u)
	}
	if code, _ := me(c); code != 401 {
		t.Errorf("the session with the old role must be revoked, got %d", code)
	}
	if n, _ := e.st.CountUsers(t.Context()); n != 4 {
		t.Errorf("the second sign-in must reuse the account: %d users", n)
	}
}

func TestOIDCGroupObjectIDMapsToRole(t *testing.T) {
	e := newTenv(t)
	f := newFakeEntra(t)
	enableOIDC(t, e, e.login("admin"), f, nil)
	c := e.client()
	state, nonce, ch := startFlow(t, c)
	cl := f.claims(nonce, "oid-g", "grp@corp.test")
	cl["groups"] = []string{"99999999-0000-0000-0000-000000000001"}
	if st, body := finish(t, f, c, state, ch, cl); st != 200 {
		t.Fatalf("%d %s", st, body)
	}
	if _, u := me(c); u["role"] != model.RoleApprover {
		t.Errorf("group object id must map to approver: %+v", u)
	}
}

func TestOIDCRejections(t *testing.T) {
	e := newTenv(t)
	f := newFakeEntra(t)
	enableOIDC(t, e, e.login("admin"), f, nil)

	cases := []struct {
		name   string
		mutate func(cl map[string]any)
		status int
		page   string
	}{
		{"no role", func(cl map[string]any) { cl["roles"] = []string{"Something.Else"} }, 403, "not assigned an IDLifecycle role"},
		{"wrong nonce", func(cl map[string]any) { cl["nonce"] = "other" }, 401, "does not belong to this sign-in"},
		{"wrong audience", func(cl map[string]any) { cl["aud"] = "someone-else" }, 401, "could not be verified"},
		{"expired", func(cl map[string]any) { cl["exp"] = time.Now().Add(-time.Hour).Unix() }, 401, "could not be verified"},
		{"other issuer", func(cl map[string]any) { cl["iss"] = f.srv.URL + "/99999999-2222-3333-4444-555555555555/v2.0" }, 401, "could not be verified"},
		{"other tenant id", func(cl map[string]any) { cl["tid"] = "99999999-2222-3333-4444-555555555555" }, 403, "configured Entra ID tenant"},
		{"no object id", func(cl map[string]any) { delete(cl, "oid") }, 403, "configured Entra ID tenant"},
	}
	for _, x := range cases {
		t.Run(x.name, func(t *testing.T) {
			c := e.client()
			state, nonce, ch := startFlow(t, c)
			cl := f.claims(nonce, "oid-x", "x@corp.test", "IDLifecycle.Admin")
			x.mutate(cl)
			st, body := finish(t, f, c, state, ch, cl)
			if st != x.status || !strings.Contains(body, x.page) {
				t.Fatalf("got %d %s", st, body)
			}
			if code, _ := me(c); code != 401 {
				t.Error("a rejected sign-in must not create a session")
			}
		})
	}
	if _, err := e.st.GetUserByName(t.Context(), "x@corp.test"); err == nil {
		t.Error("no account may be created by a rejected sign-in")
	}
}

func TestOIDCStateIsBoundToTheBrowserAndSingleUse(t *testing.T) {
	e := newTenv(t)
	f := newFakeEntra(t)
	enableOIDC(t, e, e.login("admin"), f, nil)

	victim, attacker := e.client(), e.client()
	state, nonce, ch := startFlow(t, attacker)
	// Login CSRF: the attacker's code and state replayed in another browser (no matching cookie).
	if st, body := finish(t, f, victim, state, ch, f.claims(nonce, "oid-a", "attacker@corp.test", "IDLifecycle.Admin")); st != 400 || !strings.Contains(body, "did not start in this browser") {
		t.Fatalf("cross-browser callback: %d %s", st, body)
	}
	// The refused attempt did not consume the flow, so a stray replay cannot block the real browser.
	if st, _ := finish(t, f, attacker, state, ch, f.claims(nonce, "oid-a", "attacker@corp.test", "IDLifecycle.Admin")); st != 200 {
		t.Fatalf("the real browser can still finish: %d", st)
	}
	// Replay of the same state in the same browser after success.
	attacker.http.Jar.SetCookies(mustURL(t, e.srv.URL+"/api/auth/oidc/"), []*http.Cookie{{Name: oidcCookie, Value: state, Path: "/api/auth/oidc"}})
	if st, body := finish(t, f, attacker, state, ch, f.claims(nonce, "oid-a", "attacker@corp.test", "IDLifecycle.Admin")); st != 400 || !strings.Contains(body, "expired") {
		t.Fatalf("replayed state: %d %s", st, body)
	}

	// Error returned by Entra ID (user cancelled) is shown, escaped.
	c := e.client()
	state, _, _ = startFlow(t, c)
	resp, body := c.raw("GET", "/api/auth/oidc/callback?state="+url.QueryEscape(state)+"&error=access_denied&error_description="+url.QueryEscape("<b>cancelled</b>"), nil, nil)
	if resp.StatusCode != 401 || strings.Contains(string(body), "<b>") || !strings.Contains(string(body), "&lt;b&gt;cancelled") {
		t.Errorf("IdP error page: %d %s", resp.StatusCode, body)
	}
}

func TestOIDCPKCEVerifierIsRequired(t *testing.T) {
	e := newTenv(t)
	f := newFakeEntra(t)
	enableOIDC(t, e, e.login("admin"), f, nil)
	c := e.client()
	state, nonce, _ := startFlow(t, c)
	// The code was issued for a different challenge: the token endpoint refuses the server's verifier.
	if st, body := finish(t, f, c, state, "not-the-challenge", f.claims(nonce, "oid-p", "p@corp.test", "IDLifecycle.Admin")); st != 401 || !strings.Contains(body, "could not be redeemed") {
		t.Fatalf("%d %s", st, body)
	}
}

func TestOIDCAccountsAndLocalAccountsStaySeparate(t *testing.T) {
	e := newTenv(t)
	f := newFakeEntra(t)
	admin := e.login("admin")
	enableOIDC(t, e, admin, f, nil)

	// An Entra ID account whose name equals an existing local account is refused (no silent takeover).
	c := e.client()
	if st, body := signIn(t, f, c, "oid-v", "viewer", "IDLifecycle.Admin"); st != 409 || !strings.Contains(body, "local account named viewer") {
		t.Fatalf("name collision: %d %s", st, body)
	}

	if st, _ := signIn(t, f, c, "oid-j", "jo@corp.test", "IDLifecycle.Viewer"); st != 200 {
		t.Fatal("sign-in")
	}
	// No password sign-in for an Entra ID account, and no password to change.
	want(t, "password login for oidc account", e.client().do("POST", "/api/auth/login", map[string]string{"username": "jo@corp.test", "password": ""}, nil), 401)
	me(c)
	want(t, "change password", c.do("POST", "/api/auth/password", map[string]string{"current": "x", "new": "a-long-new-password"}, nil), 400)

	var users struct{ Items []model.User }
	admin.do("GET", "/api/users", nil, &users)
	var jo model.User
	for _, u := range users.Items {
		if u.Username == "jo@corp.test" {
			jo = u
		}
	}
	if jo.AuthSource != model.AuthOIDC {
		t.Fatalf("users list must show the source: %+v", users.Items)
	}
	put := func(body map[string]any) int { return admin.do("PUT", "/api/users/"+jo.ID, body, nil) }
	want(t, "no password for oidc", put(map[string]any{"password": "a-long-new-password"}), 400)
	want(t, "no role change for oidc", put(map[string]any{"role": "admin"}), 400)
	want(t, "no rename for oidc", put(map[string]any{"username": "other"}), 400)
	want(t, "disable is allowed", put(map[string]any{"username": jo.Username, "role": jo.Role, "disabled": true}), 200)
	if code, _ := me(c); code != 401 {
		t.Error("disabling must sign the account out")
	}
	if st, body := signIn(t, f, e.client(), "oid-j", "jo@corp.test", "IDLifecycle.Viewer"); st != 403 || !strings.Contains(body, "disabled") {
		t.Errorf("a disabled account must not sign in through Entra ID: %d %s", st, body)
	}
}

func TestLocalLoginAdminsOnly(t *testing.T) {
	e := newTenv(t)
	f := newFakeEntra(t)
	admin := e.login("admin")
	approver := e.login("approver")

	// Needs Entra ID sign-in to be on.
	var doc map[string]any
	admin.do("GET", "/api/settings", nil, &doc)
	doc["auth"] = map[string]any{"local_login": "admins_only", "oidc": map[string]any{"enabled": false}}
	want(t, "admins_only without oidc", admin.do("PUT", "/api/settings", doc, nil), 400)

	enableOIDC(t, e, admin, f, func(a map[string]any) { a["local_login"] = "admins_only" })
	if code, _ := me(approver); code != 401 {
		t.Error("switching to admins_only must sign out local non-admins")
	}
	if code, _ := me(admin); code != 200 {
		t.Error("local admins keep their session")
	}
	resp, body := e.client().raw("POST", "/api/auth/login", map[string]string{"username": "approver", "password": pw}, nil)
	if resp.StatusCode != 403 || !strings.Contains(string(body), "limited to administrators") {
		t.Errorf("local approver: %d %s", resp.StatusCode, body)
	}
	want(t, "bad password still looks like a bad password", e.client().do("POST", "/api/auth/login", map[string]string{"username": "approver", "password": "wrong-password-123"}, nil), 401)
	e.login("admin") // break-glass still works

	// Turning Entra ID sign-in off signs out its accounts.
	c := e.client()
	if st, _ := signIn(t, f, c, "oid-z", "z@corp.test", "IDLifecycle.Viewer"); st != 200 {
		t.Fatal("sign-in")
	}
	admin.do("GET", "/api/settings", nil, &doc)
	doc["auth"] = map[string]any{"local_login": "all", "oidc": map[string]any{"enabled": false, "tenant_id": tTenant, "client_id": tClient, "client_secret": model.MaskedSecret, "redirect_url": tRedir}}
	want(t, "disable oidc", admin.do("PUT", "/api/settings", doc, nil), 200)
	if code, _ := me(c); code != 401 {
		t.Error("turning Entra ID sign-in off must revoke its sessions")
	}
}

func TestOIDCSettingsSecretHandlingAndValidation(t *testing.T) {
	e := newTenv(t)
	f := newFakeEntra(t)
	admin := e.login("admin")
	enableOIDC(t, e, admin, f, nil)

	var doc map[string]any
	admin.do("GET", "/api/settings", nil, &doc)
	o := doc["auth"].(map[string]any)["oidc"].(map[string]any)
	if o["client_secret"] != model.MaskedSecret {
		t.Fatalf("secret must be masked: %v", o["client_secret"])
	}
	raw, _ := e.st.GetSettings(t.Context())
	if !secrets.IsEncrypted(raw.Auth.OIDC.ClientSecret) {
		t.Fatal("secret must be encrypted at rest")
	}
	viewer := e.login("viewer")
	var vdoc map[string]any
	viewer.do("GET", "/api/settings", nil, &vdoc)
	if b, _ := json.Marshal(vdoc); strings.Contains(string(b), tSecret) {
		t.Fatal("viewers must never see the secret")
	}

	// Saving the masked value keeps the secret: sign-in still works.
	want(t, "save masked", admin.do("PUT", "/api/settings", doc, nil), 200)
	if st, _ := signIn(t, f, e.client(), "oid-k", "k@corp.test", "IDLifecycle.Viewer"); st != 200 {
		t.Fatal("the kept secret must still work")
	}
	// A client that omits "auth" leaves sign-in alone.
	delete(doc, "auth")
	want(t, "save without auth", admin.do("PUT", "/api/settings", doc, nil), 200)
	if after, _ := e.st.GetSettings(t.Context()); !after.Auth.OIDC.Enabled || after.Auth.OIDC.ClientSecret != raw.Auth.OIDC.ClientSecret {
		t.Fatal("omitting auth must not change sign-in settings")
	}

	bad := func(name string, mutate func(o map[string]any), msg string) {
		t.Helper()
		admin.do("GET", "/api/settings", nil, &doc)
		o := doc["auth"].(map[string]any)["oidc"].(map[string]any)
		mutate(o)
		resp, body := admin.raw("PUT", "/api/settings", doc, nil)
		if resp.StatusCode != 400 || !strings.Contains(string(body), msg) {
			t.Errorf("%s: %d %s", name, resp.StatusCode, body)
		}
	}
	bad("tenant domain", func(o map[string]any) { o["tenant_id"] = "corp.onmicrosoft.com"; o["client_secret"] = "x" }, "GUID")
	bad("client id", func(o map[string]any) { o["client_id"] = "app" }, "client_id")
	bad("http redirect", func(o map[string]any) { o["redirect_url"] = "http://idl.corp.test/api/auth/oidc/callback" }, "https")
	bad("wrong path", func(o map[string]any) { o["redirect_url"] = "https://idl.corp.test/callback" }, "/api/auth/oidc/callback")
	bad("no roles", func(o map[string]any) {
		o["admin_values"], o["approver_values"], o["viewer_values"] = []string{}, []string{" "}, nil
	}, "at least one app role")
	bad("new app keeps old secret", func(o map[string]any) { o["client_id"] = "bbbbbbbb-bbbb-cccc-dddd-eeeeeeeeeeee" }, "enter the client secret")
	bad("ciphertext", func(o map[string]any) { o["client_secret"] = raw.Auth.OIDC.ClientSecret }, "encrypted value")
	bad("no secret at all", func(o map[string]any) {
		o["client_id"] = "bbbbbbbb-bbbb-cccc-dddd-eeeeeeeeeeee"
		o["client_secret"] = ""
	}, "client secret")
}

func TestOIDCTestEndpoint(t *testing.T) {
	e := newTenv(t)
	f := newFakeEntra(t)
	e.api.OIDCAuthority = f.srv.URL
	admin := e.login("admin")
	var res struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	body := func(secret, tenant string) map[string]any {
		return map[string]any{"auth": map[string]any{"oidc": map[string]any{"tenant_id": tenant, "client_id": tClient, "client_secret": secret, "redirect_url": tRedir,
			"admin_values": []string{"IDLifecycle.Admin"}}}}
	}
	want(t, "test", admin.do("POST", "/api/auth/oidc/test", body(tSecret, tTenant), &res), 200)
	if !res.OK {
		t.Fatalf("good config: %+v", res)
	}
	admin.do("POST", "/api/auth/oidc/test", body("wrong", tTenant), &res)
	if res.OK || !strings.Contains(res.Error, "invalid_client") || strings.Contains(res.Error, "Trace ID") || strings.Contains(res.Error, "wrong") {
		t.Errorf("bad secret: %+v", res)
	}
	admin.do("POST", "/api/auth/oidc/test", body(tSecret, "corp.onmicrosoft.com"), &res)
	if res.OK || !strings.Contains(res.Error, "GUID") {
		t.Errorf("bad tenant: %+v", res)
	}
	want(t, "viewer", e.login("viewer").do("POST", "/api/auth/oidc/test", body(tSecret, tTenant), nil), 403)
	if s, _ := e.st.GetSettings(t.Context()); s.Auth.OIDC.Enabled || s.Auth.OIDC.ClientSecret != "" {
		t.Error("the test endpoint must not store anything")
	}
}

func TestOIDCRoleMapping(t *testing.T) {
	o := model.DefaultOIDC()
	o.ApproverValues = append(o.ApproverValues, "ABCDEF00-0000-0000-0000-000000000000")
	cases := []struct {
		roles, groups []string
		want          string
	}{
		{[]string{"IDLifecycle.Viewer"}, nil, model.RoleViewer},
		{[]string{"idlifecycle.admin"}, nil, model.RoleAdmin},
		{[]string{"IDLifecycle.Viewer"}, []string{"abcdef00-0000-0000-0000-000000000000"}, model.RoleApprover},
		{[]string{"IDLifecycle.Viewer", "IDLifecycle.Admin"}, nil, model.RoleAdmin},
		{[]string{"Other"}, []string{"g"}, ""},
		{nil, nil, ""},
	}
	for _, c := range cases {
		if got := oidcRole(o, c.roles, c.groups); got != c.want {
			t.Errorf("%v %v: got %q want %q", c.roles, c.groups, got, c.want)
		}
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
