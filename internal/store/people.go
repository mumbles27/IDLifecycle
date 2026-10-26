package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"idlifecycle/internal/model"
)

// ---- choices a person made for one identity ---------------------------------------
// They are written only by these methods (never by UpdateIdentity), so a sync run cannot overwrite a choice made
// while it was planning.

// SetIdentityLinkDecision records "" | "new" | "guid:<objectGUID>" for an identity.
func (s *Store) SetIdentityLinkDecision(ctx context.Context, id, decision string) error {
	n, err := s.affected(ctx, `UPDATE identities SET link_decision = ? WHERE id = ?`, decision, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetIdentityNameOverride records (nil clears) the account names a person chose for an identity.
func (s *Store) SetIdentityNameOverride(ctx context.Context, id string, c *model.Candidate) error {
	return s.updateChoices(ctx, id, func(names *string, _ *[]string) {
		*names = ""
		if c != nil {
			*names = toJSON(c)
		}
	})
}

// AddIdentityLinkRejects adds AD objectGUIDs a person said do not belong to the identity.
func (s *Store) AddIdentityLinkRejects(ctx context.Context, id string, guids ...string) error {
	return s.updateChoices(ctx, id, func(_ *string, rejects *[]string) {
		for _, g := range guids {
			if g = strings.ToLower(strings.TrimSpace(g)); g != "" && !containsFold(*rejects, g) {
				*rejects = append(*rejects, g)
			}
		}
	})
}

func containsFold(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

func (s *Store) updateChoices(ctx context.Context, id string, fn func(names *string, rejects *[]string)) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRowContext(ctx, s.rebind(`SELECT 1 FROM identities WHERE id = ?`), id).Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		var names, rejectsJSON string
		found := true
		err := tx.QueryRowContext(ctx, s.rebind(`SELECT name_override, link_rejects FROM identity_choices WHERE identity_id = ?`), id).Scan(&names, &rejectsJSON)
		if errors.Is(err, sql.ErrNoRows) {
			found = false
		} else if err != nil {
			return err
		}
		rejects := []string{}
		fromJSON(rejectsJSON, &rejects)
		fn(&names, &rejects)
		if found {
			_, err = tx.ExecContext(ctx, s.rebind(`UPDATE identity_choices SET name_override = ?, link_rejects = ?, updated_at = ? WHERE identity_id = ?`),
				names, toJSON(rejects), ts(time.Now()), id)
			return err
		}
		_, err = tx.ExecContext(ctx, s.rebind(`INSERT INTO identity_choices (identity_id, name_override, link_rejects, updated_at) VALUES (?, ?, ?, ?)`),
			id, names, toJSON(rejects), ts(time.Now()))
		return err
	})
}

// ---- manual people -------------------------------------------------------------------

const manualCols = `id, source_id, external_id, attrs_json, ended, created_by, created_at, updated_by, updated_at`

func scanManual(sc interface{ Scan(...any) error }) (model.ManualPerson, error) {
	var p model.ManualPerson
	var attrs, created, updated string
	var ended int
	err := sc.Scan(&p.ID, &p.SourceID, &p.ExternalID, &attrs, &ended, &p.CreatedBy, &created, &p.UpdatedBy, &updated)
	p.Attrs = map[string]string{}
	fromJSON(attrs, &p.Attrs)
	p.Ended = ended != 0
	p.CreatedAt, p.UpdatedAt = parseTS(created), parseTS(updated)
	return p, err
}

// CreateManualPerson stores a person typed into the console. ErrConflict: the external id exists in that source.
func (s *Store) CreateManualPerson(ctx context.Context, p model.ManualPerson) error {
	_, err := s.exec(ctx, `INSERT INTO manual_people (`+manualCols+`) VALUES (?,?,?,?,?,?,?,?,?)`,
		p.ID, p.SourceID, p.ExternalID, toJSON(p.Attrs), b2i(p.Ended), p.CreatedBy, ts(p.CreatedAt), p.UpdatedBy, ts(p.UpdatedAt))
	if IsUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

// UpdateManualPerson replaces the attributes and the ended flag.
func (s *Store) UpdateManualPerson(ctx context.Context, p model.ManualPerson) error {
	n, err := s.affected(ctx, `UPDATE manual_people SET attrs_json = ?, ended = ?, updated_by = ?, updated_at = ? WHERE id = ? AND source_id = ?`,
		toJSON(p.Attrs), b2i(p.Ended), p.UpdatedBy, ts(p.UpdatedAt), p.ID, p.SourceID)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) GetManualPerson(ctx context.Context, sourceID, id string) (model.ManualPerson, error) {
	p, err := scanManual(s.queryRow(ctx, `SELECT `+manualCols+` FROM manual_people WHERE id = ? AND source_id = ?`, id, sourceID))
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// ListManualPeople returns a manual source's people, newest first.
func (s *Store) ListManualPeople(ctx context.Context, sourceID string) ([]model.ManualPerson, error) {
	rows, err := s.query(ctx, `SELECT `+manualCols+` FROM manual_people WHERE source_id = ? ORDER BY created_at DESC, id`, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ManualPerson{}
	for rows.Next() {
		p, err := scanManual(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) DeleteManualPerson(ctx context.Context, sourceID, id string) error {
	n, err := s.affected(ctx, `DELETE FROM manual_people WHERE id = ? AND source_id = ?`, id, sourceID)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- small state documents (kept in the settings table under "state:<name>") ------------------

// PutState stores a JSON document the service regenerates (e.g. the last correlation report).
func (s *Store) PutState(ctx context.Context, name string, v any) error {
	key := "state:" + name
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.rebind(`UPDATE settings SET setting_value = ?, updated_at = ?, updated_by = ? WHERE setting_key = ?`),
			toJSON(v), ts(time.Now()), "system", key)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}
		_, err = tx.ExecContext(ctx, s.rebind(`INSERT INTO settings (setting_key, setting_value, updated_at, updated_by) VALUES (?, ?, ?, ?)`),
			key, toJSON(v), ts(time.Now()), "system")
		return err
	})
}

// GetState loads a document stored with PutState; ok is false when there is none.
func (s *Store) GetState(ctx context.Context, name string, v any) (bool, error) {
	var raw string
	err := s.queryRow(ctx, `SELECT setting_value FROM settings WHERE setting_key = ?`, "state:"+name).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	fromJSON(raw, v)
	return true, nil
}
