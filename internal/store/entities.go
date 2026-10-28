package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"idlifecycle/internal/model"
)

// ---- settings --------------------------------------------------------------

// GetSettings returns stored settings or defaults when none were saved yet.
func (s *Store) GetSettings(ctx context.Context) (model.Settings, error) {
	st := model.DefaultSettings()
	var v string
	err := s.queryRow(ctx, `SELECT setting_value FROM settings WHERE setting_key = ?`, "settings").Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		st.ApplyDefaults()
		return st, nil
	}
	if err != nil {
		return st, err
	}
	fromJSON(v, &st)
	st.ApplyDefaults()
	return st, nil
}

func (s *Store) SaveSettings(ctx context.Context, st model.Settings, by string) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, s.rebind(`UPDATE settings SET setting_value = ?, updated_at = ?, updated_by = ? WHERE setting_key = ?`),
			toJSON(st), ts(time.Now()), by, "settings")
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}
		_, err = tx.ExecContext(ctx, s.rebind(`INSERT INTO settings (setting_key, setting_value, updated_at, updated_by) VALUES (?, ?, ?, ?)`),
			"settings", toJSON(st), ts(time.Now()), by)
		return err
	})
}

// ---- users -----------------------------------------------------------------

const userCols = `id, username, password_hash, role, disabled, failed_logins, locked_until, created_at, last_login_at, auth_source, external_id`

func scanUser(sc interface{ Scan(...any) error }) (model.User, error) {
	var u model.User
	var disabled int
	var locked, created, last string
	err := sc.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &disabled, &u.FailedLogins, &locked, &created, &last, &u.AuthSource, &u.ExternalID)
	u.Disabled = disabled != 0
	u.LockedUntil = parseTSP(locked)
	u.CreatedAt = parseTS(created)
	u.LastLoginAt = parseTSP(last)
	return u, err
}

func (s *Store) CreateUser(ctx context.Context, u model.User) error {
	if u.AuthSource == "" {
		u.AuthSource = model.AuthLocal
	}
	_, err := s.exec(ctx, `INSERT INTO users (`+userCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		u.ID, u.Username, u.PasswordHash, u.Role, b2i(u.Disabled), 0, "", ts(u.CreatedAt), tsp(u.LastLoginAt), u.AuthSource, u.ExternalID)
	if IsUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

func (s *Store) getUser(ctx context.Context, col string, val string) (model.User, error) {
	u, err := scanUser(s.queryRow(ctx, `SELECT `+userCols+` FROM users WHERE `+col+` = ?`, val))
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

func (s *Store) GetUser(ctx context.Context, id string) (model.User, error) {
	return s.getUser(ctx, "id", id)
}

func (s *Store) GetUserByName(ctx context.Context, name string) (model.User, error) {
	return s.getUser(ctx, "username", name)
}

// GetUserByExternalID finds an Entra ID (OIDC) account by "<tenant id>:<object id>".
func (s *Store) GetUserByExternalID(ctx context.Context, externalID string) (model.User, error) {
	if externalID == "" {
		return model.User{}, ErrNotFound
	}
	return s.getUser(ctx, "external_id", externalID)
}

func (s *Store) ListUsers(ctx context.Context) ([]model.User, error) {
	rows, err := s.query(ctx, `SELECT `+userCols+` FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) CountUsers(ctx context.Context) (int, error) {
	return s.count(ctx, "users", &where{})
}

// CountActiveAdmins counts enabled admin users (used to prevent locking everyone out).
func (s *Store) CountActiveAdmins(ctx context.Context) (int, error) {
	w := &where{}
	w.add("role = ?", model.RoleAdmin)
	w.add("disabled = 0")
	return s.count(ctx, "users", w)
}

func (s *Store) UpdateUser(ctx context.Context, u model.User) error {
	n, err := s.affected(ctx, `UPDATE users SET username = ?, password_hash = ?, role = ?, disabled = ? WHERE id = ?`,
		u.Username, u.PasswordHash, u.Role, b2i(u.Disabled), u.ID)
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

func (s *Store) DeleteUser(ctx context.Context, id string) error {
	if _, err := s.exec(ctx, `DELETE FROM sessions WHERE user_id = ?`, id); err != nil {
		return err
	}
	n, err := s.affected(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordLoginFailure increments the counter and applies a lockout when it reaches max.
func (s *Store) RecordLoginFailure(ctx context.Context, id string, max int, lock time.Duration) error {
	u, err := s.GetUser(ctx, id)
	if err != nil {
		return err
	}
	fails := u.FailedLogins + 1
	locked := ""
	if fails >= max {
		locked = ts(time.Now().Add(lock))
		fails = 0
	}
	_, err = s.exec(ctx, `UPDATE users SET failed_logins = ?, locked_until = ? WHERE id = ?`, fails, locked, id)
	return err
}

func (s *Store) RecordLoginSuccess(ctx context.Context, id string) error {
	_, err := s.exec(ctx, `UPDATE users SET failed_logins = 0, locked_until = '', last_login_at = ? WHERE id = ?`, ts(time.Now()), id)
	return err
}

// ---- sessions --------------------------------------------------------------

func (s *Store) CreateSession(ctx context.Context, se model.Session) error {
	_, err := s.exec(ctx, `INSERT INTO sessions (token_hash, user_id, csrf_token, created_at, expires_at, last_seen_at) VALUES (?,?,?,?,?,?)`,
		se.TokenHash, se.UserID, se.CSRFToken, ts(se.CreatedAt), ts(se.ExpiresAt), ts(se.LastSeenAt))
	return err
}

func (s *Store) GetSession(ctx context.Context, tokenHash string) (model.Session, error) {
	var se model.Session
	var c, e, l string
	err := s.queryRow(ctx, `SELECT token_hash, user_id, csrf_token, created_at, expires_at, last_seen_at FROM sessions WHERE token_hash = ?`, tokenHash).
		Scan(&se.TokenHash, &se.UserID, &se.CSRFToken, &c, &e, &l)
	if errors.Is(err, sql.ErrNoRows) {
		return se, ErrNotFound
	}
	se.CreatedAt, se.ExpiresAt, se.LastSeenAt = parseTS(c), parseTS(e), parseTS(l)
	return se, err
}

func (s *Store) TouchSession(ctx context.Context, tokenHash string, expires time.Time) error {
	_, err := s.exec(ctx, `UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE token_hash = ?`, ts(time.Now()), ts(expires), tokenHash)
	return err
}

func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.exec(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	return err
}

func (s *Store) DeleteUserSessions(ctx context.Context, userID string) error {
	_, err := s.exec(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}

func (s *Store) DeleteExpiredSessions(ctx context.Context) error {
	_, err := s.exec(ctx, `DELETE FROM sessions WHERE expires_at < ?`, ts(time.Now()))
	return err
}

// ---- sources ---------------------------------------------------------------

const sourceCols = `id, name, source_type, enabled, options_json, config_json, created_at, updated_at, last_sync_at, last_status, last_error`

func scanSource(sc interface{ Scan(...any) error }) (model.Source, error) {
	var x model.Source
	var en int
	var opts, cfg, c, u, l string
	err := sc.Scan(&x.ID, &x.Name, &x.Type, &en, &opts, &cfg, &c, &u, &l, &x.LastStatus, &x.LastError)
	x.Enabled = en != 0
	fromJSON(opts, &x.Options)
	x.Config = map[string]any{}
	fromJSON(cfg, &x.Config)
	if x.Options.TerminatedValues == nil {
		x.Options.TerminatedValues = []string{}
	}
	x.CreatedAt, x.UpdatedAt, x.LastSyncAt = parseTS(c), parseTS(u), parseTSP(l)
	return x, err
}

func (s *Store) CreateSource(ctx context.Context, x model.Source) error {
	_, err := s.exec(ctx, `INSERT INTO sources (`+sourceCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		x.ID, x.Name, x.Type, b2i(x.Enabled), toJSON(x.Options), toJSON(x.Config), ts(x.CreatedAt), ts(x.UpdatedAt), "", orDefault(x.LastStatus, "never"), x.LastError)
	if IsUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func (s *Store) GetSource(ctx context.Context, id string) (model.Source, error) {
	x, err := scanSource(s.queryRow(ctx, `SELECT `+sourceCols+` FROM sources WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return x, ErrNotFound
	}
	return x, err
}

func (s *Store) ListSources(ctx context.Context) ([]model.Source, error) {
	rows, err := s.query(ctx, `SELECT `+sourceCols+` FROM sources ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Source{}
	for rows.Next() {
		x, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Store) UpdateSource(ctx context.Context, x model.Source) error {
	n, err := s.affected(ctx, `UPDATE sources SET name = ?, source_type = ?, enabled = ?, options_json = ?, config_json = ?, updated_at = ? WHERE id = ?`,
		x.Name, x.Type, b2i(x.Enabled), toJSON(x.Options), toJSON(x.Config), ts(time.Now()), x.ID)
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

func (s *Store) SetSourceStatus(ctx context.Context, id, status, errMsg string) error {
	_, err := s.exec(ctx, `UPDATE sources SET last_sync_at = ?, last_status = ?, last_error = ? WHERE id = ?`, ts(time.Now()), status, errMsg, id)
	return err
}

// DeleteSource refuses while identities (or manual people) still reference the source.
func (s *Store) DeleteSource(ctx context.Context, id string) error {
	w := &where{}
	w.add("source_id = ?", id)
	n, err := s.count(ctx, "identities", w)
	if err != nil {
		return err
	}
	if n == 0 {
		if n, err = s.count(ctx, "manual_people", w); err != nil {
			return err
		}
	}
	if n > 0 {
		return ErrConflict
	}
	c, err := s.affected(ctx, `DELETE FROM sources WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if c == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- mappings --------------------------------------------------------------

const mappingCols = `id, name, target_attr, template, mode, on_empty, value_type, enabled, sort_order, description, created_at, updated_at`

func scanMapping(sc interface{ Scan(...any) error }) (model.Mapping, error) {
	var m model.Mapping
	var en int
	var c, u string
	err := sc.Scan(&m.ID, &m.Name, &m.TargetAttr, &m.Template, &m.Mode, &m.OnEmpty, &m.ValueType, &en, &m.SortOrder, &m.Description, &c, &u)
	m.Enabled = en != 0
	m.CreatedAt, m.UpdatedAt = parseTS(c), parseTS(u)
	return m, err
}

func (s *Store) CreateMapping(ctx context.Context, m model.Mapping) error {
	_, err := s.exec(ctx, `INSERT INTO mappings (`+mappingCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.Name, m.TargetAttr, m.Template, m.Mode, m.OnEmpty, m.ValueType, b2i(m.Enabled), m.SortOrder, m.Description, ts(m.CreatedAt), ts(m.UpdatedAt))
	if IsUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

func (s *Store) GetMapping(ctx context.Context, id string) (model.Mapping, error) {
	m, err := scanMapping(s.queryRow(ctx, `SELECT `+mappingCols+` FROM mappings WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return m, ErrNotFound
	}
	return m, err
}

func (s *Store) ListMappings(ctx context.Context) ([]model.Mapping, error) {
	rows, err := s.query(ctx, `SELECT `+mappingCols+` FROM mappings ORDER BY sort_order, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Mapping{}
	for rows.Next() {
		m, err := scanMapping(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) UpdateMapping(ctx context.Context, m model.Mapping) error {
	n, err := s.affected(ctx, `UPDATE mappings SET name = ?, target_attr = ?, template = ?, mode = ?, on_empty = ?, value_type = ?, enabled = ?, sort_order = ?, description = ?, updated_at = ? WHERE id = ?`,
		m.Name, m.TargetAttr, m.Template, m.Mode, m.OnEmpty, m.ValueType, b2i(m.Enabled), m.SortOrder, m.Description, ts(time.Now()), m.ID)
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

func (s *Store) DeleteMapping(ctx context.Context, id string) error {
	n, err := s.affected(ctx, `DELETE FROM mappings WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- role rules ------------------------------------------------------------

const roleCols = `id, name, description, enabled, priority, match_mode, conditions_json, grants_json, created_at, updated_at`

func scanRole(sc interface{ Scan(...any) error }) (model.RoleRule, error) {
	var r model.RoleRule
	var en int
	var cond, grants, c, u string
	err := sc.Scan(&r.ID, &r.Name, &r.Description, &en, &r.Priority, &r.Match, &cond, &grants, &c, &u)
	r.Enabled = en != 0
	r.Conditions = []model.Condition{}
	fromJSON(cond, &r.Conditions)
	fromJSON(grants, &r.Grants)
	if r.Grants.Groups == nil {
		r.Grants.Groups = []string{}
	}
	if r.Grants.Attributes == nil {
		r.Grants.Attributes = []model.AttrGrant{}
	}
	r.CreatedAt, r.UpdatedAt = parseTS(c), parseTS(u)
	return r, err
}

func (s *Store) CreateRole(ctx context.Context, r model.RoleRule) error {
	_, err := s.exec(ctx, `INSERT INTO role_rules (`+roleCols+`) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Name, r.Description, b2i(r.Enabled), r.Priority, r.Match, toJSON(r.Conditions), toJSON(r.Grants), ts(r.CreatedAt), ts(r.UpdatedAt))
	if IsUniqueViolation(err) {
		return ErrConflict
	}
	return err
}

func (s *Store) GetRole(ctx context.Context, id string) (model.RoleRule, error) {
	r, err := scanRole(s.queryRow(ctx, `SELECT `+roleCols+` FROM role_rules WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

func (s *Store) ListRoles(ctx context.Context) ([]model.RoleRule, error) {
	rows, err := s.query(ctx, `SELECT `+roleCols+` FROM role_rules ORDER BY priority, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.RoleRule{}
	for rows.Next() {
		r, err := scanRole(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) UpdateRole(ctx context.Context, r model.RoleRule) error {
	n, err := s.affected(ctx, `UPDATE role_rules SET name = ?, description = ?, enabled = ?, priority = ?, match_mode = ?, conditions_json = ?, grants_json = ?, updated_at = ? WHERE id = ?`,
		r.Name, r.Description, b2i(r.Enabled), r.Priority, r.Match, toJSON(r.Conditions), toJSON(r.Grants), ts(time.Now()), r.ID)
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

func (s *Store) DeleteRole(ctx context.Context, id string) error {
	n, err := s.affected(ctx, `DELETE FROM role_rules WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
