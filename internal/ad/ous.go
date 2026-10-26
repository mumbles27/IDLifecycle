package ad

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// OU is one organizational unit, as Get-IDLOUs.ps1 reports it.
type OU struct {
	DN   string `json:"dn"`
	Name string `json:"name"`
}

// OUReader is implemented by clients that can list OUs (for the OU picker). Optional, like SchemaReader.
type OUReader interface {
	OUs(ctx context.Context, server string, bases []string) ([]OU, error)
}

const scriptOUs = "Get-IDLOUs.ps1"

// OUs runs Get-IDLOUs.ps1: the OUs below bases (the whole domain when bases is empty), sorted as a tree.
func (c *PowerShellClient) OUs(ctx context.Context, server string, bases []string) ([]OU, error) {
	if _, err := os.Stat(filepath.Join(c.ScriptsDir, scriptOUs)); err != nil {
		return nil, fmt.Errorf("%s is not installed in %s", scriptOUs, c.ScriptsDir)
	}
	if bases == nil {
		bases = []string{}
	}
	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		OUs   []OU   `json:"ous"`
	}
	if err := c.run(ctx, scriptOUs, false, map[string]any{"server": server, "searchBases": bases}, &resp, nil); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("%s: %s", scriptOUs, orUnknown(resp.Error))
	}
	SortOUs(resp.OUs)
	return resp.OUs, nil
}

// SortOUs orders OUs as a tree: by their path from the domain down (OU=B,OU=A,DC=x sorts as A/B).
func SortOUs(ous []OU) {
	key := func(dn string) string {
		parts := strings.Split(dn, ",")
		for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
			parts[i], parts[j] = parts[j], parts[i]
		}
		return strings.ToLower(strings.Join(parts, "/"))
	}
	sort.Slice(ous, func(i, j int) bool { return key(ous[i].DN) < key(ous[j].DN) })
}

// AddOU seeds OUs the fake reports from OUs.
func (f *Fake) AddOU(dns ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.OUList = append(f.OUList, dns...)
}

// OUs returns the seeded OUs (below bases when given).
func (f *Fake) OUs(_ context.Context, _ string, bases []string) ([]OU, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "ous")
	var out []OU
	for _, dn := range f.OUList {
		if len(bases) > 0 {
			in := false
			for _, b := range bases {
				if strings.EqualFold(dn, b) || strings.HasSuffix(strings.ToLower(dn), ","+strings.ToLower(b)) {
					in = true
				}
			}
			if !in {
				continue
			}
		}
		name := strings.SplitN(dn, ",", 2)[0]
		if i := strings.IndexByte(name, '='); i >= 0 {
			name = name[i+1:]
		}
		out = append(out, OU{DN: dn, Name: name})
	}
	SortOUs(out)
	return out, nil
}

// MoveUser moves an account as an administrator would by hand in AD (tests).
func (f *Fake) MoveUser(external, ou string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.Users {
		if u.External == external {
			u.DN = strings.SplitN(u.DN, ",", 2)[0] + "," + ou
		}
	}
}
