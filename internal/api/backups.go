package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"idlifecycle/internal/backup"
	"idlifecycle/internal/model"
	"idlifecycle/internal/secrets"
)

// prepareBackup encrypts the backup passphrase for storage; model.MaskedSecret or "" keeps the stored one.
func prepareBackup(in, prev model.BackupSettings, c *secrets.Cipher) (model.BackupSettings, error) {
	out := in
	switch v := in.Passphrase; {
	case v == model.MaskedSecret || v == "":
		out.Passphrase = prev.Passphrase
	case secrets.IsEncrypted(v):
		return out, fmt.Errorf("backup.passphrase looks like an encrypted value; send the plain passphrase or %q", model.MaskedSecret)
	case len([]rune(v)) < backup.MinPassphrase:
		return out, fmt.Errorf("backup.passphrase must be at least %d characters", backup.MinPassphrase)
	default:
		if c == nil {
			return out, errors.New("backup: no cipher configured")
		}
		enc, err := c.Encrypt(v)
		if err != nil {
			return out, err
		}
		out.Passphrase = enc
	}
	if out.Enabled && out.Passphrase == "" {
		return out, errors.New("backup.passphrase is required for scheduled backups (it is needed to restore them, so keep a copy somewhere safe)")
	}
	return out, nil
}

func maskBackup(b model.BackupSettings) model.BackupSettings {
	if b.Passphrase != "" {
		b.Passphrase = model.MaskedSecret
	}
	return b
}

// handleListBackups: the backup folder, its files (newest first) and when the next scheduled one is due.
func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request) {
	st, err := s.Store.GetSettings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	folder := backup.Folder(s.Config, st.Backup)
	files, err := backup.List(folder)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var next *time.Time
	if st.Backup.Enabled {
		t := time.Now().UTC()
		if len(files) > 0 {
			t = files[0].Created.Add(time.Duration(st.Backup.IntervalHours) * time.Hour)
		}
		next = &t
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"folder": folder, "files": files, "enabled": st.Backup.Enabled, "interval_hours": st.Backup.IntervalHours,
		"keep": st.Backup.Keep, "passphrase_set": st.Backup.Passphrase != "", "next_due": next,
	})
}

// handleCreateBackup makes a backup now (with the stored passphrase).
func (s *Server) handleCreateBackup(w http.ResponseWriter, r *http.Request) {
	fi, err := backup.RunNow(r.Context(), s.Store, s.Config, s.Cipher, s.Version, principal(r).User.Username, "manual")
	if err != nil {
		if errors.Is(err, backup.ErrNoPassphrase) {
			badRequest(w, err.Error())
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "backup failed: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, fi)
}

// handleDownloadBackup streams one backup file (encrypted; useless without the passphrase).
func (s *Server) handleDownloadBackup(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !backup.ValidName(name) {
		http.NotFound(w, r)
		return
	}
	st, err := s.Store.GetSettings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	path := filepath.Join(backup.Folder(s.Config, st.Backup), name)
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		http.NotFound(w, r)
		return
	}
	s.audit(r, "backup.download", "backup", name, map[string]any{"size": fi.Size()})
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeContent(w, r, name, fi.ModTime(), f)
}
