//go:build !windows

package datadir

import (
	"fmt"
	"os"
	"syscall"
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

// untrustedOwner reports an owner other than root or the current user.
func untrustedOwner(path string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid != 0 && int(st.Uid) != os.Getuid() {
		return fmt.Sprintf("uid %d", st.Uid), nil
	}
	return "", nil
}

// broadWriters reports group/other write bits.
func broadWriters(path string, _ bool) ([]Problem, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return []Problem{{Path: path, Principal: fmt.Sprintf("group/other (mode %v)", fi.Mode().Perm())}}, nil
	}
	return nil, nil
}

func setOwnerAdministrators(string) error { return nil }

// broadReplacers reports group/other write bits on a parent folder (without the sticky bit).
func broadReplacers(path string) ([]Problem, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Mode().Perm()&0o022 != 0 && fi.Mode()&os.ModeSticky == 0 {
		return []Problem{{Path: path, Principal: fmt.Sprintf("group/other (mode %v)", fi.Mode().Perm())}}, nil
	}
	return nil, nil
}

func isReparsePoint(path string) (bool, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	return fi.Mode()&os.ModeSymlink != 0, nil
}
