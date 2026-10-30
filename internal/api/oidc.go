package api

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"

	"idlifecycle/internal/model"
	"idlifecycle/internal/secrets"
	"idlifecycle/internal/store"
)

// Optional sign-in with Microsoft Entra ID: OpenID Connect authorization code flow with PKCE, a nonce and a
// state value bound to the browser by a short-lived cookie. The ID token is verified with go-oidc (issuer,
// audience, expiry, signature from the tenant's published keys). Accounts are created on first sign-in
// (auth_source "oidc", keyed by tenant + object ID) and their role follows Entra ID on every sign-in.
// Local accounts keep working; settings can limit password sign-in to administrators (break-glass).

const (
	defaultOIDCAuthority = "https://login.microsoftonline.com"
	oidcCookie           = "idl_oidc"
	oidcFlowTTL          = 10 * time.Minute
	maxOIDCFlows         = 1000
	oidcHTTPTimeout      = 20 * time.Second
)

type oidcFlow struct {
	nonce    string
	verifier string
	tenant   string
	client   string
	created  time.Time
}

func (s *Server) authority() string {
	if s.OIDCAuthority != "" {
		return strings.TrimRight(s.OIDCAuthority, "/")
	}
	return defaultOIDCAuthority
}

func (s *Server) oidcIssuer(tenant string) string { return s.authority() + "/" + tenant + "/v2.0" }

// oidcContext carries the HTTP client go-oidc and oauth2 use (tests swap it; production gets a timeout).
func (s *Server) oidcContext(ctx context.Context) context.Context {
	c := s.OIDCHTTPClient
	if c == nil {
		c = &http.Client{Timeout: oidcHTTPTimeout}
	}
	return oidc.ClientContext(ctx, c)
}

// provider returns the tenant's discovered OIDC provider, cached per issuer (failures are not cached).
func (s *Server) provider(ctx context.Context, tenant string) (*oidc.Provider, error) {
	issuer := s.oidcIssuer(tenant)
	s.oidcMu.Lock()
	p := s.oidcProviders[issuer]
	s.oidcMu.Unlock()
	if p != nil {
		return p, nil
	}
	ctx, cancel := context.WithTimeout(s.oidcContext(ctx), oidcHTTPTimeout)
	defer cancel()
	p, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("discovery for tenant %s failed: %w", tenant, err)
	}
	s.oidcMu.Lock()
	if s.oidcProviders == nil {
		s.oidcProviders = map[string]*oidc.Provider{}
	}
	s.oidcProviders[issuer] = p
	s.oidcMu.Unlock()
	return p, nil
}

func (s *Server) oauthConfig(p *oidc.Provider, o model.OIDCSettings, secret string) *oauth2.Config {
	return &oauth2.Config{
		ClientID: o.ClientID, ClientSecret: secret, RedirectURL: o.RedirectURL, Endpoint: p.Endpoint(),
		Scopes: []string{oidc.ScopeOpenID, "profile", "email"},
	}
}

func (s *Server) oidcSecret(o model.OIDCSettings) (string, error) {
	if o.ClientSecret == "" {
		return "", errors.New("no client secret is saved")
	}
	if !secrets.IsEncrypted(o.ClientSecret) {
		return o.ClientSecret, nil
	}
	if s.Cipher == nil {
		return "", errors.New("no cipher configured to decrypt the client secret")
	}
	return s.Cipher.Decrypt(o.ClientSecret)
}

// oidcReady reports whether Entra ID sign-in can be offered.
func oidcReady(a model.AuthSettings) bool {
	o := a.OIDC
	return o.Enabled && o.TenantID != "" && o.ClientID != "" && o.RedirectURL != "" && o.ClientSecret != ""
}

// handleProviders tells the login page what to offer. Public: it reveals only whether Entra ID sign-in is on.
func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	st, err := s.Store.GetSettings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"local_login": st.Auth.LocalLogin,
		"oidc":        map[string]any{"enabled": oidcReady(st.Auth), "label": st.Auth.OIDC.ButtonLabel},
	})
}

func (s *Server) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.allow(clientIP(r)) {
		s.oidcPage(w, http.StatusTooManyRequests, "Too many sign-in attempts", "Wait a few minutes and try again.")
		return
	}
	st, err := s.Store.GetSettings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	o := st.Auth.OIDC
	if !oidcReady(st.Auth) {
		s.oidcPage(w, http.StatusNotFound, "Microsoft sign-in is not enabled", "Sign in with your IDLifecycle username and password, or ask an administrator to turn on Entra ID sign-in.")
		return
	}
	p, err := s.provider(r.Context(), o.TenantID)
	if err != nil {
		s.Log.Error("oidc discovery failed", "error", err)
		s.oidcPage(w, http.StatusBadGateway, "Cannot reach Microsoft sign-in", "The server could not read the Entra ID configuration for this tenant. An administrator can check Settings → Sign-in.")
		return
	}
	state, err1 := randomToken(32)
	nonce, err2 := randomToken(32)
	if err1 != nil || err2 != nil {
		s.fail(w, r, errors.Join(err1, err2))
		return
	}
	verifier := oauth2.GenerateVerifier()

	s.oidcMu.Lock()
	if s.oidcFlows == nil {
		s.oidcFlows = map[string]oidcFlow{}
	}
	if len(s.oidcFlows) >= maxOIDCFlows {
		for k, f := range s.oidcFlows {
			if time.Since(f.created) > oidcFlowTTL {
				delete(s.oidcFlows, k)
			}
		}
	}
	full := len(s.oidcFlows) >= maxOIDCFlows
	if !full {
		s.oidcFlows[state] = oidcFlow{nonce: nonce, verifier: verifier, tenant: o.TenantID, client: o.ClientID, created: time.Now()}
	}
	s.oidcMu.Unlock()
	if full {
		s.oidcPage(w, http.StatusServiceUnavailable, "Too many sign-ins in progress", "Try again in a few minutes.")
		return
	}

	// Lax (not Strict): the browser comes back from login.microsoftonline.com with a top-level GET.
	http.SetCookie(w, &http.Cookie{Name: oidcCookie, Value: state, Path: "/api/auth/oidc", MaxAge: int(oidcFlowTTL.Seconds()), // #nosec G124 -- HttpOnly, SameSite=Lax (needed for the redirect back), Secure whenever TLS is on
		HttpOnly: true, Secure: s.TLS, SameSite: http.SameSiteLaxMode})
	url := s.oauthConfig(p, o, "").AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("response_mode", "query"), oauth2.SetAuthURLParam("prompt", "select_account"))
	http.Redirect(w, r, url, http.StatusFound)
}

type entraClaims struct {
	OID               string            `json:"oid"`
	TID               string            `json:"tid"`
	Name              string            `json:"name"`
	PreferredUsername string            `json:"preferred_username"`
	Email             string            `json:"email"`
	UPN               string            `json:"upn"`
	Roles             []string          `json:"roles"`
	Groups            []string          `json:"groups"`
	ClaimNames        map[string]string `json:"_claim_names"`
}

func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	http.SetCookie(w, &http.Cookie{Name: oidcCookie, Value: "", Path: "/api/auth/oidc", MaxAge: -1, HttpOnly: true, Secure: s.TLS, SameSite: http.SameSiteLaxMode}) // #nosec G124 -- clears the flow cookie, same attributes

	deny := func(status int, who, reason, title, msg string) {
		_ = s.Store.AppendAudit(ctx, orAnon(who), "auth.login_failed", "user", who, map[string]any{"remote": clientIP(r), "method": "oidc", "reason": reason})
		s.oidcPage(w, status, title, msg)
	}

	state := q.Get("state")
	c, err := r.Cookie(oidcCookie)
	if state == "" || err != nil || !constantEq(c.Value, state) {
		deny(http.StatusBadRequest, "", "state missing or not bound to this browser", "Sign-in could not be completed",
			"This sign-in did not start in this browser, or it took too long. Go back and choose Sign in with Microsoft again.")
		return
	}
	s.oidcMu.Lock()
	flow, ok := s.oidcFlows[state]
	delete(s.oidcFlows, state) // one use only
	s.oidcMu.Unlock()
	if !ok || time.Since(flow.created) > oidcFlowTTL {
		deny(http.StatusBadRequest, "", "unknown or expired state", "Sign-in expired", "The sign-in took longer than 10 minutes or was already used. Start again.")
		return
	}
	if e := q.Get("error"); e != "" {
		desc := q.Get("error_description")
		if len(desc) > 300 {
			desc = desc[:300] + "…"
		}
		deny(http.StatusUnauthorized, "", "identity provider error: "+e, "Microsoft sign-in did not succeed", desc)
		return
	}

	st, err := s.Store.GetSettings(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	o := st.Auth.OIDC
	if !oidcReady(st.Auth) || o.TenantID != flow.tenant || o.ClientID != flow.client {
		deny(http.StatusConflict, "", "configuration changed during sign-in", "Sign-in settings changed", "Entra ID sign-in was turned off or reconfigured while you were signing in. Start again.")
		return
	}
	secret, err := s.oidcSecret(o)
	if err != nil {
		s.Log.Error("oidc client secret unavailable", "error", err)
		deny(http.StatusInternalServerError, "", "client secret unavailable", "Sign-in is misconfigured", "The server cannot use its Entra ID client secret. An administrator must re-enter it in Settings → Sign-in.")
		return
	}
	p, err := s.provider(ctx, o.TenantID)
	if err != nil {
		deny(http.StatusBadGateway, "", "discovery failed", "Cannot reach Microsoft sign-in", "Try again in a moment.")
		return
	}
	octx, cancel := context.WithTimeout(s.oidcContext(ctx), oidcHTTPTimeout)
	defer cancel()
	tok, err := s.oauthConfig(p, o, secret).Exchange(octx, q.Get("code"), oauth2.VerifierOption(flow.verifier))
	if err != nil {
		s.Log.Warn("oidc code exchange failed", "error", err)
		deny(http.StatusUnauthorized, "", "code exchange failed", "Microsoft sign-in did not succeed",
			"The sign-in code could not be redeemed. If this keeps happening, an administrator should check the client secret and redirect URL.")
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	if raw == "" {
		deny(http.StatusUnauthorized, "", "no id_token in the token response", "Microsoft sign-in did not succeed", "No identity was returned.")
		return
	}
	idt, err := p.Verifier(&oidc.Config{ClientID: o.ClientID}).Verify(octx, raw)
	if err != nil {
		s.Log.Warn("oidc id token rejected", "error", err)
		deny(http.StatusUnauthorized, "", "id token rejected: "+err.Error(), "Microsoft sign-in did not succeed", "The identity token could not be verified.")
		return
	}
	if !constantEq(idt.Nonce, flow.nonce) {
		deny(http.StatusUnauthorized, "", "nonce mismatch", "Microsoft sign-in did not succeed", "The identity token does not belong to this sign-in.")
		return
	}
	var cl entraClaims
	if err := idt.Claims(&cl); err != nil {
		deny(http.StatusUnauthorized, "", "unreadable claims", "Microsoft sign-in did not succeed", "The identity token could not be read.")
		return
	}
	if !strings.EqualFold(cl.TID, o.TenantID) || cl.OID == "" {
		deny(http.StatusForbidden, "", "token from another tenant or without an object id", "This account cannot sign in here",
			"Only accounts from the configured Entra ID tenant can sign in.")
		return
	}
	name := firstNonEmptyStr(cl.PreferredUsername, cl.UPN, cl.Email, cl.OID)
	if len(name) > 100 {
		name = name[:100]
	}
	if cl.ClaimNames["groups"] != "" {
		s.Log.Warn("oidc group overage: the token lists no groups because the user is in too many; map roles with app roles instead", "user", name)
	}
	role := oidcRole(o, cl.Roles, cl.Groups)
	if role == "" {
		deny(http.StatusForbidden, name, "no IDLifecycle role in the token", "No access to IDLifecycle",
			"Your Entra ID account is not assigned an IDLifecycle role. Ask an administrator to assign you the Admin, Approver or Viewer app role.")
		return
	}

	u, status, msg, err := s.upsertOIDCUser(ctx, strings.ToLower(cl.TID)+":"+strings.ToLower(cl.OID), name, role)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if msg != "" {
		deny(status, name, msg, "This account cannot sign in", msg)
		return
	}
	if _, err := s.startSession(ctx, w, u); err != nil {
		s.fail(w, r, err)
		return
	}
	_ = s.Store.AppendAudit(ctx, u.Username, "auth.login", "user", u.ID, map[string]any{"remote": clientIP(r), "method": "oidc", "role": u.Role})
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	// A same-site navigation (meta refresh), so the SameSite=Strict session cookie is sent with it.
	fmt.Fprint(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta http-equiv="refresh" content="0;url=/">`+
		`<title>Signed in - IDLifecycle</title><link rel="stylesheet" href="/app.css"></head>`+
		`<body><main class="login-wrap"><div class="card login-card"><h1>IDLifecycle</h1><p>Signed in. <a href="/">Continue</a></p></div></main></body></html>`)
}

// upsertOIDCUser finds or creates the account for an Entra ID identity and brings its name and role up to date.
// A non-empty msg means the sign-in is refused (status is the HTTP status for the page).
func (s *Server) upsertOIDCUser(ctx context.Context, externalID, name, role string) (u model.User, status int, msg string, err error) {
	u, err = s.Store.GetUserByExternalID(ctx, externalID)
	if errors.Is(err, store.ErrNotFound) {
		if other, e := s.Store.GetUserByName(ctx, name); e == nil {
			kind := "a local account"
			if other.AuthSource == model.AuthOIDC {
				kind = "another Entra ID account"
			}
			return u, http.StatusConflict, fmt.Sprintf("%s named %s already exists in IDLifecycle. An administrator must rename or delete it first.", kind, name), nil
		}
		u = model.User{ID: uuid.NewString(), Username: name, Role: role, AuthSource: model.AuthOIDC, ExternalID: externalID, CreatedAt: time.Now()}
		if err := s.Store.CreateUser(ctx, u); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return u, http.StatusConflict, "An account named " + name + " already exists in IDLifecycle.", nil
			}
			return u, 0, "", err
		}
		_ = s.Store.AppendAudit(ctx, "oidc", "user.create", "user", u.ID, map[string]any{"username": u.Username, "role": u.Role, "auth_source": model.AuthOIDC})
		return u, 0, "", nil
	}
	if err != nil {
		return u, 0, "", err
	}
	if u.Disabled {
		return u, http.StatusForbidden, "Your IDLifecycle account is disabled. Ask an administrator to enable it.", nil
	}
	next := u
	next.Role = role
	if name != u.Username {
		if _, e := s.Store.GetUserByName(ctx, name); errors.Is(e, store.ErrNotFound) {
			next.Username = name // renamed in Entra ID
		}
	}
	if next.Role != u.Role || next.Username != u.Username {
		if err := s.Store.UpdateUser(ctx, next); err != nil {
			return u, 0, "", err
		}
		if next.Role != u.Role {
			_ = s.Store.DeleteUserSessions(ctx, u.ID) // other sessions must not keep the old role
		}
		_ = s.Store.AppendAudit(ctx, "oidc", "user.update", "user", u.ID, map[string]any{"username": next.Username, "role": next.Role, "previous_role": u.Role})
	}
	return next, 0, "", nil
}

// oidcRole is the highest role whose list contains one of the token's app roles or group object IDs.
func oidcRole(o model.OIDCSettings, roles, groups []string) string {
	has := map[string]bool{}
	for _, v := range append(append([]string{}, roles...), groups...) {
		has[strings.ToLower(strings.TrimSpace(v))] = true
	}
	for _, c := range []struct {
		role   string
		values []string
	}{{model.RoleAdmin, o.AdminValues}, {model.RoleApprover, o.ApproverValues}, {model.RoleViewer, o.ViewerValues}} {
		for _, v := range c.values {
			if has[strings.ToLower(strings.TrimSpace(v))] {
				return c.role
			}
		}
	}
	return ""
}

// handleOIDCTest checks unsaved sign-in settings: the tenant's discovery document, then the client ID and
// secret with a client-credentials token request (needs no API permissions). Nothing is stored.
func (s *Server) handleOIDCTest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Auth model.AuthSettings `json:"auth"`
	}
	if !decode(w, r, &in) {
		return
	}
	stored, err := s.Store.GetSettings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	res := map[string]any{"ok": false, "error": ""}
	defer func() {
		s.audit(r, "auth.oidc_test", "settings", "auth", map[string]any{"ok": res["ok"]})
		writeJSON(w, http.StatusOK, res)
	}()
	in.Auth.OIDC.Enabled = true
	if in.Auth.LocalLogin == "" {
		in.Auth.LocalLogin = model.LocalLoginAll
	}
	cfg, err := prepareAuth(in.Auth, stored.Auth, s.Cipher)
	if err == nil {
		err = cfg.Validate()
	}
	if err != nil {
		res["error"] = err.Error()
		return
	}
	o := cfg.OIDC
	p, err := s.provider(r.Context(), o.TenantID)
	if err != nil {
		res["error"] = err.Error()
		return
	}
	secret, err := s.oidcSecret(o)
	if err != nil {
		res["error"] = "client secret: " + err.Error()
		return
	}
	ctx, cancel := context.WithTimeout(s.oidcContext(r.Context()), oidcHTTPTimeout)
	defer cancel()
	cc := clientcredentials.Config{ClientID: o.ClientID, ClientSecret: secret, TokenURL: p.Endpoint().TokenURL, Scopes: []string{"https://graph.microsoft.com/.default"}}
	if _, err := cc.Token(ctx); err != nil {
		res["error"] = "the tenant was found, but the client ID or secret was refused: " + scrubOAuthError(err)
		return
	}
	res["ok"] = true
}

func scrubOAuthError(err error) string {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		if re.ErrorDescription != "" {
			return re.ErrorCode + ": " + firstLine(re.ErrorDescription)
		}
		return re.ErrorCode
	}
	return err.Error()
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// startSession creates a session for u, sets the cookie and returns the CSRF token.
func (s *Server) startSession(ctx context.Context, w http.ResponseWriter, u model.User) (string, error) {
	token, err := randomToken(32)
	if err != nil {
		return "", err
	}
	csrf, err := randomToken(24)
	if err != nil {
		return "", err
	}
	now := time.Now()
	if err := s.Store.CreateSession(ctx, model.Session{TokenHash: hashToken(token), UserID: u.ID, CSRFToken: csrf, CreatedAt: now, ExpiresAt: now.Add(sessionIdle), LastSeenAt: now}); err != nil {
		return "", err
	}
	_ = s.Store.RecordLoginSuccess(ctx, u.ID)
	_ = s.Store.DeleteExpiredSessions(ctx)
	s.setCookie(w, token, now.Add(sessionIdle))
	return csrf, nil
}

// oidcPage renders a small standalone page (the callback is a browser navigation, not an API call).
func (s *Server) oidcPage(w http.ResponseWriter, status int, title, msg string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<!doctype html><html lang="en"><head><meta charset="utf-8"><title>%s - IDLifecycle</title>`+
		`<link rel="stylesheet" href="/app.css"></head><body><main class="login-wrap"><div class="card login-card">`+
		`<h1>IDLifecycle</h1><div class="notice notice-error" data-test="oidc-error"><strong>%s</strong><p>%s</p></div>`+
		`<p><a class="btn" href="/">Back to sign-in</a></p></div></main></body></html>`,
		html.EscapeString(title), html.EscapeString(title), html.EscapeString(msg))
}

func orAnon(s string) string {
	if s == "" {
		return "anonymous"
	}
	return s
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
