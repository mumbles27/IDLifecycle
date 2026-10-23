package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCSVPathPolicy(t *testing.T) {
	data := t.TempDir()
	hr := t.TempDir()
	c := Default(data)

	if err := c.CSVPathPolicy()(filepath.Join(data, "secret.key")); err == nil {
		t.Error("the data directory must never be readable as a source")
	}
	if err := c.CSVPathPolicy()(filepath.Join(data, "sub", "..", "config.json")); err == nil {
		t.Error("path tricks must not bypass the data-dir rule")
	}
	if err := c.CSVPathPolicy()(filepath.Join(hr, "people.csv")); err != nil {
		t.Errorf("elsewhere is fine by default: %v", err)
	}

	c.CSVAllowedDirs = []string{hr}
	if err := c.CSVPathPolicy()(filepath.Join(hr, "people.csv")); err != nil {
		t.Errorf("allowed dir: %v", err)
	}
	if err := c.CSVPathPolicy()(filepath.Join(t.TempDir(), "x.csv")); err == nil {
		t.Error("outside the allow-list must be refused")
	}
	if err := c.CSVPathPolicy()(filepath.Join(hr, "..", filepath.Base(data), "x.csv")); err == nil {
		t.Error("traversal out of the allowed dir must be refused")
	}

	// symlinks/junctions cannot smuggle the data dir in (skipped where symlinks are not permitted)
	link := filepath.Join(hr, "link")
	if err := os.Symlink(data, link); err == nil {
		if err := c.CSVPathPolicy()(filepath.Join(link, "secret.key")); err == nil {
			t.Error("symlink into the data dir must be refused")
		}
	}
}
