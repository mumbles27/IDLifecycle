package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"idlifecycle/internal/model"
)

type recordingNotifier struct {
	mu   sync.Mutex
	sent []model.Notification
}

func (r *recordingNotifier) Notify(_ context.Context, n model.Notification) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = append(r.sent, n)
}

func (r *recordingNotifier) events(kind string) []model.Notification {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []model.Notification
	for _, n := range r.sent {
		if n.Event == kind {
			out = append(out, n)
		}
	}
	return out
}

func countCalls(e *env, prefix string) []string {
	var out []string
	for _, c := range e.fake.CallLog() {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func TestExecuteSplitsLargeBatchesIntoChunks(t *testing.T) {
	old := ExecuteChunk
	ExecuteChunk = 2
	defer func() { ExecuteChunk = old }()

	e := newEnv(t)
	e.writeCSV(
		row("E1", "Ann", "One", "Eng", "Dev", "A", ""), row("E2", "Ben", "Two", "Eng", "Dev", "A", ""),
		row("E3", "Cat", "Three", "Eng", "Dev", "A", ""), row("E4", "Dan", "Four", "Eng", "Dev", "A", ""),
		row("E5", "Eve", "Five", "Eng", "Dev", "A", ""),
	)
	e.run()
	eq(t, "all created", e.approveAll(model.ChangeCreate), 5)
	calls := countCalls(e, "create(")
	eq(t, "three PowerShell invocations", strings.Join(calls, " "), "create(n=2,dry=false) create(n=2,dry=false) create(n=1,dry=false)")
	eq(t, "five accounts", len(e.fake.Users), 5)
}

// threeDeletesDue provisions three people, terminates and disables them, then moves the clock past retention.
func threeDeletesDue(e *env) {
	e.t.Helper()
	e.writeCSV(row("E1", "Ann", "One", "Eng", "Dev", "A", ""), row("E2", "Ben", "Two", "Eng", "Dev", "A", ""), row("E3", "Cat", "Three", "Eng", "Dev", "A", ""))
	e.run()
	eq(e.t, "created", e.approveAll(model.ChangeCreate), 3)
	e.writeCSV(row("E1", "Ann", "One", "Eng", "Dev", "T", ""), row("E2", "Ben", "Two", "Eng", "Dev", "T", ""), row("E3", "Cat", "Three", "Eng", "Dev", "T", ""))
	e.run()
	eq(e.t, "disabled", e.approveAll(model.ChangeDisable), 3)
	e.clock = e.clock.Add(31 * 24 * time.Hour)
}

func TestMaxDeletesPerRunHoldsAutoDeletes(t *testing.T) {
	e := newEnv(t)
	n := &recordingNotifier{}
	e.eng.Notifier = n
	e.tweak(func(s *model.Settings) {
		s.Approval.Delete = "auto"
		s.Engine.MaxDeletesPerRun = 2
		s.Engine.MaxDeletesPerDay = 0
	})
	threeDeletesDue(e)

	run := e.run()
	eq(t, "planned deletes", run.Counters["planned_delete"], 3)
	eq(t, "delete guard tripped", run.Counters["delete_guard_tripped"], 1)
	eq(t, "none auto-approved", len(e.changes(model.StatusApplied, model.ChangeDelete)), 0)
	pend := e.changes(model.StatusPending, model.ChangeDelete)
	eq(t, "all held", len(pend), 3)
	if !strings.Contains(pend[0].Summary[0], "HELD") || !strings.Contains(pend[0].Summary[0], "limit of 2") {
		t.Errorf("summary should explain the hold: %v", pend[0].Summary)
	}
	if len(n.events(model.EventThreshold)) == 0 || len(n.events(model.EventApprovalsPending)) == 0 {
		t.Errorf("expected threshold + approvals notifications, got %+v", n.sent)
	}

	// At or under the limit, auto approval works again.
	e.tweak(func(s *model.Settings) { s.Engine.MaxDeletesPerRun = 3 })
	for _, c := range pend {
		e.must(e.st.TransitionChange(e.ctx, c.ID, model.StatusPending, model.StatusRejected, "t", "reset"))
	}
	run = e.run()
	eq(t, "not tripped", run.Counters["delete_guard_tripped"], 0)
	eq(t, "auto-applied", run.Counters["applied"], 3)
}

func TestMaxDeletesPerDayDefersTheRest(t *testing.T) {
	e := newEnv(t)
	n := &recordingNotifier{}
	e.eng.Notifier = n
	e.tweak(func(s *model.Settings) {
		s.Engine.MaxDeletesPerRun = 0
		s.Engine.MaxDeletesPerDay = 2
	})
	threeDeletesDue(e)
	e.run()
	eq(t, "approved and run", e.approveAll(model.ChangeDelete), 3)

	eq(t, "two deleted today", len(e.changes(model.StatusApplied, model.ChangeDelete)), 2)
	left := e.changes(model.StatusApproved, model.ChangeDelete)
	eq(t, "one deferred, still approved", len(left), 1)
	eq(t, "AD keeps the deferred account", len(e.fake.Users), 1)
	eq(t, "one threshold notification", len(n.events(model.EventThreshold)), 1)

	// The worker retries every 30s: no repeated mail while the backlog is unchanged.
	if _, err := e.eng.Execute(e.ctx); err != nil {
		t.Fatal(err)
	}
	eq(t, "still deferred", len(e.changes(model.StatusApproved, model.ChangeDelete)), 1)
	eq(t, "no duplicate notification", len(n.events(model.EventThreshold)), 1)

	// Raising the limit releases it.
	e.tweak(func(s *model.Settings) { s.Engine.MaxDeletesPerDay = 3 })
	if _, err := e.eng.Execute(e.ctx); err != nil {
		t.Fatal(err)
	}
	eq(t, "all deleted", len(e.changes(model.StatusApplied, model.ChangeDelete)), 3)
}

func TestApprovedChangeIsSupersededWhenItsPlanDisappears(t *testing.T) {
	e := newEnv(t)
	e.provision()

	// Jane is terminated, an approver signs off, but the change has not run yet (e.g. dry-run was on).
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "T", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	e.tweak(func(s *model.Settings) { s.Engine.DryRun = true })
	e.run()
	e.tweak(func(s *model.Settings) { s.Engine.DryRun = false })
	e.run()
	pend := e.changes(model.StatusPending, model.ChangeDisable)
	eq(t, "disable proposed", len(pend), 1)
	if _, err := e.eng.Approve(e.ctx, pend[0].ID, "alice", "leaver"); err != nil {
		t.Fatal(err)
	}

	// Before it executes, HR reverses the termination (rehire / data correction).
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	run := e.run()
	eq(t, "stale approval superseded", run.Counters["superseded"] >= 1, true)
	c, err := e.st.GetChange(e.ctx, pend[0].ID)
	e.must(err)
	eq(t, "change status", c.Status, model.StatusSuperseded)
	eq(t, "Jane still enabled", e.user("E1").Enabled, true)
}

func TestNotificationsForFailuresAndRunErrors(t *testing.T) {
	e := newEnv(t)
	n := &recordingNotifier{}
	e.eng.Notifier = n
	e.tweak(func(s *model.Settings) { s.Approval.Create = "auto" })
	e.fake.FailExternal = map[string]string{"E2": "insufficient access rights"}
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	e.run()

	failed := n.events(model.EventChangesFailed)
	if len(failed) != 1 || !strings.Contains(strings.Join(failed[0].Lines, "\n"), "insufficient access rights") {
		t.Fatalf("changes_failed notification: %+v", failed)
	}
	if len(n.events(model.EventRunSummary)) != 1 {
		t.Errorf("expected one run summary, got %+v", n.sent)
	}

	e.writeCSV() // header only: refused as "everyone left"
	run := e.run()
	eq(t, "run error", run.Status, model.RunError)
	if rf := n.events(model.EventRunFailed); len(rf) != 1 || rf[0].RunID != run.ID {
		t.Errorf("run_failed notification: %+v", rf)
	}
}
