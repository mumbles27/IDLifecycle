package api

import (
	"context"
	"strings"
	"testing"

	"idlifecycle/internal/model"
	"idlifecycle/internal/secrets"
)

func TestNotificationSecretsAreEncryptedAndMasked(t *testing.T) {
	e := newTenv(t)
	admin := e.login("admin")

	var s model.Settings
	admin.do("GET", "/api/settings", nil, &s)
	s.Notify = model.NotifySettings{
		Enabled: true, Provider: model.NotifySMTP, From: "idl@corp.example", To: []string{"ops@corp.example"},
		SMTP:   model.SMTPSettings{Host: "smtp.corp.example", Port: 587, Security: "starttls", Auth: "login", Username: "idl", Password: "Sup3r-Secret-Pw"},
		Events: model.NotifyEvents{RunFailed: true},
	}
	var saved model.Settings
	want(t, "save", admin.do("PUT", "/api/settings", s, &saved), 200)
	if saved.Notify.SMTP.Password != model.MaskedSecret {
		t.Errorf("PUT response must mask the secret: %q", saved.Notify.SMTP.Password)
	}

	stored, _ := e.st.GetSettings(context.Background())
	if !secrets.IsEncrypted(stored.Notify.SMTP.Password) {
		t.Fatalf("secret must be encrypted at rest, got %q", stored.Notify.SMTP.Password)
	}
	for _, who := range []string{"admin", "viewer"} {
		_, body := e.login(who).raw("GET", "/api/settings", nil, nil)
		if strings.Contains(string(body), "Sup3r-Secret-Pw") || strings.Contains(string(body), stored.Notify.SMTP.Password) {
			t.Errorf("%s can see the secret or its ciphertext: %s", who, body)
		}
	}

	// Sending the mask back keeps the stored value; changing the host without the secret is refused.
	saved.Notify.Events.Threshold = true
	want(t, "resave masked", admin.do("PUT", "/api/settings", saved, nil), 200)
	again, _ := e.st.GetSettings(context.Background())
	if again.Notify.SMTP.Password != stored.Notify.SMTP.Password {
		t.Error("masked value must keep the stored secret")
	}
	saved.Notify.SMTP.Host = "smtp.attacker.example"
	want(t, "redirect secret refused", admin.do("PUT", "/api/settings", saved, nil), 400)

	bad := saved
	bad.Notify.SMTP.Host, bad.Notify.To = "smtp.corp.example", nil
	want(t, "enabled without recipients", admin.do("PUT", "/api/settings", bad, nil), 400)
}

func TestNotificationTestEndpoint(t *testing.T) {
	e := newTenv(t)
	cfg := model.NotifySettings{
		Enabled: true, Provider: model.NotifySMTP, From: "idl@corp.example", To: []string{"ops@corp.example"},
		SMTP: model.SMTPSettings{Host: "127.0.0.1", Port: 1, Security: "none", Auth: "none"},
	}
	want(t, "viewer refused", e.login("viewer").do("POST", "/api/notifications/test", map[string]any{"notifications": cfg}, nil), 403)
	want(t, "approver refused", e.login("approver").do("POST", "/api/notifications/test", map[string]any{"notifications": cfg}, nil), 403)

	var res struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	want(t, "admin", e.login("admin").do("POST", "/api/notifications/test", map[string]any{"notifications": cfg}, &res), 200)
	if res.OK || res.Error == "" {
		t.Errorf("a closed port must report a delivery error: %+v", res)
	}
}

func TestMappingRulesForLinkKeyAndDuplicates(t *testing.T) {
	e := newTenv(t)
	admin := e.login("admin")
	mk := func(name, attr string, enabled bool) map[string]any {
		return map[string]any{"name": name, "target_attr": attr, "template": "{{ .X }}", "mode": "sync", "enabled": enabled}
	}
	want(t, "link key", admin.do("POST", "/api/mappings", mk("id", "EmployeeID", true), nil), 400)
	want(t, "home directory", admin.do("POST", "/api/mappings", mk("home", "homeDirectory", true), nil), 400)

	var first model.Mapping
	want(t, "first", admin.do("POST", "/api/mappings", mk("Title", "title", true), &first), 201)
	want(t, "duplicate enabled", admin.do("POST", "/api/mappings", mk("Title2", "Title", true), nil), 400)
	var spare model.Mapping
	want(t, "duplicate disabled is fine", admin.do("POST", "/api/mappings", mk("Title-alt", "title", false), &spare), 201)
	spare.Enabled = true
	want(t, "enabling the duplicate is refused", admin.do("PUT", "/api/mappings/"+spare.ID, spare, nil), 400)
	want(t, "re-saving itself is fine", admin.do("PUT", "/api/mappings/"+first.ID, first, nil), 200)

	want(t, "role grant on link key", admin.do("POST", "/api/roles", map[string]any{"name": "r", "grants": map[string]any{
		"attributes": []map[string]string{{"target_attr": "employeeID", "template": "x"}}}}, nil), 400)
}
