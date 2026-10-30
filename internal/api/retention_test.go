package api

import (
	"context"
	"testing"

	"idlifecycle/internal/model"
	"idlifecycle/internal/store"
)

func TestRetentionPoliciesOverHTTP(t *testing.T) {
	e := newTenv(t)
	admin, approver, viewer := e.login("admin"), e.login("approver"), e.login("viewer")

	pol := map[string]any{"name": "Executives", "enabled": true, "priority": 10, "match_mode": "all", "delete_after_days": 365,
		"disabled_ou": "OU=Legal Hold,DC=x", "extra_keep_groups": []string{"GG-Exec"}, "approval": "manual",
		"conditions": []map[string]any{{"attr": "ad.memberOf", "op": "contains", "value": "GG-Exec"}}}
	want(t, "viewer cannot create", viewer.do("POST", "/api/retention-policies", pol, nil), 403)
	want(t, "approver cannot create", approver.do("POST", "/api/retention-policies", pol, nil), 403)
	var created model.RetentionPolicy
	want(t, "admin creates", admin.do("POST", "/api/retention-policies", pol, &created), 201)
	want(t, "duplicate name", admin.do("POST", "/api/retention-policies", pol, nil), 409)
	want(t, "Default is reserved", admin.do("POST", "/api/retention-policies", map[string]any{"name": "Default"}, nil), 400)
	want(t, "bad days", admin.do("POST", "/api/retention-policies", map[string]any{"name": "x", "delete_after_days": -3}, nil), 400)

	var listed struct {
		Items         []model.RetentionPolicy `json:"items"`
		Total         int                     `json:"total"`
		DefaultPeople int                     `json:"default_people"`
	}
	want(t, "viewer lists", viewer.do("GET", "/api/retention-policies", nil, &listed), 200)
	if listed.Total != 1 || listed.Items[0].Name != "Executives" || listed.Items[0].DeleteAfterDays != 365 || listed.Items[0].Approval != "manual" {
		t.Fatalf("list: %+v", listed)
	}
	want(t, "viewer reads one", viewer.do("GET", "/api/retention-policies/"+created.ID, nil, nil), 200)

	pol["delete_after_days"] = 400
	want(t, "admin updates", admin.do("PUT", "/api/retention-policies/"+created.ID, pol, nil), 200)
	rows, _, err := e.st.ListAudit(context.Background(), store.AuditFilter{EntityType: "retention_policy", EntityID: created.ID, Action: "retention.update"}, store.Page{Limit: 5})
	if err != nil || len(rows) != 1 {
		t.Fatalf("audit: %v %v", rows, err)
	}
	if ch, _ := rows[0].Detail["changes"].(map[string]any); ch["delete_after_days"] == nil {
		t.Errorf("before/after detail: %+v", rows[0].Detail)
	}

	var test struct {
		Matched bool             `json:"matched"`
		Explain []map[string]any `json:"explain"`
	}
	body := map[string]any{"policy": map[string]any{"name": "t", "match_mode": "all", "conditions": []map[string]any{{"attr": "Dept", "op": "eq", "value": "Legal"}}},
		"record": map[string]string{"Dept": "legal"}}
	want(t, "approver cannot test", approver.do("POST", "/api/retention-policies/test", body, nil), 403)
	want(t, "admin tests", admin.do("POST", "/api/retention-policies/test", body, &test), 200)
	if !test.Matched || len(test.Explain) != 1 || test.Explain[0]["actual"] != "legal" {
		t.Fatalf("test: %+v", test)
	}
	want(t, "nothing to test against", admin.do("POST", "/api/retention-policies/test", map[string]any{"policy": body["policy"]}, nil), 400)

	// A person: retention on the identity page, assignment by an approver.
	e.seedSource(admin)
	var started struct{ ID string }
	admin.do("POST", "/api/runs", nil, &started)
	e.waitRun(admin, started.ID)
	var ids struct {
		Items []model.Identity `json:"items"`
	}
	viewer.do("GET", "/api/identities", nil, &ids)
	if len(ids.Items) == 0 {
		t.Fatal("no identities")
	}
	path := "/api/identities/" + ids.Items[0].ID
	var detail struct {
		Retention model.IdentityRetention `json:"retention"`
	}
	want(t, "identity", viewer.do("GET", path, nil, &detail), 200)
	if detail.Retention.PolicyID != model.RetentionDefaultID || detail.Retention.DeleteAfterDays != 30 {
		t.Fatalf("default retention: %+v", detail.Retention)
	}
	assign := map[string]any{"policy_id": created.ID, "reason": "litigation hold #42"}
	want(t, "viewer cannot assign", viewer.do("PUT", path+"/retention", assign, nil), 403)
	want(t, "short reason", approver.do("PUT", path+"/retention", map[string]any{"policy_id": created.ID, "reason": "x"}, nil), 400)
	want(t, "unknown policy", approver.do("PUT", path+"/retention", map[string]any{"policy_id": "nope", "reason": "litigation hold #42"}, nil), 400)
	want(t, "approver assigns", approver.do("PUT", path+"/retention", assign, nil), 200)
	viewer.do("GET", path, nil, &detail)
	if detail.Retention.PolicyID != created.ID || detail.Retention.Reason != "assigned" || detail.Retention.AssignedBy != "approver" || detail.Retention.DeleteAfterDays != 400 {
		t.Fatalf("assigned retention: %+v", detail.Retention)
	}

	var up struct {
		Items []map[string]any `json:"items"`
		Total int              `json:"total"`
	}
	want(t, "upcoming", viewer.do("GET", "/api/retention/upcoming?days=30&kind=delete", nil, &up), 200)
	want(t, "bad kind", viewer.do("GET", "/api/retention/upcoming?kind=x", nil, nil), 400)
	want(t, "bad days", viewer.do("GET", "/api/retention/upcoming?days=0", nil, nil), 400)

	want(t, "viewer cannot delete", viewer.do("DELETE", "/api/retention-policies/"+created.ID, nil, nil), 403)
	want(t, "admin deletes", admin.do("DELETE", "/api/retention-policies/"+created.ID, nil, nil), 204)
	viewer.do("GET", path, nil, &detail)
	eqStr(t, "back to default", detail.Retention.PolicyID, model.RetentionDefaultID)

	var imp map[string]any
	want(t, "impact with proposed policies", admin.do("POST", "/api/preview/impact", map[string]any{"retention_policies": []map[string]any{{"name": "x", "match_mode": "all", "delete_after_days": -1}}}, &imp), 400)
}

func eqStr(t *testing.T, what, got, exp string) {
	t.Helper()
	if got != exp {
		t.Errorf("%s: got %q, want %q", what, got, exp)
	}
}
