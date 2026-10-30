package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"idlifecycle/internal/engine"
	"idlifecycle/internal/model"
	"idlifecycle/internal/sources"
	"idlifecycle/internal/store"
)

// One identity's history: its own audit rows, its changes' rows and its console (manual) record's rows, newest
// first, each with a plain-English title and lines built here so every client shows the same story.

const (
	historyDefaultLimit = 50
	historyMaxLimit     = 200
)

type historyItem struct {
	Seq        int64          `json:"seq"`
	TS         time.Time      `json:"ts"`
	Actor      string         `json:"actor"`
	Action     string         `json:"action"`
	EntityType string         `json:"entity_type"`
	EntityID   string         `json:"entity_id"`
	Kind       string         `json:"kind"` // "identity" | "change" | "console"
	Title      string         `json:"title"`
	Lines      []string       `json:"lines"`
	ChangeID   string         `json:"change_id,omitempty"`
	ChangeType string         `json:"change_type,omitempty"`
	Detail     map[string]any `json:"detail"`
}

func (s *Server) handleIdentityHistory(w http.ResponseWriter, r *http.Request) {
	id, err := s.Store.GetIdentity(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = historyDefaultLimit
	}
	if limit > historyMaxLimit {
		limit = historyMaxLimit
	}
	before, _ := strconv.ParseInt(q.Get("before_seq"), 10, 64)
	rows, err := s.Store.ListIdentityHistory(r.Context(), id.ID, id.SourceID, id.ExternalID, before, limit)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	changes := map[string]*model.Change{}
	items := make([]historyItem, 0, len(rows))
	for _, e := range rows {
		items = append(items, s.historyItem(r.Context(), e, changes))
	}
	var next int64
	if len(rows) == limit {
		next = rows[len(rows)-1].Seq
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_before_seq": next})
}

// change loads (once per request) the change a history row is about; nil when it is gone.
func (s *Server) historyChange(ctx context.Context, id string, cache map[string]*model.Change) *model.Change {
	if c, ok := cache[id]; ok {
		return c
	}
	c, err := s.Store.GetChange(ctx, id)
	if err != nil {
		cache[id] = nil
		return nil
	}
	cache[id] = &c
	return &c
}

func (s *Server) historyItem(ctx context.Context, e model.AuditEntry, cache map[string]*model.Change) historyItem {
	it := historyItem{Seq: e.Seq, TS: e.TS, Actor: e.Actor, Action: e.Action, EntityType: e.EntityType, EntityID: e.EntityID,
		Detail: e.Detail, Lines: []string{}}
	d := e.Detail
	switch e.EntityType {
	case "change":
		it.Kind, it.ChangeID = "change", e.EntityID
		c := s.historyChange(ctx, e.EntityID, cache)
		ctype := str(d, "type")
		if c != nil {
			ctype = c.Type
		}
		it.ChangeType = ctype
		it.Title, it.Lines = changeTitle(e, ctype, c)
	case "manual_person":
		it.Kind = "console"
		it.Title, it.Lines = manualTitle(e)
	default:
		it.Kind = "identity"
		it.Title, it.Lines = identityTitle(e)
	}
	if it.Lines == nil {
		it.Lines = []string{}
	}
	return it
}

// changeTypeLabel names a change type for people.
func changeTypeLabel(t string) string {
	switch t {
	case model.ChangeCreate:
		return "Account creation"
	case model.ChangeUpdate:
		return "Account update"
	case model.ChangeDisable:
		return "Disable"
	case model.ChangeEnable:
		return "Enable"
	case model.ChangeDelete:
		return "Account deletion"
	case model.ChangeLink:
		return "Link to an existing account"
	case model.ChangeCloudGroups:
		return "Microsoft 365 group change"
	case model.ChangeUndo:
		return "Undo"
	case "":
		return "Change"
	}
	return t
}

func changeTitle(e model.AuditEntry, ctype string, c *model.Change) (string, []string) {
	d := e.Detail
	label := changeTypeLabel(ctype)
	var summary []string
	if c != nil {
		summary = c.Summary
	}
	who := ""
	if e.Actor != "" && e.Actor != "system" {
		who = " by " + e.Actor
	}
	switch e.Action {
	case "change.create":
		switch str(d, "status") {
		case model.StatusSimulated:
			return label + " planned (dry run: simulated, never applied)", summary
		case model.StatusApproved:
			return label + " planned and approved automatically", summary
		}
		return label + " planned, waiting for approval", summary
	case "change.approve":
		return label + " approved" + who, noteLines(d)
	case "change.reject":
		return label + " rejected" + who, noteLines(d)
	case "change.retry":
		return label + " retried" + who, noteLines(d)
	case "change.supersede":
		return label + " no longer needed (replaced by a newer plan)", nil
	case "change.applied":
		lines := append([]string{}, summary...)
		switch ctype {
		case model.ChangeCreate:
			t := "Account created in AD"
			if sam := str(d, "sam"); sam != "" {
				t += " (" + sam + ")"
			}
			return t, lines
		case model.ChangeDelete:
			return "Account deleted from AD", lines
		case model.ChangeDisable:
			return "Account disabled in AD", lines
		case model.ChangeEnable:
			return "Account enabled in AD", lines
		case model.ChangeLink:
			return "Linked to an existing AD account", lines
		case model.ChangeUndo:
			return "Undo applied", lines
		}
		return label + " applied", lines
	case "change.failed":
		lines := []string{}
		if msg := firstNonEmptyStr(str(d, "error"), str(d, "message")); msg != "" {
			lines = append(lines, "Error: "+msg)
		}
		return label + " failed", append(lines, summary...)
	case "change.set_ou":
		return "OU chosen" + who, kvLines(d, "ou", "OU")
	case "change.set_name":
		return "Username chosen" + who, kvLines(d, "sam", "Username")
	case "change.set_link":
		if b, _ := d["create_new"].(bool); b {
			return "Decided" + who + ": create a new account", nil
		}
		return "Account to link chosen" + who, kvLines(d, "sam", "Account")
	case "change.undo_requested":
		lines := []string{}
		if reason := str(d, "reason"); reason != "" {
			lines = append(lines, "Reason: "+reason)
		}
		if f, _ := d["force"].(bool); f {
			lines = append(lines, "Later changes will be overwritten (confirmed)")
		}
		if items := strList(d["items"]); len(items) > 0 {
			lines = append(lines, "Only these parts: "+strings.Join(items, ", "))
		}
		if b := str(d, "batch"); b != "" {
			lines = append(lines, "Part of a run undo (batch "+b+")")
		}
		return "Undo of " + strings.ToLower(changeTypeLabel(str(d, "of_type"))) + " requested" + who, append(lines, summary...)
	case "change.undone":
		if items := strList(d["items"]); len(items) > 0 {
			return label + " partly undone", []string{"Parts undone: " + strings.Join(items, ", ")}
		}
		return label + " undone", nil
	}
	return label + ": " + strings.TrimPrefix(e.Action, "change."), summary
}

func identityTitle(e model.AuditEntry) (string, []string) {
	d := e.Detail
	src := str(d, "source")
	who := ""
	if e.Actor != "" && e.Actor != "system" {
		who = " by " + e.Actor
	}
	switch e.Action {
	case engine.ActFirstSeen:
		lines := fieldLines(d["fields"])
		if str(d, "origin") == engine.OriginManual {
			t := "Added in the console"
			if by := str(d, "created_by"); by != "" {
				t += " by " + by
			}
			if src != "" {
				t += " (" + src + ")"
			}
			return t, lines
		}
		return "First seen in " + orText(src, "a source"), lines
	case engine.ActSourceChanged:
		return "Record changed in " + orText(src, "the source"), changeLines(d["changes"])
	case engine.ActInactive:
		if str(d, "reason") == "missing" {
			return fmt.Sprintf("Marked inactive: missing from %s for %v syncs", orText(src, "the source"), d["missing_runs"]), nil
		}
		if str(d, "reason") == "termination_date" {
			return fmt.Sprintf("Marked inactive: termination date %s has passed (%s)", str(d, "termination_date"), str(d, "termination_date_field")), nil
		}
		t := "Marked inactive: terminated in " + orText(src, "the source")
		if f := str(d, "status_field"); f != "" {
			t += fmt.Sprintf(" (%s = %s)", f, str(d, "status"))
		}
		return t, nil
	case engine.ActReactivated:
		return "Active again in " + orText(src, "the source"), nil
	case engine.ActState:
		return stateTitle(d), accountLines(d, "ad_sam", "ad_dn", "ad_guid")
	case engine.ActAccountMissing:
		return "AD account no longer found (removed outside IDLifecycle)", lastKnownLines(d)
	case engine.ActAccountDeleted:
		lines := lastKnownLines(d)
		if attrs := fieldLines(d["attributes"]); len(attrs) > 0 {
			lines = append(lines, "Last values set by IDLifecycle:")
			lines = append(lines, attrs...)
		}
		return "Account deleted from AD (kept here as a record)", lines
	case "identity.merge":
		return "Record merged into another person's record (" + str(d, "new_external_id") + ")", nil
	case "identity.link_decision":
		return "Link decision" + who + ": " + orText(str(d, "decision"), "cleared"), nil
	case "identity.link_reject":
		return "Accounts marked as not this person" + who, nil
	case "identity.hold_added":
		lines := kvLines(d, "by", "By")
		if r := str(d, "reason"); r != "" {
			lines = append(lines, "Reason: "+r)
		}
		if ch := str(d, "change"); ch != "" {
			lines = append(lines, "Set by undo change "+ch)
		}
		return "Set to Manual: " + holdLabel(d), lines
	case engine.ActRetentionChanged:
		lines := []string{"Before: " + orText(str(d, "from_name"), "none")}
		if due := str(d, "delete_due"); due != "" {
			lines = append(lines, "Delete due: "+due)
		}
		return "Retention policy now " + str(d, "to_name") + " (" + str(d, "reason") + ")", lines
	case engine.ActRetentionSet:
		var lines []string
		if r := str(d, "reason"); r != "" {
			lines = append(lines, "Reason: "+r)
		}
		return "Retention set" + who + ": " + orText(str(d, "to_name"), "Automatic"), lines
	case "identity.hold_released":
		var lines []string
		if n := str(d, "note"); n != "" {
			lines = append(lines, "Note: "+n)
		}
		return "Set back to Auto" + who + ": " + holdLabel(d), lines
	}
	return e.Action, nil
}

func manualTitle(e model.AuditEntry) (string, []string) {
	d := e.Detail
	switch e.Action {
	case "manual.create":
		return "Added in the console by " + e.Actor, fieldLines(d["attrs"])
	case "manual.update":
		return "Edited in the console by " + e.Actor, changeLines(d["changes"])
	case "manual.end":
		return "Ended in the console by " + e.Actor, kvLines(d, "note", "Note")
	case "manual.delete":
		return "Removed from the console by " + e.Actor, nil
	}
	return e.Action, nil
}

func stateTitle(d map[string]any) string {
	from, to := str(d, "from"), str(d, "to")
	var t string
	switch {
	case to == model.StateLinked && (from == model.StateNew || from == ""):
		t = "Linked to AD account " + str(d, "ad_sam")
	case to == model.StateLinked && from == model.StateDisabled:
		t = "Account is enabled"
	case to == model.StateDisabled && from == model.StateNew:
		t = "Linked to disabled AD account " + str(d, "ad_sam")
	case to == model.StateDisabled:
		t = "Account is disabled"
	case to == model.StateNew:
		t = "No longer linked to an AD account"
	case to == model.StateDeleted:
		t = "Account deleted"
	default:
		t = fmt.Sprintf("State changed from %s to %s", orText(from, "(none)"), to)
	}
	if b, _ := d["observed"].(bool); b {
		t += " (seen in AD)"
	}
	return strings.TrimSpace(t)
}

func holdLabel(d map[string]any) string {
	t := str(d, "kind")
	switch t {
	case model.HoldState:
		t = "no disable, enable, create, link or delete"
	case model.HoldOU:
		t = "OU stays as it is"
	case model.HoldAll:
		t = "the whole person (nothing is changed automatically)"
	case model.HoldGroup, model.HoldCloudGroup:
		t = "group"
	}
	if k := str(d, "key"); k != "" {
		t += " " + k
	}
	if u := str(d, "until"); u != "" {
		if ts, err := time.Parse(time.RFC3339, u); err == nil {
			u = ts.Format("2006-01-02")
		}
		t += " until " + u
	}
	return t
}

func lastKnownLines(d map[string]any) []string {
	var out []string
	for _, k := range [][2]string{{"ad_sam", "Username"}, {"ad_upn", "UPN"}, {"ad_dn", "DN"}, {"ad_guid", "GUID"}} {
		if v := str(d, k[0]); v != "" {
			out = append(out, "Last known "+k[1]+": "+v)
		}
	}
	return out
}

func accountLines(d map[string]any, keys ...string) []string {
	labels := map[string]string{"ad_sam": "Username", "ad_dn": "DN", "ad_guid": "GUID"}
	var out []string
	for _, k := range keys {
		if v := str(d, k); v != "" {
			out = append(out, labels[k]+": "+v)
		}
	}
	return out
}

func noteLines(d map[string]any) []string { return kvLines(d, "note", "Note") }

func kvLines(d map[string]any, key, label string) []string {
	if v := str(d, key); v != "" {
		return []string{label + ": " + v}
	}
	return nil
}

// fieldLines renders a {field: value} map as "field: value", sorted.
func fieldLines(v any) []string {
	m, _ := v.(map[string]any)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+": "+valueText(m[k]))
	}
	return out
}

// changeLines renders {field: {from, to}} (or {changed: true}) as "field: old → new", sorted.
func changeLines(v any) []string {
	m, _ := v.(map[string]any)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		ch, _ := m[k].(map[string]any)
		if b, _ := ch["changed"].(bool); b {
			out = append(out, k+": changed (hidden)")
			continue
		}
		out = append(out, k+": "+valueText(ch["from"])+" → "+valueText(ch["to"]))
	}
	return out
}

func valueText(v any) string {
	switch x := v.(type) {
	case nil:
		return "(empty)"
	case string:
		if x == "" {
			return "(empty)"
		}
		return x
	}
	return fmt.Sprint(v)
}

func str(d map[string]any, k string) string {
	switch v := d[k].(type) {
	case string:
		return v
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func orText(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// identityExtra is what the identity detail adds about where the person came from and, once the account is gone,
// what it was.
type identityExtra struct {
	Origin      string     `json:"origin"` // "source" | "manual"
	CreatedBy   string     `json:"created_by,omitempty"`
	ADLastKnown *lastKnown `json:"ad_last_known,omitempty"`
}

type lastKnown struct {
	GUID      string     `json:"guid"`
	DN        string     `json:"dn"`
	Sam       string     `json:"sam"`
	UPN       string     `json:"upn,omitempty"`
	Reason    string     `json:"reason"` // "deleted" (by IDLifecycle) | "missing" (removed outside it)
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

func (s *Server) identityExtra(ctx context.Context, id model.Identity) (identityExtra, error) {
	out := identityExtra{Origin: engine.OriginSource}
	src, err := s.Store.GetSource(ctx, id.SourceID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return out, err
	}
	if src.Type == sources.TypeManual {
		out.Origin = engine.OriginManual
		people, err := s.Store.ListManualPeople(ctx, src.ID)
		if err != nil {
			return out, err
		}
		for _, p := range people {
			if p.ExternalID == id.ExternalID {
				out.CreatedBy = p.CreatedBy
				break
			}
		}
		if out.CreatedBy == "" { // the console record was removed; its first history row still says who added it
			if ev, ok, err := s.Store.LastIdentityEvent(ctx, id.ID, engine.ActFirstSeen); err == nil && ok {
				out.CreatedBy = str(ev.Detail, "created_by")
			}
		}
	}
	if id.ADGUID != "" {
		return out, nil
	}
	ev, ok, err := s.Store.LastIdentityEvent(ctx, id.ID, engine.ActAccountDeleted, engine.ActAccountMissing, engine.ActState)
	if err != nil {
		return out, err
	}
	switch {
	case ok && ev.Action == engine.ActAccountDeleted:
		t := ev.TS
		out.ADLastKnown = &lastKnown{GUID: str(ev.Detail, "ad_guid"), DN: str(ev.Detail, "ad_dn"), Sam: str(ev.Detail, "ad_sam"),
			UPN: str(ev.Detail, "ad_upn"), Reason: "deleted", DeletedAt: &t}
	case ok && ev.Action == engine.ActAccountMissing:
		t := ev.TS
		out.ADLastKnown = &lastKnown{GUID: str(ev.Detail, "ad_guid"), DN: str(ev.Detail, "ad_dn"), Sam: str(ev.Detail, "ad_sam"),
			Reason: "missing", DeletedAt: &t}
	case id.State == model.StateDeleted:
		// Deleted before the history rows existed: the delete change itself still says which account it was.
		cs, _, err := s.Store.ListChanges(ctx, store.ChangeFilter{IdentityID: id.ID, Type: model.ChangeDelete, Status: model.StatusApplied}, store.Page{Limit: 1})
		if err != nil {
			return out, err
		}
		if len(cs) > 0 {
			out.ADLastKnown = &lastKnown{GUID: cs[0].Payload.ObjectGUID, DN: cs[0].Payload.DN, Reason: "deleted", DeletedAt: cs[0].AppliedAt}
		}
	}
	return out, nil
}

// strList reads a JSON list of strings from audit detail.
func strList(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		if s, ok := x.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}
