// Package config loads the bootstrap configuration (everything needed before the DB is reachable).
// All operational settings (schedule, approvals, mappings...) live in the database.
package config

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

type TLS struct {
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
}

type Database struct {
	Driver string `json:"driver"`
	DSN    string `json:"dsn"` // may be "env:VAR_NAME" to keep credentials out of the file
}

type PowerShell struct {
	Exe             string `json:"exe"`
	ScriptsDir      string `json:"scripts_dir"`
	ExecutionPolicy string `json:"execution_policy"`
	TimeoutSeconds  int    `json:"timeout_seconds"`
}

type Config struct {
	Listen     string     `json:"listen"`
	TLS        TLS        `json:"tls"`
	DataDir    string     `json:"data_dir"`
	Database   Database   `json:"database"`
	PowerShell PowerShell `json:"powershell"`
	LogLevel   string     `json:"log_level"`
	// CSVAllowedDirs restricts where CSV sources may read from. Empty means anywhere except the data directory.
	CSVAllowedDirs []string `json:"csv_allowed_dirs"`

	path string
}

// Default returns sane defaults rooted at dataDir.
func Default(dataDir string) Config {
	return Config{
		Listen:  "127.0.0.1:8443",
		DataDir: dataDir,
		Database: Database{
			Driver: "sqlite",
			DSN:    "file:" + filepath.ToSlash(filepath.Join(dataDir, "idlifecycle.db")),
		},
		PowerShell: PowerShell{
			Exe:             "pwsh.exe",
			ScriptsDir:      filepath.Join(dataDir, "scripts"),
			ExecutionPolicy: "RemoteSigned",
			TimeoutSeconds:  600,
		},
		LogLevel: "info",
	}
}

// Load reads and validates path. Missing fields take defaults.
func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	c := Default(filepath.Dir(path))
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("parse config %s: %w", path, err)
	}
	c.path = path
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) Path() string { return c.path }

// ResolvedDSN expands env:VAR indirection.
func (c Config) ResolvedDSN() (string, error) {
	if name, ok := strings.CutPrefix(c.Database.DSN, "env:"); ok {
		v := os.Getenv(name)
		if v == "" {
			return "", fmt.Errorf("database.dsn refers to environment variable %s which is empty", name)
		}
		return v, nil
	}
	return c.Database.DSN, nil
}

func (c Config) Validate() error {
	switch c.Database.Driver {
	case "sqlite", "postgres", "mssql", "mysql":
	default:
		return fmt.Errorf("database.driver must be sqlite, postgres, mssql or mysql (got %q)", c.Database.Driver)
	}
	if c.Database.DSN == "" {
		return fmt.Errorf("database.dsn is required")
	}
	if c.DataDir == "" {
		return fmt.Errorf("data_dir is required")
	}
	if c.PowerShell.Exe == "" || c.PowerShell.ScriptsDir == "" {
		return fmt.Errorf("powershell.exe and powershell.scripts_dir are required")
	}
	if c.PowerShell.TimeoutSeconds < 10 {
		return fmt.Errorf("powershell.timeout_seconds must be at least 10")
	}
	if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
		return fmt.Errorf("tls.cert_file and tls.key_file must be set together")
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen must be host:port: %w", err)
	}
	if c.TLS.CertFile == "" && !isLoopback(host) {
		return fmt.Errorf("listen %q is not loopback: configure tls.cert_file/tls.key_file (credentials and session cookies must not cross the network in clear text)", c.Listen)
	}
	return nil
}

func (c Config) TLSEnabled() bool { return c.TLS.CertFile != "" }

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
