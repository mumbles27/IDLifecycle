// Package datadir checks and tightens the permissions of the service data directory, which holds the
// database, config.json and secret.key. On Windows the key is DPAPI-wrapped with machine scope, so any
// local user who can read the file can unwrap it: the directory ACL is the real protection.
package datadir

import (
	"errors"
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
// inheritable ACEs, files created later), or when the directory or one of its sensitive files is owned by
// an untrusted account: an owner can always rewrite the ACL, so a non-admin who pre-created the folder (any
// user may create folders in ProgramData) would keep access through every lockdown. Trusted owners are
// SYSTEM, Administrators, TrustedInstaller and the account running the check (the installing admin or the
// service account). A missing directory is not an error (it is created on first start).
func Check(dir string) error {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return nil
	}
	var problems []Problem
	for _, path := range append([]string{dir}, existingSensitive(dir)...) {
		p, err := broadReaders(path, path == dir)
		if err != nil {
			return fmt.Errorf("read ACL of %s: %w", path, err)
		}
		problems = append(problems, p...)
		owner, err := untrustedOwner(path)
		if err != nil {
			return fmt.Errorf("read owner of %s: %w", path, err)
		}
		if owner != "" {
			problems = append(problems, Problem{Path: path, Principal: owner + " (owner)"})
		}
	}
	if len(problems) > 0 {
		return &InsecureError{Dir: dir, Problems: problems}
	}
	return nil
}

func existingSensitive(dir string) []string {
	var out []string
	for _, name := range sensitiveFiles {
		if p := filepath.Join(dir, name); fileExists(p) {
			out = append(out, p)
		}
	}
	return out
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// Lockdown replaces the directory ACL with a protected one (inheritance from the parent off): SYSTEM and
// Administrators full control, the current user full control, and each named account Modify. Children
// inherit it. accounts may be "DOMAIN\name", "name$" (gMSA) or a SID string.
//
// It refuses a directory, or a sensitive file in it, owned by an untrusted account rather than adopting it:
// the content may have been planted (a config.json pointing scripts_dir at an attacker's folder). When the
// process may do so (elevated), the directory's owner is set to Administrators.
func Lockdown(dir string, accounts ...string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, path := range append([]string{dir}, existingSensitive(dir)...) {
		owner, err := untrustedOwner(path)
		if err != nil {
			return fmt.Errorf("read owner of %s: %w", path, err)
		}
		if owner != "" {
			return fmt.Errorf("%s is owned by %s, not by an administrator: it may have been created by someone else to keep access; "+
				"if you did not create it, delete it and run again, otherwise take ownership as an administrator (takeown /a /r /f <path>)", path, owner)
		}
	}
	if err := lockdown(dir, accounts); err != nil {
		return err
	}
	_ = setOwnerAdministrators(dir) // best effort: needs an elevated token; the current user is a trusted owner anyway
	return nil
}

// CheckNotWritable returns an *InsecureError when a broad principal (Everyone, Users, Authenticated Users,
// Domain Users...) can create, change or delete anything in dir or below (skip names whole subtrees, e.g.
// "Logs"), or when anything there is owned by an untrusted account. Used for the program and scripts
// folders: whoever can write there can make the service account run their code against Active Directory.
func CheckNotWritable(dir string, skip ...string) error {
	var problems []Problem
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != dir {
			for _, s := range skip {
				if strings.EqualFold(d.Name(), s) {
					return filepath.SkipDir
				}
			}
		}
		p, err := broadWriters(path, d.IsDir())
		if err != nil {
			return fmt.Errorf("read ACL of %s: %w", path, err)
		}
		problems = append(problems, p...)
		owner, err := untrustedOwner(path)
		if err != nil {
			return fmt.Errorf("read owner of %s: %w", path, err)
		}
		if owner != "" {
			problems = append(problems, Problem{Path: path, Principal: owner + " (owner)"})
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		return &WritableError{Dir: dir, Problems: problems}
	}
	return nil
}

// CheckEntryNotWritable is CheckNotWritable for one file or folder object, without descending into it
// (the program folder may hold unrelated files; what matters is that nobody can drop files into it or
// replace the executable).
func CheckEntryNotWritable(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	problems, err := broadWriters(path, fi.IsDir())
	if err != nil {
		return fmt.Errorf("read ACL of %s: %w", path, err)
	}
	owner, err := untrustedOwner(path)
	if err != nil {
		return fmt.Errorf("read owner of %s: %w", path, err)
	}
	if owner != "" {
		problems = append(problems, Problem{Path: path, Principal: owner + " (owner)"})
	}
	if len(problems) > 0 {
		return &WritableError{Dir: path, Problems: problems}
	}
	return nil
}

// WritableError lists who besides administrators can change a program/scripts folder.
type WritableError struct {
	Dir      string
	Problems []Problem
}

func (e *WritableError) Error() string {
	return fmt.Sprintf("%s can be changed by non-administrators (%s); anyone who can write there can make the service account run their code. "+
		"Install into a protected folder such as C:\\Program Files\\IDLifecycle (Install-IDLifecycle.ps1 does this)", e.Dir, e.list())
}

// list names who can change what (at most five entries).
func (e *WritableError) list() string {
	parts := make([]string, 0, min(len(e.Problems), 5))
	for i, p := range e.Problems {
		if i == 5 {
			parts = append(parts, fmt.Sprintf("... and %d more", len(e.Problems)-5))
			break
		}
		parts = append(parts, strings.Replace(p.String(), " can read ", " can change ", 1))
	}
	return strings.Join(parts, "; ")
}

// CheckTrustedFolder returns an error unless dir is a real folder (no symbolic link or junction, also nothing below it)
// that only SYSTEM, Administrators, TrustedInstaller and the current account can change, with a trusted owner on
// everything, in a parent folder nobody else can replace it in. Used for optional override folders (branding) that
// sit in ProgramData, where any user may create folders.
func CheckTrustedFolder(dir string) error {
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rp, err := isReparsePoint(path)
		if err != nil {
			return err
		}
		if rp {
			return fmt.Errorf("%s is a symbolic link or junction", path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	var we *WritableError
	if err := CheckNotWritable(dir); errors.As(err, &we) {
		return errors.New(we.list())
	} else if err != nil {
		return err
	}
	parent := filepath.Dir(filepath.Clean(dir))
	problems, err := broadReplacers(parent)
	if err != nil {
		return fmt.Errorf("read ACL of %s: %w", parent, err)
	}
	if owner, err := untrustedOwner(parent); err != nil {
		return fmt.Errorf("read owner of %s: %w", parent, err)
	} else if owner != "" {
		problems = append(problems, Problem{Path: parent, Principal: owner + " (owner)"})
	}
	if len(problems) > 0 {
		return errors.New((&WritableError{Dir: parent, Problems: problems}).list() + " (in the parent folder, so the folder could be replaced)")
	}
	return nil
}
