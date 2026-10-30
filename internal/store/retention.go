package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"idlifecycle/internal/model"
)

// ---- retention policies ----------------------------------------------------

const retentionCols = `id, name, description, enabled, priority, match_mode, conditions_json, delete_after_days, disabled_ou, keep_groups_json, approval, created_at, updated_at`

func scanRetention(sc interface{ Scan(...any) error }) (model.RetentionPolicy, error) {
	var p model.RetentionPolicy
	var en int
	var cond, keep, c, u string
	err := sc.Scan(&p.ID, &p.Name, &p.Description, &en, &p.Priority, &p.Match, &cond, &p.DeleteAfterDays, &p.DisabledOU, &keep, &p.Approval, &c, &u)
	p.Enabled = en != 0
	p.Conditions = []model.Condition{}
	fromJSON(cond, &p.Conditions)
	fromJSON(keep, &p.ExtraKeepGroups)
	if p.Conditions == nil {
		p.Conditions = []model.Condition{}
	}
	if p.ExtraKeepGroups == nil {
		p.ExtraKeepGroups = []string{}
	}
	p.CreatedAt, p.UpdatedAt = parseTS(c), parseTS(u)
	return p, err
}

func (s *Store) CreateRetentionPolicy(ctx context.Context, p model.RetentionPolicy) error {
	_, err := s.exec(ctx, `INSERT INTO retention_policies (`+retentionCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		p.ID, p.Name, p.Description, b2i(p.Enabled), p.Priority, p.Match, toJSON(p.Conditions), p.DeleteAfterDays, p.DisabledOU,
		toJSON(p.ExtraKeepGroups), p.Approval, ts(p.CreatedAt), ts(p.UpdatedAt))
	if IsUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

func (s *Store) GetRetentionPolicy(ctx context.Context, id string) (model.RetentionPolicy, error) {
	p, err := scanRetention(s.queryRow(ctx, `SELECT `+retentionCols+` FROM retention_policies WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// ListRetentionPolicies returns every policy in evaluation order (priority, then name).
func (s *Store) ListRetentionPolicies(ctx context.Context) ([]model.RetentionPolicy, error) {
	rows, err := s.query(ctx, `SELECT `+retentionCols+` FROM retention_policies ORDER BY priority, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.RetentionPolicy{}
	for rows.Next() {
		p, err := scanRetention(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) UpdateRetentionPolicy(ctx context.Context, p model.RetentionPolicy) error {
	n, err := s.affected(ctx, `UPDATE retention_policies SET name = ?, description = ?, enabled = ?, priority = ?, match_mode = ?, conditions_json = ?,
		delete_after_days = ?, disabled_ou = ?, keep_groups_json = ?, approval = ?, updated_at = ? WHERE id = ?`,
		p.Name, p.Description, b2i(p.Enabled), p.Priority, p.Match, toJSON(p.Conditions), p.DeleteAfterDays, p.DisabledOU,
		toJSON(p.ExtraKeepGroups), p.Approval, ts(time.Now()), p.ID)
	if IsUniqueViolation(err) {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteRetentionPolicy removes a policy. People assigned to it fall back to automatic resolution (their assignment
// is cleared in the same transaction).
func (s *Store) DeleteRetentionPolicy(ctx context.Context, id string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.rebind(`DELETE FROM retention_policies WHERE id = ?`), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		_, err = tx.ExecContext(ctx, s.rebind(`UPDATE identities SET retention_policy_id = ?, retention_assigned_by = ? WHERE retention_policy_id = ?`), "", "", id)
		return err
	})
}

// SetIdentityRetention assigns a policy to an identity ("" = automatic, model.RetentionDefaultID = the default policy).
// It is separate from UpdateIdentity so a sync never overwrites the choice.
func (s *Store) SetIdentityRetention(ctx context.Context, identityID, policyID, by string) error {
	if policyID == "" {
		by = ""
	}
	n, err := s.affected(ctx, `UPDATE identities SET retention_policy_id = ?, retention_assigned_by = ? WHERE id = ?`, policyID, by, identityID)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- resolved retention per identity ---------------------------------------

const identityRetentionCols = `identity_id, policy_id, policy_name, reason, delete_due, resolved_at`

func scanIdentityRetention(sc interface{ Scan(...any) error }) (model.IdentityRetention, error) {
	var r model.IdentityRetention
	var due, at string
	err := sc.Scan(&r.IdentityID, &r.PolicyID, &r.PolicyName, &r.Reason, &due, &at)
	r.DeleteDue, r.ResolvedAt = parseTSP(due), parseTS(at)
	return r, err
}

// GetIdentityRetention returns the policy the last sync resolved for an identity (ErrNotFound = none yet).
func (s *Store) GetIdentityRetention(ctx context.Context, identityID string) (model.IdentityRetention, error) {
	r, err := scanIdentityRetention(s.queryRow(ctx, `SELECT `+identityRetentionCols+` FROM identity_retention WHERE identity_id = ?`, identityID))
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// ListIdentityRetention returns every resolved retention, by identity id.
func (s *Store) ListIdentityRetention(ctx context.Context) (map[string]model.IdentityRetention, error) {
	rows, err := s.query(ctx, `SELECT `+identityRetentionCols+` FROM identity_retention`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]model.IdentityRetention{}
	for rows.Next() {
		r, err := scanIdentityRetention(rows)
		if err != nil {
			return nil, err
		}
		out[r.IdentityID] = r
	}
	return out, rows.Err()
}

// PutIdentityRetention stores the policy a sync resolved for an identity (insert or replace).
func (s *Store) PutIdentityRetention(ctx context.Context, r model.IdentityRetention) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		n := 0
		if err := tx.QueryRowContext(ctx, s.rebind(`SELECT COUNT(*) FROM identity_retention WHERE identity_id = ?`), r.IdentityID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			_, err := tx.ExecContext(ctx, s.rebind(`UPDATE identity_retention SET policy_id = ?, policy_name = ?, reason = ?, delete_due = ?, resolved_at = ? WHERE identity_id = ?`),
				r.PolicyID, r.PolicyName, r.Reason, tsp(r.DeleteDue), ts(r.ResolvedAt), r.IdentityID)
			return err
		}
		_, err := tx.ExecContext(ctx, s.rebind(`INSERT INTO identity_retention (`+identityRetentionCols+`) VALUES (?,?,?,?,?,?)`),
			r.IdentityID, r.PolicyID, r.PolicyName, r.Reason, tsp(r.DeleteDue), ts(r.ResolvedAt))
		return err
	})
}

// RetentionCounts returns how many identities the last sync resolved to each policy id.
func (s *Store) RetentionCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.query(ctx, `SELECT policy_id, COUNT(*) FROM identity_retention GROUP BY policy_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}
