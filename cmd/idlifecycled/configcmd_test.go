package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writePair(t *testing.T, dir string) (string, string) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "idl.test"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"idl.test"}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(k)
	cert, key := filepath.Join(dir, "idl.pem"), filepath.Join(dir, "idl.key")
	os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600)
	return cert, key
}

func view(t *testing.T, out string) configView {
	t.Helper()
	var v configView
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	return v
}

func TestConfigGetSet(t *testing.T) {
	dir := t.TempDir()
	if code, out, errs := run(t, "init", "-dir", dir); code != 0 {
		t.Fatalf("init: %s %s", out, errs)
	}
	cfg := filepath.Join(dir, "config.json")
	// Fields the CLI does not touch must survive a set unchanged.
	raw, _ := os.ReadFile(cfg)
	var doc map[string]any
	json.Unmarshal(raw, &doc)
	doc["csv_allowed_dirs"] = []string{`D:\HR`}
	b, _ := json.MarshalIndent(doc, "", "  ")
	os.WriteFile(cfg, b, 0o600)

	code, out, errs := run(t, "config", "get", "-config", cfg)
	if code != 0 {
		t.Fatalf("get: %d %s", code, errs)
	}
	v := view(t, out)
	if v.Listen != "127.0.0.1:8443" || v.ConsoleURL != "http://localhost:8443/" || v.DBDriver != "sqlite" || v.LogLevel != "info" || v.DataDir == "" {
		t.Fatalf("get: %+v", v)
	}
	if strings.Contains(out, "dsn") {
		t.Error("the DSN is never printed")
	}

	before, _ := os.ReadFile(cfg)
	for _, bad := range [][]string{
		{"-listen", "0.0.0.0:8443"}, // non-loopback without TLS
		{"-listen", "nonsense"},     // not host:port
		{"-log-level", "loud"},      // unknown level
		{"-tls-cert", filepath.Join(dir, "missing.pem"), "-tls-key", filepath.Join(dir, "missing.key")},
		{}, // nothing to change
	} {
		code, _, errs := run(t, append([]string{"config", "set", "-config", cfg}, bad...)...)
		if code != 2 || errs == "" {
			t.Errorf("%v: exit %d (%s), want 2", bad, code, errs)
		}
		if now, _ := os.ReadFile(cfg); string(now) != string(before) {
			t.Fatalf("%v: a refused change must not write anything", bad)
		}
	}

	if code, out, errs = run(t, "config", "set", "-config", cfg, "-log-level", "DEBUG"); code != 0 || view(t, out).LogLevel != "debug" {
		t.Fatalf("log level: %d %s %s", code, out, errs)
	}

	cert, key := writePair(t, dir)
	code, out, errs = run(t, "config", "set", "-config", cfg, "-listen", "0.0.0.0:9443", "-tls-cert", cert, "-tls-key", key)
	if code != 0 {
		t.Fatalf("https: %d %s", code, errs)
	}
	v = view(t, out)
	host, _ := os.Hostname()
	if v.TLSCertFile != cert || !strings.HasPrefix(v.ConsoleURL, "https://") || !strings.HasSuffix(v.ConsoleURL, ":9443/") ||
		!strings.Contains(strings.ToLower(v.ConsoleURL), strings.ToLower(host)) {
		t.Fatalf("https view: %+v", v)
	}
	if code, out, _ = run(t, "config", "set", "-config", cfg, "-listen", "idl.corp.test:9443"); code != 0 || view(t, out).ConsoleURL != "https://idl.corp.test:9443/" {
		t.Fatalf("named host: %d %s", code, out)
	}

	// Turning HTTPS off while the listen address is not loopback is refused; with a loopback address it works.
	if code, _, _ = run(t, "config", "set", "-config", cfg, "-tls-cert", ""); code != 2 {
		t.Fatalf("https off on a network address must be refused, got %d", code)
	}
	if code, out, errs = run(t, "config", "set", "-config", cfg, "-tls-cert", "", "-listen", "127.0.0.1:8443"); code != 0 || view(t, out).TLSKeyFile != "" {
		t.Fatalf("https off: %d %s %s", code, out, errs)
	}

	raw, _ = os.ReadFile(cfg)
	doc = map[string]any{}
	json.Unmarshal(raw, &doc)
	if dirs, _ := doc["csv_allowed_dirs"].([]any); len(dirs) != 1 || dirs[0] != `D:\HR` {
		t.Errorf("other fields must be kept: %v", doc["csv_allowed_dirs"])
	}
	if doc["database"] == nil || doc["powershell"] == nil {
		t.Error("database and powershell sections must be kept")
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".config-*")); len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
	if code, _, _ = run(t, "config"); code != 2 {
		t.Error("config without get/set is a usage error")
	}
}
