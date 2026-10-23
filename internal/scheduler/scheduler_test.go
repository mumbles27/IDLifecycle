package scheduler

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/engine"
	"idlifecycle/internal/model"
	"idlifecycle/internal/secrets"
	"idlifecycle/internal/store"
	"idlifecycle/internal/store/storetest"
)

func at(t *testing.T, loc, v string) time.Time {
	t.Helper()
	l, err := time.LoadLocation(loc)
	if err != nil {
		t.Fatal(err)
	}
	tm, err := time.ParseInLocation("2006-01-02 15:04", v, l)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func TestNextRun(t *testing.T) {
	cases := []struct {
		name string
		s    model.ScheduleSettings
		from time.Time
		want time.Time
	}{
		{"interval", model.ScheduleSettings{Kind: "interval", IntervalMinutes: 90}, at(t, "UTC", "2026-03-01 10:00"), at(t, "UTC", "2026-03-01 11:30")},
		{"daily later today", model.ScheduleSettings{Kind: "daily", DailyTime: "14:30", Timezone: "America/Chicago"}, at(t, "America/Chicago", "2026-03-01 09:00"), at(t, "America/Chicago", "2026-03-01 14:30")},
		{"daily already passed", model.ScheduleSettings{Kind: "daily", DailyTime: "02:00", Timezone: "America/Chicago"}, at(t, "America/Chicago", "2026-03-01 09:00"), at(t, "America/Chicago", "2026-03-02 02:00")},
		{"daily exactly now moves to tomorrow", model.ScheduleSettings{Kind: "daily", DailyTime: "09:00", Timezone: "UTC"}, at(t, "UTC", "2026-03-01 09:00"), at(t, "UTC", "2026-03-02 09:00")},
		{"daily honours the time zone", model.ScheduleSettings{Kind: "daily", DailyTime: "02:00", Timezone: "Asia/Tokyo"}, at(t, "UTC", "2026-03-01 10:00"), at(t, "Asia/Tokyo", "2026-03-02 02:00")},
		{"cron every 4h", model.ScheduleSettings{Kind: "cron", CronExpr: "0 */4 * * *", Timezone: "UTC"}, at(t, "UTC", "2026-03-01 09:10"), at(t, "UTC", "2026-03-01 12:00")},
		{"cron weekdays 06:15 local", model.ScheduleSettings{Kind: "cron", CronExpr: "15 6 * * 1-5", Timezone: "America/Chicago"}, at(t, "America/Chicago", "2026-03-06 07:00"), at(t, "America/Chicago", "2026-03-09 06:15")},
	}
	for _, tc := range cases {
		got, err := NextRun(tc.s, tc.from)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if !got.Equal(tc.want) {
			t.Errorf("%s: got %s want %s", tc.name, got, tc.want)
		}
	}
}

func TestNextRunDaylightSavingGap(t *testing.T) {
	// 2026-03-08 02:30 does not exist in Chicago (clocks jump 02:00 -> 03:00). We still get one run that day.
	s := model.ScheduleSettings{Kind: "daily", DailyTime: "02:30", Timezone: "America/Chicago"}
	from := at(t, "America/Chicago", "2026-03-08 00:10")
	got, err := NextRun(s, from)
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation("America/Chicago")
	if got.In(loc).Day() != 8 || !got.After(from) || got.Sub(from) > 4*time.Hour {
		t.Errorf("DST gap handled badly: %s", got.In(loc))
	}
}

func TestNextRunErrors(t *testing.T) {
	for name, s := range map[string]model.ScheduleSettings{
		"kind":     {Kind: "weekly"},
		"interval": {Kind: "interval", IntervalMinutes: 0},
		"daily":    {Kind: "daily", DailyTime: "99:99"},
		"cron":     {Kind: "cron", CronExpr: "nope"},
		"tz":       {Kind: "interval", IntervalMinutes: 5, Timezone: "Mars/Olympus"},
	} {
		if _, err := NextRun(s, time.Now()); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestSchedulerFiresAndHonoursReload(t *testing.T) {
	st := storetest.New(t)
	c, _ := secrets.New(make([]byte, 32))
	eng := engine.New(st, ad.NewFake(), c, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// A real (empty) CSV source so a triggered run has something to do.
	csv := filepath.Join(t.TempDir(), "x.csv")
	_ = os.WriteFile(csv, []byte("EmployeeID,FirstName,LastName\nE1,A,B\n"), 0o600)
	ctx := context.Background()
	_ = st.CreateSource(ctx, model.Source{ID: "s", Name: "s", Type: "csv", Enabled: true, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		Options: model.SourceOptions{ExternalIDField: "EmployeeID"}, Config: map[string]any{"path": csv}})

	settings := model.DefaultSettings()
	settings.AD.SearchBases = []string{"OU=Staff,DC=x"}
	settings.Schedule = model.ScheduleSettings{Enabled: false, Kind: "interval", IntervalMinutes: 5, Timezone: "UTC"}
	_ = st.SaveSettings(ctx, settings, "t")

	fire := make(chan time.Time)
	var waits []time.Duration
	s := New(st, eng, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.After = func(d time.Duration) <-chan time.Time { waits = append(waits, d); return fire }

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { s.Run(runCtx); close(done) }()

	waitFor := func(what string, cond func() bool) {
		t.Helper()
		for i := 0; i < 400; i++ {
			if cond() {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", what)
	}

	// disabled: nothing scheduled
	time.Sleep(50 * time.Millisecond)
	if _, ok := s.Next(); ok {
		t.Fatal("disabled schedule must not report a next run")
	}

	// enabling through settings + Reload (what the API does) schedules the next run
	settings.Schedule.Enabled = true
	_ = st.SaveSettings(ctx, settings, "t")
	s.Reload()
	waitFor("next run to be scheduled", func() bool { _, ok := s.Next(); return ok })

	fire <- time.Now() // timer elapses -> run starts
	waitFor("a scheduled run to appear", func() bool {
		runs, total, _ := st.ListRuns(ctx, store.Page{Limit: 5})
		return total == 1 && runs[0].Trigger == "schedule" && runs[0].Status != model.RunRunning
	})

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("scheduler must stop when the context is cancelled")
	}
}
