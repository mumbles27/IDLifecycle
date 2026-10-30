package ad

import (
	"fmt"
	"sort"
	"strings"

	"idlifecycle/internal/model"
)

// fakeOps is one item's changes to an account (Update, Disable, Enable share it).
type fakeOps struct {
	enabled       *bool
	set           map[string]string
	clear         []string
	add, remove   []string
	moveTo        string
	reportMissing bool // unknown groups fail the item (Update); Disable/Enable ignore them like the scripts do
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

// attrFold returns an attribute value by case-insensitive name.
func attrFold(u *FakeUser, name string) (string, bool) {
	if v, ok := u.Attrs[name]; ok {
		return v, true
	}
	for k, v := range u.Attrs {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return "", false
}

func setAttrFold(u *FakeUser, name, v string) {
	for k := range u.Attrs {
		if k != name && strings.EqualFold(k, name) {
			delete(u.Attrs, k)
		}
	}
	u.Attrs[name] = v
}

func clearAttrFold(u *FakeUser, name string) bool {
	found := false
	for k := range u.Attrs {
		if strings.EqualFold(k, name) {
			delete(u.Attrs, k)
			found = true
		}
	}
	return found
}

func isMember(u *FakeUser, dn string) bool {
	for _, m := range u.MemberOf {
		if strings.EqualFold(m, dn) {
			return true
		}
	}
	return false
}

func rdnValue(dn string) string {
	first := strings.SplitN(dn, ",", 2)[0]
	if i := strings.Index(first, "="); i >= 0 {
		return first[i+1:]
	}
	return first
}

// drift compares an account with what an undo expects; "" = no drift.
func (f *Fake) drift(u *FakeUser, e *model.Expect) string {
	if e == nil {
		return ""
	}
	var d []string
	if e.Enabled != nil && u.Enabled != *e.Enabled {
		if u.Enabled {
			d = append(d, "account is enabled, expected disabled")
		} else {
			d = append(d, "account is disabled, expected enabled")
		}
	}
	if e.DN != "" && !strings.EqualFold(u.DN, e.DN) {
		d = append(d, fmt.Sprintf("account is at %s, expected %s", u.DN, e.DN))
	}
	for _, k := range sortedKeysOf(e.Attributes) {
		want := e.Attributes[k]
		cur, _ := attrFold(u, k)
		switch {
		case want == nil && cur != "":
			d = append(d, fmt.Sprintf("%s is '%s', expected empty", k, cur))
		case want != nil && cur != *want:
			if cur == "" {
				d = append(d, fmt.Sprintf("%s is empty, expected '%s'", k, *want))
			} else {
				d = append(d, fmt.Sprintf("%s is '%s', expected '%s'", k, cur, *want))
			}
		}
	}
	for _, g := range e.MemberOf {
		dn, ok := f.resolveGroup(g)
		if !ok {
			dn = g
		}
		if !isMember(u, dn) {
			d = append(d, "no longer a member of "+rdnValue(dn))
		}
	}
	for _, g := range e.NotMemberOf {
		dn, ok := f.resolveGroup(g)
		if !ok {
			dn = g
		}
		if isMember(u, dn) {
			d = append(d, "a member of "+rdnValue(dn)+" again")
		}
	}
	if e.IDValue != nil && !strings.EqualFold(u.External, *e.IDValue) {
		d = append(d, fmt.Sprintf("id attribute is '%s', expected '%s'", u.External, *e.IDValue))
	}
	if n := e.Names; n != nil {
		mail, _ := attrFold(u, "mail")
		for _, c := range []struct{ field, have, want string }{
			{"sAMAccountName", u.Sam, n.Sam}, {"userPrincipalName", u.UPN, n.UPN}, {"name", rdnValue(u.DN), n.CN}, {"mail", mail, n.Mail},
		} {
			if c.want != "" && !strings.EqualFold(c.have, c.want) {
				d = append(d, fmt.Sprintf("%s is '%s', expected '%s'", c.field, c.have, c.want))
			}
		}
	}
	if len(d) == 0 {
		return ""
	}
	return "drift: " + strings.Join(d, "; ")
}

// runOps checks expect, then applies ops to u (to a copy in dry run) and reports the account before the change.
func (f *Fake) runOps(u *FakeUser, dryRun bool, changeID string, expect *model.Expect, force bool, ops fakeOps) Result {
	if !force {
		if msg := f.drift(u, expect); msg != "" {
			return Result{ChangeID: changeID, Status: StatusFailed, Error: msg, ObjectGUID: u.GUID, DN: u.DN, Sam: u.Sam, UPN: u.UPN}
		}
	}
	target := u
	if dryRun {
		c := cloneUser(u)
		target = &c
	}
	before := &model.BeforeState{Enabled: boolPtr(u.Enabled), DN: u.DN}
	changed := false
	if ops.enabled != nil && target.Enabled != *ops.enabled {
		target.Enabled, changed = *ops.enabled, true
	}
	note := func(k string) {
		if before.Attributes == nil {
			before.Attributes = map[string]*string{}
		}
		if v, ok := attrFold(u, k); ok && v != "" {
			before.Attributes[k] = strPtr(v)
		} else {
			before.Attributes[k] = nil
		}
	}
	for _, k := range sortedKeysOf(ops.set) {
		if cur, _ := attrFold(target, k); cur != ops.set[k] {
			note(k)
			setAttrFold(target, k, ops.set[k])
			changed = true
		}
	}
	for _, k := range ops.clear {
		if cur, _ := attrFold(target, k); cur != "" {
			note(k)
			clearAttrFold(target, k)
			changed = true
		}
	}
	var missing []string
	for _, g := range ops.remove {
		if dn, ok := f.resolveGroup(g); ok && removeMember(target, dn) {
			before.GroupsRemoved = append(before.GroupsRemoved, dn)
			changed = true
		}
	}
	for _, g := range ops.add {
		dn, ok := f.resolveGroup(g)
		if !ok {
			missing = append(missing, g)
			continue
		}
		if !isMember(target, dn) {
			addMember(target, dn)
			before.GroupsAdded = append(before.GroupsAdded, dn)
			changed = true
		}
	}
	if ops.moveTo != "" && !strings.EqualFold(parentOf(target.DN), ops.moveTo) {
		target.DN = strings.SplitN(target.DN, ",", 2)[0] + "," + ops.moveTo
		changed = true
	}
	r := Result{ChangeID: changeID, Status: StatusApplied, ObjectGUID: u.GUID, DN: target.DN, Sam: u.Sam, UPN: u.UPN, Before: before}
	switch {
	case ops.reportMissing && len(missing) > 0:
		r.Status, r.Error = StatusFailed, "groups not found: "+strings.Join(missing, ", ")
	case dryRun:
		r.Status = StatusWould
	case !changed:
		r.Status, r.Before = StatusSkipped, nil
	}
	return r
}

func sortedKeysOf[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
