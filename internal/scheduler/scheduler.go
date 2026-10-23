// Package scheduler triggers sync runs on the schedule stored in the database settings.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"idlifecycle/internal/engine"
	"idlifecycle/internal/model"
	"idlifecycle/internal/store"
)

// NextRun computes the first run strictly after from for the given schedule.
func NextRun(s model.ScheduleSettings, from time.Time) (time.Time, error) {
	loc, err := time.LoadLocation(orUTC(s.Timezone))
	if err != nil {
		return time.Time{}, fmt.Errorf("timezone: %w", err)
	}
	switch s.Kind {
	case "interval":
		if s.IntervalMinutes < 1 {
			return time.Time{}, errors.New("interval_minutes must be >= 1")
		}
		return from.Add(time.Duration(s.IntervalMinutes) * time.Minute), nil
	case "daily":
		t, err := time.Parse("15:04", s.DailyTime)
		if err != nil {
			return time.Time{}, fmt.Errorf("daily_time: %w", err)
		}
		local := from.In(loc)
		// time.Date normalizes across DST gaps, so a skipped hour still yields a valid instant.
		next := time.Date(local.Year(), local.Month(), local.Day(), t.Hour(), t.Minute(), 0, 0, loc)
		if !next.After(from) {
			next = time.Date(local.Year(), local.Month(), local.Day()+1, t.Hour(), t.Minute(), 0, 0, loc)
		}
		return next, nil
	case "cron":
		sched, err := cron.ParseStandard(s.CronExpr)
		if err != nil {
			return time.Time{}, fmt.Errorf("cron_expr: %w", err)
		}
		return sched.Next(from.In(loc)), nil
	}
	return time.Time{}, fmt.Errorf("unknown schedule kind %q", s.Kind)
}

func orUTC(tz string) string {
	if tz == "" {
		return "UTC"
	}
	return tz
}

// Scheduler runs Engine.Start according to settings, re-reading them on Reload.
type Scheduler struct {
	Store  *store.Store
	Engine *engine.Engine
	Log    *slog.Logger
	Now    func() time.Time
	// After returns a channel firing after d; replaceable in tests.
	After func(d time.Duration) <-chan time.Time

	reload chan struct{}
	mu     sync.Mutex
	next   time.Time
	has    bool
}

func New(st *store.Store, eng *engine.Engine, log *slog.Logger) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{Store: st, Engine: eng, Log: log, Now: time.Now, After: time.After, reload: make(chan struct{}, 1)}
}

// Reload tells the loop to re-read settings (call after settings are saved).
func (s *Scheduler) Reload() {
	select {
	case s.reload <- struct{}{}:
	default:
	}
}

// Next reports the upcoming run time, if one is scheduled.
func (s *Scheduler) Next() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.next, s.has
}

func (s *Scheduler) setNext(t time.Time, ok bool) {
	s.mu.Lock()
	s.next, s.has = t, ok
	s.mu.Unlock()
}

// Run blocks until ctx is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	for {
		settings, err := s.Store.GetSettings(ctx)
		if err != nil {
			s.Log.Error("scheduler: read settings", "error", err)
			if !s.wait(ctx, time.Minute) {
				return
			}
			continue
		}

		sc := settings.Schedule
		if !sc.Enabled {
			s.setNext(time.Time{}, false)
			select {
			case <-ctx.Done():
				return
			case <-s.reload:
				continue
			}
		}

		next, err := NextRun(sc, s.Now())
		if err != nil {
			s.Log.Error("scheduler: invalid schedule; waiting for a settings change", "error", err)
			s.setNext(time.Time{}, false)
			select {
			case <-ctx.Done():
				return
			case <-s.reload:
				continue
			}
		}
		s.setNext(next, true)
		s.Log.Info("scheduler: next sync", "at", next.Format(time.RFC3339))

		select {
		case <-ctx.Done():
			return
		case <-s.reload:
			continue
		case <-s.After(next.Sub(s.Now())):
		}

		if _, _, err := s.Engine.Start(ctx, "schedule", "scheduler"); err != nil {
			if errors.Is(err, engine.ErrRunning) {
				s.Log.Warn("scheduler: previous run still in progress; skipping this slot")
			} else {
				s.Log.Error("scheduler: could not start run", "error", err)
			}
		}
	}
}

// wait sleeps d or until ctx ends; it returns false when ctx ended.
func (s *Scheduler) wait(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-s.After(d):
		return true
	}
}
