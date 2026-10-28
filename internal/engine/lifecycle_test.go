package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/model"
	"idlifecycle/internal/store"
)

const (
	gAllUsers  = "CN=ALLUSERS,OU=Groups,DC=corp,DC=example,DC=com"
	gDisabled  = "CN=Disabled Users,OU=Groups,DC=corp,DC=example,DC=com"
	gRetention = "CN=Mailbox-Retention,OU=Groups,DC=corp,DC=example,DC=com"
	gOther     = "CN=Other,OU=Groups,DC=corp,DC=example,DC=com"
)

func (e *env) lifecycleGroups() {
	e.fake.AddGroup("ALLUSERS", gAllUsers)
	e.fake.AddGroup("Disabled Users", gDisabled)
	e.fake.AddGroup("Mailbox-Retention", gRetention)
	e.fake.AddGroup("Other", gOther)
}

func groupsOf(u ad.FakeUser) string {
	out := []string{}
	for _, g := range u.MemberOf {
		out = append(out, shortCN(g))
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func TestActiveAddOnCreateUpdateAndEnable(t *testing.T) {
	e := newEnv(t)
	e.lifecycleGroups()
	e.tweak(func(s *model.Settings) { s.Lifecycle.ActiveAdd = []string{"ALLUSERS"} })
	e.provision()
	eq(t, "create adds active_add", groupsOf(e.user("E1")), "ALLUSERS,GG-All,GG-Mkt")

	// Someone removes the membership by hand: the next update puts it back.
	e.fake.Mutate("E2", func(u *ad.FakeUser) { removeFrom(u, gAllUsers) })
	e.run()
	e.approveAll(model.ChangeUpdate)
	eq(t, "update adds active_add", groupsOf(e.user("E2")), "ALLUSERS,GG-All")

	// remove_stale_groups: a group dropped from active_add is managed no more and stays (no surprise removals).
	e.tweak(func(s *model.Settings) { s.Lifecycle.ActiveAdd = []string{} })
	run := e.run()
	eq(t, "nothing to change", run.Counters["planned_update"], 0)
}

func removeFrom(u *ad.FakeUser, dn string) {
	keep := u.MemberOf[:0]
	for _, g := range u.MemberOf {
		if !strings.EqualFold(g, dn) {
			keep = append(keep, g)
		}
	}
	u.MemberOf = keep
}

func TestDisableRemovalModesKeepAndAddThenRehire(t *testing.T) {
	for _, tc := range []struct {
		mode, want string
	}{
		{model.DisabledRemoveAllExcept, "Disabled Users,Mailbox-Retention"},
		{model.DisabledRemoveManaged, "Disabled Users,Mailbox-Retention,Other"},
		{model.DisabledRemoveNone, "ALLUSERS,Disabled Users,GG-All,GG-Mkt,Mailbox-Retention,Other"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			e := newEnv(t)
			e.lifecycleGroups()
			e.tweak(func(s *model.Settings) {
				s.Lifecycle = model.LifecycleGroups{ActiveAdd: []string{"ALLUSERS"}, DisabledAdd: []string{"Disabled Users"},
					DisabledRemove: tc.mode, DisabledKeep: []string{"Mailbox-Retention"}}
			})
			e.provision()
			e.fake.Mutate("E1", func(u *ad.FakeUser) { u.MemberOf = append(u.MemberOf, gRetention, gOther) })

			e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "T", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
			e.run()
			d := e.changes(model.StatusPending, model.ChangeDisable)
			eq(t, "one disable", len(d), 1)
			eq(t, "disabled_add on disable", strings.Join(d[0].Payload.AddGroups, ","), gDisabled)
			eq(t, "approved", e.approveAll(model.ChangeDisable), 1)
			u := e.user("E1")
			eq(t, "disabled", u.Enabled, false)
			eq(t, "groups after disable", groupsOf(u), tc.want)

			// Rehire: the account is enabled again, leaves the disabled-only group and gets its groups back.
			e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
			e.run()
			en := e.changes(model.StatusPending, model.ChangeEnable)
			eq(t, "one enable", len(en), 1)
			eq(t, "enable removes disabled_add", strings.Join(en[0].Payload.RemoveGroups, ","), gDisabled)
			e.approveAll(model.ChangeEnable)
			u = e.user("E1")
			eq(t, "enabled", u.Enabled, true)
			if strings.Contains(groupsOf(u), "Disabled Users") || !strings.Contains(groupsOf(u), "ALLUSERS") || !strings.Contains(groupsOf(u), "GG-Mkt") {
				t.Errorf("groups after rehire: %s", groupsOf(u))
			}
		})
	}
}

func TestStripGroupsOnDisableMigratesToLifecycleMode(t *testing.T) {
	e := newEnv(t)
	s := e.settings()
	s.AD.StripGroupsOnDisable = false
	s.Lifecycle = model.LifecycleGroups{} // a document saved before lifecycle_groups existed
	e.must(e.st.SaveSettings(e.ctx, s, "test"))
	got := e.settings()
	eq(t, "strip false -> none", got.Lifecycle.DisabledRemove, model.DisabledRemoveNone)
	if got.Lifecycle.ActiveAdd == nil || got.Lifecycle.DisabledKeep == nil {
		t.Error("lists are never null")
	}
	s.AD.StripGroupsOnDisable = true
	e.must(e.st.SaveSettings(e.ctx, s, "test"))
	eq(t, "strip true -> all_except", e.settings().Lifecycle.DisabledRemove, model.DisabledRemoveAllExcept)

	// Saving through Validate keeps the old switch in step with the new mode.
	v := e.settings()
	v.Lifecycle.DisabledRemove = model.DisabledRemoveNone
	e.must(v.Validate())
	eq(t, "old switch follows", v.AD.StripGroupsOnDisable, false)
	v.Lifecycle.DisabledRemove = "sometimes"
	if err := v.Validate(); err == nil {
		t.Error("an unknown mode is refused")
	}
}

// ---- impact preview ------------------------------------------------------------------

func snapshotCalls(f *ad.Fake) int {
	n := 0
	for _, c := range f.CallLog() {
		if c == "snapshot" {
			n++
		}
	}
	return n
}

func (e *env) storeState() string {
	ids, err := e.st.ListIdentitiesBySource(e.ctx, e.src.ID)
	e.must(err)
	changes, total, err := e.st.ListChanges(e.ctx, store.ChangeFilter{}, store.Page{Limit: 500})
	e.must(err)
	var b strings.Builder
	fmt.Fprintf(&b, "changes=%d;", total)
	for _, c := range changes {
		fmt.Fprintf(&b, "%s/%s;", c.ID, c.Status)
	}
	for _, id := range ids {
		fmt.Fprintf(&b, "%s/%s/%s/%v;", id.ExternalID, id.State, id.ADGUID, id.Applied)
	}
	return b.String()
}

func TestImpactPreviewComparesProposedWithSavedAndWritesNothing(t *testing.T) {
	e := newEnv(t)
	e.lifecycleGroups()
	e.fake.AddGroup("GG-New", "CN=GG-New,OU=Groups,DC=corp,DC=example,DC=com")
	e.provision()
	e.run()
	e.approveAll(model.ChangeUpdate)
	beforeStore := e.storeState()
	beforeUsers := groupsOf(e.user("E1")) + "|" + groupsOf(e.user("E2"))
	beforeSnaps := snapshotCalls(e.fake)

	// What the next sync would do: nothing (converged), so nothing changes either.
	res, err := e.eng.PreviewImpact(e.ctx, ImpactRequest{OnlyChanged: false})
	e.must(err)
	eq(t, "people", res.People, 2)
	eq(t, "no differences", res.Summary.ChangedPeople, 0)
	eq(t, "the sync's snapshot is reused", snapshotCalls(e.fake), beforeSnaps)

	// A changed role: Marketing also gets GG-New.
	rules, err := e.st.ListRoles(e.ctx)
	e.must(err)
	for i := range rules {
		if rules[i].ID == "r-mkt" {
			rules[i].Grants.Groups = append(rules[i].Grants.Groups, "GG-New")
		}
	}
	res, err = e.eng.PreviewImpact(e.ctx, ImpactRequest{Roles: &rules, OnlyChanged: true})
	e.must(err)
	eq(t, "one person changes", res.Summary.ChangedPeople, 1)
	eq(t, "items", len(res.Items), 1)
	it := res.Items[0]
	eq(t, "who", it.ExternalID, "E1")
	eq(t, "new plan", it.Difference, "new")
	eq(t, "proposed type", it.Proposed.Type, model.ChangeUpdate)
	if len(it.Lines) != 1 || it.Lines[0].Op != "+" || it.Lines[0].Text != "Add to group GG-New" {
		t.Errorf("lines: %+v", it.Lines)
	}
	if len(res.Summary.GroupsAdded) != 1 || res.Summary.GroupsAdded[0].Group != "CN=GG-New,OU=Groups,DC=corp,DC=example,DC=com" || res.Summary.GroupsAdded[0].People != 1 {
		t.Errorf("groups_added: %+v", res.Summary.GroupsAdded)
	}
	eq(t, "a group the cached snapshot did not resolve needs a fresh one", snapshotCalls(e.fake), beforeSnaps+1)

	// A changed lifecycle setting: everyone joins ALLUSERS.
	s := e.settings()
	s.Lifecycle.ActiveAdd = []string{"ALLUSERS"}
	res, err = e.eng.PreviewImpact(e.ctx, ImpactRequest{Settings: &s, OnlyChanged: true})
	e.must(err)
	eq(t, "both change", res.Summary.ChangedPeople, 2)
	eq(t, "proposed updates", res.Summary.Proposed[model.ChangeUpdate], 2)
	if len(res.Summary.GroupsAdded) != 1 || res.Summary.GroupsAdded[0].People != 2 {
		t.Errorf("groups_added: %+v", res.Summary.GroupsAdded)
	}

	// A changed mapping: title gets a suffix.
	maps, err := e.st.ListMappings(e.ctx)
	e.must(err)
	for i := range maps {
		if maps[i].TargetAttr == "title" {
			maps[i].Template = "{{ .Title }} (HR)"
		}
	}
	res, err = e.eng.PreviewImpact(e.ctx, ImpactRequest{Mappings: &maps, OnlyChanged: true, Limit: 1, Offset: 1})
	e.must(err)
	eq(t, "total before paging", res.Total, 2)
	eq(t, "page", len(res.Items), 1)
	eq(t, "second by name", res.Items[0].Display, "Jane Doe")
	if len(res.Summary.Attributes) != 1 || res.Summary.Attributes[0].Attr != "title" || res.Summary.Attributes[0].People != 2 {
		t.Errorf("attributes: %+v", res.Summary.Attributes)
	}
	res, err = e.eng.PreviewImpact(e.ctx, ImpactRequest{Mappings: &maps, OnlyChanged: true, Q: "bob"})
	e.must(err)
	eq(t, "q filters", res.Total, 1)
	res, err = e.eng.PreviewImpact(e.ctx, ImpactRequest{Mappings: &maps, OnlyChanged: true, Type: model.ChangeDisable})
	e.must(err)
	eq(t, "type filters", res.Total, 0)

	// refresh forces a new snapshot.
	n := snapshotCalls(e.fake)
	_, err = e.eng.PreviewImpact(e.ctx, ImpactRequest{Refresh: true})
	e.must(err)
	eq(t, "refresh", snapshotCalls(e.fake), n+1)

	// Nothing was written: changes, identities and AD are as before.
	eq(t, "store unchanged", e.storeState(), beforeStore)
	eq(t, "AD unchanged", groupsOf(e.user("E1"))+"|"+groupsOf(e.user("E2")), beforeUsers)
	audits, _, err := e.st.ListAudit(e.ctx, store.AuditFilter{Action: "preview.impact"}, store.Page{})
	e.must(err)
	if len(audits) == 0 {
		t.Error("previews are audited")
	}
}

func TestImpactPreviewInvalidProposalAndBusy(t *testing.T) {
	e := newEnv(t)
	e.provision()
	bad := []model.RoleRule{{Name: "x", Match: "all", Conditions: []model.Condition{{Attr: "a", Op: "nope"}}}}
	if _, err := e.eng.PreviewImpact(e.ctx, ImpactRequest{Roles: &bad}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), `roles[0] (x): conditions[0].op "nope"`) {
		t.Fatalf("invalid proposal: %v", err)
	}

	e.eng.runMu.Lock()
	_, err := e.eng.PreviewImpact(e.ctx, ImpactRequest{})
	e.eng.runMu.Unlock()
	if !errors.Is(err, ErrRunning) {
		t.Fatalf("during a sync: %v", err)
	}
	e.eng.previewing.Store(true)
	_, err = e.eng.PreviewImpact(e.ctx, ImpactRequest{})
	e.eng.previewing.Store(false)
	if !errors.Is(err, ErrPreviewBusy) {
		t.Fatalf("during another preview: %v", err)
	}
	// A sync cannot start while a preview holds the run lock either.
	e.eng.runMu.Lock()
	_, _, err = e.eng.Start(context.Background(), "test", "t")
	e.eng.runMu.Unlock()
	if !errors.Is(err, ErrRunning) {
		t.Fatalf("sync during preview: %v", err)
	}
}

func TestImpactPreviewScalesToThousandsOfPeople(t *testing.T) {
	if testing.Short() {
		t.Skip("large preview")
	}
	e := newEnv(t)
	rows := make([]string, 0, 3000)
	for i := 0; i < 3000; i++ {
		dept := []string{"Marketing", "Engineering", "Sales"}[i%3]
		rows = append(rows, row(fmt.Sprintf("E%05d", i), fmt.Sprintf("First%d", i), fmt.Sprintf("Last%d", i), dept, "Staff", "A", ""))
	}
	e.writeCSV(rows...)
	e.tweak(func(s *model.Settings) { s.Naming.ConflictMode = model.ConflictNumber })
	e.run()
	rules, err := e.st.ListRoles(e.ctx)
	e.must(err)
	for i := range rules {
		rules[i].Grants.Groups = append(rules[i].Grants.Groups, "GG-Mkt")
	}
	start := time.Now()
	res, err := e.eng.PreviewImpact(e.ctx, ImpactRequest{Roles: &rules, OnlyChanged: true, Limit: 10})
	e.must(err)
	t.Logf("preview of %d people: %s", res.People, time.Since(start))
	if time.Since(start) > 10*time.Second {
		t.Errorf("preview too slow: %s", time.Since(start))
	}
	eq(t, "people", res.People, 3000)
	eq(t, "page", len(res.Items), 10)
	if res.Total == 0 {
		t.Error("the changed role shows up for many people")
	}
}
