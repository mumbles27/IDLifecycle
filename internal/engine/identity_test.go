package engine

import (
	"errors"
	"strings"
	"testing"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/model"
	"idlifecycle/internal/sources"
	"idlifecycle/internal/store"
)

func (e *env) onePending(typ string) model.Change {
	e.t.Helper()
	list := e.changes(model.StatusPending, typ)
	if len(list) != 1 {
		e.t.Fatalf("want 1 pending %s, got %d", typ, len(list))
	}
	return list[0]
}

func (e *env) execute() {
	e.t.Helper()
	if _, err := e.eng.Execute(e.ctx); err != nil {
		e.t.Fatal(err)
	}
}

func TestManualNameConflictWaitsForAPersonToChoose(t *testing.T) {
	e := newEnv(t)
	n := &recordingNotifier{}
	e.eng.Notifier = n
	e.fake.AddUser(ad.FakeUser{DN: "CN=John Doe,OU=Other,DC=corp,DC=example,DC=com", Sam: "jdoe", UPN: "john@corp.example.com", Enabled: true})
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""))
	e.run()

	c := e.onePending(model.ChangeCreate)
	if !c.Payload.NeedsName || len(c.Payload.NameConflicts) != 1 || c.Payload.NameConflicts[0].Field != "sam" ||
		!strings.Contains(c.Payload.NameConflicts[0].DN, "John Doe") {
		t.Fatalf("manual mode: a taken sAMAccountName must wait for a person: %+v", c.Payload)
	}
	if _, err := e.eng.Approve(e.ctx, c.ID, "alice", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("approving without a name must be refused, got %v", err)
	}
	if got := n.events(model.EventIdentityIssues); len(got) != 1 || !strings.Contains(strings.Join(got[0].Lines, "\n"), "Username needed: Jane Doe (E1)") {
		t.Fatalf("identity_issues notification: %+v", got)
	}

	_, _, sugg, err := e.eng.ChangeNames(e.ctx, c.ID)
	e.must(err)
	if len(sugg) == 0 || sugg[0].Candidate.Sam == "jdoe" {
		t.Fatalf("suggestions must be free names: %+v", sugg)
	}

	var nt *NameTakenError
	_, err = e.eng.SetChangeName(e.ctx, c.ID, model.Candidate{Sam: "jdoe", UPN: "jane.doe@corp.example.com", CN: "Jane Doe"}, "alice")
	if !errors.As(err, &nt) || nt.Conflicts[0].Field != "sam" {
		t.Fatalf("a taken name must be refused with its conflicts, got %v", err)
	}
	if _, err := e.eng.SetChangeName(e.ctx, c.ID, model.Candidate{Sam: "jane doe", UPN: "jane.doe@corp.example.com", CN: "Jane Doe"}, "alice"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an invalid sAMAccountName must be refused, got %v", err)
	}
	if _, err := e.eng.SetChangeName(e.ctx, c.ID, model.Candidate{Sam: "janed", UPN: "jane.doe@other.example.com", CN: "Jane Doe"}, "alice"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a UPN with another suffix must be refused, got %v", err)
	}
	c2, err := e.eng.SetChangeName(e.ctx, c.ID, model.Candidate{Sam: "janed", UPN: "jane.d@corp.example.com", CN: "Jane Doe"}, "alice")
	e.must(err)
	eq(t, "name set", c2.Payload.NeedsName, false)
	eq(t, "summary", c2.Summary[0], "Username chosen by alice: janed")

	// The choice is remembered: the next sync keeps the same change instead of replacing it.
	run := e.run()
	eq(t, "kept", run.Counters["already_queued"], 1)
	eq(t, "same change", e.onePending(model.ChangeCreate).ID, c.ID)

	eq(t, "created", e.approveAll(model.ChangeCreate), 1)
	u := e.user("E1")
	eq(t, "chosen sam", u.Sam, "janed")
	eq(t, "chosen upn", u.UPN, "jane.d@corp.example.com")
}

func TestNameCheckCountsMailOfOtherObjects(t *testing.T) {
	e := newEnv(t)
	e.tweak(func(s *model.Settings) {
		s.Naming.ConflictMode = model.ConflictNumber
		s.Naming.MailTemplate = `{{ .FirstName | lower }}.{{ .LastName | lower }}`
	})
	e.fake.AddMailObject("CN=Jane Doe (contact),OU=Contacts,DC=corp,DC=example,DC=com", "jane.doe@corp.example.com")
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""))
	e.run()
	c := e.onePending(model.ChangeCreate)
	eq(t, "first free candidate", c.Payload.Candidates[0].Mail, "jane.doe2@corp.example.com")
	eq(t, "created", e.approveAll(model.ChangeCreate), 1)
	u := e.user("E1")
	eq(t, "sam follows the mail", u.Sam, "jdoe2")
	eq(t, "mail", u.Attrs["mail"], "jane.doe2@corp.example.com")
}

func TestTemplateAndRandomConflictModes(t *testing.T) {
	t.Run("template", func(t *testing.T) {
		e := newEnv(t)
		e.tweak(func(s *model.Settings) {
			s.Naming.ConflictMode = model.ConflictTemplate
			s.Naming.ConflictSuffix = `{{ last .EmployeeID 3 }}`
		})
		e.fake.AddUser(ad.FakeUser{DN: "CN=John Doe,OU=Other,DC=corp,DC=example,DC=com", Sam: "jdoe", UPN: "jane.doe@corp.example.com", Enabled: true})
		e.writeCSV(row("E1042", "Jane", "Doe", "Marketing", "Designer", "A", ""))
		e.run()
		eq(t, "created", e.approveAll(model.ChangeCreate), 1)
		eq(t, "employee id suffix", e.user("E1042").Sam, "jdoe042")
	})
	t.Run("random", func(t *testing.T) {
		e := newEnv(t)
		e.tweak(func(s *model.Settings) {
			s.Naming.ConflictMode = model.ConflictRandom
			s.Naming.ConflictRandomDigits = 4
		})
		e.fake.AddUser(ad.FakeUser{DN: "CN=John Doe,OU=Other,DC=corp,DC=example,DC=com", Sam: "jdoe", UPN: "x@corp.example.com", Enabled: true})
		e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""))
		e.run()
		first := e.onePending(model.ChangeCreate)
		sam := first.Payload.Candidates[0].Sam
		if len(sam) != len("jdoe")+4 || !strings.HasPrefix(sam, "jdoe") {
			t.Fatalf("random suffix of 4 digits: %q", sam)
		}
		run := e.run()
		eq(t, "random names are stable between runs", run.Counters["already_queued"], 1)
	})
}

// hrAccount seeds an AD account without an id attribute (as found on the first rollout).
func (e *env) hrAccount(sam, first, last, dept string) string {
	return e.fake.AddUser(ad.FakeUser{DN: "CN=" + first + " " + last + "," + baseOU, Sam: sam, UPN: sam + "@corp.example.com", Enabled: true,
		Attrs: map[string]string{"givenName": first, "sn": last, "department": dept}})
}

func (e *env) correlation(fn func(c *model.CorrelationSettings)) {
	e.tweak(func(s *model.Settings) {
		s.Correlate.Enabled = true
		if fn != nil {
			fn(&s.Correlate)
		}
	})
}

func TestCorrelationFullMatchLinksTheExistingAccount(t *testing.T) {
	e := newEnv(t)
	e.correlation(nil)
	guid := e.hrAccount("jdoe", "Jane", "Doe", "Marketing")
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""))
	run := e.run()

	eq(t, "no create", run.Counters["planned_create"], 0)
	eq(t, "link auto-approved and applied", run.Counters["auto_approved"], 1)
	u := e.user("E1")
	eq(t, "same account", u.GUID, guid)
	eq(t, "not renamed (it had no owner)", u.Sam, "jdoe")
	id := e.identity("E1")
	eq(t, "linked", id.State, model.StateLinked)
	eq(t, "linked account", id.ADGUID, guid)

	run = e.run()
	eq(t, "no further link", run.Counters["planned_link"], 0)
	eq(t, "no create later either", run.Counters["planned_create"], 0)
}

func TestCorrelationPartialMatchIsVerifiedAndRejectIsRemembered(t *testing.T) {
	e := newEnv(t)
	n := &recordingNotifier{}
	e.eng.Notifier = n
	e.correlation(nil)
	e.hrAccount("jdoe", "Jane", "Doe", "Sales") // department differs
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""))
	e.run()

	c := e.onePending(model.ChangeLink)
	if !c.Payload.Link.Verify || c.Payload.Link.Chosen == "" || c.Payload.Link.Candidates[0].Full {
		t.Fatalf("a partial match is proposed for verification: %+v", c.Payload.Link)
	}
	if !strings.HasPrefix(c.Summary[0], "VERIFY the match") {
		t.Errorf("the hold explains what to do: %q", c.Summary[0])
	}
	if got := n.events(model.EventIdentityIssues); len(got) != 1 || !strings.Contains(strings.Join(got[0].Lines, "\n"), "Verify the match: Jane Doe (E1) -> jdoe") {
		t.Fatalf("identity_issues: %+v", got)
	}

	_, err := e.eng.Reject(e.ctx, c.ID, "alice", "different person")
	e.must(err)
	eq(t, "rejected account remembered", len(e.identity("E1").LinkRejects), 1)

	run := e.run()
	eq(t, "no new link for a rejected account", run.Counters["planned_link"], 0)
	eq(t, "a create instead", run.Counters["planned_create"], 1)
}

func TestCorrelationSeveralCandidatesNeedAChoice(t *testing.T) {
	e := newEnv(t)
	e.correlation(nil)
	e.hrAccount("jdoe", "Jane", "Doe", "")
	g2 := e.fake.AddUser(ad.FakeUser{DN: "CN=Jane Doe2," + baseOU, Sam: "jdoe2", UPN: "jdoe2@corp.example.com", Enabled: true,
		Attrs: map[string]string{"givenName": "Jane", "sn": "Doe"}})
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""))
	e.run()

	c := e.onePending(model.ChangeLink)
	eq(t, "two candidates", len(c.Payload.Link.Candidates), 2)
	eq(t, "nothing chosen", c.Payload.Link.Chosen, "")
	if _, err := e.eng.Approve(e.ctx, c.ID, "alice", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("approve without a choice must be refused, got %v", err)
	}
	if _, err := e.eng.SetChangeLink(e.ctx, c.ID, "guid-nope", false, "alice"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("only a candidate can be chosen, got %v", err)
	}
	c2, err := e.eng.SetChangeLink(e.ctx, c.ID, g2, false, "alice")
	e.must(err)
	eq(t, "chosen", c2.Payload.Link.Chosen, g2)
	eq(t, "decision remembered", e.identity("E1").LinkDecision, model.LinkDecisionGUID+g2)

	run := e.run()
	eq(t, "the choice survives the next sync", run.Counters["already_queued"], 1)
	eq(t, "linked", e.approveAll(model.ChangeLink), 1)
	eq(t, "the chosen account", e.user("E1").GUID, g2)
}

func TestCorrelationCreateNewInstead(t *testing.T) {
	e := newEnv(t)
	e.correlation(nil)
	e.hrAccount("jdoe", "Jane", "Doe", "Sales")
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""))
	e.run()
	c := e.onePending(model.ChangeLink)
	c2, err := e.eng.SetChangeLink(e.ctx, c.ID, "", true, "alice")
	e.must(err)
	eq(t, "rejected", c2.Status, model.StatusRejected)
	run := e.run()
	eq(t, "create planned", run.Counters["planned_create"], 1)
	eq(t, "no link", run.Counters["planned_link"], 0)
}

func TestCorrelationAccountProposedForTwoPeopleIsNotAutoLinked(t *testing.T) {
	e := newEnv(t)
	e.correlation(func(c *model.CorrelationSettings) { c.Rules = c.Rules[:2] }) // names only
	e.hrAccount("jdoe", "Jane", "Doe", "Marketing")
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""), row("E2", "Jane", "Doe", "Sales", "Rep", "A", ""))
	run := e.run()
	eq(t, "nothing auto-approved", run.Counters["auto_approved"], 0)
	for _, c := range e.changes(model.StatusPending, model.ChangeLink) {
		eq(t, "no pick for "+c.ExternalID, c.Payload.Link.Chosen, "")
	}
}

func TestContractorBecomesEmployeeKeepsTheAccount(t *testing.T) {
	e := newEnv(t)
	e.tweak(func(s *model.Settings) { s.Approval.Create = "auto" })
	man := model.Source{ID: "src-man", Name: "Contractors", Type: sources.TypeManual, Enabled: true, CreatedAt: e.clock, UpdatedAt: e.clock, Config: map[string]any{}}
	sources.NormalizeManual(&man)
	e.must(e.st.CreateSource(e.ctx, man))
	p := model.ManualPerson{ID: "p1", SourceID: man.ID, ExternalID: "C-ABC123", CreatedAt: e.clock, UpdatedAt: e.clock,
		Attrs: map[string]string{"FirstName": "Jane", "LastName": "Doe", "Department": "Marketing"}}
	e.must(e.st.CreateManualPerson(e.ctx, p))
	e.writeCSV() // HR does not know her yet
	e.run()
	contractor := e.user("C-ABC123")
	e.fake.Mutate("C-ABC123", func(u *ad.FakeUser) {
		u.Sam, u.UPN = "c-jdoe", "c-jdoe@corp.example.com"
		u.Attrs["givenName"], u.Attrs["sn"], u.Attrs["department"] = "Jane", "Doe", "Marketing"
	})

	e.correlation(nil)
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""))
	run := e.run()
	eq(t, "link applied in the same run", run.Counters["applied"] >= 1, true)
	eq(t, "no second account", run.Counters["planned_create"], 0)

	u := e.user("E1")
	eq(t, "same account", u.GUID, contractor.GUID)
	eq(t, "renamed to the employee naming", u.Sam, "jdoe")
	eq(t, "upn renamed", u.UPN, "jane.doe@corp.example.com")

	ids, err := e.st.ListIdentitiesBySource(e.ctx, man.ID)
	e.must(err)
	eq(t, "contractor record merged", ids[0].State, model.StateMerged)
	eq(t, "merged into the employee", ids[0].MergedInto, e.identity("E1").ID)
	merges, _, _ := e.st.ListAudit(e.ctx, store.AuditFilter{Action: "identity.merge"}, store.Page{})
	eq(t, "merge audited", len(merges), 1)

	// Ending the contractor entry later must not touch the account the employee owns now.
	p.Ended = true
	e.must(e.st.UpdateManualPerson(e.ctx, p))
	run = e.run()
	eq(t, "no disable", run.Counters["planned_disable"], 0)
	eq(t, "still enabled", e.user("E1").Enabled, true)
}

func TestManualSourceEndDateTerminates(t *testing.T) {
	e := newEnv(t)
	e.tweak(func(s *model.Settings) { s.Approval.Create = "auto" })
	man := model.Source{ID: "src-man", Name: "Contractors", Type: sources.TypeManual, Enabled: true, CreatedAt: e.clock, UpdatedAt: e.clock, Config: map[string]any{}}
	sources.NormalizeManual(&man)
	e.must(e.st.CreateSource(e.ctx, man))
	e.must(e.st.CreateManualPerson(e.ctx, model.ManualPerson{ID: "p1", SourceID: man.ID, ExternalID: "C-1", CreatedAt: e.clock, UpdatedAt: e.clock,
		Attrs: map[string]string{"FirstName": "Old", "LastName": "Contract", "Department": "IT", "EndDate": "2020-01-31"}}))
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""))
	run := e.run()
	eq(t, "only the HR person is created", run.Counters["planned_create"], 1)
	ids, err := e.st.ListIdentitiesBySource(e.ctx, man.ID)
	e.must(err)
	eq(t, "past end date = terminated", ids[0].Active, false)
}

func TestCorrelationReportListsAndIgnoresAccounts(t *testing.T) {
	e := newEnv(t)
	n := &recordingNotifier{}
	e.eng.Notifier = n
	e.correlation(nil)
	e.fake.AddUser(ad.FakeUser{DN: "CN=kiosk01," + baseOU, Sam: "kiosk01", UPN: "kiosk01@corp.example.com", Enabled: true})
	e.hrAccount("jdoe", "Jane", "Doe", "Marketing")
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""))
	e.run()

	var rep CorrelationReport
	_, err := e.st.GetState(e.ctx, ReportState, &rep)
	e.must(err)
	if len(rep.Uncorrelated) != 1 || rep.Uncorrelated[0].Sam != "kiosk01" {
		t.Fatalf("the kiosk account matches nobody: %+v", rep.Uncorrelated)
	}
	if got := n.events(model.EventIdentityIssues); len(got) != 1 || !strings.Contains(strings.Join(got[0].Lines, "\n"), "kiosk01") {
		t.Fatalf("new uncorrelated accounts are notified: %+v", got)
	}
	e.run()
	eq(t, "an account already reported is not notified again", len(n.events(model.EventIdentityIssues)), 1)

	e.correlation(func(c *model.CorrelationSettings) { c.IgnoreAccounts = []string{"kiosk*"} })
	e.run()
	_, err = e.st.GetState(e.ctx, ReportState, &rep)
	e.must(err)
	eq(t, "ignored", len(rep.Uncorrelated), 0)
}

func TestLinkGuardHoldsBulkAutoLinks(t *testing.T) {
	e := newEnv(t)
	e.correlation(func(c *model.CorrelationSettings) { c.MaxAutoLinksPerRun = 1 })
	e.hrAccount("jdoe", "Jane", "Doe", "Marketing")
	e.hrAccount("jsmith", "John", "Smith", "Sales")
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""), row("E2", "John", "Smith", "Sales", "Rep", "A", ""))
	run := e.run()
	eq(t, "guard tripped", run.Counters["link_guard_tripped"], 1)
	eq(t, "both held", len(e.changes(model.StatusPending, model.ChangeLink)), 2)
}

func TestCompareAndIgnore(t *testing.T) {
	for _, tc := range []struct {
		kind, a, b string
		want       bool
	}{
		{model.CompareName, "O'Brien", "obrien", true},
		{model.CompareName, "José", "Jose", true},
		{model.CompareName, "Anne-Marie", "Anne Marie", true},
		{model.CompareCI, "Human  Resources", "human resources", true},
		{model.CompareCI, "HR", "Human Resources", false},
		{model.CompareExact, "Sales", "sales", false},
		{model.CompareExact, " Sales", "Sales", true},
		{model.CompareCI, "", "", false},
	} {
		if got := CompareValues(tc.kind, tc.a, tc.b); got != tc.want {
			t.Errorf("%s %q vs %q: got %v", tc.kind, tc.a, tc.b, got)
		}
	}
	ign := []string{"svc_*", "OU=Service,DC=corp,DC=example,DC=com"}
	eq(t, "sam wildcard", IgnoredAccount(ign, "SVC_backup", "CN=x,OU=Staff,DC=corp,DC=example,DC=com"), true)
	eq(t, "dn suffix", IgnoredAccount(ign, "app1", "CN=app1,OU=Service,DC=corp,DC=example,DC=com"), true)
	eq(t, "other", IgnoredAccount(ign, "jdoe", "CN=jdoe,OU=Staff,DC=corp,DC=example,DC=com"), false)
}

func TestCorrelationNeverMatchesPrivilegedAccounts(t *testing.T) {
	e := newEnv(t)
	e.correlation(nil)
	e.fake.AddUser(ad.FakeUser{DN: "CN=Jane Doe (admin)," + baseOU, Sam: "adm-jdoe", UPN: "adm-jdoe@corp.example.com", Enabled: true,
		Attrs: map[string]string{"givenName": "Jane", "sn": "Doe", "department": "Marketing", "adminCount": "1"}})
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""))
	run := e.run()
	eq(t, "no link to an admin account", run.Counters["planned_link"], 0)
	eq(t, "a create instead", run.Counters["planned_create"], 1)
	var rep CorrelationReport
	_, err := e.st.GetState(e.ctx, ReportState, &rep)
	e.must(err)
	eq(t, "admin accounts are not reported", len(rep.Uncorrelated), 0)
}
