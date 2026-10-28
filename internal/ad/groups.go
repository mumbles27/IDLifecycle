package ad

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// GroupHit is one group found by a group search (Search-IDLGroups.ps1).
type GroupHit struct {
	DN          string `json:"dn"`
	Sam         string `json:"sam"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Category    string `json:"category"` // security | distribution
	Scope       string `json:"scope"`    // global | universal | domainlocal
	Privileged  bool   `json:"privileged"`
}

// GroupSearchRequest asks for groups whose cn, sAMAccountName, displayName or description contains Query ('*' is a
// wildcard), in the whole domain. Max caps the result.
type GroupSearchRequest struct {
	Server string `json:"server"`
	Query  string `json:"query"`
	Max    int    `json:"max"`
}

// GroupSearcher is implemented by clients that can search groups (for the group picker). Optional, like OUReader.
type GroupSearcher interface {
	SearchGroups(ctx context.Context, req GroupSearchRequest) (groups []GroupHit, truncated bool, err error)
}

const scriptGroups = "Search-IDLGroups.ps1"

// SearchGroups runs Search-IDLGroups.ps1 (read-only).
func (c *PowerShellClient) SearchGroups(ctx context.Context, req GroupSearchRequest) ([]GroupHit, bool, error) {
	if _, err := os.Stat(filepath.Join(c.ScriptsDir, scriptGroups)); err != nil {
		return nil, false, fmt.Errorf("%s is not installed in %s", scriptGroups, c.ScriptsDir)
	}
	var resp struct {
		OK        bool       `json:"ok"`
		Error     string     `json:"error"`
		Groups    []GroupHit `json:"groups"`
		Truncated bool       `json:"truncated"`
	}
	if err := c.run(ctx, scriptGroups, false, req, &resp, nil); err != nil {
		return nil, false, err
	}
	if !resp.OK {
		return nil, false, fmt.Errorf("%s: %s", scriptGroups, orUnknown(resp.Error))
	}
	if resp.Groups == nil {
		resp.Groups = []GroupHit{}
	}
	return resp.Groups, resp.Truncated, nil
}

// SetGroupInfo adds search details to a group seeded with AddGroup (the fake reports name = the DN's CN otherwise).
func (f *Fake) SetGroupInfo(sam string, hit GroupHit) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.GroupInfo == nil {
		f.GroupInfo = map[string]GroupHit{}
	}
	f.GroupInfo[strings.ToLower(sam)] = hit
}

// SearchGroups searches the fake's groups like the script: case-insensitive "contains" on name, sAMAccountName and
// description, '*' as a wildcard.
func (f *Fake) SearchGroups(_ context.Context, req GroupSearchRequest) ([]GroupHit, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "searchgroups")
	if f.ActionErr != nil {
		return nil, false, f.ActionErr
	}
	parts := strings.Split(strings.ToLower(req.Query), "*")
	for i := range parts {
		parts[i] = regexp.QuoteMeta(parts[i])
	}
	re := regexp.MustCompile(strings.Join(parts, ".*"))
	seen := map[string]bool{}
	out := []GroupHit{}
	for key, dn := range f.Groups {
		if strings.Contains(key, "=") || seen[strings.ToLower(dn)] {
			continue // the DN alias of a group added with AddGroup
		}
		seen[strings.ToLower(dn)] = true
		hit, ok := f.GroupInfo[key]
		if !ok {
			hit = GroupHit{Sam: key, Name: strings.TrimPrefix(strings.SplitN(dn, ",", 2)[0], "CN="), Category: "security", Scope: "global"}
		}
		hit.DN = dn
		if hit.Sam == "" {
			hit.Sam = key
		}
		if re.MatchString(strings.ToLower(hit.Name)) || re.MatchString(strings.ToLower(hit.Sam)) || re.MatchString(strings.ToLower(hit.Description)) {
			out = append(out, hit)
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	max := req.Max
	if max <= 0 {
		max = 25
	}
	if len(out) > max {
		return out[:max], true, nil
	}
	return out, false, nil
}

var (
	_ GroupSearcher = (*Fake)(nil)
	_ GroupSearcher = (*PowerShellClient)(nil)
)
