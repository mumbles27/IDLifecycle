package notify

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"idlifecycle/internal/model"
	"idlifecycle/internal/secrets"
)

const (
	testTenant = "11111111-2222-3333-4444-555555555555"
	testClient = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	testSecret = "s3cr3t-Client~Value"
	testToken  = "eyJ0eXAi.access-token-XYZ987"
	testPass   = "Hunter2-Passw0rd!"
)

func testCipher(t *testing.T) *secrets.Cipher {
	t.Helper()
	c, err := secrets.New([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newSvc(clientTLS *tls.Config) *Service {
	return &Service{Log: quietLog(), TLSConfig: clientTLS}
}

func smtpCfg(port int, security, auth string) model.NotifySettings {
	return model.NotifySettings{
		Enabled: true, Provider: model.NotifySMTP,
		From: "IDLifecycle <idl@example.com>", To: []string{"ops@example.com", "Second <two@example.com>"},
		SubjectPrefix: "[IDL]",
		SMTP:          model.SMTPSettings{Host: "127.0.0.1", Port: port, Security: security, Auth: auth, Username: "idl", Password: testPass},
	}
}

func testNote() model.Notification {
	return model.Notification{Event: model.EventRunFailed, Subject: "Run failed", Lines: []string{"Run abc failed.", "Error: boom"}, RunID: "abc"}
}

func wait(t *testing.T, f *fakeSMTP) captured {
	t.Helper()
	select {
	case m := <-f.Got:
		return m
	case <-time.After(10 * time.Second):
		t.Fatal("no message received by fake SMTP server")
	}
	return captured{}
}

func parseMsg(t *testing.T, data string) (*mail.Message, string) {
	t.Helper()
	m, err := mail.ReadMessage(strings.NewReader(data))
	if err != nil {
		t.Fatalf("parse message: %v\n%s", err, data)
	}
	if m.Header.Get("Content-Transfer-Encoding") != "quoted-printable" {
		t.Fatalf("CTE = %q", m.Header.Get("Content-Transfer-Encoding"))
	}
	b, err := io.ReadAll(quotedprintable.NewReader(m.Body))
	if err != nil {
		t.Fatal(err)
	}
	return m, string(b)
}

func TestSMTPStartTLSLogin(t *testing.T) {
	srvTLS, cliTLS := testCert(t)
	f := newFakeSMTP(t, srvTLS, "STARTTLS", "AUTH LOGIN PLAIN")
	f.User, f.Pass = "idl", testPass
	s := newSvc(cliTLS)
	if err := s.Send(context.Background(), smtpCfg(f.Port(), "starttls", "login"), testNote()); err != nil {
		t.Fatal(err)
	}
	m := wait(t, f)
	if !m.TLS || m.AuthMec != "LOGIN" || m.AuthRaw != "idl:"+testPass {
		t.Fatalf("tls=%v auth=%s %q", m.TLS, m.AuthMec, m.AuthRaw)
	}
	if m.From != "idl@example.com" || strings.Join(m.To, ",") != "ops@example.com,two@example.com" {
		t.Fatalf("envelope from=%q to=%v", m.From, m.To)
	}
	msg, body := parseMsg(t, m.Data)
	if got := msg.Header.Get("Subject"); got != "[IDL] Run failed" {
		t.Fatalf("subject %q", got)
	}
	for _, h := range []string{"From", "To", "Date", "Message-Id", "Mime-Version"} {
		if msg.Header.Get(h) == "" {
			t.Fatalf("missing header %s", h)
		}
	}
	if !strings.HasSuffix(msg.Header.Get("Message-Id"), "@example.com>") {
		t.Fatalf("message-id %q", msg.Header.Get("Message-Id"))
	}
	if !strings.Contains(msg.Header.Get("Content-Type"), "charset=utf-8") {
		t.Fatalf("content-type %q", msg.Header.Get("Content-Type"))
	}
	if !strings.Contains(body, "Run abc failed.\r\nError: boom") {
		t.Fatalf("body %q", body)
	}
}

func TestSMTPPlainFallback(t *testing.T) {
	srvTLS, cliTLS := testCert(t)
	f := newFakeSMTP(t, srvTLS, "STARTTLS", "AUTH PLAIN")
	f.User, f.Pass = "idl", testPass
	if err := newSvc(cliTLS).Send(context.Background(), smtpCfg(f.Port(), "starttls", "login"), testNote()); err != nil {
		t.Fatal(err)
	}
	if m := wait(t, f); m.AuthMec != "PLAIN" {
		t.Fatalf("auth %s", m.AuthMec)
	}
}

func TestSMTPBadPasswordScrubbed(t *testing.T) {
	srvTLS, cliTLS := testCert(t)
	f := newFakeSMTP(t, srvTLS, "STARTTLS", "AUTH LOGIN")
	f.User, f.Pass = "idl", "other"
	f.Echo535 = " for " + testPass
	err := newSvc(cliTLS).Send(context.Background(), smtpCfg(f.Port(), "starttls", "login"), testNote())
	if err == nil || !strings.Contains(err.Error(), "authentication failed") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), testPass) {
		t.Fatalf("password leaked: %v", err)
	}
}

func TestSMTPRequiresSTARTTLS(t *testing.T) {
	srvTLS, cliTLS := testCert(t)
	f := newFakeSMTP(t, srvTLS, "AUTH LOGIN")
	f.User, f.Pass = "idl", testPass
	err := newSvc(cliTLS).Send(context.Background(), smtpCfg(f.Port(), "starttls", "login"), testNote())
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("err = %v", err)
	}
	if len(f.Messages()) != 0 {
		t.Fatal("message was sent without TLS")
	}
}

func TestSMTPImplicitTLS(t *testing.T) {
	srvTLS, cliTLS := testCert(t)
	f := newFakeSMTP(t, srvTLS, "AUTH LOGIN")
	f.ImplicitTLS = true
	f.User, f.Pass = "idl", testPass
	if err := newSvc(cliTLS).Send(context.Background(), smtpCfg(f.Port(), "tls", "login"), testNote()); err != nil {
		t.Fatal(err)
	}
	if m := wait(t, f); !m.TLS || m.AuthMec != "LOGIN" {
		t.Fatalf("tls=%v auth=%s", m.TLS, m.AuthMec)
	}
}

func TestSMTPUntrustedCertFails(t *testing.T) {
	srvTLS, _ := testCert(t)
	_, otherCli := testCert(t)
	f := newFakeSMTP(t, srvTLS, "STARTTLS")
	err := newSvc(otherCli).Send(context.Background(), smtpCfg(f.Port(), "starttls", "none"), testNote())
	if err == nil {
		t.Fatal("expected certificate error")
	}
}

func TestSMTPNoneSecurity(t *testing.T) {
	f := newFakeSMTP(t, nil, "AUTH LOGIN PLAIN")
	f.User, f.Pass = "idl", testPass
	// No auth over cleartext: fine.
	if err := newSvc(nil).Send(context.Background(), smtpCfg(f.Port(), "none", "none"), testNote()); err != nil {
		t.Fatal(err)
	}
	if m := wait(t, f); m.TLS || m.AuthMec != "" {
		t.Fatalf("tls=%v auth=%s", m.TLS, m.AuthMec)
	}
	// Auth over cleartext is allowed to a loopback host only.
	if err := newSvc(nil).Send(context.Background(), smtpCfg(f.Port(), "none", "login"), testNote()); err != nil {
		t.Fatal(err)
	}
	wait(t, f)

	cfg := smtpCfg(f.Port(), "none", "login")
	cfg.SMTP.Host = "mail.example.com"
	var dialed atomic.Bool
	s := newSvc(nil)
	s.Dial = func(ctx context.Context, addr string) (net.Conn, error) {
		dialed.Store(true)
		return (&net.Dialer{}).DialContext(ctx, "tcp", f.ln.Addr().String())
	}
	err := s.Send(context.Background(), cfg, testNote())
	if err == nil || !strings.Contains(err.Error(), "unencrypted") || dialed.Load() {
		t.Fatalf("err = %v dialed=%v", err, dialed.Load())
	}
}

func tokenServer(t *testing.T, wantScope string, calls *atomic.Int32, fail bool) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/"+testTenant+"/oauth2/v2.0/token" {
			t.Errorf("token request %s %s", r.Method, r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		for k, v := range map[string]string{"grant_type": "client_credentials", "scope": wantScope, "client_id": testClient, "client_secret": testSecret} {
			if got := r.PostForm.Get(k); got != v {
				t.Errorf("form %s = %q, want %q", k, got, v)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if fail {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"AADSTS7000215: Invalid client secret provided. ` + testSecret + `\r\nTrace ID: x"}`))
			return
		}
		_, _ = w.Write([]byte(`{"token_type":"Bearer","expires_in":3599,"access_token":"` + testToken + `"}`))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func m365SMTPCfg(port int) model.NotifySettings {
	return model.NotifySettings{
		Enabled: true, Provider: model.NotifyM365SMTP,
		From: "idl@contoso.com", To: []string{"ops@contoso.com"},
		SMTP:  model.SMTPSettings{Host: "127.0.0.1", Port: port, Auth: "oauth2"},
		OAuth: model.OAuthSettings{TenantID: testTenant, ClientID: testClient, ClientSecret: testSecret},
	}
}

func TestM365SMTPOAuth2(t *testing.T) {
	srvTLS, cliTLS := testCert(t)
	f := newFakeSMTP(t, srvTLS, "STARTTLS", "AUTH LOGIN XOAUTH2")
	f.Token = testToken
	var calls atomic.Int32
	ts := tokenServer(t, "https://outlook.office365.com/.default", &calls, false)
	s := newSvc(cliTLS)
	s.TokenURL = ts.URL + "/{tenant}/oauth2/v2.0/token"

	// Encrypted secrets are accepted too.
	cfg := m365SMTPCfg(f.Port())
	enc, _ := testCipher(t).Encrypt(testSecret)
	cfg.OAuth.ClientSecret = enc
	s.Cipher = testCipher(t)
	if err := s.Send(context.Background(), cfg, testNote()); err != nil {
		t.Fatal(err)
	}
	m := wait(t, f)
	want := "user=idl@contoso.com\x01auth=Bearer " + testToken + "\x01\x01"
	if m.AuthMec != "XOAUTH2" || m.AuthRaw != want || !m.TLS {
		t.Fatalf("auth %s %q tls=%v", m.AuthMec, m.AuthRaw, m.TLS)
	}
	// A second send reuses the cached token.
	if err := s.Send(context.Background(), cfg, testNote()); err != nil {
		t.Fatal(err)
	}
	wait(t, f)
	if calls.Load() != 1 {
		t.Fatalf("token requests = %d, want 1 (cached)", calls.Load())
	}
}

func TestM365SMTPDefaults(t *testing.T) {
	n := normalize(model.NotifySettings{Provider: model.NotifyM365SMTP})
	if n.SMTP.Host != "smtp.office365.com" || n.SMTP.Port != 587 || n.SMTP.Security != "starttls" || n.SMTP.Auth != "oauth2" {
		t.Fatalf("%+v", n.SMTP)
	}
	// Send dials the default host when none is configured.
	var addr atomic.Value
	s := &Service{Log: quietLog(), Dial: func(ctx context.Context, a string) (net.Conn, error) {
		addr.Store(a)
		return nil, io.EOF
	}}
	cfg := model.NotifySettings{Provider: model.NotifyM365SMTP, From: "idl@contoso.com", To: []string{"a@contoso.com"}, SMTP: model.SMTPSettings{Auth: "login", Username: "u", Password: "p"}}
	_ = s.Send(context.Background(), cfg, testNote())
	if addr.Load() != "smtp.office365.com:587" {
		t.Fatalf("dialed %v", addr.Load())
	}
}

func TestM365SMTPBadToken(t *testing.T) {
	srvTLS, cliTLS := testCert(t)
	f := newFakeSMTP(t, srvTLS, "STARTTLS", "AUTH XOAUTH2")
	f.Token = "a-different-token"
	f.Echo535 = " token=" + testToken
	var calls atomic.Int32
	ts := tokenServer(t, "https://outlook.office365.com/.default", &calls, false)
	s := newSvc(cliTLS)
	s.TokenURL = ts.URL + "/{tenant}/oauth2/v2.0/token"
	err := s.Send(context.Background(), m365SMTPCfg(f.Port()), testNote())
	if err == nil || !strings.Contains(err.Error(), "535") {
		t.Fatalf("err = %v", err)
	}
	for _, secret := range []string{testToken, testSecret} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("secret leaked in %v", err)
		}
	}
	f.mu.Lock()
	empty := f.emptyAfter
	f.mu.Unlock()
	if !empty {
		t.Fatal("client did not answer the 334 challenge with an empty line")
	}
	// The rejected token is dropped from the cache.
	_ = s.Send(context.Background(), m365SMTPCfg(f.Port()), testNote())
	if calls.Load() != 2 {
		t.Fatalf("token requests = %d, want 2", calls.Load())
	}
}

func TestTokenErrorScrubbed(t *testing.T) {
	var calls atomic.Int32
	ts := tokenServer(t, "https://outlook.office365.com/.default", &calls, true)
	s := newSvc(nil)
	s.TokenURL = ts.URL + "/{tenant}/oauth2/v2.0/token"
	s.Dial = func(ctx context.Context, addr string) (net.Conn, error) {
		t.Error("must not connect when the token request fails")
		return nil, io.EOF
	}
	err := s.Send(context.Background(), m365SMTPCfg(25), testNote())
	if err == nil || !strings.Contains(err.Error(), "invalid_client") || !strings.Contains(err.Error(), "AADSTS7000215") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), testSecret) || strings.Contains(err.Error(), "Trace") {
		t.Fatalf("leak or multi-line: %v", err)
	}
}

func TestHeaderInjection(t *testing.T) {
	var dialed atomic.Bool
	s := &Service{Log: quietLog(), Dial: func(ctx context.Context, a string) (net.Conn, error) {
		dialed.Store(true)
		return nil, io.EOF
	}}
	n := testNote()
	n.Subject = "hi\r\nBcc: victim@example.com"
	if err := s.Send(context.Background(), smtpCfg(25, "starttls", "none"), n); err == nil {
		t.Fatal("subject injection accepted")
	}
	cfg := smtpCfg(25, "starttls", "none")
	cfg.From = "a@example.com\r\nBcc: victim@example.com"
	if err := s.Send(context.Background(), cfg, testNote()); err == nil {
		t.Fatal("from injection accepted")
	}
	cfg = smtpCfg(25, "starttls", "none")
	cfg.To = []string{"ok@example.com", "bad@example.com\nBcc: x@example.com"}
	// normalize splits on newlines, so the injected line becomes an invalid address
	if err := s.Send(context.Background(), cfg, testNote()); err == nil {
		t.Fatal("to injection accepted")
	}
	cfg.To = []string{"not an address"}
	if err := s.Send(context.Background(), cfg, testNote()); err == nil {
		t.Fatal("invalid address accepted")
	}
	if dialed.Load() {
		t.Fatal("connected despite invalid message")
	}
	if _, err := buildMessage(&mail.Address{Address: "a@b.c"}, []*mail.Address{{Address: "x@y.z"}}, "a\nb", "", time.Now()); err == nil {
		t.Fatal("buildMessage accepted CRLF subject")
	}
}

func TestTooManyRecipients(t *testing.T) {
	cfg := smtpCfg(25, "starttls", "none")
	cfg.To = nil
	for i := 0; i < 51; i++ {
		cfg.To = append(cfg.To, "u"+strings.Repeat("x", i)+"@example.com")
	}
	if err := Validate(cfg); err == nil {
		t.Fatal("51 recipients accepted")
	}
}

func TestNonASCIISubjectAndQPBody(t *testing.T) {
	srvTLS, cliTLS := testCert(t)
	f := newFakeSMTP(t, srvTLS, "STARTTLS")
	n := testNote()
	n.Subject = "Prüfung fehlgeschlagen ✓"
	long := strings.Repeat("Zoë ", 40)
	n.Lines = []string{"Benutzer: Jürgen Müller", long, ".leading dot"}
	if err := newSvc(cliTLS).Send(context.Background(), smtpCfg(f.Port(), "starttls", "none"), n); err != nil {
		t.Fatal(err)
	}
	m := wait(t, f)
	raw := m.Data
	if !strings.Contains(raw, "Subject: =?utf-8?q?") {
		t.Fatalf("subject not encoded:\n%s", raw)
	}
	for _, l := range strings.Split(raw, "\r\n") {
		if len(l) > 78 && !strings.HasPrefix(l, "Subject:") {
			t.Fatalf("line too long (%d): %q", len(l), l)
		}
		for _, r := range l {
			if r > 127 {
				t.Fatalf("non-ASCII byte on the wire: %q", l)
			}
		}
	}
	msg, body := parseMsg(t, raw)
	dec, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil || dec != "[IDL] Prüfung fehlgeschlagen ✓" {
		t.Fatalf("subject %q %v", dec, err)
	}
	if !strings.Contains(raw, "J=C3=BCrgen") {
		t.Fatalf("body not quoted-printable:\n%s", raw)
	}
	if !strings.Contains(body, "Benutzer: Jürgen Müller\r\n"+long+"\r\n.leading dot") {
		t.Fatalf("decoded body %q", body)
	}
}

func TestGraphSend(t *testing.T) {
	var calls atomic.Int32
	var got struct {
		path, auth, ctype string
		body              map[string]any
	}
	status := http.StatusAccepted
	mux := http.NewServeMux()
	mux.HandleFunc("/"+testTenant+"/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = r.ParseForm()
		if r.PostForm.Get("scope") != "https://graph.microsoft.com/.default" || r.PostForm.Get("client_secret") != testSecret {
			t.Errorf("token form %v", r.PostForm)
		}
		_, _ = w.Write([]byte(`{"access_token":"` + testToken + `","expires_in":"3600"}`))
	})
	mux.HandleFunc("/v1.0/users/", func(w http.ResponseWriter, r *http.Request) {
		got.path, got.auth, got.ctype = r.URL.EscapedPath(), r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		got.body = nil
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		w.WriteHeader(status)
		if status != http.StatusAccepted {
			_, _ = w.Write([]byte(`{"error":{"code":"ErrorAccessDenied","message":"Access is denied. Bearer ` + testToken + `"}}`))
		}
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	s := &Service{Log: quietLog(), TokenURL: ts.URL + "/{tenant}/oauth2/v2.0/token", GraphURL: ts.URL}
	cfg := model.NotifySettings{
		Provider: model.NotifyM365Graph, From: "IDL Bot <idl+alerts@contoso.com>", To: []string{"ops@contoso.com", "Sec <sec@contoso.com>"},
		SubjectPrefix: "[IDL]",
		OAuth:         model.OAuthSettings{TenantID: testTenant, ClientID: testClient, ClientSecret: testSecret},
	}
	if err := s.Send(context.Background(), cfg, testNote()); err != nil {
		t.Fatal(err)
	}
	if got.path != "/v1.0/users/idl+alerts@contoso.com/sendMail" && got.path != "/v1.0/users/"+url.PathEscape("idl+alerts@contoso.com")+"/sendMail" {
		t.Fatalf("path %q", got.path)
	}
	if got.auth != "Bearer "+testToken || got.ctype != "application/json" {
		t.Fatalf("auth %q ctype %q", got.auth, got.ctype)
	}
	msg := got.body["message"].(map[string]any)
	if msg["subject"] != "[IDL] Run failed" || got.body["saveToSentItems"] != false {
		t.Fatalf("body %v", got.body)
	}
	b := msg["body"].(map[string]any)
	if b["contentType"] != "Text" || !strings.Contains(b["content"].(string), "Error: boom") {
		t.Fatalf("message body %v", b)
	}
	rc := msg["toRecipients"].([]any)
	if len(rc) != 2 || rc[1].(map[string]any)["emailAddress"].(map[string]any)["address"] != "sec@contoso.com" {
		t.Fatalf("recipients %v", rc)
	}

	status = http.StatusForbidden
	err := s.Send(context.Background(), cfg, testNote())
	if err == nil || !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "ErrorAccessDenied") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), testSecret) {
		t.Fatalf("secret leaked: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("token calls %d", calls.Load())
	}
}

func TestGraphPathEscape(t *testing.T) {
	var path string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			_, _ = w.Write([]byte(`{"access_token":"t0ken-value","expires_in":3600}`))
			return
		}
		path = r.URL.EscapedPath()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ts.Close()
	s := &Service{Log: quietLog(), TokenURL: ts.URL + "/{tenant}/token", GraphURL: ts.URL}
	cfg := model.NotifySettings{Provider: model.NotifyM365Graph, From: `"a/b?c"@contoso.com`, To: []string{"x@contoso.com"},
		OAuth: model.OAuthSettings{TenantID: "contoso.onmicrosoft.com", ClientID: testClient, ClientSecret: testSecret}}
	if err := s.Send(context.Background(), cfg, testNote()); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, "/v1.0/users/a%2Fb%3Fc@contoso.com/sendMail") {
		t.Fatalf("path %q", path)
	}
}

func TestValidate(t *testing.T) {
	ok := smtpCfg(587, "starttls", "login")
	graph := model.NotifySettings{Enabled: true, Provider: model.NotifyM365Graph, From: "a@contoso.com", To: []string{"b@contoso.com"},
		OAuth: model.OAuthSettings{TenantID: "contoso.onmicrosoft.com", ClientID: testClient, ClientSecret: model.MaskedSecret}}
	if err := Validate(ok); err != nil {
		t.Fatal(err)
	}
	if err := Validate(graph); err != nil {
		t.Fatal(err)
	}
	if err := Validate(m365SMTPCfg(0)); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(c *model.NotifySettings){
		"provider":      func(c *model.NotifySettings) { c.Provider = "sendgrid" },
		"from":          func(c *model.NotifySettings) { c.From = "nope" },
		"no to":         func(c *model.NotifySettings) { c.To = nil },
		"bad to":        func(c *model.NotifySettings) { c.To = []string{"x"} },
		"host":          func(c *model.NotifySettings) { c.SMTP.Host = "" },
		"port":          func(c *model.NotifySettings) { c.SMTP.Port = 70000 },
		"security":      func(c *model.NotifySettings) { c.SMTP.Security = "ssl3" },
		"auth":          func(c *model.NotifySettings) { c.SMTP.Auth = "ntlm" },
		"oauth on smtp": func(c *model.NotifySettings) { c.SMTP.Auth = "oauth2" },
		"no password":   func(c *model.NotifySettings) { c.SMTP.Password = "" },
		"no username":   func(c *model.NotifySettings) { c.SMTP.Username = "" },
		"gui url":       func(c *model.NotifySettings) { c.GUIBaseURL = "javascript:alert(1)" },
		"prefix crlf":   func(c *model.NotifySettings) { c.SubjectPrefix = "a\nb" },
	}
	for name, mut := range cases {
		c := ok
		c.To = append([]string(nil), ok.To...)
		mut(&c)
		if err := Validate(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	gcases := map[string]func(c *model.NotifySettings){
		"tenant": func(c *model.NotifySettings) { c.OAuth.TenantID = "not a tenant" },
		"client": func(c *model.NotifySettings) { c.OAuth.ClientID = "my-app" },
		"secret": func(c *model.NotifySettings) { c.OAuth.ClientSecret = "" },
	}
	for name, mut := range gcases {
		c := graph
		mut(&c)
		if err := Validate(c); err == nil {
			t.Errorf("graph %s: accepted", name)
		}
	}
	m := m365SMTPCfg(0)
	m.SMTP.Security = "none"
	if err := Validate(m); err == nil {
		t.Error("m365 with security none accepted")
	}

	// Disabled: incomplete is fine, malformed is not.
	if err := Validate(model.DefaultSettings().Notify); err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if err := Validate(model.NotifySettings{Provider: model.NotifyM365Graph}); err != nil {
		t.Fatalf("disabled incomplete: %v", err)
	}
	for _, c := range []model.NotifySettings{
		{Provider: "bogus"},
		{SMTP: model.SMTPSettings{Port: -1}},
		{GUIBaseURL: "ftp://x"},
		{SMTP: model.SMTPSettings{Security: "weird"}},
	} {
		if err := Validate(c); err == nil {
			t.Errorf("disabled malformed accepted: %+v", c)
		}
	}
}

func TestPrepareForStorageAndMask(t *testing.T) {
	c := testCipher(t)
	in := smtpCfg(587, "starttls", "login")
	in.To = []string{"a@example.com, b@example.com", "c@example.com\nA@example.com"}
	out, err := PrepareForStorage(in, model.NotifySettings{}, c)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(out.To, "|") != "a@example.com|b@example.com|c@example.com" {
		t.Fatalf("to %v", out.To)
	}
	if !secrets.IsEncrypted(out.SMTP.Password) {
		t.Fatalf("password not encrypted: %q", out.SMTP.Password)
	}
	if p, _ := c.Decrypt(out.SMTP.Password); p != testPass {
		t.Fatalf("decrypt %q", p)
	}

	masked := Mask(out)
	if masked.SMTP.Password != model.MaskedSecret || masked.OAuth.ClientSecret != "" {
		t.Fatalf("mask %+v", masked)
	}
	// Round trip: masked value keeps the stored ciphertext.
	again, err := PrepareForStorage(masked, out, c)
	if err != nil || again.SMTP.Password != out.SMTP.Password {
		t.Fatalf("masked keep: %v %q", err, again.SMTP.Password)
	}
	// Empty keeps it too.
	masked.SMTP.Password = ""
	if again, err = PrepareForStorage(masked, out, c); err != nil || again.SMTP.Password != out.SMTP.Password {
		t.Fatalf("empty keep: %v", err)
	}
	// Client-sent ciphertext is rejected.
	bad := Mask(out)
	bad.SMTP.Password = out.SMTP.Password
	if _, err := PrepareForStorage(bad, out, c); err == nil {
		t.Fatal("encrypted value accepted")
	}
	// Host change with masked password: must re-enter.
	moved := Mask(out)
	moved.SMTP.Host = "evil.example.net"
	if _, err := PrepareForStorage(moved, out, c); err == nil || !strings.Contains(err.Error(), "re-enter") {
		t.Fatalf("host change: %v", err)
	}
	// ... but a newly entered password is fine.
	moved.SMTP.Password = "new-pass"
	if _, err := PrepareForStorage(moved, out, c); err != nil {
		t.Fatal(err)
	}
	// Port change alone keeps the secret.
	port := Mask(out)
	port.SMTP.Port = 2525
	if _, err := PrepareForStorage(port, out, c); err != nil {
		t.Fatal(err)
	}

	// Graph: tenant/client change with masked client secret refused.
	g := model.NotifySettings{Provider: model.NotifyM365Graph, From: "a@contoso.com", To: []string{"b@contoso.com"},
		OAuth: model.OAuthSettings{TenantID: testTenant, ClientID: testClient, ClientSecret: testSecret}}
	gs, err := PrepareForStorage(g, model.NotifySettings{}, c)
	if err != nil || !secrets.IsEncrypted(gs.OAuth.ClientSecret) {
		t.Fatalf("graph store: %v", err)
	}
	for _, mut := range []func(*model.NotifySettings){
		func(x *model.NotifySettings) { x.OAuth.TenantID = "fabrikam.onmicrosoft.com" },
		func(x *model.NotifySettings) { x.OAuth.ClientID = "bbbbbbbb-bbbb-cccc-dddd-eeeeeeeeeeee" },
		func(x *model.NotifySettings) { x.Provider = model.NotifyM365SMTP },
	} {
		x := Mask(gs)
		mut(&x)
		if _, err := PrepareForStorage(x, gs, c); err == nil {
			t.Errorf("oauth endpoint change accepted: %+v", x.OAuth)
		}
	}
	// Switching from smtp/login to graph drops the now-unused password instead of failing.
	sw := Mask(out)
	sw.Provider = model.NotifyM365Graph
	sw.OAuth = model.OAuthSettings{TenantID: testTenant, ClientID: testClient, ClientSecret: "fresh"}
	res, err := PrepareForStorage(sw, out, c)
	if err != nil || res.SMTP.Password != "" || !secrets.IsEncrypted(res.OAuth.ClientSecret) {
		t.Fatalf("switch: %v %+v", err, res)
	}
}

func TestFormat(t *testing.T) {
	cfg := model.NotifySettings{SubjectPrefix: "[IDL]", GUIBaseURL: "https://idl.corp.example/"}
	now := time.Date(2026, 10, 24, 13, 5, 0, 0, time.UTC)
	subj, body := Format(cfg, testNote(), now)
	if subj != "[IDL] Run failed" {
		t.Fatalf("subject %q", subj)
	}
	host, _ := os.Hostname()
	for _, want := range []string{"Run abc failed.\nError: boom\n", "https://idl.corp.example/#/runs/abc", host, "2026-10-24 13:05:00 UTC"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	subj, body = Format(model.NotifySettings{GUIBaseURL: "http://x"}, model.Notification{Event: model.EventApprovalsPending}, now)
	if subj != "Changes are waiting for approval" || !strings.Contains(body, "http://x/#/approvals") {
		t.Fatalf("%q %q", subj, body)
	}
	_, body = Format(model.NotifySettings{}, model.Notification{Event: model.EventTest, Lines: []string{"x"}}, now)
	if strings.Contains(body, "Open in") {
		t.Fatalf("link without base url: %q", body)
	}
}

func TestNotifyAsyncAndFilters(t *testing.T) {
	srvTLS, cliTLS := testCert(t)
	f := newFakeSMTP(t, srvTLS, "STARTTLS")
	cfg := smtpCfg(f.Port(), "starttls", "none")
	cfg.Events = model.NotifyEvents{RunFailed: true}
	settings := model.DefaultSettings()
	settings.Notify = cfg

	release := make(chan struct{})
	var dials atomic.Int32
	s := newSvc(cliTLS)
	s.Settings = func(context.Context) (model.Settings, error) { return settings, nil }
	s.Dial = func(ctx context.Context, addr string) (net.Conn, error) {
		dials.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Notify(ctx, testNote())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Notify blocked the caller")
	}
	cancel() // the caller's context ending must not abort the background send
	close(release)
	m := wait(t, f)
	if !strings.Contains(m.Data, "Run failed") {
		t.Fatalf("data %q", m.Data)
	}

	// Event switched off, then notifications disabled: nothing is sent.
	s.Notify(context.Background(), model.Notification{Event: model.EventRunSummary, Subject: "x"})
	settings.Notify.Enabled = false
	s.Notify(context.Background(), testNote())
	// Test events are always on (when enabled).
	settings.Notify.Enabled = true
	s.Notify(context.Background(), model.Notification{Event: model.EventTest, Subject: "t"})
	m = wait(t, f)
	if !strings.Contains(m.Data, "Subject: [IDL] t") {
		t.Fatalf("expected the test message, got %q", m.Data)
	}
	time.Sleep(200 * time.Millisecond)
	if dials.Load() != 2 {
		t.Fatalf("dials = %d, want 2", dials.Load())
	}

	// A failing send and a nil/erroring settings func never panic.
	settings.Notify.SMTP.Port = 1
	s.Dial = func(ctx context.Context, addr string) (net.Conn, error) { panic("boom") }
	s.Notify(context.Background(), testNote())
	(&Service{}).Notify(context.Background(), testNote())
	var nilSvc *Service
	nilSvc.Notify(context.Background(), testNote())
	time.Sleep(100 * time.Millisecond)
}
