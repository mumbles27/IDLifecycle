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

func TestCheckNotWritableCatchesUsersWriteAndSkipsLogs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "prog")
	scripts := filepath.Join(dir, "scripts")
	logs := filepath.Join(scripts, "Logs")
	if err := os.MkdirAll(logs, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scripts, "New-IDLUser.ps1"), []byte("#"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Lockdown(dir); err != nil { // protected ACL, like Program Files
		t.Fatal(err)
	}
	if err := CheckNotWritable(dir, "Logs"); err != nil {
		t.Fatalf("locked folder reported writable: %v", err)
	}

	// The service writes Logs; a writable Logs folder must not fail the program-files check.
	icacls(t, logs, "/grant", "*S-1-5-32-545:(OI)(CI)(M)")
	if err := CheckNotWritable(dir, "Logs"); err != nil {
		t.Fatalf("Logs must be skipped: %v", err)
	}

	// Users able to modify one script = Users able to run code as the service account.
	icacls(t, filepath.Join(scripts, "New-IDLUser.ps1"), "/grant", "*S-1-5-32-545:(M)")
	err := CheckNotWritable(dir, "Logs")
	var we *WritableError
	if !errors.As(err, &we) || !strings.Contains(err.Error(), "New-IDLUser.ps1") || !strings.Contains(err.Error(), `BUILTIN\Users`) {
		t.Fatalf("writable script not reported: %v", err)
	}

	// Create-files on the folder itself (what C:\ grants Users) is also caught.
	icacls(t, filepath.Join(scripts, "New-IDLUser.ps1"), "/remove:g", "*S-1-5-32-545")
	icacls(t, dir, "/grant", "*S-1-5-32-545:(CI)(AD,WD)")
	if err := CheckNotWritable(dir, "Logs"); err == nil {
		t.Fatal("create-files right for Users must be reported")
	}
}

func TestTrustedOwnersAreAccepted(t *testing.T) {
	f := filepath.Join(t.TempDir(), "mine.txt")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{f, os.Getenv("SystemRoot") + `\System32\kernel32.dll`} { // current user; TrustedInstaller
		if o, err := untrustedOwner(p); err != nil || o != "" {
			t.Errorf("%s: owner reported untrusted: %q %v", p, o, err)
		}
	}
}
