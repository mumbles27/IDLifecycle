package config

import (
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

// canon makes a comparable absolute path. Symlinks, junctions and 8.3 short names are resolved on the
// deepest existing ancestor, so a file that does not exist yet cannot dodge the comparison.
func canon(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = p
	}
	abs = filepath.Clean(abs)
	rest := ""
	cur := abs
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			abs = filepath.Join(r, rest)
			break
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
	abs = filepath.Clean(abs)
	if runtime.GOOS == "windows" {
		abs = strings.ToLower(abs)
	}
	return abs
}
func within(path, dir string) bool {
	p, d := canon(path), canon(dir)
	return p == d || strings.HasPrefix(p, d+string(filepath.Separator))
}

// CSVPathPolicy returns the check applied to CSV source paths: never inside the data directory
// (database, key file, config, logs) and, when csv_allowed_dirs is set, only inside those directories.
func (c Config) CSVPathPolicy() func(string) error {
	return func(path string) error {
		if strings.TrimSpace(path) == "" {
			return fmt.Errorf("path is required")
		}
		if within(path, c.DataDir) {
			return fmt.Errorf("files inside the service data directory cannot be used as a source")
		}
		if len(c.CSVAllowedDirs) == 0 {
			return nil
		}
		for _, d := range c.CSVAllowedDirs {
			if within(path, d) {
				return nil
			}
		}
		return fmt.Errorf("path is outside the directories allowed by csv_allowed_dirs")
	}
}
