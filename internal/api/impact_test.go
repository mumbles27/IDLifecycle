package api

import (
	"net/url"
	"testing"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/model"
)

func TestGroupSearchOverHTTP(t *testing.T) {
	e := newTenv(t)
	admin, approver := e.login("admin"), e.login("approver")
	e.fake.AddGroup("GG-Finance", "CN=GG-Finance,OU=Groups,DC=x")
	e.fake.AddGroup("Domain Admins", "CN=Domain Admins,CN=Users,DC=x")
	e.fake.SetGroupInfo("Domain Admins", ad.GroupHit{Name: "Domain Admins", Description: "Designated administrators", Category: "security", Scope: "global", Privileged: true})

	var res struct {
		Groups    []ad.GroupHit
		Truncated bool
		Error     string
	}
	want(t, "approver cannot search", approver.do("GET", "/api/ad/groups?q=fin", nil, nil), 403)
	want(t, "too short", admin.do("GET", "/api/ad/groups?q=f", nil, nil), 400)
	want(t, "bad max", admin.do("GET", "/api/ad/groups?q=fin&max=500", nil, nil), 400)
	want(t, "search", admin.do("GET", "/api/ad/groups?q=fin", nil, &res), 200)
	if len(res.Groups) != 1 || res.Groups[0].Sam != "gg-finance" || res.Groups[0].DN != "CN=GG-Finance,OU=Groups,DC=x" || res.Error != "" {
		t.Fatalf("groups: %+v", res)
	}
	want(t, "wildcard + description", admin.do("GET", "/api/ad/groups?q="+url.QueryEscape("design*admin"), nil, &res), 200)
	if len(res.Groups) != 1 || !res.Groups[0].Privileged {
		t.Fatalf("privileged hit: %+v", res)
	}
	want(t, "max + truncated", admin.do("GET", "/api/ad/groups?q=gg&max=1", nil, &res), 200)
	if len(res.Groups) != 1 || !res.Truncated {
		t.Fatalf("truncated: %+v", res)
	}
}

func TestRolePreviewExplainsAnUnsavedRule(t *testing.T) {
	e := newTenv(t)
	admin := e.login("admin")
	rule := model.RoleRule{ID: "new", Name: "Seniors", Match: "all", Enabled: true, Conditions: []model.Condition{
		{Attr: "Grade", Op: "gte", Value: "7"},
		{Op: "any_of", Conditions: []model.Condition{{Attr: "Title", Op: "contains", Value: "senior"}, {Attr: "Title", Op: "contains", Value: "lead"}}},
	}, Grants: model.Grants{Groups: []string{"GG-Seniors"}}}
	var prev struct {
		RuleMatched bool `json:"rule_matched"`
		Groups      []string
		Explain     []struct {
			Path    string
			Matched bool
			Actual  string
		}
	}
	want(t, "preview", admin.do("POST", "/api/roles/preview", map[string]any{"record": map[string]string{"Grade": "8", "Title": "Team Lead"}, "rule": rule}, &prev), 200)
	if !prev.RuleMatched || len(prev.Explain) != 4 || prev.Explain[3].Path != "conditions[1].conditions[1]" || !prev.Explain[3].Matched {
		t.Fatalf("explain: %+v", prev)
	}
	if len(prev.Groups) != 1 || prev.Groups[0] != "GG-Seniors" {
		t.Errorf("the unsaved rule's grants are part of the preview: %+v", prev.Groups)
	}
	bad := rule
	bad.Conditions = []model.Condition{{Attr: "a", Op: "older_than_days", Value: "x"}}
	want(t, "invalid rule", admin.do("POST", "/api/roles/preview", map[string]any{"record": map[string]string{}, "rule": bad}, nil), 400)
}

func TestImpactPreviewOverHTTP(t *testing.T) {
	e := newTenv(t)
	admin, approver := e.login("admin"), e.login("approver")
	e.fake.AddGroup("ALLUSERS", "CN=ALLUSERS,OU=Groups,DC=x")
	e.seedSource(admin)
	e.runOnce(admin)

	var res struct {
		People       int
		NamesChecked bool `json:"names_checked"`
		Summary      struct {
			ChangedPeople int            `json:"changed_people"`
			Baseline      map[string]int `json:"baseline"`
			Proposed      map[string]int `json:"proposed"`
		}
		Items []struct {
			ExternalID string `json:"external_id"`
			Difference string
			Lines      []struct{ Op, Text string }
		}
		Total int
	}
	want(t, "approver cannot preview", approver.do("POST", "/api/preview/impact", map[string]any{}, nil), 403)
	want(t, "next sync", admin.do("POST", "/api/preview/impact", map[string]any{"only_changed": false}, &res), 200)
	if res.People != 2 || res.Summary.ChangedPeople != 0 || res.Summary.Baseline["create"] != 2 || res.Total != 2 || res.NamesChecked {
		t.Fatalf("baseline: %+v", res)
	}
	// A partial settings document: only lifecycle_groups changes.
	body := map[string]any{"settings": map[string]any{"lifecycle_groups": map[string]any{"active_add": []string{"ALLUSERS"}, "disabled_remove": "all_except"}}}
	want(t, "proposed", admin.do("POST", "/api/preview/impact", body, &res), 200)
	if res.Summary.ChangedPeople != 2 || res.Total != 2 || res.Items[0].Difference != "changed" {
		t.Fatalf("proposed: %+v", res)
	}
	found := false
	for _, l := range res.Items[0].Lines {
		if l.Op == "+" && l.Text == "Add to group ALLUSERS" {
			found = true
		}
	}
	if !found {
		t.Errorf("lines: %+v", res.Items[0].Lines)
	}
	want(t, "bad settings", admin.do("POST", "/api/preview/impact", map[string]any{"settings": map[string]any{"lifecycle_groups": map[string]any{"disabled_remove": "x"}}}, nil), 400)
	want(t, "bad role", admin.do("POST", "/api/preview/impact", map[string]any{"roles": []map[string]any{{"name": "x", "match": "all", "conditions": []map[string]string{{"attr": "a", "op": "zz"}}}}}, nil), 400)

	var hist struct{ Total int }
	admin.do("GET", "/api/audit?q="+urlq("action:preview.impact"), nil, &hist)
	if hist.Total < 2 {
		t.Errorf("previews are audited: %d", hist.Total)
	}
	// Nothing was planned into the store.
	var pend struct{ Total int }
	admin.do("GET", "/api/changes?status=pending", nil, &pend)
	eqi(t, "pending changes only from the real run", pend.Total, 2)
}
