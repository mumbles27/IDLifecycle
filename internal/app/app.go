// Package app assembles the service: store, engine, scheduler, executor worker and HTTP server.
package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/api"
	"idlifecycle/internal/config"
	"idlifecycle/internal/datadir"
	"idlifecycle/internal/engine"
	"idlifecycle/internal/notify"
	"idlifecycle/internal/scheduler"
	"idlifecycle/internal/secrets"
	"idlifecycle/internal/sources"
	"idlifecycle/internal/store"
	"idlifecycle/internal/web"
)

// Version is stamped at build time with -ldflags "-X idlifecycle/internal/app.Version=...".
var Version = "dev"

// NewLogger writes JSON logs to <data_dir>/logs/idlifecycle.log (rotated) and, optionally, to stderr.
func NewLogger(cfg config.Config, alsoStderr bool) (*slog.Logger, io.Closer, error) {
	dir := filepath.Join(cfg.DataDir, "logs")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, nil, err
	}
	lj := &lumberjack.Logger{Filename: filepath.Join(dir, "idlifecycle.log"), MaxSize: 20, MaxBackups: 10, MaxAge: 90, Compress: true}
	var w io.Writer = lj
	if alsoStderr {
		w = io.MultiWriter(lj, os.Stderr)
	}
	level := slog.LevelInfo
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})), lj, nil
}

// OpenStore connects using the bootstrap config and keys the audit hash chain. Every writer (the service
// and the CLI) must go through here: an entry written without the key breaks verification for good.
func OpenStore(ctx context.Context, cfg config.Config) (*store.Store, error) {
	dsn, err := cfg.ResolvedDSN()
	if err != nil {
		return nil, err
	}
	cipher, err := secrets.Load(cfg.DataDir)
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	st, err := store.Open(ctx, cfg.Database.Driver, dsn)
	if err != nil {
		return nil, err
	}
	st.SetAuditKey(cipher.Derive(auditKeyLabel))
	return st, nil
}

const auditKeyLabel = "audit-chain-v1"

// App is a running service instance.
type App struct {
	Cfg       config.Config
	Log       *slog.Logger
	Store     *store.Store
	Engine    *engine.Engine
	Scheduler *scheduler.Scheduler
	Server    *http.Server

	listener net.Listener
}

// New builds everything and verifies prerequisites, but does not start serving.
func New(ctx context.Context, cfg config.Config, log *slog.Logger) (*App, error) {
	if err := datadir.Check(cfg.DataDir); err != nil {
		return nil, err
	}
	st, err := OpenStore(ctx, cfg)
	if err != nil {
		return nil, err
	}
	cipher, err := secrets.Load(cfg.DataDir)
	if err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("secrets: %w", err)
	}

	pwsh := &ad.PowerShellClient{
		Exe:             cfg.PowerShell.Exe,
		ScriptsDir:      cfg.PowerShell.ScriptsDir,
		ExecutionPolicy: cfg.PowerShell.ExecutionPolicy,
		Timeout:         time.Duration(cfg.PowerShell.TimeoutSeconds) * time.Second,
		Logger:          log,
	}
	for _, f := range []string{"Get-IDLSnapshot.ps1", "New-IDLUser.ps1", "Set-IDLUser.ps1", "Disable-IDLUser.ps1", "Enable-IDLUser.ps1", "Remove-IDLUser.ps1"} {
		if _, err := os.Stat(filepath.Join(cfg.PowerShell.ScriptsDir, f)); err != nil {
			log.Warn("AD script missing; syncs will fail until it is installed", "script", f, "dir", cfg.PowerShell.ScriptsDir)
		}
	}
	if blocked := ad.BlockedScripts(cfg.PowerShell.ScriptsDir); len(blocked) > 0 {
		log.Error("PowerShell scripts are blocked (downloaded-file mark); with execution policy "+cfg.PowerShell.ExecutionPolicy+
			" they will not run. Fix: Get-ChildItem <scripts_dir> -Recurse | Unblock-File (the installer does this)",
			"dir", cfg.PowerShell.ScriptsDir, "files", blocked)
	}

	sources.PathPolicy = cfg.CSVPathPolicy()

	notifier := &notify.Service{Settings: st.GetSettings, Cipher: cipher, Log: log}
	eng := engine.New(st, pwsh, cipher, log)
	eng.Notifier = notifier
	if err := eng.Recover(ctx); err != nil {
		_ = st.Close()
		return nil, fmt.Errorf("recover: %w", err)
	}
	sched := scheduler.New(st, eng, log)

	a := &App{Cfg: cfg, Log: log, Store: st, Engine: eng, Scheduler: sched}
	apiSrv := &api.Server{
		Store: st, Engine: eng, Cipher: cipher, Scheduler: sched, Notifier: notifier, Static: web.Handler(), Log: log,
		Version: Version, TLS: cfg.TLSEnabled(), BaseCtx: ctx,
	}
	a.Server = &http.Server{
		Handler:           apiSrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      150 * time.Second, // the source "test" endpoint can run up to 90s
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	if cfg.TLSEnabled() {
		a.Server.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return a, nil
}

// Listen binds the configured address (separate so tests can learn the port).
func (a *App) Listen() error {
	ln, err := net.Listen("tcp", a.Cfg.Listen)
	if err != nil {
		return err
	}
	a.listener = ln
	return nil
}

func (a *App) Addr() string {
	if a.listener == nil {
		return a.Cfg.Listen
	}
	return a.listener.Addr().String()
}

// Run serves until ctx is cancelled, then shuts down gracefully.
func (a *App) Run(ctx context.Context) error {
	if a.listener == nil {
		if err := a.Listen(); err != nil {
			return err
		}
	}
	if n, err := a.Store.CountUsers(ctx); err == nil && n == 0 {
		a.Log.Warn("no GUI users exist; create the first administrator with: idlifecycled user add -config <config.json> -username <name> -role admin")
	}

	var wg sync.WaitGroup
	bg := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	bg(func() { a.Engine.RunWorker(ctx) })
	bg(func() { a.Scheduler.Run(ctx) })
	bg(func() {
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = a.Store.DeleteExpiredSessions(ctx)
			}
		}
	})

	errc := make(chan error, 1)
	go func() {
		a.Log.Info("listening", "addr", a.Addr(), "tls", a.Cfg.TLSEnabled(), "db", a.Cfg.Database.Driver, "version", Version)
		var err error
		if a.Cfg.TLSEnabled() {
			err = a.Server.ServeTLS(a.listener, a.Cfg.TLS.CertFile, a.Cfg.TLS.KeyFile)
		} else {
			err = a.Server.Serve(a.listener)
		}
		if !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
		close(errc)
	}()

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = a.Server.Shutdown(shutdownCtx)
	a.Engine.Shutdown(20 * time.Second)
	wg.Wait()
	_ = a.Store.Close()
	a.Log.Info("stopped")
	return runErr
}
