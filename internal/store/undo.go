package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"idlifecycle/internal/model"
)

// ---- undo: the state an applied change found, and the undo that rolled it back -------------------------------

// SetChangeAppliedState records what the account looked like before a change was applied.
func (s *Store) SetChangeAppliedState(ctx context.Context, changeID string, st model.AppliedState) error {
	return s.upsertChangeUndo(ctx, changeID, func(state *string, _ *string) { *state = toJSON(st) })
}

// SetChangeUndoneBy marks a change as rolled back by the undo change undoID.
func (s *Store) SetChangeUndoneBy(ctx context.Context, changeID, undoID string) error {
	return s.upsertChangeUndo(ctx, changeID, func(_ *string, by *string) { *by = undoID })
}

func (s *Store) upsertChangeUndo(ctx context.Context, changeID string, fn func(state, undoneBy *string)) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var state, by string
		found := true
		err := tx.QueryRowContext(ctx, s.rebind(`SELECT applied_state, undone_by FROM change_undo WHERE change_id = ?`), changeID).Scan(&state, &by)
		if errors.Is(err, sql.ErrNoRows) {
			found = false
		} else if err != nil {
			return err
		}
		fn(&state, &by)
		if found {
			_, err = tx.ExecContext(ctx, s.rebind(`UPDATE change_undo SET applied_state = ?, undone_by = ?, updated_at = ? WHERE change_id = ?`),
				state, by, ts(time.Now()), changeID)
			return err
		}
		_, err = tx.ExecContext(ctx, s.rebind(`INSERT INTO change_undo (change_id, applied_state, undone_by, updated_at) VALUES (?, ?, ?, ?)`),
			changeID, state, by, ts(time.Now()))
		return err
	})
}

// attachUndo fills the undo bookkeeping of changes read from the table: applied state, undone by, an open undo, and
// (for undo changes) the change they roll back.
func (s *Store) attachUndo(ctx context.Context, list []model.Change) error {
	if len(list) == 0 {
		return nil
	}
	idx := map[string]int{}
	ph := ""
	args := make([]any, 0, len(list))
	for i := range list {
		if p := list[i].Payload.Undo; list[i].Type == model.ChangeUndo && p != nil {
			list[i].UndoOf = p.Of
		}
		if list[i].Status != model.StatusApplied {
			continue
		}
		idx[list[i].ID] = i
		if ph != "" {
			ph += ","
		}
		ph += "?"
		args = append(args, list[i].ID)
	}
	if len(args) == 0 {
		return nil
	}
	rows, err := s.query(ctx, `SELECT change_id, applied_state, undone_by FROM change_undo WHERE change_id IN (`+ph+`)`, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, state, by string
		if err := rows.Scan(&id, &state, &by); err != nil {
			_ = rows.Close()
			return err
		}
		c := &list[idx[id]]
		if state != "" {
			var st model.AppliedState
			fromJSON(state, &st)
			c.AppliedState = &st
		}
		c.UndoneBy = by
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	items, err := s.queryUndoneItems(ctx, ph, args)
	if err != nil {
		return err
	}
	for id, m := range items {
		list[idx[id]].UndoneItems = m
	}
	open, err := s.OpenUndoList(ctx)
	if err != nil {
		return err
	}
	for _, o := range open {
		i, ok := idx[o.Of]
		if !ok {
			continue
		}
		if len(o.Items) == 0 {
			list[i].UndoPending = o.ID
			continue
		}
		if list[i].UndoPendingItems == nil {
			list[i].UndoPendingItems = map[string]string{}
		}
		for _, k := range o.Items {
			list[i].UndoPendingItems[k] = o.ID
		}
	}
	return nil
}

// OpenUndo is an undo change waiting for approval, approved or running: the change it rolls back and the parts it
// covers (none = the whole change).
type OpenUndo struct {
	ID    string
	Of    string
	Items []string
	Batch string
}

// OpenUndoList lists every open undo change, oldest first.
func (s *Store) OpenUndoList(ctx context.Context) ([]OpenUndo, error) {
	rows, err := s.query(ctx, `SELECT id, payload_json FROM changes WHERE change_type = ? AND status IN (?,?,?) ORDER BY created_at, id`,
		model.ChangeUndo, model.StatusPending, model.StatusApproved, model.StatusApplying)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OpenUndo
	for rows.Next() {
		var id, payload string
		if err := rows.Scan(&id, &payload); err != nil {
			return nil, err
		}
		var p model.ChangePayload
		fromJSON(payload, &p)
		if p.Undo != nil {
			out = append(out, OpenUndo{ID: id, Of: p.Undo.Of, Items: p.Undo.Items, Batch: p.Undo.Batch})
		}
	}
	return out, rows.Err()
}

// OpenUndos maps a change id to an open undo change for it (whole or partial).
func (s *Store) OpenUndos(ctx context.Context) (map[string]string, error) {
	list, err := s.OpenUndoList(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, o := range list {
		out[o.Of] = o.ID
	}
	return out, nil
}

// ---- undo per item ---------------------------------------------------------------------------------------------

// AddUndoneItems records that undo change undoID rolled back these parts of change changeID.
func (s *Store) AddUndoneItems(ctx context.Context, changeID, undoID string, keys []string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		for _, k := range keys {
			if _, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO change_undo_items (id, change_id, item_key, undo_change_id, created_at) VALUES (?, ?, ?, ?, ?)`),
				undoItemID(changeID, k, undoID), changeID, k, undoID, ts(time.Now())); err != nil {
				return err
			}
		}
		return nil
	})
}

// UndoneItems returns the parts of change changeID already rolled back (item key -> undo change id).
func (s *Store) UndoneItems(ctx context.Context, changeID string) (map[string]string, error) {
	m, err := s.queryUndoneItems(ctx, "?", []any{changeID})
	if err != nil {
		return nil, err
	}
	return m[changeID], nil
}

func (s *Store) queryUndoneItems(ctx context.Context, ph string, args []any) (map[string]map[string]string, error) {
	rows, err := s.query(ctx, `SELECT change_id, item_key, undo_change_id FROM change_undo_items WHERE change_id IN (`+ph+`) ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]string{}
	for rows.Next() {
		var c, k, u string
		if err := rows.Scan(&c, &k, &u); err != nil {
			return nil, err
		}
		if out[c] == nil {
			out[c] = map[string]string{}
		}
		out[c][k] = u
	}
	return out, rows.Err()
}

// ---- holds ---------------------------------------------------------------------------------------------------

const holdCols = `id, identity_id, kind, hold_key, until_at, reason, created_by, created_at, change_id`

func scanHold(sc interface{ Scan(...any) error }) (model.Hold, error) {
	var h model.Hold
	var until, created string
	err := sc.Scan(&h.ID, &h.IdentityID, &h.Kind, &h.Key, &until, &h.Reason, &h.CreatedBy, &created, &h.ChangeID)
	h.Until, h.CreatedAt = parseTSP(until), parseTS(created)
	return h, err
}

func (s *Store) InsertHold(ctx context.Context, h model.Hold) error {
	_, err := s.exec(ctx, `INSERT INTO identity_holds (`+holdCols+`) VALUES (?,?,?,?,?,?,?,?,?)`,
		h.ID, h.IdentityID, h.Kind, h.Key, tsp(h.Until), h.Reason, h.CreatedBy, ts(h.CreatedAt), h.ChangeID)
	return err
}

func (s *Store) listHolds(ctx context.Context, q string, args ...any) ([]model.Hold, error) {
	rows, err := s.query(ctx, `SELECT `+holdCols+` FROM identity_holds`+q+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	out := []model.Hold{}
	for rows.Next() {
		h, err := scanHold(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, h)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil || len(out) == 0 {
		return out, err
	}
	sup, err := s.holdSuppressed(ctx)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Suppressed = sup[out[i].ID]
	}
	return out, nil
}

// holdSuppressed reads what each hold last kept the planner from doing (hold id -> text and time).
func (s *Store) holdSuppressed(ctx context.Context) (map[string]*model.HoldSuppressed, error) {
	rows, err := s.query(ctx, `SELECT hold_id, suppressed, updated_at FROM identity_hold_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*model.HoldSuppressed{}
	for rows.Next() {
		var id, text, at string
		if err := rows.Scan(&id, &text, &at); err != nil {
			return nil, err
		}
		if text != "" {
			out[id] = &model.HoldSuppressed{Summary: text, At: parseTS(at)}
		}
	}
	return out, rows.Err()
}

// SetHoldSuppressed records what a hold kept the last sync from doing ("" = nothing).
func (s *Store) SetHoldSuppressed(ctx context.Context, holdID, summary string, at time.Time) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		n := 0
		if err := tx.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*) FROM identity_hold_state WHERE hold_id = ?`), holdID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			_, err := tx.ExecContext(ctx, s.rebind(`UPDATE identity_hold_state SET suppressed = ?, updated_at = ? WHERE hold_id = ?`), summary, ts(at), holdID)
			return err
		}
		_, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO identity_hold_state (hold_id, suppressed, updated_at) VALUES (?, ?, ?)`), holdID, summary, ts(at))
		return err
	})
}

// ManualCounts returns how many active holds (Manual pins) each identity has at now.
func (s *Store) ManualCounts(ctx context.Context, now time.Time) (map[string]int, error) {
	all, err := s.ActiveHolds(ctx, now)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(all))
	for id, hs := range all {
		out[id] = len(hs)
	}
	return out, nil
}

// ListHolds returns an identity's holds, expired ones included (the console shows them until released).
func (s *Store) ListHolds(ctx context.Context, identityID string) ([]model.Hold, error) {
	return s.listHolds(ctx, ` WHERE identity_id = ?`, identityID)
}

// ActiveHolds returns every hold still in force at now, by identity id.
func (s *Store) ActiveHolds(ctx context.Context, now time.Time) (map[string][]model.Hold, error) {
	all, err := s.listHolds(ctx, "")
	if err != nil {
		return nil, err
	}
	out := map[string][]model.Hold{}
	for _, h := range all {
		if h.Active(now) {
			out[h.IdentityID] = append(out[h.IdentityID], h)
		}
	}
	return out, nil
}

// DeleteHold releases one hold of an identity and returns it.
func (s *Store) DeleteHold(ctx context.Context, identityID, holdID string) (model.Hold, error) {
	h, err := scanHold(s.queryRow(ctx, `SELECT `+holdCols+` FROM identity_holds WHERE id = ? AND identity_id = ?`, holdID, identityID))
	if errors.Is(err, sql.ErrNoRows) {
		return h, ErrNotFound
	}
	if err != nil {
		return h, err
	}
	if _, err := s.exec(ctx, `DELETE FROM identity_holds WHERE id = ?`, holdID); err != nil {
		return h, err
	}
	if _, err := s.exec(ctx, `DELETE FROM identity_hold_state WHERE hold_id = ?`, holdID); err != nil {
		return h, err
	}
	return h, nil
}

// undoItemID is the row id of one undone part: stable, so the same part recorded twice by one undo is one row.
func undoItemID(changeID, key, undoID string) string {
	sum := sha256.Sum256([]byte(changeID + "\x00" + key + "\x00" + undoID))
	return hex.EncodeToString(sum[:])
}
