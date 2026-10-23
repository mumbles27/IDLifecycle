package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Column type tokens expanded per dialect.
func (s *Store) ddl(stmt string) string {
	var str, text, opts string
	switch s.driver {
	case DriverMSSQL:
		str, text, opts = "NVARCHAR(255)", "NVARCHAR(MAX)", ""
	case DriverMySQL:
		str, text, opts = "VARCHAR(255)", "LONGTEXT", " ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"
	case DriverPostgres:
		str, text, opts = "VARCHAR(255)", "TEXT", ""
	default:
		str, text, opts = "VARCHAR(255)", "TEXT", ""
	}
	r := strings.NewReplacer(
		"{ID}", "VARCHAR(64)",
		"{STR}", str,
		"{TS}", "VARCHAR(40)",
		"{TEXT}", text,
		"{INT}", "INT",
		"{BIGINT}", "BIGINT",
		"{OPTS}", opts,
	)
	return r.Replace(stmt)
}

// migrations are append-only; never edit an applied entry, add a new one.
var migrations = []struct {
	version int
	stmts   []string
}{
	{1, []string{
		`CREATE TABLE settings (
			setting_key {STR} NOT NULL PRIMARY KEY,
			setting_value {TEXT} NOT NULL,
			updated_at {TS} NOT NULL,
			updated_by {STR} NOT NULL){OPTS}`,
		`CREATE TABLE users (
			id {ID} NOT NULL PRIMARY KEY,
			username {STR} NOT NULL,
			password_hash {STR} NOT NULL,
			role VARCHAR(20) NOT NULL,
			disabled {INT} NOT NULL,
			failed_logins {INT} NOT NULL,
			locked_until {TS} NOT NULL,
			created_at {TS} NOT NULL,
			last_login_at {TS} NOT NULL){OPTS}`,
		`CREATE UNIQUE INDEX ux_users_username ON users (username)`,
		`CREATE TABLE sessions (
			token_hash VARCHAR(64) NOT NULL PRIMARY KEY,
			user_id {ID} NOT NULL,
			csrf_token VARCHAR(64) NOT NULL,
			created_at {TS} NOT NULL,
			expires_at {TS} NOT NULL,
			last_seen_at {TS} NOT NULL){OPTS}`,
		`CREATE INDEX ix_sessions_user ON sessions (user_id)`,
		`CREATE TABLE sources (
			id {ID} NOT NULL PRIMARY KEY,
			name {STR} NOT NULL,
			source_type VARCHAR(40) NOT NULL,
			enabled {INT} NOT NULL,
			options_json {TEXT} NOT NULL,
			config_json {TEXT} NOT NULL,
			created_at {TS} NOT NULL,
			updated_at {TS} NOT NULL,
			last_sync_at {TS} NOT NULL,
			last_status VARCHAR(20) NOT NULL,
			last_error {TEXT} NOT NULL){OPTS}`,
		`CREATE UNIQUE INDEX ux_sources_name ON sources (name)`,
		`CREATE TABLE identities (
			id {ID} NOT NULL PRIMARY KEY,
			source_id {ID} NOT NULL,
			external_id {STR} NOT NULL,
			display {STR} NOT NULL,
			attrs_json {TEXT} NOT NULL,
			attrs_hash VARCHAR(64) NOT NULL,
			active {INT} NOT NULL,
			missing_runs {INT} NOT NULL,
			first_seen {TS} NOT NULL,
			last_seen {TS} NOT NULL,
			ad_guid VARCHAR(64) NOT NULL,
			ad_dn {TEXT} NOT NULL,
			ad_sam {STR} NOT NULL,
			applied_json {TEXT} NOT NULL,
			disabled_at {TS} NOT NULL,
			lifecycle_state VARCHAR(20) NOT NULL){OPTS}`,
		`CREATE UNIQUE INDEX ux_identities_src_ext ON identities (source_id, external_id)`,
		`CREATE INDEX ix_identities_state ON identities (lifecycle_state)`,
		`CREATE TABLE mappings (
			id {ID} NOT NULL PRIMARY KEY,
			name {STR} NOT NULL,
			target_attr {STR} NOT NULL,
			template {TEXT} NOT NULL,
			mode VARCHAR(20) NOT NULL,
			on_empty VARCHAR(10) NOT NULL,
			value_type VARCHAR(20) NOT NULL,
			enabled {INT} NOT NULL,
			sort_order {INT} NOT NULL,
			description {TEXT} NOT NULL,
			created_at {TS} NOT NULL,
			updated_at {TS} NOT NULL){OPTS}`,
		`CREATE UNIQUE INDEX ux_mappings_name ON mappings (name)`,
		`CREATE TABLE role_rules (
			id {ID} NOT NULL PRIMARY KEY,
			name {STR} NOT NULL,
			description {TEXT} NOT NULL,
			enabled {INT} NOT NULL,
			priority {INT} NOT NULL,
			match_mode VARCHAR(10) NOT NULL,
			conditions_json {TEXT} NOT NULL,
			grants_json {TEXT} NOT NULL,
			created_at {TS} NOT NULL,
			updated_at {TS} NOT NULL){OPTS}`,
		`CREATE UNIQUE INDEX ux_role_rules_name ON role_rules (name)`,
		`CREATE TABLE sync_runs (
			id {ID} NOT NULL PRIMARY KEY,
			trigger_kind VARCHAR(20) NOT NULL,
			triggered_by {STR} NOT NULL,
			started_at {TS} NOT NULL,
			finished_at {TS} NOT NULL,
			status VARCHAR(20) NOT NULL,
			counters_json {TEXT} NOT NULL,
			error {TEXT} NOT NULL){OPTS}`,
		`CREATE INDEX ix_sync_runs_started ON sync_runs (started_at)`,
		`CREATE TABLE changes (
			id {ID} NOT NULL PRIMARY KEY,
			run_id {ID} NOT NULL,
			identity_id {ID} NOT NULL,
			external_id {STR} NOT NULL,
			display {STR} NOT NULL,
			change_type VARCHAR(20) NOT NULL,
			status VARCHAR(20) NOT NULL,
			payload_json {TEXT} NOT NULL,
			summary_json {TEXT} NOT NULL,
			payload_hash VARCHAR(64) NOT NULL,
			created_at {TS} NOT NULL,
			decided_by {STR} NOT NULL,
			decided_at {TS} NOT NULL,
			decision_note {TEXT} NOT NULL,
			applied_at {TS} NOT NULL,
			error {TEXT} NOT NULL,
			attempts {INT} NOT NULL){OPTS}`,
		`CREATE INDEX ix_changes_status ON changes (status, created_at)`,
		`CREATE INDEX ix_changes_identity ON changes (identity_id)`,
		`CREATE INDEX ix_changes_run ON changes (run_id)`,
		`CREATE TABLE audit_log (
			seq {BIGINT} NOT NULL PRIMARY KEY,
			ts {TS} NOT NULL,
			actor {STR} NOT NULL,
			action VARCHAR(60) NOT NULL,
			entity_type VARCHAR(40) NOT NULL,
			entity_id {STR} NOT NULL,
			detail_json {TEXT} NOT NULL,
			prev_hash VARCHAR(64) NOT NULL,
			hash VARCHAR(64) NOT NULL){OPTS}`,
	}},
}

func (s *Store) migrate(ctx context.Context) error {
	var create string
	if s.driver == DriverMSSQL {
		create = `IF OBJECT_ID('schema_migrations','U') IS NULL CREATE TABLE schema_migrations (version {INT} NOT NULL PRIMARY KEY, applied_at {TS} NOT NULL)`
	} else {
		create = `CREATE TABLE IF NOT EXISTS schema_migrations (version {INT} NOT NULL PRIMARY KEY, applied_at {TS} NOT NULL){OPTS}`
	}
	if _, err := s.db.ExecContext(ctx, s.ddl(create)); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied := map[int]bool{}
	rows, err := s.query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			_ = rows.Close()
			return err
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_ = rows.Close()

	for _, m := range migrations {
		if applied[m.version] {
			continue
		}
		for _, stmt := range m.stmts {
			if _, err := s.db.ExecContext(ctx, s.ddl(stmt)); err != nil {
				return fmt.Errorf("migration %d: %w", m.version, err)
			}
		}
		if _, err := s.exec(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, m.version, ts(time.Now())); err != nil {
			return fmt.Errorf("record migration %d: %w", m.version, err)
		}
	}
	return nil
}
