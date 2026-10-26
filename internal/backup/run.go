package backup

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"idlifecycle/internal/config"
	"idlifecycle/internal/model"
	"idlifecycle/internal/secrets"
	"idlifecycle/internal/store"
)

// runMu makes a manual and a scheduled backup never run at the same time.
var runMu sync.Mutex

// ErrNoPassphrase means settings.backup has no passphrase yet.
var ErrNoPassphrase = errors.New("set a backup passphrase in Settings -> Backups first")

// Folder is the configured backup folder, or <data_dir>\backups.
func Folder(cfg config.Config, s model.BackupSettings) string {
	if s.Folder != "" {
		return s.Folder
	}
	return DefaultFolder(cfg)
}

// RunNow makes a backup with the settings' passphrase into the settings' folder, then keeps only the newest
// settings.backup.keep files. actor is recorded in the audit log ("system" for the schedule).
func RunNow(ctx context.Context, st *store.Store, cfg config.Config, c *secrets.Cipher, appVersion, actor, note string) (FileInfo, error) {
	runMu.Lock()
	defer runMu.Unlock()
	s, err := st.GetSettings(ctx)
	if err != nil {
		return FileInfo{}, err
	}
	folder := Folder(cfg, s.Backup)
	fi, h, err := func() (FileInfo, Header, error) {
		if s.Backup.Passphrase == "" {
			return FileInfo{}, Header{}, ErrNoPassphrase
		}
		pass, err := c.Decrypt(s.Backup.Passphrase)
		if err != nil {
			return FileInfo{}, Header{}, fmt.Errorf("the stored backup passphrase cannot be read (%v); enter it again in Settings -> Backups", err)
		}
		return CreateFile(ctx, st, cfg, c, folder, pass, appVersion, note, time.Now())
	}()
	if err != nil {
		_ = st.AppendAudit(ctx, actor, "backup.failed", "backup", "", map[string]any{"error": err.Error(), "folder": folder})
		return FileInfo{}, err
	}
	pruned, perr := Prune(folder, s.Backup.Keep)
	detail := map[string]any{"size": fi.Size, "rows": h.Tables, "folder": folder, "pruned": pruned}
	if perr != nil {
		detail["prune_error"] = perr.Error()
	}
	_ = st.AppendAudit(ctx, actor, "backup.create", "backup", fi.Name, detail)
	return fi, nil
}

// RunIfDue makes a scheduled backup when backups are enabled and the newest one is at least interval_hours old.
// It returns whether a backup was attempted.
func RunIfDue(ctx context.Context, st *store.Store, cfg config.Config, c *secrets.Cipher, appVersion string, now time.Time) (bool, FileInfo, error) {
	s, err := st.GetSettings(ctx)
	if err != nil || !s.Backup.Enabled {
		return false, FileInfo{}, err
	}
	files, err := List(Folder(cfg, s.Backup))
	if err != nil {
		return false, FileInfo{}, err
	}
	if !Due(files, s.Backup.IntervalHours, now) {
		return false, FileInfo{}, nil
	}
	fi, err := RunNow(ctx, st, cfg, c, appVersion, "system", "scheduled")
	return true, fi, err
}
