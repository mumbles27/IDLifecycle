// Package datadir checks and tightens the permissions of the service data directory, which holds the
// database, config.json and secret.key. On Windows the key is DPAPI-wrapped with machine scope, so any
// local user who can read the file can unwrap it: the directory ACL is the real protection.
package datadir

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// sensitiveFiles are checked individually because a file can carry its own (explicit) ACL.
var sensitiveFiles = []string{"secret.key", "config.json", "idlifecycle.db", "idlifecycle.db-wal", "idlifecycle.db-shm"}

// Problem is one principal that should not be able to read the data directory.
type Problem struct {
	Path      string
	Principal string
}

func (p Problem) String() string { return p.Principal + " can read " + p.Path }

// InsecureError lists every broad principal (Everyone, Users, Authenticated Users, ...) with read access.
type InsecureError struct {
	Dir      string
	Problems []Problem
}

func (e *InsecureError) Error() string {
	parts := make([]string, 0, len(e.Problems))
	for _, p := range e.Problems {
		parts = append(parts, p.String())
	}
	return fmt.Sprintf("the data directory %s is readable by non-administrators (%s); it holds the database and the key that decrypts source secrets. "+
		"Fix it with: idlifecycled lockdown -config <config.json> -account <service account>  (or run scripts\\Install-IDLifecycleService.ps1)",
		e.Dir, strings.Join(parts, "; "))
}

// Check returns an *InsecureError when broad principals can read the directory's files (now or, through
// inheritable ACEs, files created later). A missing directory is not an error (it is created on first start).
func Check(dir string) error {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}
	var problems []Problem
	p, err := broadReaders(dir, true)
	if err != nil {
		return fmt.Errorf("read ACL of %s: %w", dir, err)
	}
	problems = append(problems, p...)
	for _, name := range sensitiveFiles {
		path := filepath.Join(dir, name)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		p, err := broadReaders(path, false)
		if err != nil {
			return fmt.Errorf("read ACL of %s: %w", path, err)
		}
		problems = append(problems, p...)
	}
	if len(problems) > 0 {
		return &InsecureError{Dir: dir, Problems: problems}
	}
	return nil
}

// Lockdown replaces the directory ACL with a protected one (inheritance from the parent off): SYSTEM and
// Administrators full control, the current user full control, and each named account Modify. Children
// inherit it. accounts may be "DOMAIN\name", "name$" (gMSA) or a SID string.
func Lockdown(dir string, accounts ...string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return lockdown(dir, accounts)
}
