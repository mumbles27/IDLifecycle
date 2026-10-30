package sources

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"idlifecycle/internal/model"
)

// autoDateLayouts are the formats termination_date_format "auto" understands. Dates with a time keep their own
// calendar day (the time and zone are ignored), so "2026-10-30T23:00:00-05:00" is October 30.
var autoDateLayouts = []string{
	time.DateOnly, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02T15:04:05.999999999",
	"01/02/2006", "1/2/2006", "01/02/2006 15:04:05", "1/2/2006 15:04:05", "02.01.2006", "2.1.2006",
}

// ParseTerminationDate reads a termination date. format is "" or "auto" (the common formats above, plus an Excel
// serial day number) or a Go layout. The result is midnight UTC of that calendar day; ok is false for an empty or
// unreadable value.
func ParseTerminationDate(v, format string) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return time.Time{}, false
	}
	day := func(t time.Time) time.Time { return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC) }
	if f := strings.TrimSpace(format); f != "" && !strings.EqualFold(f, "auto") {
		t, err := time.Parse(f, v)
		if err != nil {
			return time.Time{}, false
		}
		return day(t), true
	}
	for _, l := range autoDateLayouts {
		if t, err := time.Parse(l, v); err == nil {
			return day(t), true
		}
	}
	// Excel stores dates as days since 1899-12-30; only plausible serials (1954-2119) are accepted.
	if n, err := strconv.ParseFloat(v, 64); err == nil && n >= 20000 && n <= 80000 {
		return day(time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC).AddDate(0, 0, int(n))), true
	}
	return time.Time{}, false
}

// TerminationPassed reports whether a person with this termination date counts as terminated at now: the day after
// date + afterDays has begun in loc.
func TerminationPassed(date time.Time, afterDays int, now time.Time, loc *time.Location) bool {
	if loc == nil {
		loc = time.UTC
	}
	l := now.In(loc)
	today := time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, time.UTC)
	return today.After(date.AddDate(0, 0, afterDays))
}

// LeaveDate returns a record's termination date as YYYY-MM-DD under the source's options ("" = none or unreadable).
func LeaveDate(opt model.SourceOptions, value string) string {
	if strings.TrimSpace(opt.TerminationDateField) == "" {
		return ""
	}
	d, ok := ParseTerminationDate(value, opt.TerminationDateFormat)
	if !ok {
		return ""
	}
	return d.Format(time.DateOnly)
}

// validateTermination checks a source's termination date options.
func validateTermination(opt model.SourceOptions) error {
	if opt.TerminationAfterDays < 0 || opt.TerminationAfterDays > 3650 {
		return fmt.Errorf("options.termination_after_days must be between 0 and 3650")
	}
	f := strings.TrimSpace(opt.TerminationDateFormat)
	if f == "" || strings.EqualFold(f, "auto") {
		return nil
	}
	if strings.TrimSpace(opt.TerminationDateField) == "" {
		return fmt.Errorf("options.termination_date_format needs options.termination_date_field")
	}
	// A usable layout round-trips a date with distinct year, month and day.
	ref := time.Date(2031, 11, 27, 0, 0, 0, 0, time.UTC)
	t, err := time.Parse(f, ref.Format(f))
	if err != nil || t.Year() != 2031 || t.Month() != 11 || t.Day() != 27 {
		return fmt.Errorf("options.termination_date_format %q is not a date layout (use auto, or a Go layout such as 2006-01-02 or 02/01/2006)", f)
	}
	return nil
}
