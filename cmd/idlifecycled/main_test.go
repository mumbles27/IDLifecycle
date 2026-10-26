package main

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
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

// checkJSON parses `check -json` as the installer does and pins its contract: top-level keys
// exactly ok and checks; every check name, in CheckNames order (pinned here too); per check
// exactly name, ok, detail, plus skipped (true) only on checks that did not run.
func checkJSON(t *testing.T, out string) map[string]map[string]any {
	t.Helper()
	want := []string{"config", "data_dir_acl", "database", "admin_user", "powershell", "scripts", "scripts_unblocked", "program_files_acl", "ad_module"}
	if !slices.Equal(CheckNames, want) {
		t.Fatalf("CheckNames changed (the installer depends on it): %v", CheckNames)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("check -json output: %v %s", err, out)
	}
	if keys := slices.Sorted(maps.Keys(doc)); !slices.Equal(keys, []string{"checks", "ok"}) {
		t.Fatalf("top-level keys %v: %s", keys, out)
	}
	checks, _ := doc["checks"].([]any)
	if len(checks) != len(want) {
		t.Fatalf("want %d checks, got %d: %s", len(want), len(checks), out)
	}
	byName := map[string]map[string]any{}
	allOK := true
	for i, c := range checks {
		r := c.(map[string]any)
		if r["name"] != want[i] {
			t.Errorf("check %d is %v, want %s", i, r["name"], want[i])
		}
		keys := slices.Sorted(maps.Keys(r))
		_, isBool := r["ok"].(bool)
		_, isStr := r["detail"].(string)
		switch {
		case !isBool || !isStr:
			t.Errorf("check %v: ok must be a bool and detail a string: %v", r["name"], r)
		case r["skipped"] == nil && !slices.Equal(keys, []string{"detail", "name", "ok"}):
			t.Errorf("check %v keys %v", r["name"], keys)
		case r["skipped"] != nil && (r["skipped"] != true || r["ok"] != false || !slices.Equal(keys, []string{"detail", "name", "ok", "skipped"})):
			t.Errorf("skipped check %v: %v", r["name"], r)
		}
		if r["skipped"] == nil {
			allOK = allOK && r["ok"] == true
		}
		byName[want[i]] = r
	}
	if doc["ok"] != (allOK && byName["config"]["ok"] == true) {
		t.Errorf("top-level ok %v must be: every check that ran passed, and config did: %s", doc["ok"], out)
	}
	return byName
}

func TestInitFlagsAndCheck(t *testing.T) {
	api.BcryptCost = 4
	dir := filepath.Join(t.TempDir(), "data")
	if code, _, errs := run(t, "init", "-dir", dir, "-listen", "0.0.0.0:8443"); code != 2 || !strings.Contains(errs, "loopback") {
		t.Fatalf("non-loopback without TLS must be refused before writing: %d %s", code, errs)
	}
	if code, _, errs := run(t, "init", "-dir", dir, "-db-driver", "mssql"); code != 2 || !strings.Contains(errs, "-db-dsn") {
		t.Fatalf("server driver without dsn: %d %s", code, errs)
	}
	// A DSN without a driver must not silently become a sqlite config.
	if code, _, errs := run(t, "init", "-dir", dir, "-db-dsn", "env:IDL_DB_DSN"); code != 2 || !strings.Contains(errs, "-db-dsn requires -db-driver") {
		t.Fatalf("-db-dsn without -db-driver: %d %s", code, errs)
	}
	for _, dsn := range []string{"Driver={IBM DB2 ODBC DRIVER};Hostname=h;Database=IDLDB;", "dsn=IDLDB", "Database=IDLDB;Uid=idl;Pwd=x"} {
		if code, _, errs := run(t, "init", "-dir", dir, "-db-driver", "sqlite", "-db-dsn", dsn); code != 2 || !strings.Contains(errs, "ODBC") {
			t.Fatalf("sqlite with ODBC dsn %q: %d %s", dsn, code, errs)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err == nil {
		t.Fatal("a refused init must not write a config")
	}
	sqliteDir := filepath.Join(t.TempDir(), "s")
	if code, _, errs := run(t, "init", "-dir", sqliteDir, "-db-driver", "sqlite", "-db-dsn", filepath.Join(sqliteDir, "x.db")); code != 0 {
		t.Fatalf("sqlite with a file path: %d %s", code, errs)
	}
	scripts, _ := filepath.Abs(filepath.Join("..", "..", "scripts"))
	if code, _, errs := run(t, "init", "-dir", dir, "-scripts", scripts, "-listen", "127.0.0.1:18999"); code != 0 {
		t.Fatalf("init: %d %s", code, errs)
	}
	cfgPath := filepath.Join(dir, "config.json")
	c, err := config.Load(cfgPath)
	if err != nil || c.Listen != "127.0.0.1:18999" || c.PowerShell.ScriptsDir != scripts {
		t.Fatalf("init flags not written: %+v %v", c, err)
	}

	// No admin yet: check fails and says how to fix it.
	code, out, _ := run(t, "check", "-config", cfgPath, "-json", "-skip-ad-module")
	byName := checkJSON(t, out)
	if code != 1 || byName["admin_user"]["ok"] != false || !strings.Contains(byName["admin_user"]["detail"].(string), "user add") {
		t.Fatalf("check must fail on a missing admin: %d %s", code, out)
	}
	for _, n := range []string{"config", "data_dir_acl", "database", "scripts"} {
		if byName[n]["ok"] != true {
			t.Errorf("%s should pass: %+v", n, byName[n])
		}
	}
	if r := byName["ad_module"]; r["skipped"] != true || r["ok"] != false || !strings.Contains(r["detail"].(string), "-skip-ad-module") {
		t.Errorf("ad_module must be reported as skipped: %+v", r)
	}

	// A config that does not load: every check is still listed, the rest skipped, ok false.
	code, out, _ = run(t, "check", "-config", filepath.Join(dir, "missing.json"), "-json")
	byName = checkJSON(t, out)
	if code != 1 || byName["config"]["ok"] != false || byName["config"]["skipped"] != nil {
		t.Fatalf("missing config: %d %s", code, out)
	}
	for _, n := range CheckNames[1:] {
		if byName[n]["skipped"] != true || byName[n]["ok"] != false {
			t.Errorf("%s must be skipped when the config does not load: %+v", n, byName[n])
		}
	}

	t.Setenv("IDL_PASSWORD", "correct-horse-battery")
	if code, _, errs := run(t, "user", "add", "-config", cfgPath, "-username", "root", "-role", "admin"); code != 0 {
		t.Fatalf("user add: %s", errs)
	}
	code, out, _ = run(t, "check", "-config", cfgPath, "-skip-ad-module")
	if !strings.Contains(out, "OK    admin_user") {
		t.Errorf("admin check should pass now:\n%s", out)
	}
	_ = code // scripts_unblocked/powershell depend on this machine; covered by their own tests
}
