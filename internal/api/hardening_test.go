package api

import (
	"context"
	"strings"
	"testing"

	"idlifecycle/internal/model"
	"idlifecycle/internal/sources"
	"idlifecycle/internal/store"
)

func mustAudit(t *testing.T, e *tenv, action string) []model.AuditEntry {
	t.Helper()
	items, _, err := e.st.ListAudit(context.Background(), store.AuditFilter{Action: action}, store.Page{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	return items
}

func TestPartialUserUpdateDoesNotReenableAccount(t *testing.T) {
	e := newTenv(t)
	admin := e.login("admin")
	var u model.User
	want(t, "create", admin.do("POST", "/api/users", map[string]any{"username": "gone", "role": "viewer", "password": pw, "disabled": true}, &u), 201)
	if !u.Disabled {
		t.Fatal("created disabled")
	}
	want(t, "reset password only", admin.do("PUT", "/api/users/"+u.ID, map[string]any{"password": "another-long-password"}, &u), 200)
	if !u.Disabled {
		t.Fatal("a partial update must not silently re-enable a suspended account")
	}
	want(t, "explicit enable", admin.do("PUT", "/api/users/"+u.ID, map[string]any{"disabled": false}, &u), 200)
	if u.Disabled {
		t.Fatal("explicit disabled:false must enable")
	}
}

func TestDeniedActionsAreAudited(t *testing.T) {
	e := newTenv(t)
	viewer := e.login("viewer")
	want(t, "denied", viewer.do("POST", "/api/runs", nil, nil), 403)
	tok := viewer.csrf
	viewer.csrf = "bad"
	want(t, "csrf", viewer.do("GET", "/api/status", nil, nil), 200)
	viewer.csrf = tok
	if n := len(mustAudit(t, e, "auth.denied")); n != 1 {
		t.Errorf("denials must be audited, got %d", n)
	}
}

func TestChangingEndpointRequiresSecretReentry(t *testing.T) {
	e := newTenv(t)
	admin := e.login("admin")
	var s model.Source
	body := map[string]any{"name": "p", "type": "rest", "enabled": true,
		"options": map[string]any{"external_id_field": "employeeId"},
		"config":  map[string]any{"base_url": "https://hr.example.com", "auth": "bearer", "bearer_token": "s3cret"}}
	want(t, "create", admin.do("POST", "/api/sources", body, &s), 201)

	s.Config["base_url"] = "https://attacker.example"
	want(t, "redirect with masked secret refused", admin.do("PUT", "/api/sources/"+s.ID, s, nil), 400)
	s.Config["bearer_token"] = "new-secret"
	want(t, "redirect with fresh secret allowed", admin.do("PUT", "/api/sources/"+s.ID, s, nil), 200)
}

func TestCSVPathPolicyIsEnforcedOnSave(t *testing.T) {
	e := newTenv(t)
	admin := e.login("admin")
	sources.PathPolicy = func(p string) error {
		if strings.Contains(p, "secret.key") {
			return context.DeadlineExceeded // any error
		}
		return nil
	}
	defer func() { sources.PathPolicy = nil }()
	want(t, "blocked path", admin.do("POST", "/api/sources", map[string]any{"name": "k", "type": "csv", "enabled": true,
		"options": map[string]any{"external_id_field": "id"}, "config": map[string]any{"path": `C:\ProgramData\IDLifecycle\secret.key`}}, nil), 400)
}
