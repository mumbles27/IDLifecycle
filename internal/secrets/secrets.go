// Package secrets encrypts source credentials at rest with AES-256-GCM.
// The key comes from IDL_MASTER_KEY (base64, 32 bytes) or from a key file in the
// data directory that is wrapped with Windows DPAPI (machine scope) on Windows.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	prefix     = "enc:v1:"
	keyFile    = "secret.key"
	envKey     = "IDL_MASTER_KEY"
	rawPrefix  = "RAW1:"
	dpapiLabel = "DPAPI1:"
)

type Cipher struct {
	aead cipher.AEAD
	key  []byte
}

// Derive returns a purpose-bound subkey (HMAC-SHA256 of label under the master key).
func (c *Cipher) Derive(label string) []byte {
	m := hmac.New(sha256.New, c.key)
	m.Write([]byte(label))
	return m.Sum(nil)
}

// New builds a Cipher from an explicit 32-byte key.
func New(key []byte) (*Cipher, error) {
	if len(key) != 32 {
		return nil, errors.New("secrets: key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead, key: append([]byte(nil), key...)}, nil
}

// Load returns a Cipher using IDL_MASTER_KEY or the key file in dataDir (created on first use).
func Load(dataDir string) (*Cipher, error) {
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		key, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("%s is not valid base64: %w", envKey, err)
		}
		return New(key)
	}

	path := filepath.Join(dataDir, keyFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		wrapped, err := wrapKey(key)
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(dataDir, 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(wrapped), 0o600); err != nil {
			return nil, err
		}
		return New(key)
	}
	if err != nil {
		return nil, err
	}
	key, err := unwrapKey(strings.TrimSpace(string(data)))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return New(key)
}

// KeyBytes returns a copy of the master key. Only the backup code uses it, to put the key into a backup file
// encrypted with the backup passphrase (so a restore on another server can still read the stored secrets).
func (c *Cipher) KeyBytes() []byte { return append([]byte(nil), c.key...) }

// ReplaceKeyFile writes key as dataDir\secret.key (DPAPI-wrapped on Windows), for a restore. An existing key file is
// first renamed to secret.key.replaced-<stamp> (returned) so nothing is lost if the restore was a mistake. It refuses
// when IDL_MASTER_KEY is set: then the key lives in that variable and must be changed there.
func ReplaceKeyFile(dataDir string, key []byte, stamp string) (string, error) {
	if len(key) != 32 {
		return "", errors.New("secrets: key must be 32 bytes")
	}
	if strings.TrimSpace(os.Getenv(envKey)) != "" {
		return "", fmt.Errorf("%s is set, so the key is not read from %s: set %s to the backup's key instead (or unset it)", envKey, keyFile, envKey)
	}
	wrapped, err := wrapKey(key)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dataDir, keyFile)
	replaced := ""
	if _, err := os.Stat(path); err == nil {
		replaced = path + ".replaced-" + stamp
		if err := os.Rename(path, replaced); err != nil {
			return "", err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(wrapped), 0o600); err != nil {
		return replaced, err
	}
	return replaced, os.Rename(tmp, path)
}

func wrapKey(key []byte) (string, error) {
	if protect != nil {
		out, err := protect(key)
		if err != nil {
			return "", err
		}
		return dpapiLabel + base64.StdEncoding.EncodeToString(out), nil
	}
	return rawPrefix + base64.StdEncoding.EncodeToString(key), nil
}

func unwrapKey(s string) ([]byte, error) {
	switch {
	case strings.HasPrefix(s, dpapiLabel):
		if unprotect == nil {
			return nil, errors.New("key file is DPAPI-protected and can only be read on Windows")
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, dpapiLabel))
		if err != nil {
			return nil, err
		}
		return unprotect(raw)
	case strings.HasPrefix(s, rawPrefix):
		return base64.StdEncoding.DecodeString(strings.TrimPrefix(s, rawPrefix))
	}
	return nil, errors.New("unrecognized key file format")
}

// platform hooks, set in secrets_windows.go
var (
	protect   func([]byte) ([]byte, error)
	unprotect func([]byte) ([]byte, error)
)

// IsEncrypted reports whether s looks like output of Encrypt.
func IsEncrypted(s string) bool { return strings.HasPrefix(s, prefix) }

func (c *Cipher) Encrypt(plain string) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := c.aead.Seal(nonce, nonce, []byte(plain), nil)
	return prefix + base64.StdEncoding.EncodeToString(ct), nil
}

func (c *Cipher) Decrypt(s string) (string, error) {
	if !IsEncrypted(s) {
		return "", errors.New("value is not encrypted")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(s, prefix))
	if err != nil {
		return "", err
	}
	ns := c.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("ciphertext too short")
	}
	pt, err := c.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", errors.New("decryption failed (wrong key or corrupted value)")
	}
	return string(pt), nil
}
