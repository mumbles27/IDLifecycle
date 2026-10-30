package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/app"
	"idlifecycle/internal/cloud"
	"idlifecycle/internal/config"
	"idlifecycle/internal/datadir"
	"idlifecycle/internal/secrets"
)

// checkResult is one readiness check; the installer reads the JSON form. A check that did not
// run (a prerequisite failed, or it was switched off) has OK false and Skipped true.
type checkResult struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Detail  string `json:"detail"`
	Skipped bool   `json:"skipped,omitempty"`
}

// CheckNames is every check `check` reports, in output order. Each is always present in the
// output (skipped when not run), so the installer can rely on the list.
var CheckNames = []string{"config", "data_dir_acl", "database", "admin_user", "powershell", "scripts", "scripts_unblocked", "program_files_acl", "ad_module"}

var requiredScripts = []string{"IDLifecycle.Common.psm1", "Get-IDLSnapshot.ps1", "New-IDLUser.ps1", "Set-IDLUser.ps1",
	"Disable-IDLUser.ps1", "Enable-IDLUser.ps1", "Remove-IDLUser.ps1", "Test-IDLNames.ps1", "Set-IDLLink.ps1"}

// programFilesACL fails when a non-administrator can change the scripts the service account runs against
// AD (Logs excluded: the service writes there) or replace this executable / drop files next to it.
func programFilesACL(scriptsDir string) error {
	if err := datadir.CheckNotWritable(scriptsDir, "Logs"); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := datadir.CheckEntryNotWritable(exe); err != nil {
		return err
	}
	return datadir.CheckEntryNotWritable(filepath.Dir(exe))
}

// cmdCheck verifies that everything the service needs is in place, without starting it.
// Opening the store creates/migrates the schema and creates secret.key when missing.
// Exit 0 when every check that ran passes (and config loaded), 1 otherwise, 2 on usage errors.
func cmdCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := configFlag(fs)
	asJSON := fs.Bool("json", false, "print the results as JSON")
	skipAD := fs.Bool("skip-ad-module", false, "do not check for the ActiveDirectory PowerShell module")
	testCloud := fs.Bool("cloud", false, "also sign in with the saved Microsoft 365 / Entra ID connection (adds a \"cloud\" check; changes nothing)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	got := map[string]checkResult{}
	add := func(name string, err error, okDetail string) {
		if err != nil {
			got[name] = checkResult{Name: name, Detail: err.Error()}
			return
		}
		got[name] = checkResult{Name: name, OK: true, Detail: okDetail}
	}
	skip := func(name, why string) { got[name] = checkResult{Name: name, Detail: why, Skipped: true} }

	cfg, err := config.Load(*cfgPath)
	add("config", err, *cfgPath)
	if err == nil {
		add("data_dir_acl", datadir.Check(cfg.DataDir), cfg.DataDir+" is restricted")

		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		st, err := app.OpenStore(ctx, cfg)
		if err == nil {
			admins, cerr := st.CountActiveAdmins(ctx)
			_ = st.Close()
			add("database", nil, cfg.Database.Driver+": connected, schema up to date")
			switch {
			case cerr != nil:
				add("admin_user", cerr, "")
			case admins == 0:
				add("admin_user", fmt.Errorf("no enabled administrator; run: idlifecycled user add -config %q -username <name> -role admin", *cfgPath), "")
			default:
				add("admin_user", nil, fmt.Sprintf("%d enabled administrator(s)", admins))
			}
		} else {
			add("database", fmt.Errorf("%s: %w", cfg.Database.Driver, err), "")
			skip("admin_user", "not checked: the database is not available")
		}
		cancel()

		pwsh, perr := exec.LookPath(cfg.PowerShell.Exe)
		add("powershell", perr, pwsh)

		var missing []string
		for _, f := range requiredScripts {
			if _, err := os.Stat(filepath.Join(cfg.PowerShell.ScriptsDir, f)); err != nil {
				missing = append(missing, f)
			}
		}
		if len(missing) > 0 {
			add("scripts", fmt.Errorf("missing in %s: %s", cfg.PowerShell.ScriptsDir, strings.Join(missing, ", ")), "")
		} else {
			add("scripts", nil, cfg.PowerShell.ScriptsDir)
		}
		if blocked := ad.BlockedScripts(cfg.PowerShell.ScriptsDir); len(blocked) > 0 {
			add("scripts_unblocked", fmt.Errorf("%d script file(s) carry Mark-of-the-Web; run Unblock-File on %s", len(blocked), cfg.PowerShell.ScriptsDir), "")
		} else {
			add("scripts_unblocked", nil, "no blocked script files")
		}
		add("program_files_acl", programFilesACL(cfg.PowerShell.ScriptsDir), "only administrators can change the program and script files")

		switch {
		case *skipAD:
			skip("ad_module", "not checked: -skip-ad-module")
		case perr != nil:
			skip("ad_module", "not checked: PowerShell was not found")
		default:
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			out, err := exec.CommandContext(ctx, pwsh, "-NoProfile", "-NonInteractive", "-Command",
				"$m = Get-Module -ListAvailable ActiveDirectory | Select-Object -First 1; if ($m) { $m.Version.ToString() } else { exit 1 }").Output()
			cancel()
			if err != nil {
				add("ad_module", fmt.Errorf("the ActiveDirectory module is not available to PowerShell 7 (install RSAT: Install-WindowsFeature RSAT-AD-PowerShell)"), "")
			} else {
				add("ad_module", nil, "ActiveDirectory "+strings.TrimSpace(string(out)))
			}
		}
	}

	// Every check, in the fixed order; anything not reached is skipped.
	res := make([]checkResult, 0, len(CheckNames))
	ok := got["config"].OK
	for _, n := range CheckNames {
		r, done := got[n]
		if !done {
			r = checkResult{Name: n, Detail: "not checked: the configuration did not load", Skipped: true}
		}
		if !r.Skipped {
			ok = ok && r.OK
		}
		res = append(res, r)
	}
	// Only on request, after the fixed list: the installer never asks for it, so a tenant problem cannot fail an install.
	if *testCloud {
		r := checkResult{Name: "cloud", Detail: "not checked: the configuration did not load", Skipped: true}
		if got["config"].OK {
			r = cloudCheck(cfg)
		}
		if !r.Skipped {
			ok = ok && r.OK
		}
		res = append(res, r)
	}
	if *asJSON {
		b, _ := json.MarshalIndent(map[string]any{"ok": ok, "checks": res}, "", "  ")
		fmt.Fprintln(stdout, string(b))
	} else {
		tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
		for _, r := range res {
			mark := "OK"
			switch {
			case r.Skipped:
				mark = "SKIP"
			case !r.OK:
				mark = "FAIL"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", mark, r.Name, r.Detail)
		}
		_ = tw.Flush()
	}
	if !ok {
		return 1
	}
	return 0
}

// cloudCheck signs in with the Microsoft 365 / Entra ID connection saved in the settings (check -cloud) and reports
// the connection test's result as one check. Skipped when the connection is off.
func cloudCheck(cfg config.Config) checkResult {
	r := checkResult{Name: "cloud"}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	st, err := app.OpenStore(ctx, cfg)
	if err != nil {
		r.Detail = "not checked: the database is not available"
		r.Skipped = true
		return r
	}
	settings, err := st.GetSettings(ctx)
	_ = st.Close()
	if err != nil {
		r.Detail = "cannot read the settings: " + err.Error()
		return r
	}
	if !settings.Cloud.Enabled {
		r.Detail, r.Skipped = "not checked: the Microsoft 365 connection is off", true
		return r
	}
	cipher, err := secrets.Load(cfg.DataDir)
	if err != nil {
		r.Detail = "secrets: " + err.Error()
		return r
	}
	conn, err := cloud.ConnectionFrom(settings.Cloud, cipher)
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	ps := &ad.PowerShellClient{Exe: cfg.PowerShell.Exe, ScriptsDir: cfg.PowerShell.ScriptsDir, ExecutionPolicy: cfg.PowerShell.ExecutionPolicy,
		Timeout: time.Duration(cfg.PowerShell.TimeoutSeconds) * time.Second}
	checks, err := cloud.NewPowerShell(ps).Test(ctx, conn)
	if err != nil {
		r.Detail = err.Error()
		return r
	}
	var failed []string
	for _, c := range checks {
		if !c.OK {
			failed = append(failed, c.Name+": "+c.Detail)
		}
	}
	if len(failed) > 0 || len(checks) == 0 {
		r.Detail = strings.Join(failed, "; ")
		if r.Detail == "" {
			r.Detail = "the connection test returned no checks"
		}
		return r
	}
	r.OK, r.Detail = true, fmt.Sprintf("%d check(s) passed (tenant %s)", len(checks), settings.Cloud.TenantID)
	return r
}
