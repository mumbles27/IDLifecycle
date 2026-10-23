package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"idlifecycle/internal/api"
	"idlifecycle/internal/config"
	"idlifecycle/internal/model"
)

func TestServiceBootsServesGUIAndStopsGracefully(t *testing.T) {
	api.BcryptCost = 4
	dir := t.TempDir()
	cfg := config.Default(dir)
	cfg.Listen = "127.0.0.1:0"
	cfg.PowerShell.ScriptsDir = filepath.Join(dir, "no-scripts-here") // missing scripts only warn at startup
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	log, closer, err := NewLogger(cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	defer closer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	a, err := New(ctx, cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Listen(); err != nil {
		t.Fatal(err)
	}

	h, _ := api.HashPassword("correct-horse-battery")
	if err := a.Store.CreateUser(ctx, model.User{ID: uuid.NewString(), Username: "admin", PasswordHash: h, Role: model.RoleAdmin, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	base := "http://" + a.Addr()

	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Jar: jar, Timeout: 10 * time.Second}

	resp, err := hc.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(page), "<html") {
		t.Fatalf("GUI not served: %d %.80s", resp.StatusCode, page)
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
		t.Errorf("the API's stricter CSP must win over the static handler's: %q", csp)
	}

	resp, err = hc.Post(base+"/api/auth/login", "application/json", bytes.NewReader([]byte(`{"username":"admin","password":"correct-horse-battery"}`)))
	if err != nil {
		t.Fatal(err)
	}
	var login struct {
		CSRF string `json:"csrf_token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&login)
	resp.Body.Close()
	if resp.StatusCode != 200 || login.CSRF == "" {
		t.Fatalf("login: %d", resp.StatusCode)
	}

	resp, err = hc.Get(base + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	var st map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st["mode"] != "dry_run" || st["db_driver"] != "sqlite" {
		t.Errorf("fresh installs must start in dry-run: %v", st)
	}

	// an SPA deep link falls back to index.html, an unknown API path does not
	resp, _ = hc.Get(base + "/some/client/route")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "<html") {
		t.Error("SPA fallback expected")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("graceful stop returned %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("service did not stop")
	}
	if _, err := hc.Get(base + "/"); err == nil {
		t.Error("port should be closed after shutdown")
	}
}

func TestNewFailsClearlyOnBadDatabase(t *testing.T) {
	cfg := config.Default(t.TempDir())
	cfg.Database.Driver = "postgres"
	cfg.Database.DSN = "env:IDL_DEFINITELY_UNSET_DSN"
	if _, err := New(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil))); err == nil || !strings.Contains(err.Error(), "IDL_DEFINITELY_UNSET_DSN") {
		t.Fatalf("expected a clear DSN error, got %v", err)
	}
}
