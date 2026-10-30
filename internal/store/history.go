package store

import (
	"context"

	"idlifecycle/internal/model"
)

// ListIdentityHistory returns the audit rows about one identity, newest first: its own rows (entity "identity"),
// the rows of its changes (found through changes.identity_id, so rows written before the change detail carried the
// identity are included), and the rows of the console record it came from (manual_people by source and external id).
// beforeSeq > 0 returns only older rows (paging); limit is capped like every page.
func (s *Store) ListIdentityHistory(ctx context.Context, identityID, sourceID, externalID string, beforeSeq int64, limit int) ([]model.AuditEntry, error) {
	q := `SELECT ` + auditCols + ` FROM audit_log WHERE ((entity_type = ? AND entity_id = ?)
		OR (entity_type = ? AND entity_id IN (SELECT id FROM changes WHERE identity_id = ?))
		OR (entity_type = ? AND entity_id IN (SELECT id FROM manual_people WHERE source_id = ? AND external_id = ?)))`
	args := []any{"identity", identityID, "change", identityID, "manual_person", sourceID, externalID}
	if beforeSeq > 0 {
		q += ` AND seq < ?`
		args = append(args, beforeSeq)
	}
	pg, pargs := s.pageClause(Page{Limit: limit})
	rows, err := s.query(ctx, q+` ORDER BY seq DESC`+pg, append(args, pargs...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.AuditEntry{}
	for rows.Next() {
		e, _, err := scanAudit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// LastIdentityEvent returns the newest audit row of the identity with one of the actions (ok false when none).
func (s *Store) LastIdentityEvent(ctx context.Context, identityID string, actions ...string) (model.AuditEntry, bool, error) {
	items, _, err := s.ListAudit(ctx, AuditFilter{EntityType: "identity", EntityID: identityID, Actions: actions}, Page{Limit: 1})
	if err != nil || len(items) == 0 {
		return model.AuditEntry{}, false, err
	}
	return items[0], true, nil
}
