// Command idlifecycled is the IDLifecycle service: HR source -> approvals -> Active Directory.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"golang.org/x/term"

	"idlifecycle/internal/api"
	"idlifecycle/internal/app"
	"idlifecycle/internal/backup"
	"idlifecycle/internal/config"
	"idlifecycle/internal/datadir"
	"idlifecycle/internal/model"
	"idlifecycle/internal/secrets"
	"idlifecycle/internal/store"
)

const usage = `idlifecycled - Active Directory identity lifecycle service

Usage:
  idlifecycled run      [-config FILE]     run in the foreground (Ctrl+C to stop)
  idlifecycled service  [-config FILE]     run under the Windows Service Control Manager
  idlifecycled init     [-dir DIR] [-scripts DIR] [-db-driver D -db-dsn DSN] [-listen ADDR -tls-cert F -tls-key F]
                                                      create config.json and the (restricted) data directory
  idlifecycled check    [-config FILE] [-json] [-skip-ad-module]   verify config, data dir ACL, database,
                                                      admin user, PowerShell, scripts and the AD module. It connects
                                                      to the database and creates/migrates the schema, and creates
                                                      secret.key when missing (the installer's database setup step).
                                                      Every check is always listed, in that order; one that did not
                                                      run is marked skipped ("skipped": true in -json)
  idlifecycled lockdown [-config FILE] [-account NAME]  restrict the data directory to SYSTEM, Administrators,
                                                      you and the service account (the service refuses to start otherwise)
  idlifecycled user add    -username NAME -role viewer|approver|admin [-config FILE]
  idlifecycled user passwd -username NAME [-config FILE]
  idlifecycled user list   [-config FILE]
  idlifecycled backup   [-config FILE] [-dir DIR] [-note TEXT]
                                                      write an encrypted backup (database, config.json, master key) to
                                                      DIR (default <data_dir>\backups) as IDLifecycle_<host>_<time>.idlbak
  idlifecycled restore  -in FILE [-config FILE] [-with-config] [-force]
                                                      replace the database with a backup and put its master key in
                                                      place (stop the service first; start it afterwards). -with-config
                                                      also takes its config.json except data_dir, database and
                                                      powershell.scripts_dir. -force replaces a database that has data
  idlifecycled restore  -in FILE -inspect             show a backup's header (no passphrase needed)
  idlifecycled config get [-config FILE]           print the basic settings as JSON (listen, tls_cert_file, tls_key_file,
                                                      log_level, data_dir, db_driver, scripts_dir, console_url; never the DSN)
  idlifecycled config set [-config FILE] [-listen ADDR] [-tls-cert F] [-tls-key F] [-log-level L]
                                                      change only those fields (an empty -tls-cert or -tls-key turns HTTPS
                                                      off), checked like at startup; other fields are kept. Restart the
                                                      service to apply. Exit 2 = invalid, nothing written
  idlifecycled version

Passwords are read from the IDL_PASSWORD environment variable, or prompted for (hidden) on a terminal,
or read from one line of stdin when piped. Backup passphrases (12+ characters) the same way, from
IDL_BACKUP_PASSPHRASE. They are never accepted on the command line.
`

func main() {
	os.Exit(realMain(os.Args[1:], os.Stdout, os.Stderr))
}

func realMain(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "run":
		return cmdRun(args[1:], stderr)
	case "service":
		return runService(args[1:], stderr)
	case "init":
		return cmdInit(args[1:], stdout, stderr)
	case "lockdown":
		return cmdLockdown(args[1:], stdout, stderr)
	case "check":
		return cmdCheck(args[1:], stdout, stderr)
	case "user":
		return cmdUser(args[1:], stdout, stderr)
	case "backup":
		return cmdBackup(args[1:], stdout, stderr)
	case "restore":
		return cmdRestore(args[1:], stdout, stderr)
	case "config":
		return cmdConfig(args[1:], stdout, stderr)
	case "version", "-v", "--version":
		fmt.Fprintln(stdout, "idlifecycled", app.Version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
	return 2
}

func defaultConfigPath() string {
	if pd := os.Getenv("ProgramData"); pd != "" {
		return filepath.Join(pd, "IDLifecycle", "config.json")
	}
	return "config.json"
}

func configFlag(fs *flag.FlagSet) *string {
	return fs.String("config", defaultConfigPath(), "path to config.json")
}

func cmdRun(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := serve(ctx, *cfgPath, true); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return 0
}

// serve loads the config and runs the app until ctx ends. Shared by console and service modes.
func serve(ctx context.Context, cfgPath string, console bool) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	log, closer, err := app.NewLogger(cfg, console)
	if err != nil {
		return err
	}
	defer closer.Close()

	a, err := app.New(ctx, cfg, log)
	if err != nil {
		log.Error("startup failed", "error", err)
		return err
	}
	if err := a.Listen(); err != nil {
		log.Error("cannot listen", "addr", cfg.Listen, "error", err)
		return err
	}
	scheme := "http"
	if cfg.TLSEnabled() {
		scheme = "https"
	}
	log.Info("IDLifecycle started", "url", scheme+"://"+a.Addr(), "config", cfgPath)
	return a.Run(ctx)
}

func cmdInit(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	def := filepath.Dir(defaultConfigPath())
	dir := fs.String("dir", def, "data directory (holds config.json, the SQLite DB, logs and the key file)")
	scripts := fs.String("scripts", "", "directory containing the PowerShell scripts (default: <dir>\\scripts)")
	dbDriver := fs.String("db-driver", "", "database driver: sqlite (default), mssql, postgres, mysql or db2")
	dbDSN := fs.String("db-dsn", "", `connection string; prefer env:VAR_NAME (read from the service's environment at run time): `+
		`a literal value is visible in process listings and shell history (default for sqlite: <dir>\idlifecycle.db; needs -db-driver)`)
	listen := fs.String("listen", "", "listen address host:port (default 127.0.0.1:8443; non-loopback needs -tls-cert/-tls-key)")
	tlsCert := fs.String("tls-cert", "", "TLS certificate file (PEM)")
	tlsKey := fs.String("tls-key", "", "TLS private key file (PEM)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	// Usage errors first, before the data directory is created.
	if *dbDSN != "" && *dbDriver == "" {
		fmt.Fprintln(stderr, "error: -db-dsn requires -db-driver (sqlite, mssql, postgres, mysql or db2)")
		return 2
	}
	if *dbDriver == "sqlite" && looksODBC(*dbDSN) {
		fmt.Fprintln(stderr, "error: -db-dsn looks like an ODBC connection string, which -db-driver sqlite cannot use (did you mean -db-driver db2?)")
		return 2
	}
	if err := os.MkdirAll(*dir, 0o750); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	// Before anything sensitive is written: under ProgramData the folder would otherwise inherit Users:Read.
	if err := datadir.Lockdown(*dir); err != nil {
		fmt.Fprintln(stderr, "error: restrict the data directory:", err)
		return 1
	}
	path := filepath.Join(*dir, "config.json")
	if _, err := os.Stat(path); err == nil {
		fmt.Fprintf(stderr, "refusing to overwrite existing %s\n", path)
		return 1
	}
	cfg := config.Default(*dir)
	if *scripts != "" {
		cfg.PowerShell.ScriptsDir = *scripts
	}
	if *dbDriver != "" && *dbDriver != "sqlite" {
		if *dbDSN == "" {
			fmt.Fprintln(stderr, "error: -db-dsn is required with -db-driver", *dbDriver)
			return 2
		}
		cfg.Database.Driver = *dbDriver
	}
	if *dbDSN != "" {
		cfg.Database.DSN = *dbDSN
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	cfg.TLS.CertFile, cfg.TLS.KeyFile = *tlsCert, *tlsKey
	if err := cfg.Validate(); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Wrote %s\n\nNext steps:\n  1. Copy the scripts folder to %s\n  2. idlifecycled user add -config %q -username admin -role admin\n  3. idlifecycled run -config %q   (or install the service with scripts\\Install-IDLifecycleService.ps1)\n  4. Open http://%s/ and finish configuration in the GUI. The service starts in DRY-RUN mode.\n",
		path, cfg.PowerShell.ScriptsDir, path, path, cfg.Listen)
	return 0
}

// looksODBC reports an ODBC-style connection string (Driver=..., DSN=..., or ...;Uid=...).
func looksODBC(dsn string) bool {
	l := strings.ToLower(strings.ReplaceAll(dsn, " ", ""))
	return strings.Contains(l, "driver=") || strings.Contains(l, "dsn=") || strings.Contains(l, ";uid=")
}

func cmdLockdown(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("lockdown", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := configFlag(fs)
	account := fs.String("account", "", `service account to grant Modify (e.g. CORP\svc-idl$); omit when it runs as LocalSystem`)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if err := datadir.Lockdown(cfg.DataDir, *account); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if err := datadir.Check(cfg.DataDir); err != nil {
		fmt.Fprintln(stderr, "error: still not restricted:", err)
		return 1
	}
	who := "SYSTEM, Administrators and the current user"
	if *account != "" {
		who = "SYSTEM, Administrators, the current user and " + *account
	}
	fmt.Fprintf(stdout, "Restricted %s to %s\n", cfg.DataDir, who)
	return 0
}

func cmdUser(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	sub := args[0]
	fs := flag.NewFlagSet("user "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := configFlag(fs)
	name := fs.String("username", "", "GUI user name")
	role := fs.String("role", model.RoleViewer, "viewer, approver or admin")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	ctx := context.Background()
	st, err := app.OpenStore(ctx, cfg)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	defer st.Close()

	switch sub {
	case "list":
		users, err := st.ListUsers(ctx)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "USERNAME\tROLE\tSIGN-IN\tDISABLED\tLAST LOGIN")
		for _, u := range users {
			last := "never"
			if u.LastLoginAt != nil {
				last = u.LastLoginAt.Local().Format(time.RFC3339)
			}
			src := "password"
			if u.AuthSource == model.AuthOIDC {
				src = "entra-id"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%v\t%s\n", u.Username, u.Role, src, u.Disabled, last)
		}
		_ = tw.Flush()
		return 0

	case "add":
		if strings.TrimSpace(*name) == "" {
			fmt.Fprintln(stderr, "error: -username is required")
			return 2
		}
		if *role != model.RoleViewer && *role != model.RoleApprover && *role != model.RoleAdmin {
			fmt.Fprintln(stderr, "error: -role must be viewer, approver or admin")
			return 2
		}
		pw, err := readPassword(true)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		h, err := api.HashPassword(pw)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		u := model.User{ID: uuid.NewString(), Username: strings.TrimSpace(*name), PasswordHash: h, Role: *role, CreatedAt: time.Now()}
		if err := st.CreateUser(ctx, u); err != nil {
			if errors.Is(err, store.ErrConflict) {
				fmt.Fprintln(stderr, "error: that user already exists")
			} else {
				fmt.Fprintln(stderr, "error:", err)
			}
			return 1
		}
		_ = st.AppendAudit(ctx, "cli", "user.create", "user", u.ID, map[string]any{"username": u.Username, "role": u.Role})
		fmt.Fprintf(stdout, "Created %s user %q\n", u.Role, u.Username)
		return 0

	case "passwd":
		u, err := st.GetUserByName(ctx, strings.TrimSpace(*name))
		if err != nil {
			fmt.Fprintln(stderr, "error: no such user")
			return 1
		}
		if u.AuthSource == model.AuthOIDC {
			fmt.Fprintln(stderr, "error: this account signs in with Entra ID and has no password here; create a local break-glass admin with: idlifecycled user add -username <name> -role admin")
			return 2
		}
		pw, err := readPassword(true)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		if u.PasswordHash, err = api.HashPassword(pw); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		u.Disabled = false
		if err := st.UpdateUser(ctx, u); err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		_ = st.DeleteUserSessions(ctx, u.ID)
		_ = st.AppendAudit(ctx, "cli", "user.password_reset", "user", u.ID, map[string]any{"username": u.Username})
		fmt.Fprintf(stdout, "Password updated for %q (existing sessions ended)\n", u.Username)
		return 0
	}
	fmt.Fprint(stderr, usage)
	return 2
}

func readPassword(confirm bool) (string, error) {
	pw, err := readSecret("IDL_PASSWORD", "Password", confirm)
	if err != nil {
		return "", err
	}
	if msg := api.ValidatePassword(pw); msg != "" {
		return "", errors.New(msg)
	}
	return pw, nil
}

// readSecret takes a secret from the environment variable env, a hidden prompt on a terminal, or one line of stdin.
func readSecret(env, label string, confirm bool) (string, error) {
	pw := os.Getenv(env)
	switch {
	case pw != "":
	case term.IsTerminal(int(os.Stdin.Fd())):
		fmt.Fprint(os.Stderr, label+": ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		pw = string(b)
		if confirm {
			fmt.Fprint(os.Stderr, "Repeat "+strings.ToLower(label)+": ")
			b2, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr)
			if err != nil {
				return "", err
			}
			if string(b2) != pw {
				return "", errors.New("the two entries do not match")
			}
		}
	default:
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("no %s supplied (set %s or pipe one line on stdin)", strings.ToLower(label), env)
		}
		pw = strings.TrimRight(line, "\r\n")
	}
	return pw, nil
}

func cmdBackup(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := configFlag(fs)
	dir := fs.String("dir", "", `folder for the backup (default <data_dir>\backups)`)
	note := fs.String("note", "", "a note stored in the backup's header")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	pass, err := readSecret("IDL_BACKUP_PASSPHRASE", "Backup passphrase", true)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if len([]rune(pass)) < backup.MinPassphrase {
		fmt.Fprintf(stderr, "error: the backup passphrase must be at least %d characters\n", backup.MinPassphrase)
		return 2
	}
	ctx := context.Background()
	st, err := app.OpenStore(ctx, cfg)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	defer st.Close()
	c, err := secrets.Load(cfg.DataDir)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	folder := *dir
	if folder == "" {
		folder = backup.DefaultFolder(cfg)
	}
	fi, h, err := backup.CreateFile(ctx, st, cfg, c, folder, pass, app.Version, *note, time.Now())
	if err != nil {
		_ = st.AppendAudit(ctx, "cli", "backup.failed", "backup", "", map[string]any{"error": err.Error(), "folder": folder})
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	_ = st.AppendAudit(ctx, "cli", "backup.create", "backup", fi.Name, map[string]any{"size": fi.Size, "rows": h.Tables, "folder": folder})
	fmt.Fprintf(stdout, "Wrote %s (%d bytes)\n", fi.Path, fi.Size)
	for _, t := range store.BackupTables {
		fmt.Fprintf(stdout, "  %-12s %d rows\n", t, h.Tables[t])
	}
	fmt.Fprintln(stdout, "Keep the passphrase safe: without it the backup cannot be restored.")
	return 0
}

func cmdRestore(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := configFlag(fs)
	in := fs.String("in", "", "the .idlbak file")
	withConfig := fs.Bool("with-config", false, "also restore config.json (except data_dir, database, powershell.scripts_dir)")
	force := fs.Bool("force", false, "replace a database that already has data")
	inspect := fs.Bool("inspect", false, "only show the backup's header")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *in == "" {
		fmt.Fprintln(stderr, "error: -in is required")
		return 2
	}
	f, err := os.Open(*in)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	h, err := backup.ReadHeader(f)
	_ = f.Close()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	fmt.Fprintf(stdout, "Backup made %s by version %s (%s database, schema %d)%s\n", h.Created.Local().Format(time.RFC3339), h.AppVersion, h.Driver, h.Schema,
		map[bool]string{true: ": " + h.Note, false: ""}[h.Note != ""])
	for _, t := range store.BackupTables {
		fmt.Fprintf(stdout, "  %-12s %d rows\n", t, h.Tables[t])
	}
	if *inspect {
		return 0
	}
	pass, err := readSecret("IDL_BACKUP_PASSPHRASE", "Backup passphrase", false)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	res, err := backup.RestoreFile(context.Background(), *cfgPath, *in, backup.RestoreOptions{Passphrase: pass, Force: *force, WithConfig: *withConfig, Actor: "cli"})
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		if errors.Is(err, backup.ErrPassphrase) {
			return 2
		}
		return 1
	}
	fmt.Fprintln(stdout, "Restored:")
	for _, t := range store.BackupTables {
		fmt.Fprintf(stdout, "  %-12s %d rows\n", t, res.Rows[t])
	}
	if res.KeyReplaced != "" {
		fmt.Fprintf(stdout, "The previous secret.key was kept as %s\n", res.KeyReplaced)
	}
	if res.ConfigReplaced != "" {
		fmt.Fprintf(stdout, "config.json taken from the backup; the previous one was kept as %s\n", res.ConfigReplaced)
	}
	if !res.AuditChainValid {
		fmt.Fprintln(stdout, "WARNING: the restored audit log does not verify (it was already broken in the backup, or the backup is from a build with another audit key).")
	}
	fmt.Fprintln(stdout, "All sign-ins were ended. Start (or restart) the service now.")
	return 0
}
