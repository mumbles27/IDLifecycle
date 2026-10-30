package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"idlifecycle/internal/config"
)

// configView is what `config get` prints: the basics an operator (or the tray) shows and edits. The database DSN is
// never part of it.
type configView struct {
	Listen      string `json:"listen"`
	TLSCertFile string `json:"tls_cert_file"`
	TLSKeyFile  string `json:"tls_key_file"`
	LogLevel    string `json:"log_level"`
	DataDir     string `json:"data_dir"`
	DBDriver    string `json:"db_driver"`
	ScriptsDir  string `json:"scripts_dir"`
	ConsoleURL  string `json:"console_url"`
}

func viewOf(c config.Config) configView {
	return configView{
		Listen: c.Listen, TLSCertFile: c.TLS.CertFile, TLSKeyFile: c.TLS.KeyFile, LogLevel: c.LogLevel,
		DataDir: c.DataDir, DBDriver: c.Database.Driver, ScriptsDir: c.PowerShell.ScriptsDir, ConsoleURL: consoleURL(c),
	}
}

// consoleURL is the address to open the console at: https when TLS is configured; localhost for a loopback listen,
// this machine's DNS name for a wildcard one, else the listen host.
func consoleURL(c config.Config) string {
	scheme := "http"
	if c.TLSEnabled() {
		scheme = "https"
	}
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return ""
	}
	ip := net.ParseIP(host)
	switch {
	case host == "" || (ip != nil && ip.IsUnspecified()):
		if h, err := os.Hostname(); err == nil && h != "" {
			host = h
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second) // the DNS name, when DNS answers quickly
			cname, err := net.DefaultResolver.LookupCNAME(ctx, h)
			cancel()
			if err == nil && cname != "" {
				host = strings.TrimSuffix(cname, ".")
			}
		} else {
			host = "localhost"
		}
	case host == "localhost" || (ip != nil && ip.IsLoopback()):
		host = "localhost"
	}
	return scheme + "://" + net.JoinHostPort(host, port) + "/"
}

var logLevels = map[string]bool{"debug": true, "info": true, "warn": true, "error": true}

func cmdConfig(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "get" && args[0] != "set") {
		fmt.Fprint(stderr, usage)
		return 2
	}
	fs := flag.NewFlagSet("config "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := configFlag(fs)
	var listen, cert, key, level *string
	if args[0] == "set" {
		listen = fs.String("listen", "", "address the console listens on, host:port")
		cert = fs.String("tls-cert", "", "HTTPS certificate file (PEM); empty = no HTTPS")
		key = fs.String("tls-key", "", "HTTPS key file (PEM); empty = no HTTPS")
		level = fs.String("log-level", "", "debug | info | warn | error")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if args[0] == "get" {
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			fmt.Fprintln(stderr, "error:", err)
			return 1
		}
		return printView(stdout, stderr, cfg)
	}

	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	if len(given) == 0 || (len(given) == 1 && given["config"]) {
		fmt.Fprintln(stderr, "error: nothing to change: give -listen, -tls-cert/-tls-key or -log-level")
		return 2
	}
	raw, err := os.ReadFile(*cfgPath) // #nosec G304 G703 -- the config path an administrator passed on the command line
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if _, err := config.Parse(raw, *cfgPath); err != nil {
		fmt.Fprintln(stderr, "error: the current config.json is not valid, fix it first:", err)
		return 1
	}
	var doc map[string]any // every other field (and its value) is kept as it is
	if err := json.Unmarshal(raw, &doc); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	if given["listen"] {
		doc["listen"] = strings.TrimSpace(*listen)
	}
	if given["tls-cert"] || given["tls-key"] {
		t, _ := doc["tls"].(map[string]any)
		if t == nil {
			t = map[string]any{}
		}
		c, k := strings.TrimSpace(*cert), strings.TrimSpace(*key)
		switch {
		case given["tls-cert"] && given["tls-key"]:
		case given["tls-cert"] && c == "", given["tls-key"] && k == "":
			c, k = "", "" // clearing one clears HTTPS
		case given["tls-cert"]:
			k, _ = t["key_file"].(string)
		default:
			c, _ = t["cert_file"].(string)
		}
		t["cert_file"], t["key_file"] = c, k
		doc["tls"] = t
	}
	if given["log-level"] {
		l := strings.ToLower(strings.TrimSpace(*level))
		if !logLevels[l] {
			fmt.Fprintln(stderr, "error: -log-level must be debug, info, warn or error")
			return 2
		}
		doc["log_level"] = l
	}

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	out = append(out, '\n')
	cfg, err := config.Parse(out, *cfgPath)
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 2
	}
	if cfg.TLSEnabled() {
		if _, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile); err != nil {
			fmt.Fprintln(stderr, "error: the HTTPS certificate and key cannot be used:", err)
			return 2
		}
	}
	if bytes.Equal(bytes.TrimSpace(out), bytes.TrimSpace(raw)) {
		return printView(stdout, stderr, cfg)
	}
	if err := replaceFile(*cfgPath, out); err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	return printView(stdout, stderr, cfg)
}

func printView(stdout, stderr io.Writer, cfg config.Config) int {
	b, err := json.MarshalIndent(viewOf(cfg), "", "  ")
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	fmt.Fprintln(stdout, string(b))
	return 0
}

// replaceFile writes data next to path and renames it over path, so a reader never sees half a file. The new file
// inherits the folder's (locked-down) ACL, which is where config.json's protection comes from.
func replaceFile(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.json.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(name) // #nosec G703 -- our own temp file next to the config file
		return err
	}
	if err := os.Rename(name, path); err != nil { // #nosec G703 -- replaces the config file the administrator named
		_ = os.Remove(name) // #nosec G703 -- our own temp file next to the config file
		return err
	}
	return nil
}
