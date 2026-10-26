package backup

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"idlifecycle/internal/model"
	"idlifecycle/internal/store"
	"idlifecycle/internal/store/storetest"
)

const pass = "correct horse battery"

func seed(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	s := model.DefaultSettings()
	s.AD.UPNSuffix = "corp.example.com"
	if err := st.SaveSettings(ctx, s, "test"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	if err := st.CreateUser(ctx, model.User{ID: "u1", Username: "admin", PasswordHash: "x", Role: model.RoleAdmin, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateSource(ctx, model.Source{ID: "s1", Name: "HR \"CSV\" ü", Type: "csv", Enabled: true, CreatedAt: now, UpdatedAt: now,
		Config: map[string]any{"path": `C:\hr\x.csv`, "password": "enc:v1:abc"}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := st.AppendAudit(ctx, "alice", "settings.update", "settings", "", map[string]any{"n": i, "note": "line\nbreak"}); err != nil {
			t.Fatal(err)
		}
	}
}

func create(t *testing.T, st *store.Store) []byte {
	t.Helper()
	var buf bytes.Buffer
	_, err := Create(context.Background(), &buf, st, Options{Passphrase: pass, AppVersion: "1.2.3", ConfigJSON: []byte(`{"listen":"0.0.0.0:8443"}`), Key: bytes.Repeat([]byte{7}, 32), ScryptN: 1 << 10})
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestRoundTripIntoAnotherDatabase(t *testing.T) {
	ctx := context.Background()
	src := storetest.New(t)
	src.SetAuditKey([]byte("audit-key"))
	seed(t, src)
	file := create(t, src)

	h, err := ReadHeader(bytes.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	if h.AppVersion != "1.2.3" || h.Driver != store.DriverSQLite || h.Tables["users"] != 1 || h.Tables["audit_log"] != 3 || h.Schema != store.SchemaVersion() {
		t.Errorf("header: %+v", h)
	}
	for _, secret := range []string{"admin", "x.csv", "listen", "enc:v1:abc"} {
		if bytes.Contains(file, []byte(secret)) {
			t.Errorf("%q is readable in the backup file", secret)
		}
	}

	dst := storetest.New(t)
	dst.SetAuditKey([]byte("audit-key"))
	c, err := Restore(ctx, bytes.NewReader(file), pass, dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(c.ConfigJSON) != `{"listen":"0.0.0.0:8443"}` || !bytes.Equal(c.Key, bytes.Repeat([]byte{7}, 32)) || c.Rows["sources"] != 1 {
		t.Errorf("contents: %+v", c)
	}
	users, _ := dst.ListUsers(ctx)
	if len(users) != 1 || users[0].Username != "admin" || users[0].Role != model.RoleAdmin {
		t.Errorf("users: %+v", users)
	}
	srcs, _ := dst.ListSources(ctx)
	if len(srcs) != 1 || srcs[0].Name != `HR "CSV" ü` || srcs[0].Config["password"] != "enc:v1:abc" {
		t.Errorf("sources: %+v", srcs)
	}
	s, _ := dst.GetSettings(ctx)
	if s.AD.UPNSuffix != "corp.example.com" {
		t.Errorf("settings: %+v", s.AD)
	}
	// Same rows, same audit key: the hash chain still verifies.
	v, err := dst.VerifyAudit(ctx)
	if err != nil || !v.OK || v.Checked != 3 {
		t.Errorf("audit chain after restore: %+v %v", v, err)
	}
	// A second restore replaces (not appends).
	if _, err := Restore(ctx, bytes.NewReader(file), pass, dst); err != nil {
		t.Fatal(err)
	}
	if n, _ := dst.DataRows(ctx); n["audit_log"] != 3 || n["users"] != 1 {
		t.Errorf("rows after a second restore: %v", n)
	}
}

func TestDamagedOrWrongBackupsChangeNothing(t *testing.T) {
	ctx := context.Background()
	src := storetest.New(t)
	seed(t, src)
	file := create(t, src)

	dst := storetest.New(t)
	if err := dst.CreateUser(ctx, model.User{ID: "keep", Username: "keepme", PasswordHash: "x", Role: model.RoleViewer}); err != nil {
		t.Fatal(err)
	}
	unchanged := func(what string) {
		t.Helper()
		users, _ := dst.ListUsers(ctx)
		if len(users) != 1 || users[0].Username != "keepme" {
			t.Fatalf("%s: the target database was changed: %+v", what, users)
		}
	}

	if _, err := Restore(ctx, bytes.NewReader(file), "wrong passphrase!!", dst); !errors.Is(err, ErrPassphrase) {
		t.Errorf("wrong passphrase: %v", err)
	}
	unchanged("wrong passphrase")

	flipped := append([]byte(nil), file...)
	flipped[len(flipped)-40] ^= 0xff
	if _, err := Restore(ctx, bytes.NewReader(flipped), pass, dst); !errors.Is(err, ErrPassphrase) {
		t.Errorf("tampered: %v", err)
	}
	unchanged("tampered")

	// Truncated anywhere - also right before the final chunk - fails inside the transaction.
	for _, cut := range []int{len(file) - 1, len(file) - 30, len(file) / 2} {
		if _, err := Restore(ctx, bytes.NewReader(file[:cut]), pass, dst); err == nil {
			t.Errorf("truncated at %d accepted", cut)
		}
		unchanged("truncated")
	}

	if _, err := Restore(ctx, bytes.NewReader(append(append([]byte(nil), file...), 'x')), pass, dst); err == nil {
		t.Error("data after the final chunk accepted")
	}
	unchanged("trailing data")

	// The header is bound to the content: editing it breaks decryption...
	edited := bytes.Replace(file, []byte(`"app_version":"1.2.3"`), []byte(`"app_version":"9.9.9"`), 1)
	if _, err := Restore(ctx, bytes.NewReader(edited), pass, dst); !errors.Is(err, ErrPassphrase) {
		t.Errorf("edited header: %v", err)
	}
	// ...and a newer schema is refused before anything is decrypted.
	newer := bytes.Replace(file, []byte(`"schema":`), []byte(`"schema":9`), 1)
	if _, err := Restore(ctx, bytes.NewReader(newer), pass, dst); err == nil || !strings.Contains(err.Error(), "newer version") {
		t.Errorf("newer schema: %v", err)
	}
	unchanged("header edits")

	if _, err := ReadHeader(strings.NewReader("hello\n")); err == nil {
		t.Error("a non-backup file must be recognized as such")
	}
}

func TestCreateRules(t *testing.T) {
	st := storetest.New(t)
	var buf bytes.Buffer
	if _, err := Create(context.Background(), &buf, st, Options{Passphrase: "short", Key: make([]byte, 32)}); err == nil {
		t.Error("a short passphrase must be refused")
	}
	if _, err := Create(context.Background(), &buf, st, Options{Passphrase: pass, Key: make([]byte, 16)}); err == nil {
		t.Error("a key that is not 32 bytes must be refused")
	}
	// An empty database still makes a valid, restorable backup.
	buf.Reset()
	if _, err := Create(context.Background(), &buf, st, Options{Passphrase: pass, Key: make([]byte, 32), ScryptN: 1 << 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(context.Background(), bytes.NewReader(buf.Bytes()), pass, storetest.New(t)); err != nil {
		t.Fatal(err)
	}
}

func TestLargeTableSpansParts(t *testing.T) {
	ctx := context.Background()
	src := storetest.New(t)
	big := strings.Repeat("x", 20000)
	for i := 0; i < 600; i++ { // ~12 MB of audit detail: more than one 8 MB part
		if err := src.AppendAudit(ctx, "bulk", "test.bulk", "x", "", map[string]any{"i": i, "pad": big}); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if _, err := Create(ctx, &buf, src, Options{Passphrase: pass, Key: make([]byte, 32), ScryptN: 1 << 10}); err != nil {
		t.Fatal(err)
	}
	dst := storetest.New(t)
	c, err := Restore(ctx, bytes.NewReader(buf.Bytes()), pass, dst)
	if err != nil {
		t.Fatal(err)
	}
	if c.Rows["audit_log"] != 600 {
		t.Errorf("rows: %v", c.Rows)
	}
}
