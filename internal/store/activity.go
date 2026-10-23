package store

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"strconv"
	"time"

	"idlifecycle/internal/model"
)

// ---- identities ------------------------------------------------------------

const identityCols = `i.id, i.source_id, s.name, i.external_id, i.display, i.attrs_json, i.attrs_hash, i.active, i.missing_runs,
	i.first_seen, i.last_seen, i.ad_guid, i.ad_dn, i.ad_sam, i.applied_json, i.disabled_at, i.lifecycle_state`

const identityFrom = ` FROM identities i LEFT JOIN sources s ON s.id = i.source_id`

func scanIdentity(sc interface{ Scan(...any) error }) (model.Identity, error) {
	var x model.Identity
	var src sql.NullString
	var attrs, applied, first, last, dis string
	var active int
	err := sc.Scan(&x.ID, &x.SourceID, &src, &x.ExternalID, &x.Display, &attrs, &x.AttrsHash, &active, &x.MissingRuns,
		&first, &last, &x.ADGUID, &x.ADDN, &x.ADSam, &applied, &dis, &x.State)
	x.SourceName = src.String
	x.Active = active != 0
	x.Attrs = map[string]string{}
	fromJSON(attrs, &x.Attrs)
	x.Applied = map[string]string{}
	fromJSON(applied, &x.Applied)
	x.FirstSeen, x.LastSeen, x.DisabledAt = parseTS(first), parseTS(last), parseTSP(dis)
	return x, err
}

func (s *Store) InsertIdentity(ctx context.Context, x model.Identity) error {
	_, err := s.exec(ctx, `INSERT INTO identities (id, source_id, external_id, display, attrs_json, attrs_hash, active, missing_runs,
		first_seen, last_seen, ad_guid, ad_dn, ad_sam, applied_json, disabled_at, lifecycle_state) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		x.ID, x.SourceID, x.ExternalID, x.Display, toJSON(x.Attrs), x.AttrsHash, b2i(x.Active), x.MissingRuns,
		ts(x.FirstSeen), ts(x.LastSeen), x.ADGUID, x.ADDN, x.ADSam, toJSON(x.Applied), tsp(x.DisabledAt), x.State)
	if IsUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

func (s *Store) UpdateIdentity(ctx context.Context, x model.Identity) error {
	n, err := s.affected(ctx, `UPDATE identities SET display = ?, attrs_json = ?, attrs_hash = ?, active = ?, missing_runs = ?, last_seen = ?,
		ad_guid = ?, ad_dn = ?, ad_sam = ?, applied_json = ?, disabled_at = ?, lifecycle_state = ? WHERE id = ?`,
		x.Display, toJSON(x.Attrs), x.AttrsHash, b2i(x.Active), x.MissingRuns, ts(x.LastSeen),
		x.ADGUID, x.ADDN, x.ADSam, toJSON(x.Applied), tsp(x.DisabledAt), x.State, x.ID)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetIdentity(ctx context.Context, id string) (model.Identity, error) {
	x, err := scanIdentity(s.queryRow(ctx, `SELECT `+identityCols+identityFrom+` WHERE i.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return x, ErrNotFound
	}
	return x, err
}

// ListIdentitiesBySource returns every identity for a source (engine working set).
func (s *Store) ListIdentitiesBySource(ctx context.Context, sourceID string) ([]model.Identity, error) {
	rows, err := s.query(ctx, `SELECT `+identityCols+identityFrom+` WHERE i.source_id = ? ORDER BY i.external_id`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Identity{}
	for rows.Next() {
		x, err := scanIdentity(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

type IdentityFilter struct {
	Q     string
	State string
}

func (s *Store) ListIdentities(ctx context.Context, f IdentityFilter, p Page) ([]model.Identity, int, error) {
	w := &where{}
	if f.State != "" {
		w.add("i.lifecycle_state = ?", f.State)
	}
	w.like(f.Q, "i.external_id", "i.display", "i.ad_sam", "i.ad_dn")

	var total int
	if err := s.queryRow(ctx, `SELECT COUNT(*)`+identityFrom+w.sql(), w.args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	pg, pargs := s.pageClause(p)
	rows, err := s.query(ctx, `SELECT `+identityCols+identityFrom+w.sql()+` ORDER BY i.display, i.external_id`+pg, append(append([]any{}, w.args...), pargs...)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []model.Identity{}
	for rows.Next() {
		x, err := scanIdentity(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, x)
	}
	return out, total, rows.Err()
}

// ---- runs ------------------------------------------------------------------

const runCols = `id, trigger_kind, triggered_by, started_at, finished_at, status, counters_json, error`

func scanRun(sc interface{ Scan(...any) error }) (model.Run, error) {
	var r model.Run
	var st, fin, counters string
	err := sc.Scan(&r.ID, &r.Trigger, &r.TriggeredBy, &st, &fin, &r.Status, &counters, &r.Error)
	r.StartedAt, r.FinishedAt = parseTS(st), parseTSP(fin)
	r.Counters = map[string]int{}
	fromJSON(counters, &r.Counters)
	return r, err
}

func (s *Store) CreateRun(ctx context.Context, r model.Run) error {
	_, err := s.exec(ctx, `INSERT INTO sync_runs (`+runCols+`) VALUES (?,?,?,?,?,?,?,?)`,
		r.ID, r.Trigger, r.TriggeredBy, ts(r.StartedAt), tsp(r.FinishedAt), r.Status, toJSON(r.Counters), r.Error)
	return err
}

func (s *Store) UpdateRun(ctx context.Context, r model.Run) error {
	_, err := s.exec(ctx, `UPDATE sync_runs SET finished_at = ?, status = ?, counters_json = ?, error = ? WHERE id = ?`,
		tsp(r.FinishedAt), r.Status, toJSON(r.Counters), r.Error, r.ID)
	return err
}

func (s *Store) GetRun(ctx context.Context, id string) (model.Run, error) {
	r, err := scanRun(s.queryRow(ctx, `SELECT `+runCols+` FROM sync_runs WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

func (s *Store) ListRuns(ctx context.Context, p Page) ([]model.Run, int, error) {
	total, err := s.count(ctx, "sync_runs", &where{})
	if err != nil {
		return nil, 0, err
	}
	pg, pargs := s.pageClause(p)
	rows, err := s.query(ctx, `SELECT `+runCols+` FROM sync_runs ORDER BY started_at DESC, id`+pg, pargs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []model.Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// LatestRun returns the most recent run or ErrNotFound.
func (s *Store) LatestRun(ctx context.Context) (model.Run, error) {
	runs, _, err := s.ListRuns(ctx, Page{Limit: 1})
	if err != nil {
		return model.Run{}, err
	}
	if len(runs) == 0 {
		return model.Run{}, ErrNotFound
	}
	return runs[0], nil
}

// MarkStaleRunsAborted flags runs left in 'running' by a crashed process.
func (s *Store) MarkStaleRunsAborted(ctx context.Context) (int64, error) {
	return s.affected(ctx, `UPDATE sync_runs SET status = ?, finished_at = ?, error = ? WHERE status = ?`,
		model.RunAborted, ts(time.Now()), "service stopped while the run was in progress", model.RunRunning)
}

// ---- changes ---------------------------------------------------------------

const changeCols = `id, run_id, identity_id, external_id, display, change_type, status, payload_json, summary_json, payload_hash,
	created_at, decided_by, decided_at, decision_note, applied_at, error, attempts`

func scanChange(sc interface{ Scan(...any) error }) (model.Change, error) {
	var c model.Change
	var payload, summary, created, decided, applied string
	err := sc.Scan(&c.ID, &c.RunID, &c.IdentityID, &c.ExternalID, &c.Display, &c.Type, &c.Status, &payload, &summary, &c.PayloadHash,
		&created, &c.DecidedBy, &decided, &c.DecisionNote, &applied, &c.Error, &c.Attempts)
	fromJSON(payload, &c.Payload)
	c.Summary = []string{}
	fromJSON(summary, &c.Summary)
	c.CreatedAt, c.DecidedAt, c.AppliedAt = parseTS(created), parseTSP(decided), parseTSP(applied)
	return c, err
}

func (s *Store) InsertChange(ctx context.Context, c model.Change) error {
	_, err := s.exec(ctx, `INSERT INTO changes (`+changeCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.ID, c.RunID, c.IdentityID, c.ExternalID, c.Display, c.Type, c.Status, toJSON(c.Payload), toJSON(c.Summary), c.PayloadHash,
		ts(c.CreatedAt), c.DecidedBy, tsp(c.DecidedAt), c.DecisionNote, tsp(c.AppliedAt), c.Error, c.Attempts)
	return err
}

func (s *Store) GetChange(ctx context.Context, id string) (model.Change, error) {
	c, err := scanChange(s.queryRow(ctx, `SELECT `+changeCols+` FROM changes WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

type ChangeFilter struct {
	Status     string
	Type       string
	Q          string
	IdentityID string
	RunID      string
}

func (f ChangeFilter) where() *where {
	w := &where{}
	if f.Status != "" {
		w.add("status = ?", f.Status)
	}
	if f.Type != "" {
		w.add("change_type = ?", f.Type)
	}
	if f.IdentityID != "" {
		w.add("identity_id = ?", f.IdentityID)
	}
	if f.RunID != "" {
		w.add("run_id = ?", f.RunID)
	}
	w.like(f.Q, "display", "external_id")
	return w
}

func (s *Store) ListChanges(ctx context.Context, f ChangeFilter, p Page) ([]model.Change, int, error) {
	w := f.where()
	total, err := s.count(ctx, "changes", w)
	if err != nil {
		return nil, 0, err
	}
	pg, pargs := s.pageClause(p)
	rows, err := s.query(ctx, `SELECT `+changeCols+` FROM changes`+w.sql()+` ORDER BY created_at DESC, id`+pg, append(append([]any{}, w.args...), pargs...)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []model.Change{}
	for rows.Next() {
		c, err := scanChange(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}

// ListChangesWithStatus returns all changes in the given statuses, oldest first.
func (s *Store) ListChangesWithStatus(ctx context.Context, statuses ...string) ([]model.Change, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	ph := ""
	args := make([]any, len(statuses))
	for i, st := range statuses {
		if i > 0 {
			ph += ","
		}
		ph += "?"
		args[i] = st
	}
	rows, err := s.query(ctx, `SELECT `+changeCols+` FROM changes WHERE status IN (`+ph+`) ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Change{}
	for rows.Next() {
		c, err := scanChange(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) CountChanges(ctx context.Context, status string) (int, error) {
	w := &where{}
	w.add("status = ?", status)
	return s.count(ctx, "changes", w)
}

// TransitionChange atomically moves a change from one status to another (compare-and-set).
// It returns ErrConflict when the change was not in the expected status.
func (s *Store) TransitionChange(ctx context.Context, id, from, to, by, note string) error {
	now := ts(time.Now())
	var q string
	var args []any
	switch to {
	case model.StatusApproved, model.StatusRejected:
		q = `UPDATE changes SET status = ?, decided_by = ?, decided_at = ?, decision_note = ?, error = '' WHERE id = ? AND status = ?`
		args = []any{to, by, now, note, id, from}
	case model.StatusApplying:
		q = `UPDATE changes SET status = ?, attempts = attempts + 1 WHERE id = ? AND status = ?`
		args = []any{to, id, from}
	default:
		q = `UPDATE changes SET status = ? WHERE id = ? AND status = ?`
		args = []any{to, id, from}
	}
	n, err := s.affected(ctx, q, args...)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrConflict
	}
	return nil
}

// FinishChange records the terminal result of an applying change.
func (s *Store) FinishChange(ctx context.Context, id, status, errMsg string) error {
	applied := ""
	if status == model.StatusApplied {
		applied = ts(time.Now())
	}
	n, err := s.affected(ctx, `UPDATE changes SET status = ?, error = ?, applied_at = ? WHERE id = ? AND status = ?`,
		status, errMsg, applied, id, model.StatusApplying)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrConflict
	}
	return nil
}

// FailInterruptedChanges marks 'applying' changes (from a crash) as failed.
func (s *Store) FailInterruptedChanges(ctx context.Context) (int64, error) {
	return s.affected(ctx, `UPDATE changes SET status = ?, error = ? WHERE status = ?`,
		model.StatusFailed, "interrupted by service stop - verify the account in AD, then retry", model.StatusApplying)
}

// ---- audit log -------------------------------------------------------------

func (s *Store) auditHash(prev string, seq int64, tsStr, actor, action, etype, eid, detail string) string {
	var h hash.Hash = sha256.New()
	if len(s.auditKey) > 0 {
		h = hmac.New(sha256.New, s.auditKey)
	}
	for _, p := range []string{prev, strconv.FormatInt(seq, 10), tsStr, actor, action, etype, eid, detail} {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// AppendAudit adds a hash-chained entry to the append-only audit log.
func (s *Store) AppendAudit(ctx context.Context, actor, action, entityType, entityID string, detail map[string]any) error {
	s.auditMu.Lock()
	defer s.auditMu.Unlock()

	if detail == nil {
		detail = map[string]any{}
	}
	detailJSON := toJSON(detail)
	now := ts(time.Now())

	return s.tx(ctx, func(tx *sql.Tx) error {
		var seq int64
		var prev string
		err := tx.QueryRowContext(ctx, s.rebind(`SELECT seq, hash FROM audit_log WHERE seq = (SELECT MAX(seq) FROM audit_log)`)).Scan(&seq, &prev)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		seq++
		hash := s.auditHash(prev, seq, now, actor, action, entityType, entityID, detailJSON)
		_, err = tx.ExecContext(ctx, s.rebind(`INSERT INTO audit_log (seq, ts, actor, action, entity_type, entity_id, detail_json, prev_hash, hash) VALUES (?,?,?,?,?,?,?,?,?)`),
			seq, now, actor, action, entityType, entityID, detailJSON, prev, hash)
		return err
	})
}

type AuditFilter struct {
	Actor      string
	Action     string
	EntityType string
	EntityID   string
}

func scanAudit(sc interface{ Scan(...any) error }) (model.AuditEntry, string, error) {
	var e model.AuditEntry
	var t, detail string
	err := sc.Scan(&e.Seq, &t, &e.Actor, &e.Action, &e.EntityType, &e.EntityID, &detail, &e.PrevHash, &e.Hash)
	e.TS = parseTS(t)
	e.Detail = map[string]any{}
	fromJSON(detail, &e.Detail)
	return e, detail, err
}

const auditCols = `seq, ts, actor, action, entity_type, entity_id, detail_json, prev_hash, hash`

func (s *Store) ListAudit(ctx context.Context, f AuditFilter, p Page) ([]model.AuditEntry, int, error) {
	w := &where{}
	if f.Actor != "" {
		w.add("actor = ?", f.Actor)
	}
	if f.Action != "" {
		w.add("action = ?", f.Action)
	}
	if f.EntityType != "" {
		w.add("entity_type = ?", f.EntityType)
	}
	if f.EntityID != "" {
		w.add("entity_id = ?", f.EntityID)
	}
	total, err := s.count(ctx, "audit_log", w)
	if err != nil {
		return nil, 0, err
	}
	pg, pargs := s.pageClause(p)
	rows, err := s.query(ctx, `SELECT `+auditCols+` FROM audit_log`+w.sql()+` ORDER BY seq DESC`+pg, append(append([]any{}, w.args...), pargs...)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []model.AuditEntry{}
	for rows.Next() {
		e, _, err := scanAudit(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// AuditVerifyResult describes the outcome of recomputing the hash chain.
type AuditVerifyResult struct {
	OK          bool   `json:"ok"`
	Checked     int    `json:"checked"`
	FirstBadSeq int64  `json:"first_bad_seq"`
	Reason      string `json:"reason,omitempty"`
}

// VerifyAudit recomputes the whole chain and checks sequence continuity.
func (s *Store) VerifyAudit(ctx context.Context) (AuditVerifyResult, error) {
	rows, err := s.query(ctx, `SELECT `+auditCols+` FROM audit_log ORDER BY seq`)
	if err != nil {
		return AuditVerifyResult{}, err
	}
	defer rows.Close()

	res := AuditVerifyResult{OK: true}
	var prevHash string
	var prevSeq int64
	for rows.Next() {
		var e model.AuditEntry
		var t, detail string
		if err := rows.Scan(&e.Seq, &t, &e.Actor, &e.Action, &e.EntityType, &e.EntityID, &detail, &e.PrevHash, &e.Hash); err != nil {
			return res, err
		}
		res.Checked++
		bad := ""
		switch {
		case prevSeq == 0 && e.Seq != 1:
			bad = fmt.Sprintf("log does not start at seq 1 (starts at %d): earlier entries missing", e.Seq)
		case prevSeq != 0 && e.Seq != prevSeq+1:
			bad = fmt.Sprintf("gap in sequence between %d and %d", prevSeq, e.Seq)
		case e.PrevHash != prevHash:
			bad = "prev_hash does not match the previous entry"
		case s.auditHash(e.PrevHash, e.Seq, t, e.Actor, e.Action, e.EntityType, e.EntityID, detail) != e.Hash:
			bad = "entry content does not match its hash"
		}
		if bad != "" {
			res.OK, res.FirstBadSeq, res.Reason = false, e.Seq, bad
			return res, nil
		}
		prevSeq, prevHash = e.Seq, e.Hash
	}
	return res, rows.Err()
}

// ---- generic read-only table browser ---------------------------------------

type tableDef struct {
	cols   string
	order  string
	search []string
}

// browsable is an allow-list: sources/users/sessions/settings are intentionally absent (secrets, credentials).
var browsable = map[string]tableDef{
	"sync_runs": {"id, trigger_kind, triggered_by, started_at, finished_at, status, counters_json, error", "started_at DESC, id", []string{"id", "triggered_by", "status", "error"}},
	"changes": {"id, run_id, identity_id, external_id, display, change_type, status, payload_json, summary_json, created_at, decided_by, decided_at, decision_note, applied_at, error, attempts",
		"created_at DESC, id", []string{"id", "display", "external_id", "status", "change_type"}},
	"identities": {"id, source_id, external_id, display, active, missing_runs, first_seen, last_seen, ad_guid, ad_dn, ad_sam, disabled_at, lifecycle_state",
		"display, id", []string{"external_id", "display", "ad_sam", "ad_dn"}},
	"audit_log": {auditCols, "seq DESC", []string{"actor", "action", "entity_type", "entity_id"}},
}

type TableView struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
	Total   int      `json:"total"`
}

func BrowsableTables() []string { return []string{"sync_runs", "changes", "identities", "audit_log"} }

func (s *Store) BrowseTable(ctx context.Context, name, q string, p Page) (TableView, error) {
	def, ok := browsable[name]
	if !ok {
		return TableView{}, ErrNotFound
	}
	w := &where{}
	w.like(q, def.search...)
	total, err := s.count(ctx, name, w)
	if err != nil {
		return TableView{}, err
	}
	pg, pargs := s.pageClause(p)
	rows, err := s.query(ctx, `SELECT `+def.cols+` FROM `+name+w.sql()+` ORDER BY `+def.order+pg, append(append([]any{}, w.args...), pargs...)...)
	if err != nil {
		return TableView{}, err
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		return TableView{}, err
	}
	view := TableView{Name: name, Columns: cols, Rows: [][]any{}, Total: total}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return view, err
		}
		for i, v := range vals {
			if b, ok := v.([]byte); ok {
				vals[i] = string(b)
			}
		}
		view.Rows = append(view.Rows, vals)
	}
	return view, rows.Err()
}
