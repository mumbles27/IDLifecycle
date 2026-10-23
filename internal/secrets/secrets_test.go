package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoundTripAndTamper(t *testing.T) {
	key := make([]byte, 32)
	c, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := c.Encrypt("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(enc, "s3cret") || !IsEncrypted(enc) {
		t.Fatalf("bad ciphertext %q", enc)
	}
	enc2, _ := c.Encrypt("s3cret")
	if enc == enc2 {
		t.Fatal("nonce must be random per encryption")
	}
	if got, err := c.Decrypt(enc); err != nil || got != "s3cret" {
		t.Fatalf("decrypt: %q %v", got, err)
	}
	if _, err := c.Decrypt(enc[:len(enc)-4] + "AAAA"); err == nil {
		t.Fatal("tampered ciphertext must fail")
	}
	other, _ := New(append([]byte{1}, key[1:]...))
	if _, err := other.Decrypt(enc); err == nil {
		t.Fatal("wrong key must fail")
	}
	if _, err := New([]byte("short")); err == nil {
		t.Fatal("short key must be rejected")
	}
}

func TestLoadCreatesAndReusesKeyFile(t *testing.T) {
	t.Setenv(envKey, "")
	dir := t.TempDir()
	c1, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := c1.Encrypt("hello")
	if _, err := os.Stat(filepath.Join(dir, keyFile)); err != nil {
		t.Fatalf("key file not created: %v", err)
	}
	c2, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := c2.Decrypt(enc); err != nil || got != "hello" {
		t.Fatalf("second Load must reuse the same key: %q %v", got, err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, keyFile))
	if len(raw) > 0 && strings.Count(string(raw), "\x00") > 0 {
		t.Fatal("key file should be text")
	}
}

func TestLoadFromEnv(t *testing.T) {
	t.Setenv(envKey, "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=")
	if _, err := Load(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envKey, "not base64!!")
	if _, err := Load(t.TempDir()); err == nil {
		t.Fatal("invalid env key must error")
	}
}
