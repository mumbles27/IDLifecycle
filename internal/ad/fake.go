package ad

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"idlifecycle/internal/model"
)

// FakeUser is an account in the Fake directory.
type FakeUser struct {
	GUID           string
	DN             string
	Sam            string
	UPN            string
	SID            string // objectSid (S-1-5-21-...); set on create
	Enabled        bool
	External       string
	Attrs          map[string]string
	MemberOf       []string // group DNs
	ProxyAddresses []string // proxyAddresses (a link rename changes them)
}

// Fake is an in-memory directory implementing Client with the same semantics the PowerShell
// scripts promise (idempotent create, externalId verification, refuse-to-delete-enabled).
type Fake struct {
	mu     sync.Mutex
	Users  map[string]*FakeUser // by GUID
	Groups map[string]string    // lower(sam or DN) -> DN
	// FailExternal makes every action for that external id fail with the given message.
	FailExternal map[string]string
	SnapshotErr  error
	SchemaAttrs  []SchemaAttribute // returned by Schema (BuiltinUserAttributes when nil)
	SchemaErr    error
	ActionErr    error // returned for every action call (simulates exit 4 etc.)
	Calls        []string
	Passwords    []string            // passwords received by Create, for assertions
	MustChange   []bool              // ChangePasswordAtLogon received by Create, in the same order
	OUList       []string            // OUs reported by OUs (AddOU)
	MailObjects  map[string]string   // lower(mail) -> DN of contacts/groups holding the address (AddMailObject)
	NamesErr     error               // returned by CheckNames
	GroupInfo    map[string]GroupHit // lower(sam) -> search details (SetGroupInfo)
	guid         int
}

func NewFake() *Fake {
	return &Fake{Users: map[string]*FakeUser{}, Groups: map[string]string{}, FailExternal: map[string]string{}}
}

func (f *Fake) AddGroup(sam, dn string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Groups[strings.ToLower(sam)] = dn
	f.Groups[strings.ToLower(dn)] = dn
}

// AddUser seeds an account and returns its GUID.
func (f *Fake) AddUser(u FakeUser) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u.GUID == "" {
		f.guid++
		u.GUID = fmt.Sprintf("guid-%04d", f.guid)
	}
	if u.Attrs == nil {
		u.Attrs = map[string]string{}
	}
	cp := u
	f.Users[u.GUID] = &cp
	return u.GUID
}

// ByExternal returns a copy of the account with the given external id.
func (f *Fake) ByExternal(ext string) (FakeUser, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.Users {
		if u.External == ext {
			return cloneUser(u), true
		}
	}
	return FakeUser{}, false
}

// Mutate edits the account with the given external id in place (simulates out-of-band changes in AD).
func (f *Fake) Mutate(ext string, fn func(u *FakeUser)) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.Users {
		if u.External == ext {
			fn(u)
			return true
		}
	}
	return false
}

// Reset removes all accounts (simulates an empty/mis-scoped directory).
func (f *Fake) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Users = map[string]*FakeUser{}
}

func (f *Fake) CallLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.Calls...)
}

func cloneUser(u *FakeUser) FakeUser {
	c := *u
	c.Attrs = map[string]string{}
	for k, v := range u.Attrs {
		c.Attrs[k] = v
	}
	c.MemberOf = append([]string(nil), u.MemberOf...)
	c.ProxyAddresses = append([]string(nil), u.ProxyAddresses...)
	return c
}

func parentOf(dn string) string {
	if i := strings.Index(dn, ","); i >= 0 {
		return dn[i+1:]
	}
	return ""
}

func under(dn, base string) bool {
	d, b := strings.ToLower(dn), strings.ToLower(base)
	return d == b || strings.HasSuffix(d, ","+b)
}

func (f *Fake) Snapshot(_ context.Context, req SnapshotRequest) (*Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "snapshot")
	if f.SnapshotErr != nil {
		return nil, f.SnapshotErr
	}
	snap := &Snapshot{Users: []User{}, Groups: []GroupInfo{}}
	guids := make([]string, 0, len(f.Users))
	for g := range f.Users {
		guids = append(guids, g)
	}
	sort.Strings(guids)
	for _, g := range guids {
		u := f.Users[g]
		if u.External == "" && !req.IncludeUnlinked {
			continue
		}
		in := len(req.SearchBases) == 0
		for _, b := range req.SearchBases {
			if under(u.DN, b) {
				in = true
			}
		}
		if !in {
			continue
		}
		attrs := map[string]*string{}
		for _, p := range req.Properties {
			var val *string
			for k, v := range u.Attrs {
				if strings.EqualFold(k, p) {
					v := v
					val = &v
				}
			}
			attrs[p] = val
		}
		snap.Users = append(snap.Users, User{
			ObjectGUID: u.GUID, DN: u.DN, Sam: u.Sam, UPN: u.UPN, SID: u.SID, Enabled: u.Enabled,
			ParentOU: parentOf(u.DN), ExternalID: u.External, Attributes: attrs, MemberOf: append([]string{}, u.MemberOf...),
		})
	}
	for _, g := range req.Groups {
		dn, ok := f.Groups[strings.ToLower(g)]
		snap.Groups = append(snap.Groups, GroupInfo{Identity: g, DN: dn, Found: ok})
	}
	return snap, nil
}

func (f *Fake) begin(name string, o ActionOpts, n int) error {
	f.Calls = append(f.Calls, fmt.Sprintf("%s(n=%d,dry=%t)", name, n, o.DryRun))
	return f.ActionErr
}

func (f *Fake) failFor(ext, id string) (Result, bool) {
	if msg, ok := f.FailExternal[ext]; ok {
		return Result{ChangeID: id, Status: StatusFailed, Error: msg}, true
	}
	return Result{}, false
}

func (f *Fake) samTaken(sam string) bool {
	for _, u := range f.Users {
		if strings.EqualFold(u.Sam, sam) {
			return true
		}
	}
	return false
}

func (f *Fake) upnTaken(upn string) bool {
	for _, u := range f.Users {
		if strings.EqualFold(u.UPN, upn) {
			return true
		}
	}
	return false
}

func (f *Fake) cnTaken(ou, cn string) bool {
	want := strings.ToLower("CN=" + cn + "," + ou)
	for _, u := range f.Users {
		if strings.ToLower(u.DN) == want {
			return true
		}
	}
	return false
}

func (f *Fake) resolveGroup(id string) (string, bool) {
	dn, ok := f.Groups[strings.ToLower(id)]
	return dn, ok
}

func addMember(u *FakeUser, dn string) {
	for _, m := range u.MemberOf {
		if strings.EqualFold(m, dn) {
			return
		}
	}
	u.MemberOf = append(u.MemberOf, dn)
}

func removeMember(u *FakeUser, dn string) bool {
	for i, m := range u.MemberOf {
		if strings.EqualFold(m, dn) {
			u.MemberOf = append(u.MemberOf[:i], u.MemberOf[i+1:]...)
			return true
		}
	}
	return false
}

func (f *Fake) Create(_ context.Context, o ActionOpts, items []CreateItem) ([]Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("create", o, len(items)); err != nil {
		return nil, err
	}
	var out []Result
	for _, it := range items {
		f.Passwords = append(f.Passwords, it.Password)
		f.MustChange = append(f.MustChange, it.ChangePasswordAtLogon)
		if r, bad := f.failFor(it.ExternalID, it.ChangeID); bad {
			out = append(out, r)
			continue
		}
		for _, u := range f.Users {
			if u.External == it.ExternalID {
				out = append(out, Result{ChangeID: it.ChangeID, Status: StatusSkipped, Message: "exists", ObjectGUID: u.GUID, DN: u.DN, Sam: u.Sam, UPN: u.UPN})
				goto next
			}
		}
		{
			var chosen *struct{ sam, upn, cn, mail string }
			for _, c := range it.Candidates {
				if mailDN, _ := f.mailOwner(c.Mail, nil); !f.samTaken(c.Sam) && !f.upnTaken(c.UPN) && !f.cnTaken(it.OU, c.CN) && mailDN == "" {
					chosen = &struct{ sam, upn, cn, mail string }{c.Sam, c.UPN, c.CN, c.Mail}
					break
				}
			}
			if chosen == nil {
				out = append(out, Result{ChangeID: it.ChangeID, Status: StatusFailed, Error: "no free sAMAccountName/UPN/CN/mail among the candidates"})
				continue
			}
			dn := "CN=" + chosen.cn + "," + it.OU
			if o.DryRun {
				out = append(out, Result{ChangeID: it.ChangeID, Status: StatusWould, DN: dn, Sam: chosen.sam, UPN: chosen.upn})
				continue
			}
			f.guid++
			u := &FakeUser{GUID: fmt.Sprintf("guid-%04d", f.guid), SID: fmt.Sprintf("S-1-5-21-100-200-300-%d", 1000+f.guid), DN: dn, Sam: chosen.sam, UPN: chosen.upn, Enabled: it.Enabled, External: it.ExternalID, Attrs: map[string]string{}}
			for k, v := range it.Attributes {
				u.Attrs[k] = v
			}
			if chosen.mail != "" { // like New-IDLUser.ps1: the chosen candidate's mail is set at creation
				u.Attrs["mail"] = chosen.mail
			}
			f.Users[u.GUID] = u
			var missing []string
			for _, g := range it.Groups {
				if gdn, ok := f.resolveGroup(g); ok {
					addMember(u, gdn)
				} else {
					missing = append(missing, g)
				}
			}
			r := Result{ChangeID: it.ChangeID, Status: StatusApplied, ObjectGUID: u.GUID, DN: u.DN, Sam: u.Sam, UPN: u.UPN, Before: &model.BeforeState{Created: true}}
			if len(missing) > 0 {
				r.Status, r.Error = StatusFailed, "account created but groups not found: "+strings.Join(missing, ", ")
			}
			out = append(out, r)
		}
	next:
	}
	return out, nil
}

// find locates an account and verifies the external id guard.
func (f *Fake) find(guid, ext, id string) (*FakeUser, *Result) {
	u, ok := f.Users[guid]
	if !ok {
		return nil, &Result{ChangeID: id, Status: StatusFailed, Error: "account not found"}
	}
	if u.External != ext {
		return nil, &Result{ChangeID: id, Status: StatusFailed, Error: fmt.Sprintf("account %s has %s %q, expected %q", guid, "id attribute", u.External, ext)}
	}
	return u, nil
}

func (f *Fake) Update(_ context.Context, o ActionOpts, items []UpdateItem) ([]Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("update", o, len(items)); err != nil {
		return nil, err
	}
	var out []Result
	for _, it := range items {
		if r, bad := f.failFor(it.ExternalID, it.ChangeID); bad {
			out = append(out, r)
			continue
		}
		u, fail := f.find(it.ObjectGUID, it.ExternalID, it.ChangeID)
		if fail != nil {
			out = append(out, *fail)
			continue
		}
		out = append(out, f.runOps(u, o.DryRun, it.ChangeID, it.Expect, it.Force, fakeOps{
			set: it.Set, clear: it.Clear, add: it.AddGroups, remove: it.RemoveGroups, moveTo: it.MoveTo, reportMissing: true,
		}))
	}
	return out, nil
}

func (f *Fake) Disable(_ context.Context, o ActionOpts, items []DisableItem) ([]Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("disable", o, len(items)); err != nil {
		return nil, err
	}
	var out []Result
	off := false
	for _, it := range items {
		if r, bad := f.failFor(it.ExternalID, it.ChangeID); bad {
			out = append(out, r)
			continue
		}
		u, fail := f.find(it.ObjectGUID, it.ExternalID, it.ChangeID)
		if fail != nil {
			out = append(out, *fail)
			continue
		}
		set := it.Set
		if it.Description != "" {
			set = map[string]string{"description": it.Description}
			for k, v := range it.Set {
				set[k] = v
			}
		}
		out = append(out, f.runOps(u, o.DryRun, it.ChangeID, it.Expect, it.Force, fakeOps{
			enabled: &off, set: set, clear: it.Clear, add: it.AddGroups, remove: it.RemoveGroups, moveTo: it.MoveTo,
		}))
	}
	return out, nil
}

func (f *Fake) Enable(_ context.Context, o ActionOpts, items []EnableItem) ([]Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("enable", o, len(items)); err != nil {
		return nil, err
	}
	var out []Result
	on := true
	for _, it := range items {
		if r, bad := f.failFor(it.ExternalID, it.ChangeID); bad {
			out = append(out, r)
			continue
		}
		u, fail := f.find(it.ObjectGUID, it.ExternalID, it.ChangeID)
		if fail != nil {
			out = append(out, *fail)
			continue
		}
		out = append(out, f.runOps(u, o.DryRun, it.ChangeID, it.Expect, it.Force, fakeOps{
			enabled: &on, set: it.Set, clear: it.Clear, add: it.AddGroups, remove: it.RemoveGroups, moveTo: it.MoveTo,
		}))
	}
	return out, nil
}

func (f *Fake) Remove(_ context.Context, o ActionOpts, items []RemoveItem) ([]Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("remove", o, len(items)); err != nil {
		return nil, err
	}
	var out []Result
	for _, it := range items {
		if r, bad := f.failFor(it.ExternalID, it.ChangeID); bad {
			out = append(out, r)
			continue
		}
		u, ok := f.Users[it.ObjectGUID]
		if !ok {
			out = append(out, Result{ChangeID: it.ChangeID, Status: StatusSkipped, Message: "already gone"})
			continue
		}
		if u.External != it.ExternalID {
			out = append(out, Result{ChangeID: it.ChangeID, Status: StatusFailed, Error: "id attribute does not match external id"})
			continue
		}
		if u.Enabled {
			out = append(out, Result{ChangeID: it.ChangeID, Status: StatusFailed, Error: "account is enabled; refusing to delete"})
			continue
		}
		if o.DryRun {
			out = append(out, Result{ChangeID: it.ChangeID, Status: StatusWould, ObjectGUID: u.GUID, DN: u.DN})
			continue
		}
		delete(f.Users, it.ObjectGUID)
		out = append(out, Result{ChangeID: it.ChangeID, Status: StatusApplied, ObjectGUID: u.GUID, DN: u.DN})
	}
	return out, nil
}

var _ Client = (*Fake)(nil)
var _ Client = (*PowerShellClient)(nil)

// ErrFakeDown is a convenient canned connection failure for tests.
var ErrFakeDown = &ScriptError{Script: "fake", ExitCode: 4, Message: "domain controller unreachable", kind: ErrConnection}
