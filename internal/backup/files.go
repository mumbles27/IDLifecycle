package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"idlifecycle/internal/config"
	"idlifecycle/internal/secrets"
	"idlifecycle/internal/store"
)

// FileInfo describes one backup file in a folder.
type FileInfo struct {
	Name    string    `json:"name"`
	Path    string    `json:"-"`
	Size    int64     `json:"size"`
	Created time.Time `json:"created"`
}

// fileNameRe matches the files this package writes (the only ones it lists, serves or prunes).
var fileNameRe = regexp.MustCompile(`^IDLifecycle_[A-Za-z0-9_.-]{1,80}_\d{8}_\d{6}(-\d{1,2})?\` + Extension + `$`)

// ValidName reports whether name is a backup file name this package writes.
func ValidName(name string) bool { return fileNameRe.MatchString(name) }

// DefaultFolder is <data_dir>\backups: inside the locked-down data folder, so it has the same protection as
// secret.key (SYSTEM, Administrators, the installing user and the service account).
func DefaultFolder(cfg config.Config) string { return filepath.Join(cfg.DataDir, "backups") }

// CreateFile writes a new backup into folder (created if missing) as IDLifecycle_<host>_<yyyyMMdd_HHmmss>.idlbak,
// through a temporary file so a half-written backup never has a backup's name.
func CreateFile(ctx context.Context, st *store.Store, cfg config.Config, c *secrets.Cipher, folder, passphrase, appVersion, note string, now time.Time) (FileInfo, Header, error) {
	if err := os.MkdirAll(folder, 0o700); err != nil {
		return FileInfo{}, Header{}, err
	}
	var cfgJSON []byte
	if p := cfg.Path(); p != "" {
		b, err := os.ReadFile(p) // #nosec G304 -- a file in the protected data folder
		if err != nil {
			return FileInfo{}, Header{}, fmt.Errorf("read %s: %w", p, err)
		}
		cfgJSON = b
	}
	host, _ := os.Hostname()
	host = regexp.MustCompile(`[^A-Za-z0-9.-]`).ReplaceAllString(host, "")
	if host == "" {
		host = "host"
	}
	base := fmt.Sprintf("IDLifecycle_%s_%s", host, now.Local().Format("20060102_150405"))
	name := base + Extension
	for i := 2; fileExists(filepath.Join(folder, name)); i++ { // two backups in the same second
		if i > 99 {
			return FileInfo{}, Header{}, fmt.Errorf("too many backups named %s*", base)
		}
		name = fmt.Sprintf("%s-%d%s", base, i, Extension)
	}
	final := filepath.Join(folder, name)
	tmp := final + ".partial"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // #nosec G304 -- a new temp file in the backup folder
	if err != nil {
		return FileInfo{}, Header{}, err
	}
	h, err := Create(ctx, f, st, Options{Passphrase: passphrase, AppVersion: appVersion, ConfigJSON: cfgJSON, Key: c.KeyBytes(), Note: note, Now: func() time.Time { return now }})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return FileInfo{}, Header{}, err
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return FileInfo{}, Header{}, err
	}
	fi, err := os.Stat(final)
	if err != nil {
		return FileInfo{}, Header{}, err
	}
	return FileInfo{Name: name, Path: final, Size: fi.Size(), Created: h.Created}, h, nil
}

// List returns the backups in folder, newest first ("" or a missing folder: none).
func List(folder string) ([]FileInfo, error) {
	entries, err := os.ReadDir(folder)
	if errors.Is(err, os.ErrNotExist) {
		return []FileInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []FileInfo{}
	for _, e := range entries {
		if e.IsDir() || !ValidName(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, FileInfo{Name: e.Name(), Path: filepath.Join(folder, e.Name()), Size: info.Size(), Created: info.ModTime().UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// Prune deletes all but the newest keep backups in folder; it returns the names it deleted.
func Prune(folder string, keep int) ([]string, error) {
	files, err := List(folder)
	if err != nil || keep < 1 || len(files) <= keep {
		return nil, err
	}
	var gone []string
	for _, f := range files[keep:] {
		if err := os.Remove(f.Path); err != nil {
			return gone, err
		}
		gone = append(gone, f.Name)
	}
	return gone, nil
}

// Due reports whether a scheduled backup should run: no backup yet, or the newest is intervalHours old.
func Due(files []FileInfo, intervalHours int, now time.Time) bool {
	if len(files) == 0 {
		return true
	}
	return now.Sub(files[0].Created) >= time.Duration(intervalHours)*time.Hour
}

// RestoreOptions for RestoreFile.
type RestoreOptions struct {
	Passphrase string
	// Force allows replacing a database that already has users, sources, identities or history.
	Force bool
	// WithConfig also takes the backup's config.json, except data_dir, powershell.scripts_dir and database, which
	// stay as they are on this server.
	WithConfig bool
	Actor      string
	Now        time.Time
}

// RestoreResult reports what RestoreFile did.
type RestoreResult struct {
	Header          Header
	Rows            map[string]int
	KeyReplaced     string // the previous secret.key, renamed ("" when there was none)
	ConfigReplaced  string // the previous config.json, renamed ("" when the config was not restored)
	AuditChainValid bool
}

// RestoreFile restores a backup into the database named by the config at cfgPath, then puts the backup's master key
// in place (so the restored secrets can be read) and, with WithConfig, its settings in config.json. The service must
// be stopped while it runs, and started again afterwards.
func RestoreFile(ctx context.Context, cfgPath, inPath string, o RestoreOptions) (RestoreResult, error) {
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return RestoreResult{}, err
	}
	dsn, err := cfg.ResolvedDSN()
	if err != nil {
		return RestoreResult{}, err
	}
	st, err := store.Open(ctx, cfg.Database.Driver, dsn)
	if err != nil {
		return RestoreResult{}, err
	}
	defer st.Close()

	rows, err := st.DataRows(ctx)
	if err != nil {
		return RestoreResult{}, err
	}
	if !o.Force {
		var busy []string
		for _, t := range store.BackupTables {
			if t != "settings" && rows[t] > 0 {
				busy = append(busy, fmt.Sprintf("%s: %d", t, rows[t]))
			}
		}
		if len(busy) > 0 {
			return RestoreResult{}, fmt.Errorf("the database already has data (%s); restoring replaces ALL of it - pass -force to do that", strings.Join(busy, ", "))
		}
	}
	// IDL_MASTER_KEY overrides secret.key: the restore can only work if it already holds the backup's key.
	envKey := strings.TrimSpace(os.Getenv("IDL_MASTER_KEY"))

	f, err := os.Open(inPath) // #nosec G304 -- the backup file an administrator named on the command line
	if err != nil {
		return RestoreResult{}, err
	}
	defer f.Close()
	c, err := RestoreWith(ctx, f, o.Passphrase, st, func(key []byte) error {
		if envKey == "" {
			return nil
		}
		cur, err := secrets.Load(cfg.DataDir)
		if err != nil {
			return err
		}
		if string(cur.KeyBytes()) != string(key) {
			return errors.New("IDL_MASTER_KEY is set to a different key than the backup's: set it to the old server's key (or unset it) and try again")
		}
		return nil
	})
	if err != nil {
		return RestoreResult{}, err
	}
	res := RestoreResult{Header: c.Header, Rows: c.Rows}
	stamp := o.Now.Format("20060102_150405")
	if envKey == "" {
		if res.KeyReplaced, err = secrets.ReplaceKeyFile(cfg.DataDir, c.Key, stamp); err != nil {
			return res, fmt.Errorf("the database was restored, but writing secret.key failed (stored secrets cannot be read until it is fixed): %w", err)
		}
	}
	if o.WithConfig && len(c.ConfigJSON) > 0 {
		if res.ConfigReplaced, err = mergeConfig(cfgPath, c.ConfigJSON, stamp); err != nil {
			return res, fmt.Errorf("the database was restored, but its config.json could not be applied: %w", err)
		}
	}
	_ = st.ClearSessions(ctx)
	cipher, err := secrets.New(c.Key)
	if err != nil {
		return res, err
	}
	st.SetAuditKey(cipher.Derive(store.AuditKeyLabel))
	v, err := st.VerifyAudit(ctx)
	res.AuditChainValid = err == nil && v.OK
	_ = st.AppendAudit(ctx, firstNonEmpty(o.Actor, "cli"), "backup.restore", "backup", filepath.Base(inPath), map[string]any{
		"created": c.Header.Created, "app_version": c.Header.AppVersion, "from_driver": c.Header.Driver, "rows": c.Rows,
		"with_config": o.WithConfig, "audit_chain_valid": res.AuditChainValid,
	})
	return res, nil
}

// RestoreWith is Restore with a check of the master key before anything is imported.
func RestoreWith(ctx context.Context, r io.Reader, passphrase string, st *store.Store, checkKey func([]byte) error) (Contents, error) {
	return restore(ctx, r, passphrase, st, checkKey)
}

// mergeConfig writes the backup's config.json over cfgPath, keeping this server's data_dir, database and
// powershell.scripts_dir. The previous file is kept as config.json.replaced-<stamp>; the result must load.
func mergeConfig(cfgPath string, backupJSON []byte, stamp string) (string, error) {
	curRaw, err := os.ReadFile(cfgPath) // #nosec G304 -- config.json of this installation
	if err != nil {
		return "", err
	}
	var cur, bak map[string]any
	if err := json.Unmarshal(curRaw, &cur); err != nil {
		return "", err
	}
	if err := json.Unmarshal(backupJSON, &bak); err != nil {
		return "", fmt.Errorf("the backup's config.json is not valid JSON: %w", err)
	}
	bak["data_dir"] = cur["data_dir"]
	bak["database"] = cur["database"]
	ps, _ := bak["powershell"].(map[string]any)
	if ps == nil {
		ps = map[string]any{}
	}
	if curPS, ok := cur["powershell"].(map[string]any); ok {
		ps["scripts_dir"] = curPS["scripts_dir"]
	}
	bak["powershell"] = ps
	out, err := json.MarshalIndent(bak, "", "  ")
	if err != nil {
		return "", err
	}
	tmp := cfgPath + ".restore.tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o600); err != nil {
		return "", err
	}
	if _, err := config.Load(tmp); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("the merged config.json is not valid (kept the current one): %w", err)
	}
	replaced := cfgPath + ".replaced-" + stamp
	if err := os.Rename(cfgPath, replaced); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if err := os.Rename(tmp, cfgPath); err != nil {
		_ = os.Rename(replaced, cfgPath)
		return "", err
	}
	return replaced, nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
