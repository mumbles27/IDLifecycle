package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"idlifecycle/internal/api"
	"idlifecycle/internal/app"
	"idlifecycle/internal/config"
	"idlifecycle/internal/datadir"
)

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := realMain(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestInitAndUserLifecycle(t *testing.T) {
	api.BcryptCost = 4
	dir := t.TempDir()

	code, out, errs := run(t, "init", "-dir", dir)
	if code != 0 || !strings.Contains(out, "DRY-RUN") {
		t.Fatalf("init: %d %s %s", code, out, errs)
	}
	cfg := filepath.Join(dir, "config.json")
	if _, err := os.Stat(cfg); err != nil {
		t.Fatal(err)
	}
	if code, _, errs = run(t, "init", "-dir", dir); code == 0 || !strings.Contains(errs, "refusing to overwrite") {
		t.Fatalf("init must not clobber an existing config: %d %s", code, errs)
	}

	t.Setenv("IDL_PASSWORD", "correct-horse-battery")
	if code, out, errs = run(t, "user", "add", "-config", cfg, "-username", "root", "-role", "admin"); code != 0 {
		t.Fatalf("user add: %d %s %s", code, out, errs)
	}
	if code, _, errs = run(t, "user", "add", "-config", cfg, "-username", "root", "-role", "admin"); code == 0 || !strings.Contains(errs, "already exists") {
		t.Fatalf("duplicate: %d %s", code, errs)
	}
	if code, _, _ = run(t, "user", "add", "-config", cfg, "-username", "x", "-role", "emperor"); code != 2 {
		t.Fatalf("bad role must be a usage error, got %d", code)
	}

	t.Setenv("IDL_PASSWORD", "short")
	if code, _, errs = run(t, "user", "add", "-config", cfg, "-username", "weak", "-role", "viewer"); code == 0 || !strings.Contains(errs, "12 characters") {
		t.Fatalf("weak password must be refused: %d %s", code, errs)
	}

	t.Setenv("IDL_PASSWORD", "another-long-password")
	if code, _, errs = run(t, "user", "passwd", "-config", cfg, "-username", "root"); code != 0 {
		t.Fatalf("passwd: %d %s", code, errs)
	}
	if code, _, _ = run(t, "user", "passwd", "-config", cfg, "-username", "ghost"); code == 0 {
		t.Fatal("unknown user must fail")
	}

	code, out, _ = run(t, "user", "list", "-config", cfg)
	if code != 0 || !strings.Contains(out, "root") || !strings.Contains(out, "admin") || strings.Contains(out, "another-long-password") {
		t.Fatalf("list: %d %s", code, out)
	}

	// Entries written by the CLI must verify with the key the service uses (they once were unkeyed,
	// which broke verification at seq 1 on every install and hid any later tampering).
	c, err := config.Load(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	st, err := app.OpenStore(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.AppendAudit(ctx, "system", "service.start", "service", "", nil) // as the service would, after the CLI
	res, err := st.VerifyAudit(ctx)
	if err != nil || !res.OK || res.Checked < 3 {
		t.Fatalf("audit chain after CLI writes: %+v %v", res, err)
	}
}

func TestInitLocksDownAndServiceRefusesOpenDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if code, _, errs := run(t, "init", "-dir", dir); code != 0 {
		t.Fatalf("init: %d %s", code, errs)
	}
	if err := datadir.Check(dir); err != nil {
		t.Fatalf("init must leave the data directory restricted: %v", err)
	}
	if runtime.GOOS != "windows" {
		return
	}
	cfg := filepath.Join(dir, "config.json")
	if out, err := exec.Command("icacls", dir, "/grant", "*S-1-5-32-545:(OI)(CI)(RX)").CombinedOutput(); err != nil {
		t.Fatalf("icacls: %v %s", err, out)
	}
	if code, _, errs := run(t, "run", "-config", cfg); code == 0 || !strings.Contains(errs, "lockdown") {
		t.Fatalf("run must refuse an open data dir and say how to fix it: %d %s", code, errs)
	}
	if code, out, errs := run(t, "lockdown", "-config", cfg); code != 0 || !strings.Contains(out, "Restricted") {
		t.Fatalf("lockdown: %d %s %s", code, out, errs)
	}
	if err := datadir.Check(dir); err != nil {
		t.Fatalf("after lockdown: %v", err)
	}
}

func TestUsageAndVersion(t *testing.T) {
	if code, out, _ := run(t, "version"); code != 0 || !strings.Contains(out, "idlifecycled") {
		t.Errorf("version: %d %s", code, out)
	}
	if code, _, errs := run(t); code != 2 || !strings.Contains(errs, "Usage") {
		t.Errorf("no args: %d", code)
	}
	if code, _, _ := run(t, "frobnicate"); code != 2 {
		t.Errorf("unknown command must exit 2, got %d", code)
	}
}
