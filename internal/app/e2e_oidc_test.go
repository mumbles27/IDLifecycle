package app

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/api"
	"idlifecycle/internal/engine"
	"idlifecycle/internal/model"
	"idlifecycle/internal/secrets"
	"idlifecycle/internal/store/storetest"
	"idlifecycle/internal/web"
)

// TestOIDCSignInInHeadlessBrowser runs the whole Entra ID redirect flow in a real browser: the app on
// 127.0.0.1 and a fake identity provider on "localhost" (a different site, as login.microsoftonline.com is),
// so the browser applies its real SameSite rules to the state cookie (Lax, sent on the way back) and the
// session cookie (Strict, set on a cross-site response and used after the same-site meta refresh).
// It passes when the SPA's own /api/auth/me call succeeds with the new session.
func TestOIDCSignInInHeadlessBrowser(t *testing.T) {
	browser := findEdge()
	if browser == "" {
		t.Skip("no Edge/Chrome installed")
	}
	const tenant, client, secret = "11111111-2222-3333-4444-555555555555", "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "e2e-secret"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := storetest.New(t)
	cipher, _ := secrets.New(make([]byte, 32))
	eng := engine.New(st, ad.NewFake(), cipher, log)
	ctx := context.Background()

	// ---- fake Entra ID ----
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	var idpBase string
	var mu sync.Mutex
	codes := map[string]map[string]any{}
	idp := http.NewServeMux()
	idp.HandleFunc("GET /{tenant}/v2.0/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		b := idpBase + "/" + r.PathValue("tenant")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"issuer": b + "/v2.0", "authorization_endpoint": b + "/oauth2/v2.0/authorize",
			"token_endpoint": b + "/oauth2/v2.0/token", "jwks_uri": idpBase + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	idp.HandleFunc("GET /keys", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	// Authorize: "the user signed in" immediately and goes back to the app with a code.
	idp.HandleFunc("GET /{tenant}/oauth2/v2.0/authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		code := fmt.Sprintf("code-%d", time.Now().UnixNano())
		now := time.Now()
		mu.Lock()
		codes[code] = map[string]any{"iss": idpBase + "/" + tenant + "/v2.0", "aud": client, "sub": "s1", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
			"nonce": q.Get("nonce"), "tid": tenant, "oid": "oid-e2e", "preferred_username": "e2e@corp.test", "roles": []string{"IDLifecycle.Approver"}}
		mu.Unlock()
		http.Redirect(w, r, q.Get("redirect_uri")+"?code="+url.QueryEscape(code)+"&state="+url.QueryEscape(q.Get("state")), http.StatusFound)
	})
	idp.HandleFunc("POST /{tenant}/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		id, sec, ok := r.BasicAuth()
		if !ok {
			id, sec = r.PostForm.Get("client_id"), r.PostForm.Get("client_secret")
		}
		mu.Lock()
		claims := codes[r.PostForm.Get("code")]
		delete(codes, r.PostForm.Get("code"))
		mu.Unlock()
		if id != client || sec != secret || claims == nil || r.PostForm.Get("code_verifier") == "" {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "k1"}}, nil)
		b, _ := json.Marshal(claims)
		obj, _ := signer.Sign(b)
		tok, _ := obj.CompactSerialize()
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": tok})
	})
	idpSrv := httptest.NewServer(idp)
	defer idpSrv.Close()
	idpBase = strings.Replace(idpSrv.URL, "127.0.0.1", "localhost", 1)

	// ---- the app, recording what the SPA's session check returns ----
	meOK := make(chan struct{}, 1)
	var trail []string
	var trailMu sync.Mutex
	srv := httptest.NewUnstartedServer(nil)
	srv.Start()
	defer srv.Close()

	s := model.DefaultSettings()
	s.Auth.OIDC.Enabled, s.Auth.OIDC.TenantID, s.Auth.OIDC.ClientID = true, tenant, client
	s.Auth.OIDC.ClientSecret, s.Auth.OIDC.RedirectURL = secret, srv.URL+model.OIDCCallbackPath
	if err := st.SaveSettings(ctx, s, "e2e"); err != nil {
		t.Fatal(err)
	}
	h := (&api.Server{Store: st, Engine: eng, Cipher: cipher, Static: web.Handler(), Log: log, Version: "e2e", BaseCtx: ctx, OIDCAuthority: idpBase}).Handler()
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		h.ServeHTTP(rec, r)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			trailMu.Lock()
			trail = append(trail, fmt.Sprintf("%s %s -> %d", r.Method, r.URL.Path, rec.status))
			trailMu.Unlock()
		}
		if r.URL.Path == "/api/auth/me" && rec.status == 200 {
			select {
			case meOK <- struct{}{}:
			default:
			}
		}
	})

	profile, err := os.MkdirTemp("", "idl-e2e-oidc-")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(browser, "--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--disable-extensions", "--user-data-dir="+profile, "--window-size=1200,900", srv.URL+"/api/auth/oidc/start")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = exec.Command("taskkill", "/T", "/F", "/PID", itoa(cmd.Process.Pid)).Run()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		time.Sleep(500 * time.Millisecond)
		_ = os.RemoveAll(profile)
	}()

	select {
	case <-meOK:
	case <-time.After(90 * time.Second):
		trailMu.Lock()
		defer trailMu.Unlock()
		t.Fatalf("the SPA never got a session after the Entra ID redirect; requests:\n%s", strings.Join(trail, "\n"))
	}
	u, err := st.GetUserByName(ctx, "e2e@corp.test")
	if err != nil || u.AuthSource != model.AuthOIDC || u.Role != model.RoleApprover {
		t.Fatalf("account: %+v %v", u, err)
	}
	trailMu.Lock()
	t.Logf("browser requests:\n%s", strings.Join(trail, "\n"))
	trailMu.Unlock()
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}
