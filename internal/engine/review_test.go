package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/model"
	"idlifecycle/internal/store"
)

func TestWrongIDColumnDoesNotTerminateEveryone(t *testing.T) {
	e := newEnv(t)
	e.provision()
	e.src.Options.MissingMeansTerminated = true
	e.must(e.st.UpdateSource(e.ctx, e.src))
	// HR renamed the column: every row now has an empty EmployeeID.
	e.writeCSV(row("", "Jane", "Doe", "Marketing", "Designer", "A", ""), row("", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	run := e.run()
	eq(t, "flagged", run.Status, model.RunError)
	if !strings.Contains(run.Error, "external_id_field") {
		t.Errorf("error should point at the id field: %q", run.Error)
	}
	eq(t, "nothing proposed", len(e.changes(model.StatusPending, "")), 0)
	eq(t, "still active", e.identity("E1").Active, true)
}

func TestTruncatedFeedIsRejected(t *testing.T) {
	e := newEnv(t)
	e.tweak(func(s *model.Settings) { s.Approval.Create = "auto" })
	var all, some []string
	for i := 0; i < 12; i++ {
		r := row(fmt.Sprintf("E%02d", i), "Pat", fmt.Sprintf("Person%02d", i), "Engineering", "Dev", "A", "")
		all = append(all, r)
		if i < 4 {
			some = append(some, r)
		}
	}
	e.writeCSV(all...)
	e.run()
	e.src.Options.MissingMeansTerminated = true
	e.must(e.st.UpdateSource(e.ctx, e.src))

	e.writeCSV(some...) // 8 of 12 vanish at once: more likely a truncated export than 8 resignations
	run := e.run()
	eq(t, "flagged", run.Status, model.RunError)
	if !strings.Contains(run.Error, "truncated") {
		t.Errorf("error: %q", run.Error)
	}
	eq(t, "no disables", run.Counters["planned_disable"], 0)
	eq(t, "identities untouched", e.identity("E07").MissingRuns, 0)
}

func TestSameExternalIDInTwoSourcesIsNotGuessed(t *testing.T) {
	e := newEnv(t)
	e.provision()
	other := e.csv + ".2"
	e.must(writeFile(other, csvHeader+"\n"+row("E1", "Jane", "Doe", "Marketing", "Designer", "T", "")+"\n"))
	e.must(e.st.CreateSource(e.ctx, model.Source{ID: "src2", Name: "second", Type: "csv", Enabled: true, CreatedAt: e.clock, UpdatedAt: e.clock,
		Options: model.SourceOptions{ExternalIDField: "EmployeeID", StatusField: "Status", TerminatedValues: []string{"T"}}, Config: map[string]any{"path": other}}))
	run := e.run()
	eq(t, "plan error for the clash", run.Counters["plan_errors"] >= 1, true)
	eq(t, "no disable for an account another source says is active", run.Counters["planned_disable"], 0)
}

func TestRetentionClockIgnoresUnrelatedDisabling(t *testing.T) {
	e := newEnv(t)
	e.provision()
	// Helpdesk disables Jane (leave of absence) while HR still says active.
	e.fake.Mutate("E1", func(u *ad.FakeUser) { u.Enabled = false })
	e.clock = e.clock.Add(90 * 24 * time.Hour)
	e.run()
	eq(t, "re-enable is proposed, clock not started", e.identity("E1").DisabledAt == nil, true)
	e.must(e.eng.Store.UpdateSource(e.ctx, e.src))

	// She is then terminated: the retention period starts now, not 90 days ago.
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "T", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	run := e.run()
	eq(t, "no immediate delete", run.Counters["planned_delete"], 0)
	eq(t, "clock started at termination", e.identity("E1").DisabledAt != nil, true)
}

func TestPendingDisableSurvivesTheNextDay(t *testing.T) {
	e := newEnv(t)
	e.provision()
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "T", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	e.run()
	first := e.changes(model.StatusPending, model.ChangeDisable)[0]
	e.clock = e.clock.Add(24 * time.Hour)
	e.run()
	cur := e.changes(model.StatusPending, model.ChangeDisable)
	eq(t, "still one pending", len(cur), 1)
	eq(t, "same change (an approver may be looking at it)", cur[0].ID, first.ID)
	e.approveAll(model.ChangeDisable)
	eq(t, "description stamped at execution", strings.Contains(e.user("E1").Attrs["description"], "2026-03-02"), true)
}

func TestApprovedButStaleChangeIsReplaced(t *testing.T) {
	e := newEnv(t)
	e.provision()
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Lead", "A", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	e.run()
	old := e.changes(model.StatusPending, model.ChangeUpdate)[0]
	_, err := e.eng.Approve(e.ctx, old.ID, "alice", "")
	e.must(err)
	e.tweak(func(s *model.Settings) { s.Engine.DryRun = true }) // executor idles, so it stays 'approved'
	e.tweak(func(s *model.Settings) { s.Engine.DryRun = false })

	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Director", "A", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	e.eng.runMu.Lock() // plan only; skip the executor part of a full run
	run := model.Run{ID: "x", Counters: map[string]int{}}
	e.must(e.st.CreateRun(e.ctx, run))
	e.must(e.eng.sync(e.ctx, &run))
	e.eng.runMu.Unlock()

	got, _ := e.st.GetChange(e.ctx, old.ID)
	eq(t, "stale approval withdrawn", got.Status, model.StatusSuperseded)
	cur := e.changes(model.StatusPending, model.ChangeUpdate)
	eq(t, "fresh proposal awaits approval", len(cur), 1)
	eq(t, "with the new value", cur[0].Payload.Set["title"].To, "Director")
	eq(t, "AD untouched by the stale one", e.user("E1").Attrs["title"], "Designer")
}

func TestFailedChangeIsNotRepeatedEveryRun(t *testing.T) {
	e := newEnv(t)
	e.tweak(func(s *model.Settings) { s.Approval.Create = "auto" })
	e.fake.FailExternal["E1"] = "boom"
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""))
	e.run()
	e.run()
	e.run()
	eq(t, "one failed change, not three", len(e.changes(model.StatusFailed, "")), 1)
	eq(t, "attempted once", e.changes(model.StatusFailed, "")[0].Attempts, 1)
}

func TestRevertedSourceValueIsPushedAfterManualMatch(t *testing.T) {
	e := newEnv(t)
	e.provision()
	// Source says X; someone already set X in AD by hand, so nothing needs pushing.
	e.fake.Mutate("E1", func(u *ad.FakeUser) { u.Attrs["title"] = "Principal" })
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Principal", "A", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	run := e.run()
	eq(t, "nothing to push", run.Counters["planned_update"], 0)
	// Source goes back to the original while AD still shows X: that is a real change and must be pushed.
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	run = e.run()
	eq(t, "revert is proposed", run.Counters["planned_update"], 1)
}

func TestAuditChainIsKeyed(t *testing.T) {
	e := newEnv(t)
	e.st.SetAuditKey([]byte("k1-k1-k1-k1-k1-k1-k1-k1-k1-k1-k1"))
	_ = e.st.AppendAudit(e.ctx, "a", "x", "t", "1", nil)
	_ = e.st.AppendAudit(e.ctx, "a", "y", "t", "2", nil)
	if v, _ := e.st.VerifyAudit(e.ctx); !v.OK {
		t.Fatalf("verify with the right key: %+v", v)
	}
	e.st.SetAuditKey([]byte("someone-recomputing-without-the-key"))
	if v, _ := e.st.VerifyAudit(e.ctx); v.OK {
		t.Fatal("an attacker without the key must not be able to produce a chain that verifies")
	}
}

var _ = store.ErrNotFound
