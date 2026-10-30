package sources

import (
	"testing"
	"time"

	"idlifecycle/internal/model"
)

func TestParseTerminationDate(t *testing.T) {
	for _, c := range []struct{ v, format, want string }{
		{"2026-10-30", "", "2026-10-30"},
		{"2026-10-30T23:30:00-05:00", "auto", "2026-10-30"},
		{"2026-10-30T08:00:00Z", "", "2026-10-30"},
		{"2026-10-30 08:00:00", "", "2026-10-30"},
		{"10/30/2026", "", "2026-10-30"},
		{"3/1/2026", "", "2026-03-01"},
		{"30.10.2026", "", "2026-10-30"},
		{"46325", "", "2026-10-30"}, // Excel serial
		{"30/10/2026", "02/01/2006", "2026-10-30"},
		{"Oct 30 2026", "Jan 2 2006", "2026-10-30"},
		{"", "", ""},
		{"soon", "", ""},
		{"12", "", ""}, // not a plausible Excel date
		{"2026-10-30", "02/01/2006", ""},
	} {
		got := ""
		if d, ok := ParseTerminationDate(c.v, c.format); ok {
			got = d.Format(time.DateOnly)
		}
		if got != c.want {
			t.Errorf("ParseTerminationDate(%q, %q) = %q, want %q", c.v, c.format, got, c.want)
		}
	}
}

func TestTerminationPassedUsesTheLocalDayAndAfterDays(t *testing.T) {
	chi, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skip("no tz database")
	}
	date := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		now   time.Time
		after int
		want  bool
	}{
		{time.Date(2026, 3, 1, 23, 0, 0, 0, time.UTC), 0, false}, // still March 1 in Chicago
		{time.Date(2026, 3, 2, 4, 0, 0, 0, time.UTC), 0, false},  // March 1, 22:00 in Chicago
		{time.Date(2026, 3, 2, 7, 0, 0, 0, time.UTC), 0, true},   // March 2 in Chicago
		{time.Date(2026, 3, 3, 7, 0, 0, 0, time.UTC), 2, false},
		{time.Date(2026, 3, 4, 7, 0, 0, 0, time.UTC), 2, true},
	} {
		if got := TerminationPassed(date, c.after, c.now, chi); got != c.want {
			t.Errorf("now %s after %d: got %v", c.now, c.after, got)
		}
	}
}

func TestTerminationOptionsAreValidated(t *testing.T) {
	base := model.Source{Name: "hr", Type: "csv", Config: map[string]any{"path": "x.csv"}, Options: model.SourceOptions{ExternalIDField: "Id"}}
	old := PathPolicy
	PathPolicy = nil
	t.Cleanup(func() { PathPolicy = old })
	for _, c := range []struct {
		opt model.SourceOptions
		ok  bool
	}{
		{model.SourceOptions{ExternalIDField: "Id", TerminationDateField: "End"}, true},
		{model.SourceOptions{ExternalIDField: "Id", TerminationDateField: "End", TerminationDateFormat: "auto", TerminationAfterDays: 5}, true},
		{model.SourceOptions{ExternalIDField: "Id", TerminationDateField: "End", TerminationDateFormat: "02/01/2006"}, true},
		{model.SourceOptions{ExternalIDField: "Id", TerminationDateField: "End", TerminationDateFormat: "dd/mm/yyyy"}, false},
		{model.SourceOptions{ExternalIDField: "Id", TerminationDateFormat: "2006-01-02"}, false},
		{model.SourceOptions{ExternalIDField: "Id", TerminationDateField: "End", TerminationAfterDays: -1}, false},
	} {
		src := base
		src.Options = c.opt
		if err := Validate(src); (err == nil) != c.ok {
			t.Errorf("%+v: err %v", c.opt, err)
		}
	}
	if got := LeaveDate(model.SourceOptions{TerminationDateField: "End"}, "10/30/2026"); got != "2026-10-30" {
		t.Errorf("LeaveDate: %q", got)
	}
}
