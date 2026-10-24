//go:build windows

package datadir

import (
	"fmt"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	fileReadData = 0x1        // FILE_READ_DATA / FILE_LIST_DIRECTORY
	genericRead  = 0x80000000 // GENERIC_READ
	genericAll   = 0x10000000 // GENERIC_ALL
	objInherit   = 0x1        // OBJECT_INHERIT_ACE
	modifyRights = 0x1301bf   // FILE_GENERIC_READ|WRITE|EXECUTE|DELETE ("Modify")
)

var broadSIDs = map[string]string{
	"S-1-1-0":      "Everyone",
	"S-1-5-7":      "ANONYMOUS LOGON",
	"S-1-5-4":      "INTERACTIVE",
	"S-1-5-11":     "Authenticated Users",
	"S-1-5-32-545": "BUILTIN\\Users",
	"S-1-5-32-546": "BUILTIN\\Guests",
}

// broadRID covers domain-relative groups such as Domain Users (513), Domain Guests (514), Domain Computers (515).
func broadName(sid string) (string, bool) {
	if n, ok := broadSIDs[sid]; ok {
		return n, true
	}
	if strings.HasPrefix(sid, "S-1-5-21-") {
		switch sid[strings.LastIndex(sid, "-")+1:] {
		case "513":
			return "Domain Users", true
		case "514":
			return "Domain Guests", true
		case "515":
			return "Domain Computers", true
		}
	}
	return "", false
}

// broadReaders lists broad principals with an allow ACE that grants read. For a directory only ACEs that
// flow to files (OBJECT_INHERIT) count: listing the folder is harmless, reading files in it is not.
func broadReaders(path string, isDir bool) ([]Problem, error) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return nil, err
	}
	if dacl == nil {
		return []Problem{{Path: path, Principal: "Everyone (the ACL is empty/NULL, which allows everyone)"}}, nil
	}
	var out []Problem
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return nil, err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			continue
		}
		if ace.Mask&(fileReadData|genericRead|genericAll) == 0 {
			continue
		}
		if isDir && ace.Header.AceFlags&objInherit == 0 {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		if name, ok := broadName(sid); ok {
			out = append(out, Problem{Path: path, Principal: name})
		}
	}
	return out, nil
}

func lockdown(dir string, accounts []string) error {
	var pin runtime.Pinner
	defer pin.Unpin()

	type grant struct {
		sid  *windows.SID
		mask windows.ACCESS_MASK
	}
	var grants []grant
	for _, t := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid} {
		sid, err := windows.CreateWellKnownSid(t)
		if err != nil {
			return err
		}
		grants = append(grants, grant{sid, windows.GENERIC_ALL})
	}
	tu, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	grants = append(grants, grant{tu.User.Sid, windows.GENERIC_ALL})
	for _, a := range accounts {
		if a = strings.TrimSpace(a); a == "" {
			continue
		}
		sid, err := windows.StringToSid(a)
		if err != nil {
			if sid, _, _, err = windows.LookupSID("", a); err != nil {
				return fmt.Errorf("cannot resolve account %q: %w", a, err)
			}
		}
		grants = append(grants, grant{sid, modifyRights})
	}

	entries := make([]windows.EXPLICIT_ACCESS, 0, len(grants))
	for _, g := range grants {
		pin.Pin(g.sid)
		entries = append(entries, windows.EXPLICIT_ACCESS{
			AccessPermissions: g.mask,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_UNKNOWN,
				TrusteeValue: windows.TrusteeValueFromSID(g.sid),
			},
		})
	}
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		return err
	}
	// PROTECTED drops everything inherited from the parent (ProgramData grants Users read); SetNamedSecurityInfo
	// propagates the new inheritable ACEs to existing children.
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}
