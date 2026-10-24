//go:build windows

package datadir

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func icacls(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("icacls", args...).CombinedOutput(); err != nil {
		t.Fatalf("icacls %v: %v %s", args, err, out)
	}
}

func TestCheckDetectsUsersReadAndLockdownFixesIt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.key"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// What a data dir under ProgramData typically inherits: Users may read files.
	icacls(t, dir, "/grant", "*S-1-5-32-545:(OI)(CI)(RX)")

	err := Check(dir)
	var ie *InsecureError
	if !errors.As(err, &ie) {
		t.Fatalf("expected InsecureError, got %v", err)
	}
	if !strings.Contains(err.Error(), "BUILTIN\\Users") || !strings.Contains(err.Error(), "secret.key") {
		t.Errorf("error should name the principal and the key file: %v", err)
	}

	if err := Lockdown(dir); err != nil {
		t.Fatal(err)
	}
	if err := Check(dir); err != nil {
		t.Fatalf("after Lockdown: %v", err)
	}
	// The service still works for the current user (it was granted explicitly).
	if _, err := os.ReadFile(filepath.Join(dir, "secret.key")); err != nil {
		t.Fatalf("current user lost access: %v", err)
	}
	// New files inherit the locked-down ACL.
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Check(dir); err != nil {
		t.Fatalf("new file not covered: %v", err)
	}
}

func TestLockdownGrantsNamedAccountAndRejectsUnknown(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := Lockdown(dir, "S-1-5-20"); err != nil { // NETWORK SERVICE by SID
		t.Fatal(err)
	}
	out, _ := exec.Command("icacls", dir).CombinedOutput()
	if !strings.Contains(strings.ToUpper(string(out)), "NETWORK SERVICE") {
		t.Errorf("NETWORK SERVICE not granted:\n%s", out)
	}
	if err := Lockdown(dir, `NO-SUCH-DOMAIN\no-such-account-xyz`); err == nil {
		t.Error("an unresolvable account must be an error")
	}
}

func TestCheckMissingDirIsFine(t *testing.T) {
	if err := Check(filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Fatal(err)
	}
}
