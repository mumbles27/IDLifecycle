package engine

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/cloud"
	"idlifecycle/internal/model"
	"idlifecycle/internal/store"
)

// Cloud groups of the test tenant.
const (
	cgAll   = "a0000000-0000-0000-0000-000000000001" // security, everyone (active_add)
	cgSales = "a0000000-0000-0000-0000-000000000002" // Microsoft 365, Marketing role
	cgDL    = "a0000000-0000-0000-0000-000000000003" // distribution list, disabled accounts (disabled_add)
	cgKeep  = "a0000000-0000-0000-0000-000000000004" // mail-enabled security, never removed (disabled_keep)
	cgOther = "a0000000-0000-0000-0000-000000000005" // security, named by nothing
	cgAdmin = "a0000000-0000-0000-0000-000000000006" // role-assignable
)

func ref(id string) string { return model.CloudGroupPrefix + id }

// withCloud switches the Microsoft 365 connection on against a fake tenant.
func (e *env) withCloud() *cloud.Fake {
	e.t.Helper()
	f := cloud.NewFake()
	f.AddGroup(cgAll, "All Staff", cloud.KindSecurity)
	f.AddGroup(cgSales, "Sales Team", cloud.KindM365)
	f.AddGroup(cgDL, "Leavers", cloud.KindDistribution)
	f.AddGroup(cgKeep, "Mail Retention", cloud.KindMailSecurity)
	f.AddGroup(cgOther, "Project X", cloud.KindSecurity)
	f.AddGroup(cgAdmin, "Helpdesk Admins", cloud.KindSecurity).RoleAssignable = true
	e.eng.Cloud = f
	e.tweak(func(s *model.Settings) {
		s.Cloud = model.CloudSettings{Enabled: true, Environment: model.CloudGlobal, TenantID: "72f988bf-86f1-41af-91ab-2d7cd011db47",
			ClientID: "11111111-2222-3333-4444-555555555555", Auth: model.CloudAuthCertificate, CertThumbprint: strings.Repeat("AB", 20),
			UserMatch: model.CloudMatchSID, Exchange: model.CloudExchange{Enabled: true, Organization: "contoso.onmicrosoft.com"}}
		s.Approval.CloudGroups = "manual"
	})
	return f
}

// entraSync gives every AD account an Entra ID user (what Entra Connect does), matched by SID.
func (e *env) entraSync(f *cloud.Fake) {
	e.t.Helper()
	for _, ext := range []string{"E1", "E2"} {
		u := e.user(ext)
		f.AddUser(cloud.User{ID: "user-" + strings.ToLower(ext), UPN: u.UPN, SID: u.SID, Enabled: true})
	}
}

func (e *env) grantRole(id string, groups ...string) {
	e.t.Helper()
	r, err := e.st.GetRole(e.ctx, id)
	e.must(err)
	r.Grants.Groups = append(r.Grants.Groups, groups...)
	e.must(ValidateRole(r))
	e.must(e.st.UpdateRole(e.ctx, r))
}

func cloudMembers(f *cloud.Fake, user string) string {
	var out []string
	for id, g := range f.Groups {
		if g.Members[user] {
			out = append(out, f.Groups[id].Name)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func (e *env) oneCloud(status string) model.Change {
	e.t.Helper()
	list := e.changes(status, model.ChangeCloudGroups)
	if len(list) != 1 {
		e.t.Fatalf("want one %s cloud_groups change, got %d: %+v", status, len(list), list)
	}
	return list[0]
}

func TestCloudGroupsWaitForEntraSyncThenAddAndRemoveStale(t *testing.T) {
	e := newEnv(t)
	f := e.withCloud()
	e.grantRole("r-mkt", ref(cgSales))
	e.tweak(func(s *model.Settings) { s.Lifecycle.ActiveAdd = []string{"entra:" + strings.ToUpper(cgAll)} })
	eq(t, "references are stored lower-case", e.settings().Lifecycle.ActiveAdd[0], ref(cgAll))
	e.provision()

	// The accounts exist in AD but not yet in Entra ID: nothing to do in the cloud, no warning either.
	run := e.run()
	eq(t, "waiting for Entra sync", run.Counters["cloud_waiting"], 2)
	eq(t, "no cloud changes yet", len(e.changes("", model.ChangeCloudGroups)), 0)
	for _, c := range e.fake.CallLog() {
		if strings.Contains(c, "Group") && strings.Contains(c, "entra") {
			t.Fatalf("cloud references must never reach AD: %v", e.fake.CallLog())
		}
	}

	e.entraSync(f)
	run = e.run()
	eq(t, "cloud changes", run.Counters["planned_cloud_groups"], 2)
	pend := e.changes(model.StatusPending, model.ChangeCloudGroups)
	byExt := map[string]model.Change{}
	for _, c := range pend {
		byExt[c.ExternalID] = c
	}
	eq(t, "E1 adds", strings.Join(byExt["E1"].Payload.CloudAdd, ","), cgAll+","+cgSales)
	eq(t, "E2 adds", strings.Join(byExt["E2"].Payload.CloudAdd, ","), cgAll)
	eq(t, "summary", strings.Join(byExt["E1"].Summary, "|"), "Add to Entra group All Staff (security group)|Add to Entra group Sales Team (Microsoft 365 group)")
	eq(t, "the Entra user", byExt["E1"].Payload.CloudUserID, "user-e1")
	eq(t, "approved", e.approveAll(model.ChangeCloudGroups), 2)
	eq(t, "E1 in the cloud", cloudMembers(f, "user-e1"), "All Staff,Sales Team")
	eq(t, "applied", len(e.changes(model.StatusApplied, model.ChangeCloudGroups)), 2)

	// Converged: the next run plans nothing for the cloud.
	run = e.run()
	eq(t, "converged", run.Counters["planned_cloud_groups"], 0)

	// Jane moves to Engineering: the Marketing role's cloud group is stale and goes (remove_stale_groups).
	e.writeCSV(row("E1", "Jane", "Doe", "Engineering", "Designer", "A", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	e.run()
	c := e.oneCloud(model.StatusPending)
	eq(t, "stale removal", strings.Join(c.Payload.CloudRemove, ","), cgSales)
	e.approveAll(model.ChangeCloudGroups)
	eq(t, "E1 after the move", cloudMembers(f, "user-e1"), "All Staff")
}

func TestCloudDisableModesAndRehire(t *testing.T) {
	for _, tc := range []struct {
		mode, want string
	}{
		{model.DisabledRemoveAllExcept, "Helpdesk Admins,Leavers,Mail Retention"},
		{model.DisabledRemoveManaged, "Helpdesk Admins,Leavers,Mail Retention,Project X"},
		{model.DisabledRemoveNone, "All Staff,Helpdesk Admins,Leavers,Mail Retention,Project X,Sales Team"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			e := newEnv(t)
			f := e.withCloud()
			e.grantRole("r-mkt", ref(cgSales))
			e.tweak(func(s *model.Settings) {
				s.Lifecycle = model.LifecycleGroups{ActiveAdd: []string{ref(cgAll)}, DisabledAdd: []string{ref(cgDL)},
					DisabledRemove: tc.mode, DisabledKeep: []string{ref(cgKeep)}}
			})
			e.provision()
			e.entraSync(f)
			e.run()
			e.approveAll(model.ChangeCloudGroups)
			for _, g := range []string{cgKeep, cgOther, cgAdmin} {
				f.Groups[g].Members["user-e1"] = true
			}

			e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "T", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
			e.run()
			c := e.oneCloud(model.StatusPending)
			eq(t, "disabled_add", strings.Join(c.Payload.CloudAdd, ","), cgDL)
			eq(t, "remove all", c.Payload.CloudRemoveAll, tc.mode == model.DisabledRemoveAllExcept)
			e.approveAll(model.ChangeDisable)
			e.approveAll(model.ChangeCloudGroups)
			eq(t, "cloud groups after disable", cloudMembers(f, "user-e1"), tc.want)
			if tc.mode == model.DisabledRemoveAllExcept {
				eq(t, "keep", strings.Join(c.Payload.CloudKeep, ","), cgDL+","+cgKeep)
				eq(t, "removal recorded", e.identity("E1").Applied[cloudRemovedAllKey], "1")
			}

			// Disabled and converged: the next run plans nothing (the all-groups removal is not repeated).
			run := e.run()
			eq(t, "converged while disabled", run.Counters["planned_cloud_groups"], 0)

			// Rehire: the role and active_add groups come back, the disabled-only list goes.
			e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
			e.run()
			c = e.oneCloud(model.StatusPending)
			eq(t, "rehire removes disabled_add", strings.Join(c.Payload.CloudRemove, ","), cgDL)
			e.approveAll(model.ChangeEnable)
			e.approveAll(model.ChangeCloudGroups)
			got := cloudMembers(f, "user-e1")
			if strings.Contains(got, "Leavers") || !strings.Contains(got, "All Staff") || !strings.Contains(got, "Sales Team") {
				t.Errorf("cloud groups after rehire: %s", got)
			}
			if v := e.identity("E1").Applied[cloudRemovedAllKey]; v != "" {
				t.Errorf("the removal mark is cleared once the person is active again, got %q", v)
			}
		})
	}
}

func TestCloudGroupsThatCannotBeUsedAreWarnedAndSkipped(t *testing.T) {
	e := newEnv(t)
	f := e.withCloud()
	missing := "a0000000-0000-0000-0000-0000000000ff"
	f.AddGroup("a0000000-0000-0000-0000-000000000007", "Dynamic Staff", cloud.KindSecurity).Dynamic = true
	f.AddGroup("a0000000-0000-0000-0000-000000000008", "Synced Group", cloud.KindSecurity).Synced = true
	e.grantRole("r-all", ref(cgAdmin), ref(missing), ref("a0000000-0000-0000-0000-000000000007"), ref("a0000000-0000-0000-0000-000000000008"), ref(cgAll))
	e.provision()
	e.entraSync(f)
	run := e.run()
	if run.Counters["warnings"] < 4 {
		t.Errorf("each unusable group is warned about: %v", run.Counters)
	}
	for _, c := range e.changes(model.StatusPending, model.ChangeCloudGroups) {
		eq(t, "only the usable group "+c.ExternalID, strings.Join(c.Payload.CloudAdd, ","), cgAll)
	}

	// Exchange Online off: mail-enabled groups cannot be used either.
	f.Exchange = false
	e.grantRole("r-all", ref(cgKeep))
	run = e.run()
	for _, c := range e.changes(model.StatusPending, model.ChangeCloudGroups) {
		if strings.Contains(strings.Join(c.Payload.CloudAdd, ","), cgKeep) {
			t.Errorf("a mail-enabled group needs Exchange Online: %+v", c.Payload)
		}
	}
	if !strings.Contains(strings.ToLower(cloud.ReasonText(cloud.ReasonExchangeOff)), "exchange online") {
		t.Error("reason text")
	}
	if _, err := e.eng.PreviewImpact(e.ctx, ImpactRequest{}); err != nil {
		t.Fatal(err)
	}
}

func TestCloudSnapshotFailureLeavesADWorkAndOpenCloudChanges(t *testing.T) {
	e := newEnv(t)
	f := e.withCloud()
	e.tweak(func(s *model.Settings) { s.Lifecycle.ActiveAdd = []string{ref(cgAll)} })
	e.provision()
	e.entraSync(f)
	e.run()
	open := e.changes(model.StatusPending, model.ChangeCloudGroups)[0]

	f.SnapshotErr = errors.New("AADSTS7000215: invalid client secret")
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Lead Designer", "A", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	run := e.run()
	eq(t, "cloud error counted", run.Counters["cloud_errors"], 1)
	if !strings.Contains(run.Error, "Microsoft 365 groups") {
		t.Errorf("the run says why: %q", run.Error)
	}
	eq(t, "AD work goes on", len(e.changes(model.StatusPending, model.ChangeUpdate)), 1)
	still, err := e.st.GetChange(e.ctx, open.ID)
	e.must(err)
	eq(t, "the open cloud change is not superseded", still.Status, model.StatusPending)
}

func TestCloudConnectionOffSkipsCloudGroups(t *testing.T) {
	e := newEnv(t)
	f := e.withCloud()
	e.grantRole("r-all", ref(cgAll))
	e.tweak(func(s *model.Settings) { s.Cloud.Enabled = false })
	e.provision()
	e.entraSync(f)
	run := e.run()
	eq(t, "no cloud changes", run.Counters["planned_cloud_groups"], 0)
	for _, c := range f.Calls {
		if c == "snapshot" {
			t.Fatal("no cloud snapshot while the connection is off")
		}
	}
	if run.Counters["warnings"] == 0 {
		t.Error("configured cloud groups with the connection off are warned about")
	}
	// A cloud change approved before the connection was switched off fails with a reason instead of running.
	s := e.settings()
	_, err := e.eng.cloudConnection(s)
	if !errors.Is(err, ErrNoCloud) {
		t.Errorf("connection off: %v", err)
	}
}

func TestCloudExecutorDecryptsSecretAndNeverRunsInDryRun(t *testing.T) {
	e := newEnv(t)
	f := e.withCloud()
	const secret = "s3cr3t-Value~for.graph"
	enc, err := e.eng.Cipher.Encrypt(secret)
	e.must(err)
	e.tweak(func(s *model.Settings) {
		s.Cloud.Auth, s.Cloud.ClientSecret, s.Cloud.Exchange = model.CloudAuthSecret, enc, model.CloudExchange{}
		s.Lifecycle.ActiveAdd = []string{ref(cgAll)}
	})
	e.provision()
	e.entraSync(f)

	// Dry-run: proposals are simulated and nothing is sent to the cloud.
	e.tweak(func(s *model.Settings) { s.Engine.DryRun = true })
	run := e.run()
	eq(t, "simulated", len(e.changes(model.StatusSimulated, model.ChangeCloudGroups)), 2)
	_, err = e.eng.Execute(e.ctx)
	e.must(err)
	eq(t, "no membership change in dry-run", len(f.SetCalls), 0)
	_ = run

	e.tweak(func(s *model.Settings) { s.Engine.DryRun = false })
	e.run()
	eq(t, "approved", e.approveAll(model.ChangeCloudGroups), 2)
	eq(t, "one call", len(f.SetCalls), 1)
	eq(t, "the script gets the plain secret", f.LastConn.ClientSecret, secret)
	eq(t, "not a dry run", f.DryRuns[0], false)
	eq(t, "members", cloudMembers(f, "user-e2"), "All Staff")

	audit, _, err := e.st.ListAudit(e.ctx, store.AuditFilter{}, store.Page{Limit: 500})
	e.must(err)
	for _, a := range audit {
		if strings.Contains(fmt.Sprint(a.Detail), secret) {
			t.Fatalf("secret in the audit log: %+v", a)
		}
	}
	if strings.Contains(e.settings().Cloud.ClientSecret, secret) {
		t.Fatal("the stored secret is encrypted")
	}
}

func TestCloudUserMatching(t *testing.T) {
	guid := "33221100-5544-7766-8899-aabbccddeeff"
	eq(t, "immutable id", immutableIDFor(guid), "ABEiM0RVZneImaq7zN3u/w==")
	eq(t, "bad guid", immutableIDFor("guid-0001"), "")
	snap := &cloud.Snapshot{Users: []cloud.User{
		{ID: "u1", SID: "S-1-5-21-1-2-3-1001", UPN: "Jane.Doe@corp.example.com", ImmutableID: "ABEiM0RVZneImaq7zN3u/w=="},
	}}
	acct := &ad.User{ObjectGUID: guid, SID: "s-1-5-21-1-2-3-1001", UPN: "jane.doe@CORP.example.com"}
	for _, m := range []string{model.CloudMatchSID, model.CloudMatchImmutableID, model.CloudMatchUPN} {
		u := newCloudView(snap, m).userFor(acct)
		if u == nil || u.ID != "u1" {
			t.Errorf("%s: no match", m)
		}
	}
	other := &ad.User{ObjectGUID: "00000000-0000-0000-0000-000000000001", SID: "S-1-5-21-1-2-3-1002", UPN: "bob@corp.example.com"}
	for _, m := range []string{model.CloudMatchSID, model.CloudMatchImmutableID, model.CloudMatchUPN} {
		if newCloudView(snap, m).userFor(other) != nil {
			t.Errorf("%s: matched the wrong user", m)
		}
	}
	if newCloudView(snap, model.CloudMatchSID).userFor(&ad.User{}) != nil {
		t.Error("an account without a SID matches nobody")
	}
}

func TestPreviewImpactShowsCloudLines(t *testing.T) {
	e := newEnv(t)
	f := e.withCloud()
	e.provision()
	e.entraSync(f)
	e.run()
	e.approveAll(model.ChangeUpdate)

	rules, err := e.st.ListRoles(e.ctx)
	e.must(err)
	for i := range rules {
		if rules[i].ID == "r-mkt" {
			rules[i].Grants.Groups = append(rules[i].Grants.Groups, ref(cgSales))
		}
	}
	res, err := e.eng.PreviewImpact(e.ctx, ImpactRequest{Roles: &rules, OnlyChanged: true})
	e.must(err)
	eq(t, "one person", res.Summary.ChangedPeople, 1)
	it := res.Items[0]
	eq(t, "who", it.ExternalID, "E1")
	eq(t, "type", it.Proposed.Type, model.ChangeCloudGroups)
	if len(it.Lines) != 1 || it.Lines[0].Text != "Add to Entra group Sales Team (Microsoft 365 group)" {
		t.Errorf("lines: %+v", it.Lines)
	}
	if len(res.Summary.GroupsAdded) != 1 || res.Summary.GroupsAdded[0].Group != "Entra: Sales Team" {
		t.Errorf("groups_added: %+v", res.Summary.GroupsAdded)
	}
	eq(t, "counted", res.Summary.Proposed[model.ChangeCloudGroups], 1)
	res, err = e.eng.PreviewImpact(e.ctx, ImpactRequest{Roles: &rules, OnlyChanged: true, Type: model.ChangeCloudGroups})
	e.must(err)
	eq(t, "type filter", res.Total, 1)
	eq(t, "nothing written", len(f.SetCalls), 0)

	// The same person with an AD change too: one item, both summaries.
	s := e.settings()
	e.fake.AddGroup("GG-New", "CN=GG-New,OU=Groups,DC=corp,DC=example,DC=com")
	s.Lifecycle.ActiveAdd = []string{"GG-New"}
	for i := range rules {
		if rules[i].ID == "r-mkt" {
			rules[i].Grants.Groups = append(rules[i].Grants.Groups, ref(cgSales))
		}
	}
	res, err = e.eng.PreviewImpact(e.ctx, ImpactRequest{Roles: &rules, Settings: &s, OnlyChanged: true, Q: "jane"})
	e.must(err)
	it = res.Items[0]
	eq(t, "AD type first", it.Proposed.Type, model.ChangeUpdate)
	eq(t, "cloud too", strings.Join(it.Proposed.Also, ","), model.ChangeCloudGroups)
	eq(t, "both lines", len(it.Lines), 2)

	// A connection setting in the proposal is previewed with the saved values.
	s = e.settings()
	s.Cloud.TenantID = "contoso.onmicrosoft.com"
	res, err = e.eng.PreviewImpact(e.ctx, ImpactRequest{Settings: &s})
	e.must(err)
	if !strings.Contains(strings.Join(res.Notes, "|"), "Microsoft 365 connection is previewed with the saved values") {
		t.Errorf("notes: %v", res.Notes)
	}
}

func TestCloudRefsAreValidated(t *testing.T) {
	if err := ValidateRole(model.RoleRule{Name: "x", Match: "all", Grants: model.Grants{Groups: []string{"entra:not-a-guid"}}}); err == nil {
		t.Error("a malformed cloud reference is refused")
	}
	s := model.DefaultSettings()
	s.Lifecycle.ActiveAdd = []string{"entra:nope"}
	if err := s.Validate(); err == nil {
		t.Error("a malformed cloud reference in a lifecycle list is refused")
	}
	if got := adGroupsOnly([]string{"GG-All", ref(cgAll), "CN=X,DC=y"}); strings.Join(got, "|") != "GG-All|CN=X,DC=y" {
		t.Errorf("adGroupsOnly: %v", got)
	}
	cfg := planConfig{Settings: model.DefaultSettings(), Rules: []model.RoleRule{{Enabled: true, Grants: model.Grants{Groups: []string{ref(cgAll), "GG-All"}}}}}
	if req := snapshotRequestFor(cfg); strings.Join(req.Groups, "|") != "GG-All" {
		t.Errorf("cloud references never go into the AD snapshot request: %v", req.Groups)
	}
}
