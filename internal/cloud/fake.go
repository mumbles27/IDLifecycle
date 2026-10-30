package cloud

import (
	"context"
	"sort"
	"strings"
	"sync"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/model"
)

// Fake is an in-memory tenant: groups with direct user members, and synced users. It behaves like the scripts:
// non-manageable groups are refused per group, adds and removes are idempotent, removeAll keeps Keep and every
// non-manageable group.
type Fake struct {
	mu     sync.Mutex
	Groups map[string]*FakeGroup // by lower-case id
	Users  []User
	// Exchange mirrors cloud.exchange.enabled: mail-enabled groups are not manageable without it.
	Exchange bool

	Calls []string
	// Err fails every call; SnapshotErr only snapshots; Checks is what Test returns.
	Err         error
	SnapshotErr error
	Checks      []Check
	// LastConn is the connection of the last call (tests check that the secret was decrypted, never stored).
	LastConn Connection
	SetCalls [][]SetItem
	DryRuns  []bool
}

type FakeGroup struct {
	Group
	Dynamic, RoleAssignable, Synced bool
	Members                         map[string]bool
}

func NewFake() *Fake {
	return &Fake{Groups: map[string]*FakeGroup{}, Exchange: true}
}

// AddGroup seeds a group of a kind (security, m365, mail_security, distribution).
func (f *Fake) AddGroup(id, name, kind string, members ...string) *FakeGroup {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := &FakeGroup{Group: Group{ID: strings.ToLower(id), Name: name, Kind: kind}, Members: map[string]bool{}}
	for _, m := range members {
		g.Members[strings.ToLower(m)] = true
	}
	f.Groups[strings.ToLower(id)] = g
	return g
}

// AddUser seeds a synced user.
func (f *Fake) AddUser(u User) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u.ID = strings.ToLower(u.ID)
	f.Users = append(f.Users, u)
}

// IsMember reports a membership (for assertions).
func (f *Fake) IsMember(groupID, userID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	g := f.Groups[strings.ToLower(groupID)]
	return g != nil && g.Members[strings.ToLower(userID)]
}

func (f *Fake) view(g *FakeGroup) Group {
	out := g.Group
	out.Manageable, out.Reason = true, ""
	switch {
	case g.Synced:
		out.Manageable, out.Reason = false, ReasonSynced
	case g.RoleAssignable:
		out.Manageable, out.Reason = false, ReasonRoleAssignable
	case g.Dynamic:
		out.Manageable, out.Reason = false, ReasonDynamic
	case (g.Kind == KindMailSecurity || g.Kind == KindDistribution) && !f.Exchange:
		out.Manageable, out.Reason = false, ReasonExchangeOff
	}
	return out
}

func (f *Fake) call(name string, conn Connection) error {
	f.Calls = append(f.Calls, name)
	f.LastConn = conn
	return f.Err
}

func (f *Fake) Test(_ context.Context, conn Connection) ([]Check, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("test", conn); err != nil {
		return nil, err
	}
	if f.Checks != nil {
		return f.Checks, nil
	}
	return []Check{{Name: "token", OK: true, Detail: "signed in"}}, nil
}

func (f *Fake) SearchGroups(_ context.Context, conn Connection, query string, max int) ([]Group, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("search", conn); err != nil {
		return nil, false, err
	}
	q := strings.ToLower(strings.ReplaceAll(query, "*", ""))
	out := []Group{}
	for _, g := range f.Groups {
		if strings.Contains(strings.ToLower(g.Name), q) || strings.Contains(strings.ToLower(g.Mail), q) {
			out = append(out, f.view(g))
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	if max <= 0 {
		max = 25
	}
	if len(out) > max {
		return out[:max], true, nil
	}
	return out, false, nil
}

func (f *Fake) ResolveGroups(_ context.Context, conn Connection, ids []string) ([]Group, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("resolve", conn); err != nil {
		return nil, err
	}
	out := []Group{}
	for _, id := range ids {
		if g := f.Groups[strings.ToLower(id)]; g != nil {
			out = append(out, f.view(g))
		}
	}
	return out, nil
}

func (f *Fake) Snapshot(_ context.Context, conn Connection, req SnapshotRequest) (*Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("snapshot", conn); err != nil {
		return nil, err
	}
	if f.SnapshotErr != nil {
		return nil, f.SnapshotErr
	}
	s := &Snapshot{Users: append([]User{}, f.Users...), Groups: []SnapshotGroup{}}
	for _, id := range req.Groups {
		g := f.Groups[strings.ToLower(id)]
		if g == nil {
			s.Groups = append(s.Groups, SnapshotGroup{Group: Group{ID: strings.ToLower(id)}, Members: []string{}})
			continue
		}
		sg := SnapshotGroup{Group: f.view(g), Found: true, Members: []string{}}
		for m := range g.Members {
			sg.Members = append(sg.Members, m)
		}
		sort.Strings(sg.Members)
		s.Groups = append(s.Groups, sg)
	}
	return s, nil
}

func (f *Fake) SetGroups(_ context.Context, conn Connection, dryRun bool, items []SetItem) ([]ad.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("set", conn); err != nil {
		return nil, err
	}
	f.SetCalls = append(f.SetCalls, items)
	f.DryRuns = append(f.DryRuns, dryRun)
	out := make([]ad.Result, 0, len(items))
	for _, it := range items {
		user := strings.ToLower(it.UserID)
		var done, refused []string
		before := &model.BeforeState{}
		change := func(id string, add bool) {
			g := f.Groups[strings.ToLower(id)]
			switch {
			case g == nil:
				refused = append(refused, id+" (not found)")
			case !f.view(g).Manageable:
				refused = append(refused, g.Name+" ("+f.view(g).Reason+")")
			case g.Members[user] == add:
			default:
				if !dryRun {
					if add {
						g.Members[user] = true
					} else {
						delete(g.Members, user)
					}
				}
				done = append(done, g.Name)
				if add {
					before.CloudAdded = append(before.CloudAdded, strings.ToLower(id))
				} else {
					before.CloudRemoved = append(before.CloudRemoved, strings.ToLower(id))
				}
			}
		}
		for _, id := range it.Add {
			change(id, true)
		}
		for _, id := range it.Remove {
			change(id, false)
		}
		if it.RemoveAll {
			keep := map[string]bool{}
			for _, k := range it.Keep {
				keep[strings.ToLower(k)] = true
			}
			ids := make([]string, 0, len(f.Groups))
			for id := range f.Groups {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			for _, id := range ids {
				g := f.Groups[id]
				if g.Members[user] && !keep[id] && f.view(g).Manageable {
					change(id, false)
				}
			}
		}
		r := ad.Result{ChangeID: it.ChangeID, Status: ad.StatusApplied, Message: strings.Join(done, ", "), Before: before}
		switch {
		case len(refused) > 0:
			r.Status, r.Error = ad.StatusFailed, "refused: "+strings.Join(refused, ", ")
		case dryRun:
			r.Status = ad.StatusWould
		case len(done) == 0:
			r.Status, r.Before = ad.StatusSkipped, nil
		}
		out = append(out, r)
	}
	return out, nil
}

var _ Client = (*Fake)(nil)
