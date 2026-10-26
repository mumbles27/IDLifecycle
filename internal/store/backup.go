package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// BackupTables are the tables a backup carries, in an order that is safe to load (nothing references a later
// table). sessions (sign-ins on the old server) and schema_migrations (owned by migrate) are left out.
var BackupTables = []string{"settings", "users", "sources", "manual_people", "mappings", "role_rules", "identities", "identity_choices", "sync_runs", "changes", "audit_log"}

// SchemaVersion is the newest migration this build knows; a backup records it and a restore refuses a newer one.
func SchemaVersion() int {
	v := 0
	for _, m := range migrations {
		if m.version > v {
			v = m.version
		}
	}
	return v
}

// tableColumns returns the column names of table as the database reports them (lower case).
func (s *Store) tableColumns(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, table string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT * FROM `+table+` WHERE 1 = 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	for i := range cols {
		cols[i] = strings.ToLower(cols[i])
	}
	return cols, nil
}

// ExportTable calls fn for every row of table (one of BackupTables) as column -> value, where a value is a string,
// an int64 or nil. Rows come in primary-key order so two exports of the same data are identical.
func (s *Store) ExportTable(ctx context.Context, table string, fn func(row map[string]any) error) (int, error) {
	if !isBackupTable(table) {
		return 0, fmt.Errorf("not a backup table: %s", table)
	}
	cols, err := s.tableColumns(ctx, s.db, table)
	if err != nil {
		return 0, err
	}
	rows, err := s.query(ctx, `SELECT `+strings.Join(cols, ", ")+` FROM `+table+` ORDER BY `+cols[0])
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return n, err
		}
		row := make(map[string]any, len(cols))
		for i, c := range cols {
			row[c] = normalizeValue(vals[i])
		}
		if err := fn(row); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

func normalizeValue(v any) any {
	switch x := v.(type) {
	case nil:
		return nil
	case []byte:
		return string(x)
	case string:
		return x
	case int64:
		return x
	case int32:
		return int64(x)
	case int:
		return int64(x)
	case int16:
		return int64(x)
	case float64:
		if x == float64(int64(x)) {
			return int64(x)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		if x {
			return int64(1)
		}
		return int64(0)
	default:
		return fmt.Sprint(x)
	}
}

func isBackupTable(t string) bool {
	for _, b := range BackupTables {
		if b == t {
			return true
		}
	}
	return false
}

// DataRows counts the rows in the backup tables (a fresh database has none, or only default settings).
func (s *Store) DataRows(ctx context.Context) (map[string]int, error) {
	out := map[string]int{}
	for _, t := range BackupTables {
		n, err := s.count(ctx, t, &where{})
		if err != nil {
			return nil, err
		}
		out[t] = n
	}
	return out, nil
}

// ClearSessions ends every GUI session (after a restore the users they belong to may be different).
func (s *Store) ClearSessions(ctx context.Context) error {
	_, err := s.exec(ctx, `DELETE FROM sessions`)
	return err
}

// ImportSource yields the rows of one table from a backup; fn is called per row (values as ExportTable gives them,
// or json.Number for numbers read back from JSON).
type ImportSource func(table string, fn func(row map[string]any) error) error

// ImportTables replaces the contents of every backup table with the rows from src, in one transaction: either
// all of it lands or nothing changes. Columns the backup has but this schema does not are refused; columns this
// schema has but the backup does not get their database default (a backup from an older version).
func (s *Store) ImportTables(ctx context.Context, src ImportSource) (map[string]int, error) {
	counts := map[string]int{}
	err := s.tx(ctx, func(tx *sql.Tx) error {
		for i := len(BackupTables) - 1; i >= 0; i-- {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+BackupTables[i]); err != nil {
				return fmt.Errorf("clear %s: %w", BackupTables[i], err)
			}
		}
		for _, table := range BackupTables {
			have, err := s.tableColumns(ctx, tx, table)
			if err != nil {
				return err
			}
			known := map[string]bool{}
			for _, c := range have {
				known[c] = true
			}
			stmts := map[string]*sql.Stmt{}
			err = src(table, func(row map[string]any) error {
				cols := make([]string, 0, len(row))
				for c := range row {
					lc := strings.ToLower(c)
					if !known[lc] {
						return fmt.Errorf("%s: column %q does not exist in this version's schema", table, c)
					}
					cols = append(cols, lc)
				}
				sort.Strings(cols)
				key := strings.Join(cols, ",")
				st, ok := stmts[key]
				if !ok {
					ph := strings.TrimSuffix(strings.Repeat("?,", len(cols)), ",")
					p, err := tx.PrepareContext(ctx, s.rebind(`INSERT INTO `+table+` (`+key+`) VALUES (`+ph+`)`))
					if err != nil {
						return err
					}
					stmts[key], st = p, p
				}
				args := make([]any, len(cols))
				for i, c := range cols {
					v, err := importValue(lookup(row, c))
					if err != nil {
						return fmt.Errorf("%s.%s: %w", table, c, err)
					}
					args[i] = v
				}
				if _, err := st.ExecContext(ctx, args...); err != nil {
					return fmt.Errorf("insert into %s: %w", table, err)
				}
				counts[table]++
				return nil
			})
			for _, st := range stmts {
				_ = st.Close()
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	return counts, err
}

func lookup(row map[string]any, lc string) any {
	if v, ok := row[lc]; ok {
		return v
	}
	for k, v := range row {
		if strings.EqualFold(k, lc) {
			return v
		}
	}
	return nil
}

func importValue(v any) (any, error) {
	switch x := v.(type) {
	case json.Number:
		n, err := x.Int64()
		if err != nil {
			return nil, fmt.Errorf("number %s is not an integer", x)
		}
		return n, nil
	case float64:
		return int64(x), nil
	case nil, string, int64:
		return x, nil
	default:
		return nil, fmt.Errorf("unexpected value type %T", v)
	}
}
