package api

import (
	"strings"
	"testing"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/model"
)

func (e *tenv) runOnce(admin *client) model.Run {
	e.t.Helper()
	var started struct{ ID string }
	want(e.t, "start run", admin.do("POST", "/api/runs", nil, &started), 202)
	return e.waitRun(admin, started.ID)
}

func TestNameConflictsOverHTTP(t *testing.T) {
	e := newTenv(t)
	admin, approver, viewer := e.login("admin"), e.login("approver"), e.login("viewer")
	e.fake.AddUser(ad.FakeUser{DN: "CN=John Doe,OU=Other,DC=x", Sam: "jdoe", UPN: "john@x.test", Enabled: true})
	e.seedSource(admin)
	e.runOnce(admin)

	var pend struct{ Items []model.Change }
	admin.do("GET", "/api/changes?status=pending&type=create", nil, &pend)
	var jane model.Change
	for _, c := range pend.Items {
		if c.ExternalID == "E1" {
			jane = c
		}
	}
	if !jane.Payload.NeedsName {
		t.Fatalf("Jane's create waits for a username: %+v", jane.Payload)
	}
	want(t, "approve refused", approver.do("POST", "/api/changes/"+jane.ID+"/approve", nil, nil), 400)

	var names struct {
		Current     *model.Candidate
		Conflicts   []model.NameConflict
		Suggestions []struct {
			Mode      string
			Candidate model.Candidate
		}
	}
	want(t, "viewer cannot load names", viewer.do("GET", "/api/changes/"+jane.ID+"/names", nil, nil), 403)
	want(t, "names", approver.do("GET", "/api/changes/"+jane.ID+"/names", nil, &names), 200)
	if names.Current == nil || names.Current.Sam != "jdoe" || len(names.Conflicts) != 1 || len(names.Suggestions) == 0 || names.Suggestions[0].Candidate.Sam == "jdoe" {
		t.Fatalf("names: %+v", names)
	}

	var taken struct {
		Error     struct{ Code string }
		Conflicts []model.NameConflict
	}
	want(t, "taken name", approver.do("POST", "/api/changes/"+jane.ID+"/name", map[string]any{"sam": "jdoe", "upn": "jane.doe@x.test", "cn": "Jane Doe"}, &taken), 409)
	if taken.Error.Code != "name_taken" || len(taken.Conflicts) != 1 || taken.Conflicts[0].DN == "" {
		t.Fatalf("409 body: %+v", taken)
	}
	want(t, "bad upn suffix", approver.do("POST", "/api/changes/"+jane.ID+"/name", map[string]any{"sam": "janed", "upn": "jane@other.test", "cn": "Jane Doe"}, nil), 400)

	var check struct{ Taken []model.NameConflict }
	want(t, "check names", approver.do("POST", "/api/ad/check-names", map[string]any{"names": []map[string]string{{"field": "sam", "value": "JDOE"}, {"field": "upn", "value": "free@x.test"}}}, &check), 200)
	if len(check.Taken) != 1 || check.Taken[0].Field != "sam" {
		t.Errorf("check: %+v", check)
	}
	want(t, "check refuses unknown fields", approver.do("POST", "/api/ad/check-names", map[string]any{"names": []map[string]string{{"field": "x", "value": "a"}}}, nil), 400)

	var c model.Change
	want(t, "set name", approver.do("POST", "/api/changes/"+jane.ID+"/name", map[string]any{"sam": "janed", "upn": "jane.d@x.test", "cn": "Jane Doe"}, &c), 200)
	if c.Payload.NeedsName || c.Payload.Candidates[0].Sam != "janed" {
		t.Fatalf("after set: %+v", c.Payload)
	}
	want(t, "approve", approver.do("POST", "/api/changes/"+jane.ID+"/approve", nil, nil), 200)
	var hist struct{ Total int }
	admin.do("GET", "/api/audit?q="+urlq("action:change.set_name"), nil, &hist)
	eqi(t, "audited", hist.Total, 1)
}

func TestManualPeopleOverHTTP(t *testing.T) {
	e := newTenv(t)
	admin, approver, viewer := e.login("admin"), e.login("approver"), e.login("viewer")

	var src model.Source
	want(t, "manual source", admin.do("POST", "/api/sources", map[string]any{"name": "Contractors", "type": "manual", "enabled": true,
		"options": map[string]any{}, "config": map[string]any{"id_prefix": "CT-"}}, &src), 201)
	if src.Options.ExternalIDField != "ExternalID" || src.Options.StatusField != "Status" {
		t.Fatalf("options are forced: %+v", src.Options)
	}
	want(t, "bad form", admin.do("POST", "/api/sources", map[string]any{"name": "Bad", "type": "manual", "enabled": true,
		"config": map[string]any{"form_fields": "First Name | x"}}, nil), 400)

	var infos []struct {
		ID, Name string
		IDPrefix string `json:"id_prefix"`
		Fields   []struct{ Name, Kind string }
	}
	want(t, "viewer lists manual sources", viewer.do("GET", "/api/manual/sources", nil, &infos), 200)
	if len(infos) != 1 || len(infos[0].Fields) != 10 {
		t.Fatalf("manual sources: %+v", infos)
	}
	base := "/api/manual/sources/" + src.ID
	jane := map[string]any{"attrs": map[string]string{"FirstName": "Jane", "LastName": "Doe", "Department": "Marketing", "EndDate": "2030-06-30"}}

	want(t, "required field", approver.do("POST", base+"/preview", map[string]any{"attrs": map[string]string{"FirstName": "Jane"}}, nil), 400)
	var prev struct {
		Names     model.Candidate
		OU        string
		Conflicts []model.NameConflict
	}
	want(t, "preview", approver.do("POST", base+"/preview", jane, &prev), 200)
	if prev.Names.Sam != "jdoe" || prev.OU != "OU=Staff,DC=x" {
		t.Fatalf("preview: %+v", prev)
	}
	want(t, "viewer cannot add", viewer.do("POST", base+"/people", jane, nil), 403)
	var p model.ManualPerson
	want(t, "add", approver.do("POST", base+"/people", jane, &p), 201)
	if !strings.HasPrefix(p.ExternalID, "CT-") || len(p.ExternalID) != 9 || p.CreatedBy != "approver" {
		t.Fatalf("person: %+v", p)
	}
	var people []model.ManualPerson
	want(t, "list", viewer.do("GET", base+"/people", nil, &people), 200)
	if len(people) != 1 || people[0].State != "not synced yet" {
		t.Fatalf("people: %+v", people)
	}
	upd := map[string]any{"attrs": map[string]string{"FirstName": "Jane", "LastName": "Doe", "Department": "Sales"}}
	want(t, "edit", approver.do("PUT", base+"/people/"+p.ID, upd, nil), 200)
	want(t, "end", approver.do("POST", base+"/people/"+p.ID+"/end", map[string]any{"note": "contract over"}, &p), 200)
	if !p.Ended || p.Attrs["Department"] != "Sales" {
		t.Errorf("ended: %+v", p)
	}
	want(t, "source with people cannot be deleted", admin.do("DELETE", "/api/sources/"+src.ID, nil, nil), 409)
	want(t, "approver cannot delete people", approver.do("DELETE", base+"/people/"+p.ID, nil, nil), 403)
	want(t, "admin deletes before an account exists", admin.do("DELETE", base+"/people/"+p.ID, nil, nil), 204)

	var hist struct{ Total int }
	admin.do("GET", "/api/audit?q="+urlq("action:manual.*"), nil, &hist)
	eqi(t, "audited", hist.Total, 4)
}

func TestLinksAndReportOverHTTP(t *testing.T) {
	e := newTenv(t)
	admin, approver, viewer := e.login("admin"), e.login("approver"), e.login("viewer")
	var s model.Settings
	admin.do("GET", "/api/settings", nil, &s)
	s.Correlate.Enabled = true
	s.Correlate.Rules = []model.CorrelationRule{{Field: "FirstName", ADAttr: "givenName", Compare: "name", Required: true}, {Field: "LastName", ADAttr: "sn", Compare: "name", Required: true}}
	want(t, "enable correlation", admin.do("PUT", "/api/settings", s, nil), 200)
	none := s
	none.Correlate.Rules = []model.CorrelationRule{{Field: "FirstName", ADAttr: "givenName"}}
	want(t, "a required rule is needed", admin.do("PUT", "/api/settings", none, nil), 400)

	g1 := e.fake.AddUser(ad.FakeUser{DN: "CN=Jane Doe,OU=Staff,DC=x", Sam: "jdoe", UPN: "jdoe@x.test", Enabled: true, Attrs: map[string]string{"givenName": "Jane", "sn": "Doe"}})
	e.fake.AddUser(ad.FakeUser{DN: "CN=Jane Doe 2,OU=Staff,DC=x", Sam: "jdoe2", UPN: "jdoe2@x.test", Enabled: true, Attrs: map[string]string{"givenName": "Jane", "sn": "Doe"}})
	e.fake.AddUser(ad.FakeUser{DN: "CN=kiosk01,OU=Staff,DC=x", Sam: "kiosk01", UPN: "kiosk01@x.test", Enabled: true})
	e.seedSource(admin)
	e.runOnce(admin)

	var rep struct {
		Enabled      bool
		Uncorrelated []struct{ Sam string }
		Verify       []struct {
			ChangeID   string `json:"change_id"`
			Candidates int
		}
	}
	want(t, "viewer reads the report", viewer.do("GET", "/api/correlation/report", nil, &rep), 200)
	if !rep.Enabled || len(rep.Uncorrelated) != 1 || rep.Uncorrelated[0].Sam != "kiosk01" || len(rep.Verify) != 1 || rep.Verify[0].Candidates != 2 {
		t.Fatalf("report: %+v", rep)
	}
	id := rep.Verify[0].ChangeID
	want(t, "approve without a choice", approver.do("POST", "/api/changes/"+id+"/approve", nil, nil), 400)
	want(t, "viewer cannot choose", viewer.do("POST", "/api/changes/"+id+"/link", map[string]any{"object_guid": g1}, nil), 403)
	want(t, "choice needed", approver.do("POST", "/api/changes/"+id+"/link", map[string]any{}, nil), 400)
	var c model.Change
	want(t, "choose", approver.do("POST", "/api/changes/"+id+"/link", map[string]any{"object_guid": g1}, &c), 200)
	if c.Payload.Link.Chosen != g1 || c.Payload.ObjectGUID != g1 {
		t.Fatalf("chosen: %+v", c.Payload.Link)
	}

	// Ignoring an account takes it out of the report at once.
	admin.do("GET", "/api/settings", nil, &s)
	s.Correlate.IgnoreAccounts = []string{"kiosk*"}
	want(t, "ignore", admin.do("PUT", "/api/settings", s, nil), 200)
	viewer.do("GET", "/api/correlation/report", nil, &rep)
	eqi(t, "ignored at once", len(rep.Uncorrelated), 0)

	var ids struct{ Items []model.Identity }
	admin.do("GET", "/api/identities?q=E2", nil, &ids)
	bob := ids.Items[0]
	want(t, "bad guid", approver.do("POST", "/api/identities/"+bob.ID+"/link", map[string]any{"object_guid": "x y"}, nil), 400)
	var got model.Identity
	want(t, "link decision", approver.do("POST", "/api/identities/"+bob.ID+"/link", map[string]any{"object_guid": "guid-0003"}, &got), 200)
	if got.LinkDecision != "guid:guid-0003" {
		t.Errorf("decision: %q", got.LinkDecision)
	}
	got = model.Identity{}
	want(t, "clear", approver.do("POST", "/api/identities/"+bob.ID+"/link", map[string]any{"clear": true}, &got), 200)
	if got.LinkDecision != "" {
		t.Errorf("cleared: %q", got.LinkDecision)
	}
}

func eqi(t *testing.T, what string, got, want int) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %d, want %d", what, got, want)
	}
}
