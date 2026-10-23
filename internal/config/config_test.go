package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadAppliesDefaults(t *testing.T) {
	c, err := Load(write(t, `{"listen":"127.0.0.1:9000"}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Database.Driver != "sqlite" || c.PowerShell.ExecutionPolicy != "RemoteSigned" || c.Listen != "127.0.0.1:9000" {
		t.Fatalf("defaults not applied: %+v", c)
	}
}

func TestRejectsNonLoopbackWithoutTLS(t *testing.T) {
	_, err := Load(write(t, `{"listen":"0.0.0.0:8443"}`))
	if err == nil || !strings.Contains(err.Error(), "tls") {
		t.Fatalf("expected TLS requirement error, got %v", err)
	}
	if _, err := Load(write(t, `{"listen":"0.0.0.0:8443","tls":{"cert_file":"c.pem","key_file":"k.pem"}}`)); err != nil {
		t.Fatalf("non-loopback with TLS should be accepted: %v", err)
	}
}

func TestRejectsUnknownFieldsAndBadDriver(t *testing.T) {
	if _, err := Load(write(t, `{"listn":"127.0.0.1:1"}`)); err == nil {
		t.Fatal("typo'd field must be rejected rather than silently ignored")
	}
	if _, err := Load(write(t, `{"database":{"driver":"oracle","dsn":"x"}}`)); err == nil {
		t.Fatal("bad driver must be rejected")
	}
	if _, err := Load(write(t, `{"tls":{"cert_file":"c.pem"}}`)); err == nil {
		t.Fatal("half-configured TLS must be rejected")
	}
}

func TestDSNFromEnv(t *testing.T) {
	c := Default(t.TempDir())
	c.Database.DSN = "env:IDL_TEST_DSN_X"
	t.Setenv("IDL_TEST_DSN_X", "")
	if _, err := c.ResolvedDSN(); err == nil {
		t.Fatal("empty env var must error")
	}
	t.Setenv("IDL_TEST_DSN_X", "postgres://u:p@h/db")
	if got, err := c.ResolvedDSN(); err != nil || got != "postgres://u:p@h/db" {
		t.Fatalf("got %q %v", got, err)
	}
}
