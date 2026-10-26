package api

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"idlifecycle/internal/backup"
	"idlifecycle/internal/config"
	"idlifecycle/internal/model"
	"idlifecycle/internal/secrets"
	"idlifecycle/internal/store/storetest"
)

func TestBackupsAndHistorySearch(t *testing.T) {
	e := newTenv(t)
	e.api.Config = config.Config{DataDir: t.TempDir()}
	admin := e.login("admin")

	var list struct {
		Folder        string
		Files         []backup.FileInfo
		PassphraseSet bool `json:"passphrase_set"`
	}
	want(t, "list", admin.do("GET", "/api/backups", nil, &list), 200)
	if list.PassphraseSet || len(list.Files) != 0 || !strings.HasSuffix(list.Folder, "backups") {
		t.Errorf("empty list: %+v", list)
	}
	want(t, "no passphrase yet", admin.do("POST", "/api/backups", nil, nil), 400)

	var s model.Settings
	admin.do("GET", "/api/settings", nil, &s)
	bad := s
	bad.Backup.Passphrase = "short"
	want(t, "short passphrase", admin.do("PUT", "/api/settings", bad, nil), 400)
	bad = s
	bad.Backup.Enabled = true
	want(t, "enabled without passphrase", admin.do("PUT", "/api/settings", bad, nil), 400)
	s.Backup.Passphrase, s.Backup.Keep = "backup passphrase 1", 1
	var saved model.Settings
	want(t, "save passphrase", admin.do("PUT", "/api/settings", s, &saved), 200)
	if saved.Backup.Passphrase != model.MaskedSecret {
		t.Errorf("passphrase must come back masked: %q", saved.Backup.Passphrase)
	}
	stored, _ := e.st.GetSettings(context.Background())
	if !secrets.IsEncrypted(stored.Backup.Passphrase) {
		t.Error("the passphrase must be stored encrypted")
	}
	// Saving the masked value keeps it.
	want(t, "save again", admin.do("PUT", "/api/settings", saved, nil), 200)
	if again, _ := e.st.GetSettings(context.Background()); again.Backup.Passphrase != stored.Backup.Passphrase {
		t.Error("a masked passphrase must keep the stored one")
	}

	var fi backup.FileInfo
	want(t, "back up now", admin.do("POST", "/api/backups", nil, &fi), 201)
	if !backup.ValidName(fi.Name) || fi.Size == 0 {
		t.Errorf("created: %+v", fi)
	}
	time.Sleep(1100 * time.Millisecond) // file names carry seconds
	var fi2 backup.FileInfo
	want(t, "back up again", admin.do("POST", "/api/backups", nil, &fi2), 201)
	admin.do("GET", "/api/backups", nil, &list)
	if len(list.Files) != 1 || list.Files[0].Name != fi2.Name {
		t.Errorf("keep=1 must prune the older backup: %+v", list.Files)
	}

	// Download (admins only) and restore it elsewhere.
	viewer := e.login("viewer")
	want(t, "viewer cannot download", viewer.do("GET", "/api/backups/"+fi2.Name, nil, nil), 403)
	want(t, "unknown name", admin.do("GET", "/api/backups/..%5Cconfig.json", nil, nil), 404)
	resp, body := admin.raw("GET", "/api/backups/"+fi2.Name, nil, nil)
	if resp.StatusCode != 200 || !bytes.HasPrefix(body, []byte("IDLBAK1\n")) || !strings.Contains(resp.Header.Get("Content-Disposition"), fi2.Name) {
		t.Fatalf("download: %d %q", resp.StatusCode, resp.Header.Get("Content-Disposition"))
	}
	dst := storetest.New(t)
	c, err := backup.Restore(context.Background(), bytes.NewReader(body), "backup passphrase 1", dst)
	if err != nil || c.Rows["users"] != 3 {
		t.Fatalf("restore of the downloaded backup: %v %+v", err, c.Rows)
	}

	// History search over what just happened.
	var hits struct {
		Items []model.AuditEntry
		Total int
	}
	want(t, "search", admin.do("GET", "/api/audit?q="+urlq("action:backup.create"), nil, &hits), 200)
	if hits.Total != 2 {
		t.Errorf("backup.create twice: %+v", hits)
	}
	want(t, "bad query", admin.do("GET", "/api/audit?q="+urlq("colour:red"), nil, nil), 400)
	resp, csvBody := admin.raw("GET", "/api/audit?format=csv&q="+urlq("action:backup.create"), nil, nil)
	lines := strings.Split(strings.TrimSpace(string(csvBody)), "\n")
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") || len(lines) != 3 || !strings.HasPrefix(lines[0], "seq,time_utc,actor,action") {
		t.Errorf("csv export: %d %q", resp.StatusCode, csvBody)
	}

	// The schedule: due when there is no backup or the newest is interval_hours old.
	s.Backup.Enabled, s.Backup.IntervalHours = true, 24
	want(t, "enable schedule", admin.do("PUT", "/api/settings", s, nil), 200)
	ran, _, err := backup.RunIfDue(context.Background(), e.st, e.api.Config, e.api.Cipher, "test", time.Now())
	if err != nil || ran {
		t.Errorf("a fresh backup exists, nothing is due: %v %v", ran, err)
	}
	ran, _, err = backup.RunIfDue(context.Background(), e.st, e.api.Config, e.api.Cipher, "test", time.Now().Add(25*time.Hour))
	if err != nil || !ran {
		t.Errorf("25 hours later a backup is due: %v %v", ran, err)
	}
}

func urlq(s string) string {
	return strings.NewReplacer(" ", "%20", "*", "%2A", ":", "%3A", "\"", "%22").Replace(s)
}
