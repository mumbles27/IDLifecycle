package ad

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// PowerShellClient runs the scripts in ScriptsDir. Requests go in on stdin (never argv, so
// passwords and PII stay out of process listings) and one JSON document comes back on stdout.
type PowerShellClient struct {
	Exe             string
	ScriptsDir      string
	ExecutionPolicy string
	Timeout         time.Duration
	// ExtraEnv is appended to the child environment (tests point PSModulePath at a stub module).
	ExtraEnv []string
	Logger   *slog.Logger
}

const (
	scriptSnapshot = "Get-IDLSnapshot.ps1"
	scriptNew      = "New-IDLUser.ps1"
	scriptSet      = "Set-IDLUser.ps1"
	scriptDisable  = "Disable-IDLUser.ps1"
	scriptEnable   = "Enable-IDLUser.ps1"
	scriptRemove   = "Remove-IDLUser.ps1"
)

type actionRequest[T any] struct {
	Server      string `json:"server"`
	IDAttribute string `json:"idAttribute"`
	Items       []T    `json:"items"`
}

type actionResponse struct {
	OK      bool     `json:"ok"`
	DryRun  bool     `json:"dryRun"`
	Results []Result `json:"results"`
	Error   string   `json:"error"`
}

func (c *PowerShellClient) Snapshot(ctx context.Context, req SnapshotRequest) (*Snapshot, error) {
	var resp struct {
		OK     bool        `json:"ok"`
		Error  string      `json:"error"`
		Users  []User      `json:"users"`
		Groups []GroupInfo `json:"groups"`
	}
	if err := c.run(ctx, scriptSnapshot, false, req, &resp, nil); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("%s: %s", scriptSnapshot, orUnknown(resp.Error))
	}
	return &Snapshot{Users: resp.Users, Groups: resp.Groups}, nil
}

func (c *PowerShellClient) Create(ctx context.Context, o ActionOpts, items []CreateItem) ([]Result, error) {
	pw := make([]string, 0, len(items))
	for _, it := range items {
		pw = append(pw, it.Password)
	}
	return runAction(ctx, c, scriptNew, o, items, func(i CreateItem) string { return i.ChangeID }, pw)
}

func (c *PowerShellClient) Update(ctx context.Context, o ActionOpts, items []UpdateItem) ([]Result, error) {
	return runAction(ctx, c, scriptSet, o, items, func(i UpdateItem) string { return i.ChangeID }, nil)
}

func (c *PowerShellClient) Disable(ctx context.Context, o ActionOpts, items []DisableItem) ([]Result, error) {
	return runAction(ctx, c, scriptDisable, o, items, func(i DisableItem) string { return i.ChangeID }, nil)
}

func (c *PowerShellClient) Enable(ctx context.Context, o ActionOpts, items []EnableItem) ([]Result, error) {
	return runAction(ctx, c, scriptEnable, o, items, func(i EnableItem) string { return i.ChangeID }, nil)
}

func (c *PowerShellClient) Remove(ctx context.Context, o ActionOpts, items []RemoveItem) ([]Result, error) {
	return runAction(ctx, c, scriptRemove, o, items, func(i RemoveItem) string { return i.ChangeID }, nil)
}

func runAction[T any](ctx context.Context, c *PowerShellClient, script string, o ActionOpts, items []T, id func(T) string, redact []string) ([]Result, error) {
	if len(items) == 0 {
		return nil, nil
	}
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = id(it)
	}
	req := actionRequest[T]{Server: o.Server, IDAttribute: o.IDAttribute, Items: items}

	var resp actionResponse
	err := c.run(ctx, script, o.DryRun, req, &resp, redact)
	var se *ScriptError
	switch {
	case err == nil:
	case errors.As(err, &se) && se.ExitCode == 1:
		// Partial success: some items failed; the response still carries per-item results.
	default:
		return nil, err
	}
	if !resp.OK && len(resp.Results) == 0 {
		return nil, fmt.Errorf("%s: %s", script, orUnknown(resp.Error))
	}
	return reconcile(ids, resp.Results), nil
}

// run executes one script. For exit codes 0 and 1 stdout is decoded into out; for exit 1 the
// ScriptError is returned *and* out is populated. Any other exit code yields a ScriptError.
func (c *PowerShellClient) run(ctx context.Context, script string, dryRun bool, req any, out any, redact []string) error {
	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	policy := c.ExecutionPolicy
	if policy == "" {
		policy = "RemoteSigned"
	}
	args := []string{"-NoProfile", "-NonInteractive", "-ExecutionPolicy", policy, "-File", filepath.Join(c.ScriptsDir, script)}
	if dryRun {
		args = append(args, "-DryRun")
	}

	cmd := exec.CommandContext(ctx, c.Exe, args...)
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if len(c.ExtraEnv) > 0 {
		cmd.Env = append(cmd.Environ(), c.ExtraEnv...)
	}

	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start).Round(time.Millisecond)

	if ctx.Err() != nil {
		return fmt.Errorf("%s: %w after %s", script, ctx.Err(), elapsed)
	}

	exit := 0
	if runErr != nil {
		var ee *exec.ExitError
		if !errors.As(runErr, &ee) {
			return fmt.Errorf("start %s: %w", c.Exe, runErr)
		}
		exit = ee.ExitCode()
	}

	if c.Logger != nil {
		c.Logger.Debug("powershell finished", "script", script, "exit", exit, "elapsed", elapsed.String(), "dry_run", dryRun,
			"stderr", scrub(stderr.String(), redact))
	}

	decodeErr := decodeSingleJSON(stdout.Bytes(), out)

	switch exit {
	case 0:
		if decodeErr != nil {
			return fmt.Errorf("%s: unreadable output: %w (stderr: %s)", script, decodeErr, snippet(scrub(stderr.String(), redact)))
		}
		return nil
	case 1:
		if decodeErr != nil {
			return &ScriptError{Script: script, ExitCode: 1, Message: "partial failure; output unreadable: " + decodeErr.Error(), kind: ErrFatal}
		}
		return &ScriptError{Script: script, ExitCode: 1, Message: "some items failed", kind: nil}
	}

	msg := scriptMessage(stdout.Bytes())
	if msg == "" {
		msg = snippet(scrub(stderr.String(), redact))
	}
	if strings.Contains(stderr.String(), "is not digitally signed") {
		msg += blockedHint
	}
	return &ScriptError{Script: script, ExitCode: exit, Message: scrub(msg, redact), kind: kindFor(exit)}
}

// BlockedScripts lists .ps1/.psm1/.psd1 files under dir that carry a Zone.Identifier stream (Mark of the
// Web). Under RemoteSigned/AllSigned PowerShell refuses to load them, so every sync would fail.
func BlockedScripts(dir string) []string {
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		switch strings.ToLower(filepath.Ext(p)) {
		case ".ps1", ".psm1", ".psd1":
			if _, err := os.Stat(p + ":Zone.Identifier"); err == nil {
				out = append(out, p)
			}
		}
		return nil
	})
	return out
}

const blockedHint = " (the script files are blocked as downloaded from the internet; run: Get-ChildItem <scripts_dir> -Recurse | Unblock-File, or reinstall with Install-IDLifecycleService.ps1)"

func kindFor(code int) error {
	switch code {
	case 2:
		return ErrInvalidInput
	case 3:
		return ErrDependency
	case 4:
		return ErrConnection
	}
	return ErrFatal
}

// decodeSingleJSON requires stdout to hold exactly one JSON document.
func decodeSingleJSON(b []byte, out any) error {
	b = bytes.TrimPrefix(bytes.TrimSpace(b), []byte("\xef\xbb\xbf"))
	if len(b) == 0 {
		return errors.New("empty output")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(out); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("output contains more than one JSON document or trailing text")
	}
	return nil
}

func scriptMessage(b []byte) string {
	var m struct {
		Error string `json:"error"`
	}
	if err := decodeSingleJSON(b, &m); err != nil {
		return ""
	}
	return m.Error
}

func scrub(s string, secrets []string) string {
	for _, sec := range secrets {
		if sec != "" {
			s = strings.ReplaceAll(s, sec, "[REDACTED]")
		}
	}
	return s
}

func snippet(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 500 {
		return s[:500] + "..."
	}
	return s
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown error"
	}
	return s
}
