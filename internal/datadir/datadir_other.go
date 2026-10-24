//go:build !windows

package datadir

import (
	"fmt"
	"os"
)

// broadReaders reports group/other read bits on POSIX systems.
func broadReaders(path string, _ bool) ([]Problem, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o044 != 0 {
		return []Problem{{Path: path, Principal: fmt.Sprintf("group/other (mode %v)", fi.Mode().Perm())}}, nil
	}
	return nil, nil
}

func lockdown(dir string, _ []string) error { return os.Chmod(dir, 0o700) }
