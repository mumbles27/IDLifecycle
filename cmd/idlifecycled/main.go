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
	"idlifecycle/internal/config"
	"idlifecycle/internal/datadir"
	"idlifecycle/internal/model"
	"idlifecycle/internal/store"
)

const usage = `idlifecycled - Active Directory identity lifecycle service

Usage:
  idlifecycled run      [-config FILE]     run in the foreground (Ctrl+C to stop)
  idlifecycled service  [-config FILE]     run under the Windows Service Control Manager
  idlifecycled init     [-dir DIR] [-scripts DIR]   create a default config.json and data directory
  idlifecycled lockdown [-config FILE] [-account NAME]  restrict the data directory to SYSTEM, Administrators,
                                                      you and the service account (the service refuses to start otherwise)
  idlifecycled user add    -username NAME -role viewer|approver|admin [-config FILE]
  idlifecycled user passwd -username NAME [-config FILE]
  idlifecycled user list   [-config FILE]
  idlifecycled version

Passwords are read from the IDL_PASSWORD environment variable, or prompted for (hidden) on a terminal,
or read from one line of stdin when piped. They are never accepted on the command line.
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
	case "user":
		return cmdUser(args[1:], stdout, stderr)
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
	if err := fs.Parse(args); err != nil {
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
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintf(stdout, "Wrote %s\n\nNext steps:\n  1. Copy the scripts folder to %s\n  2. idlifecycled user add -config %q -username admin -role admin\n  3. idlifecycled run -config %q   (or install the service with scripts\\Install-IDLifecycleService.ps1)\n  4. Open http://%s/ and finish configuration in the GUI. The service starts in DRY-RUN mode.\n",
		path, cfg.PowerShell.ScriptsDir, path, path, cfg.Listen)
	return 0
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
		fmt.Fprintln(tw, "USERNAME\tROLE\tDISABLED\tLAST LOGIN")
		for _, u := range users {
			last := "never"
			if u.LastLoginAt != nil {
				last = u.LastLoginAt.Local().Format(time.RFC3339)
			}
			fmt.Fprintf(tw, "%s\t%s\t%v\t%s\n", u.Username, u.Role, u.Disabled, last)
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
	pw := os.Getenv("IDL_PASSWORD")
	switch {
	case pw != "":
	case term.IsTerminal(int(os.Stdin.Fd())):
		fmt.Fprint(os.Stderr, "Password: ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		pw = string(b)
		if confirm {
			fmt.Fprint(os.Stderr, "Repeat password: ")
			b2, err := term.ReadPassword(int(os.Stdin.Fd()))
			fmt.Fprintln(os.Stderr)
			if err != nil {
				return "", err
			}
			if string(b2) != pw {
				return "", errors.New("passwords do not match")
			}
		}
	default:
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", errors.New("no password supplied (set IDL_PASSWORD or pipe one line on stdin)")
		}
		pw = strings.TrimRight(line, "\r\n")
	}
	if msg := api.ValidatePassword(pw); msg != "" {
		return "", errors.New(msg)
	}
	return pw, nil
}
