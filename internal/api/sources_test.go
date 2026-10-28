package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"idlifecycle/internal/model"
)

func TestTestUnsavedSourceConfig(t *testing.T) {
	e := newTenv(t)
	admin, approver := e.login("admin"), e.login("approver")
	var lastToken atomic.Value
	hr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastToken.Store(r.Header.Get("Authorization"))
		if r.Header.Get("Authorization") != "Bearer tok-AAA" {
			http.Error(w, "denied", 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"E1","name":{"first":"Jane"}},{"id":"E2","name":{"first":"Bob"}}]}`))
	}))
	defer hr.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("a stored secret reached a new address")
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer other.Close()

	cfg := map[string]any{"base_url": hr.URL, "records_path": "data", "auth": "bearer", "bearer_token": "tok-AAA"}
	body := map[string]any{"type": "rest", "options": map[string]any{"external_id_field": "id"}, "config": cfg}
	var res struct {
		OK         bool
		Count      int
		Attributes []string
		Sample     []map[string]string
		Error      string
	}
	want(t, "approver cannot test", approver.do("POST", "/api/sources/test", body, nil), 403)
	want(t, "unsaved test", admin.do("POST", "/api/sources/test", body, &res), 200)
	if !res.OK || res.Count != 2 || strings.Join(res.Attributes, ",") != "id,name.first" || res.Sample[1]["name.first"] != "Bob" {
		t.Fatalf("unsaved test: %+v", res)
	}
	res = struct {
		OK         bool
		Count      int
		Attributes []string
		Sample     []map[string]string
		Error      string
	}{}
	body["options"] = map[string]any{"external_id_field": "EmployeeNumber"}
	admin.do("POST", "/api/sources/test", body, &res)
	if res.OK || !strings.Contains(res.Error, "EmployeeNumber") {
		t.Fatalf("a missing ID field is reported: %+v", res)
	}
	want(t, "invalid config", admin.do("POST", "/api/sources/test", map[string]any{"type": "rest", "config": map[string]any{"base_url": "http://hr.example.com"}}, nil), 400)

	// A saved source: masked secrets come from the store, but never go to a changed address.
	var saved model.Source
	want(t, "create", admin.do("POST", "/api/sources", map[string]any{"name": "HR", "type": "rest", "enabled": true,
		"options": map[string]any{"external_id_field": "id"}, "config": cfg}, &saved), 201)
	edit := map[string]any{"id": saved.ID, "type": "rest", "options": map[string]any{"external_id_field": "id"},
		"config": map[string]any{"base_url": hr.URL, "records_path": "data", "auth": "bearer", "bearer_token": model.MaskedSecret}}
	lastToken.Store("")
	admin.do("POST", "/api/sources/test", edit, &res)
	if !res.OK || lastToken.Load() != "Bearer tok-AAA" {
		t.Fatalf("stored secret used for a masked field: %+v %v", res, lastToken.Load())
	}
	edit["config"].(map[string]any)["base_url"] = other.URL
	want(t, "stored secret not sent to a new address", admin.do("POST", "/api/sources/test", edit, nil), 400)

	var hist struct{ Items []model.AuditEntry }
	admin.do("GET", "/api/audit?action=source.test&limit=50", nil, &hist)
	unsaved := 0
	for _, a := range hist.Items {
		if a.EntityID == "(unsaved)" {
			unsaved++
		}
	}
	if unsaved < 2 {
		t.Errorf("unsaved tests are audited: %d", unsaved)
	}
	raw, _ := json.Marshal(hist)
	if strings.Contains(string(raw), "tok-AAA") {
		t.Error("the audit must not contain the secret")
	}
}

func TestUnknownSourceTypeStaysDeletable(t *testing.T) {
	e := newTenv(t)
	admin := e.login("admin")
	now := time.Now()
	if err := e.st.CreateSource(context.Background(), model.Source{ID: "old", Name: "Old connector", Type: "retired", Enabled: true, CreatedAt: now, UpdatedAt: now,
		Options: model.SourceOptions{ExternalIDField: "employeeId"}, Config: map[string]any{"company_id": "C1", "client_secret": "enc:v1:c2VjcmV0"}}); err != nil {
		t.Fatal(err)
	}
	var list struct{ Items []model.Source }
	want(t, "list", admin.do("GET", "/api/sources", nil, &list), 200)
	if len(list.Items) != 1 || list.Items[0].Config["client_secret"] != model.MaskedSecret {
		t.Fatalf("listed with the secret masked: %+v", list.Items)
	}
	var res struct {
		OK    bool
		Error string
	}
	admin.do("POST", "/api/sources/old/test", nil, &res)
	if res.OK || !strings.Contains(res.Error, "unknown source type") {
		t.Fatalf("test explains: %+v", res)
	}
	want(t, "delete", admin.do("DELETE", "/api/sources/old", nil, nil), 204)
}
