package api

import (
	"testing"

	"idlifecycle/internal/model"
)

func TestPlacementOverHTTP(t *testing.T) {
	e := newTenv(t)
	admin, approver, viewer := e.login("admin"), e.login("approver"), e.login("viewer")
	e.fake.AddOU("OU=Mkt,OU=Staff,DC=x", "OU=Eng,OU=Staff,DC=x", "OU=Elsewhere,DC=y")

	// The OU picker lists the OUs below the search bases (OU=Staff,DC=x).
	var ous struct {
		OUs   []struct{ DN, Name string }
		Error string
	}
	want(t, "viewer cannot list OUs", viewer.do("GET", "/api/ad/ous", nil, nil), 403)
	want(t, "ous", approver.do("GET", "/api/ad/ous", nil, &ous), 200)
	if len(ous.OUs) != 2 || ous.OUs[0].Name != "Eng" || ous.Error != "" {
		t.Errorf("ous: %+v", ous)
	}

	var s model.Settings
	admin.do("GET", "/api/settings", nil, &s)
	s.Placement = model.PlacementSettings{Enabled: true, Field: "Dept", Unmapped: model.UnmappedHold,
		Rules: []model.PlacementRule{{Match: "Marketing", OU: "OU=Mkt,OU=Staff,DC=x"}}}
	bad := s
	bad.Placement.Rules = []model.PlacementRule{{Match: "X", OU: "not a dn"}}
	want(t, "an OU must be a DN", admin.do("PUT", "/api/settings", bad, nil), 400)
	want(t, "save placement", admin.do("PUT", "/api/settings", s, nil), 200)

	e.seedSource(admin) // Jane in Marketing, Bob in Eng (no rule)
	var started struct{ ID string }
	admin.do("POST", "/api/runs", nil, &started)
	e.waitRun(admin, started.ID)

	var vals struct {
		Field  string
		Values []struct {
			Value  string
			People int
			OU     string
		}
	}
	want(t, "values", admin.do("GET", "/api/placement/values", nil, &vals), 200)
	if vals.Field != "Dept" || len(vals.Values) != 2 || vals.Values[0].Value != "Eng" || vals.Values[0].OU != "" || vals.Values[1].OU != "OU=Mkt,OU=Staff,DC=x" {
		t.Errorf("values (unmapped first): %+v", vals)
	}

	var pend struct{ Items []model.Change }
	admin.do("GET", "/api/changes?status=pending&type=create", nil, &pend)
	var bob model.Change
	for _, c := range pend.Items {
		if c.ExternalID == "E2" {
			bob = c
		}
	}
	if !bob.Payload.NeedsOU {
		t.Fatalf("Bob's create must wait for an OU: %+v", bob.Payload)
	}
	want(t, "approve without an OU", approver.do("POST", "/api/changes/"+bob.ID+"/approve", nil, nil), 400)
	want(t, "viewer cannot choose", viewer.do("POST", "/api/changes/"+bob.ID+"/ou", map[string]any{"ou": "OU=Eng,OU=Staff,DC=x"}, nil), 403)
	want(t, "approver cannot add a rule", approver.do("POST", "/api/changes/"+bob.ID+"/ou", map[string]any{"ou": "OU=Eng,OU=Staff,DC=x", "add_rule": true}, nil), 403)
	want(t, "not a DN", approver.do("POST", "/api/changes/"+bob.ID+"/ou", map[string]any{"ou": "Eng"}, nil), 400)
	var c model.Change
	want(t, "approver chooses for Bob", approver.do("POST", "/api/changes/"+bob.ID+"/ou", map[string]any{"ou": "OU=Eng,OU=Staff,DC=x"}, &c), 200)
	if c.Payload.OU != "OU=Eng,OU=Staff,DC=x" || c.Payload.NeedsOU {
		t.Errorf("after choosing: %+v", c.Payload)
	}
	want(t, "admin also adds the rule", admin.do("POST", "/api/changes/"+bob.ID+"/ou", map[string]any{"ou": "OU=Eng,OU=Staff,DC=x", "add_rule": true}, nil), 200)
	admin.do("GET", "/api/settings", nil, &s)
	if len(s.Placement.Rules) != 2 {
		t.Errorf("rule for Eng: %+v", s.Placement.Rules)
	}
	want(t, "approve", approver.do("POST", "/api/changes/"+bob.ID+"/approve", nil, nil), 200)
	var hist struct{ Total int }
	admin.do("GET", "/api/audit?q="+urlq("action:change.set_ou"), nil, &hist)
	if hist.Total != 2 {
		t.Errorf("both choices are in the history: %d", hist.Total)
	}
}
