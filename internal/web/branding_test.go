package web

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func bget(t *testing.T, h http.Handler, path string) *http.Response {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Result()
}

func brandOf(t *testing.T, h http.Handler) Brand {
	t.Helper()
	resp := bget(t, h, "/branding/brand.json")
	var b Brand
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		t.Fatal(err)
	}
	return b
}

func body(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestBrandingDefaults(t *testing.T) {
	h := NewBranding("", quiet())
	if b := brandOf(t, h); b.ProductName != DefaultProductName || b.Accent != "" || b.Custom {
		t.Fatalf("defaults: %+v", b)
	}
	logo := bget(t, h, "/branding/logo")
	if logo.Header.Get("Content-Type") != "image/svg+xml" || !strings.Contains(logo.Header.Get("Content-Security-Policy"), "sandbox") ||
		logo.Header.Get("X-Content-Type-Options") != "nosniff" || logo.Header.Get("Cache-Control") != "no-cache" {
		t.Errorf("logo headers: %v", logo.Header)
	}
	if !bytes.Equal(body(t, logo), defaultFile("logo.svg")) {
		t.Error("default logo")
	}
	ico := bget(t, h, "/branding/favicon.ico")
	if ico.Header.Get("Content-Type") != "image/x-icon" || !bytes.HasPrefix(body(t, ico), []byte{0, 0, 1, 0}) {
		t.Errorf("favicon: %v", ico.Header)
	}
	for _, p := range []string{"/branding/", "/branding/logo.svg", "/branding/../index.html", "/branding/brand.json/x", "/branding/tray.ico"} {
		if got := bget(t, h, p).StatusCode; got != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", p, got)
		}
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/branding/logo", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", rec.Code)
	}
}

func writeFile(t *testing.T, dir, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBrandingOverride(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "IDLifecycle-Branding")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 32)...)
	writeFile(t, dir, "brand.json", []byte(`{"product_name":"  Acme Identity ","accent":"#0A7D5A"}`))
	writeFile(t, dir, "logo.png", png)
	writeFile(t, dir, "favicon.ico", []byte{0, 0, 1, 0, 9, 9})

	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	h := NewBranding(dir, quiet())
	h.Now = func() time.Time { return clock }

	if b := brandOf(t, h); b.ProductName != "Acme Identity" || b.Accent != "#0a7d5a" || !b.Custom {
		t.Fatalf("override: %+v", b)
	}
	logo := bget(t, h, "/branding/logo")
	if logo.Header.Get("Content-Type") != "image/png" || logo.Header.Get("Content-Security-Policy") != "" || !bytes.Equal(body(t, logo), png) {
		t.Errorf("png logo: %v", logo.Header)
	}
	if !bytes.Equal(body(t, bget(t, h, "/branding/favicon.ico")), []byte{0, 0, 1, 0, 9, 9}) {
		t.Error("favicon override")
	}

	// logo.svg wins over logo.png; changes show after the cache expires.
	writeFile(t, dir, "logo.svg", []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"/>`))
	if bget(t, h, "/branding/logo").Header.Get("Content-Type") != "image/png" {
		t.Error("cached for 60 s")
	}
	clock = clock.Add(61 * time.Second)
	if svg := bget(t, h, "/branding/logo"); svg.Header.Get("Content-Type") != "image/svg+xml" || !strings.Contains(svg.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("svg override: %v", svg.Header)
	}

	// Invalid values are ignored one by one; bad JSON as a whole; oversized and wrong-format files too.
	writeFile(t, dir, "brand.json", []byte(`{"product_name":"`+strings.Repeat("x", 41)+`","accent":"red"}`))
	writeFile(t, dir, "logo.svg", bytes.Repeat([]byte("<svg>"), MaxBrandingFile/5+1))
	writeFile(t, dir, "favicon.ico", []byte("not an icon"))
	clock = clock.Add(61 * time.Second)
	b := brandOf(t, h)
	if b.ProductName != DefaultProductName || b.Accent != "" {
		t.Errorf("invalid values must be ignored: %+v", b)
	}
	if bget(t, h, "/branding/logo").Header.Get("Content-Type") != "image/png" {
		t.Error("an oversized logo.svg falls back to logo.png")
	}
	if !bytes.Equal(body(t, bget(t, h, "/branding/favicon.ico")), defaultFile("favicon.ico")) {
		t.Error("a non-icon favicon.ico is ignored")
	}
	writeFile(t, dir, "brand.json", []byte(`{not json`))
	clock = clock.Add(61 * time.Second)
	if b := brandOf(t, h); b.ProductName != DefaultProductName {
		t.Errorf("bad JSON: %+v", b)
	}
}

func TestBrandingUntrustedFolderIsIgnored(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("ACL test")
	}
	dir := filepath.Join(t.TempDir(), "IDLifecycle-Branding")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "brand.json", []byte(`{"product_name":"Planted"}`))
	if out, err := exec.Command("icacls", dir, "/grant", "*S-1-5-32-545:(OI)(CI)(M)").CombinedOutput(); err != nil {
		t.Fatalf("icacls: %v %s", err, out)
	}
	var logs bytes.Buffer
	h := NewBranding(dir, slog.New(slog.NewTextHandler(&logs, nil)))
	if b := brandOf(t, h); b.ProductName != DefaultProductName || b.Custom {
		t.Fatalf("an untrusted folder must not rebrand the login page: %+v", b)
	}
	if !strings.Contains(logs.String(), "not used") || !strings.Contains(logs.String(), "Users can change") {
		t.Errorf("warning: %s", logs.String())
	}
	h.Now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	brandOf(t, h)
	if strings.Count(logs.String(), "not used") != 1 {
		t.Error("the same problem is logged once")
	}
}

func TestBrandingJunctionIsIgnored(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("junctions are Windows-only")
	}
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, target, "brand.json", []byte(`{"product_name":"Via junction"}`))
	link := filepath.Join(root, "IDLifecycle-Branding")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Skipf("mklink: %v %s", err, out)
	}
	if b := brandOf(t, NewBranding(link, quiet())); b.Custom {
		t.Fatalf("a junction must not be followed: %+v", b)
	}
}
