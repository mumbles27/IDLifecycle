package store

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// AuditQueryHelp describes the search syntax ParseAuditQuery understands (shown in the GUI).
const AuditQueryHelp = `words or "a phrase"   must appear (actor, action, entity or detail)
-word                  must not appear
actor:alice            who did it (repeat for any of several); actor:system for the service itself
action:change.*        what happened; * at the end matches a prefix (change.failed, auth.login_failed ...)
type:change  id:<id>   the entity it concerns
level:error            failures only (failed / denied / rejected / blocked, or an error in the detail)
since:7d  since:2026-10-01   after:  before:2026-10-25T08:00   time range (local time; d h m units)`

// ParseAuditQuery turns a search box query into an AuditFilter. now and loc resolve relative times ("since:24h")
// and dates given without a zone.
func ParseAuditQuery(q string, now time.Time, loc *time.Location) (AuditFilter, error) {
	var f AuditFilter
	if loc == nil {
		loc = time.Local
	}
	for _, tok := range tokenize(q) {
		neg := false
		if strings.HasPrefix(tok.text, "-") && len(tok.text) > 1 && !tok.quoted {
			neg, tok.text = true, tok.text[1:]
		}
		key, val, hasKey := "", tok.text, false
		if !tok.quoted {
			if i := strings.IndexByte(tok.text, ':'); i > 0 {
				key, val, hasKey = strings.ToLower(tok.text[:i]), strings.Trim(tok.text[i+1:], `"`), true
			}
		}
		if !hasKey {
			if neg {
				f.NotText = append(f.NotText, val)
			} else {
				f.Text = append(f.Text, val)
			}
			continue
		}
		if neg {
			return f, fmt.Errorf("-%s: only plain words can be excluded", key)
		}
		if val == "" {
			return f, fmt.Errorf("%s: needs a value", key)
		}
		switch key {
		case "actor", "user", "by":
			f.Actors = append(f.Actors, val)
		case "action", "event":
			f.Actions = append(f.Actions, strings.ToLower(val))
		case "type", "entity":
			f.EntityType = strings.ToLower(val)
		case "id":
			f.EntityID = val
		case "level", "is":
			switch strings.ToLower(val) {
			case "error", "errors", "failed", "failure":
				f.ErrorsOnly = true
			default:
				return f, fmt.Errorf("%s:%s: use level:error", key, val)
			}
		case "since", "after", "from":
			t, err := parseWhen(val, now, loc)
			if err != nil {
				return f, fmt.Errorf("%s: %w", key, err)
			}
			f.From = t
		case "before", "until", "to":
			t, err := parseWhen(val, now, loc)
			if err != nil {
				return f, fmt.Errorf("%s: %w", key, err)
			}
			f.To = t
		default:
			return f, fmt.Errorf("unknown filter %q (use actor:, action:, type:, id:, level:, since:, before:, or plain words)", key+":")
		}
	}
	if !f.From.IsZero() && !f.To.IsZero() && !f.From.Before(f.To) {
		return f, fmt.Errorf("since/after must be earlier than before/until")
	}
	return f, nil
}

type token struct {
	text   string
	quoted bool
}

// tokenize splits on spaces, keeping "quoted phrases" (also as key:"a value") together.
func tokenize(q string) []token {
	var out []token
	var b strings.Builder
	inQuote, quotedWhole := false, false
	flush := func() {
		if b.Len() > 0 {
			out = append(out, token{text: b.String(), quoted: quotedWhole})
		}
		b.Reset()
		quotedWhole = false
	}
	for _, r := range q {
		switch {
		case r == '"':
			if !inQuote && b.Len() == 0 {
				quotedWhole = true
			} else if !quotedWhole && !inQuote {
				b.WriteRune(r) // key:"value" - the quote stays so the value keeps its spaces
			}
			inQuote = !inQuote
		case unicode.IsSpace(r) && !inQuote:
			flush()
		default:
			b.WriteRune(r)
		}
	}
	flush()
	return out
}

// parseWhen reads 7d / 24h / 30m (ago), 2026-10-25, 2026-10-25T08:00 (local) or RFC 3339.
func parseWhen(v string, now time.Time, loc *time.Location) (time.Time, error) {
	if n := len(v); n >= 2 {
		if num, err := strconv.Atoi(v[:n-1]); err == nil && num >= 0 {
			switch v[n-1] {
			case 'd':
				return now.Add(-time.Duration(num) * 24 * time.Hour), nil
			case 'h':
				return now.Add(-time.Duration(num) * time.Hour), nil
			case 'm':
				return now.Add(-time.Duration(num) * time.Minute), nil
			}
		}
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, v, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("%q is not a time (use 7d, 24h, 2026-10-25 or 2026-10-25T08:00)", v)
}
