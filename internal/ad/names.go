package ad

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"idlifecycle/internal/model"
)

const (
	scriptNames = "Test-IDLNames.ps1"
	scriptLink  = "Set-IDLLink.ps1"
)

// Name fields (NameQuery.Field).
const (
	NameSam  = "sam"
	NameUPN  = "upn"
	NameMail = "mail"
	NameCN   = "cn"
)

// NameQuery is one account name to look up. sAMAccountName, UPN and mail are checked directory-wide against every
// object class (mail also against proxyAddresses); a CN only one level below OU.
type NameQuery struct {
	Field string `json:"field"`
	Value string `json:"value"`
	OU    string `json:"ou,omitempty"`
}

// TakenName is a name that is in use.
type TakenName struct {
	Field      string `json:"field"`
	Value      string `json:"value"`
	DN         string `json:"dn"`
	ObjectGUID string `json:"objectGuid"`
}

// NameCheckRequest asks which names are in use. Objects listed in Exclude (objectGUIDs) never count.
type NameCheckRequest struct {
	Server  string      `json:"server"`
	Names   []NameQuery `json:"names"`
	Exclude []string    `json:"exclude"`
}

// LinkItem hands an existing account to an identity: the id attribute becomes ExternalID (the current value must be
// ExpectedExternalID, "" = empty), and the account is renamed when Rename is set.
type LinkItem struct {
	ChangeID           string           `json:"changeId"`
	ObjectGUID         string           `json:"objectGuid"`
	ExternalID         string           `json:"externalId"`
	ExpectedExternalID string           `json:"expectedExternalId"`
	Rename             *model.Candidate `json:"rename"`
}

func (c *PowerShellClient) CheckNames(ctx context.Context, req NameCheckRequest) ([]TakenName, error) {
	if len(req.Names) == 0 {
		return nil, nil
	}
	if req.Exclude == nil {
		req.Exclude = []string{}
	}
	var resp struct {
		OK    bool        `json:"ok"`
		Error string      `json:"error"`
		Taken []TakenName `json:"taken"`
	}
	if err := c.run(ctx, scriptNames, false, req, &resp, nil); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("%s: %s", scriptNames, orUnknown(resp.Error))
	}
	return resp.Taken, nil
}

func (c *PowerShellClient) Link(ctx context.Context, o ActionOpts, items []LinkItem) ([]Result, error) {
	return runAction(ctx, c, scriptLink, o, items, func(i LinkItem) string { return i.ChangeID }, nil)
}

// ---- fake ---------------------------------------------------------------------------

// AddMailObject seeds a non-user object (a contact, a group) that holds a mail address or proxy address.
func (f *Fake) AddMailObject(dn, mail string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.MailObjects == nil {
		f.MailObjects = map[string]string{}
	}
	f.MailObjects[strings.ToLower(mail)] = dn
}

// mailOwner returns the DN that uses mail (user mail/proxyAddresses or a seeded mail object), skipping exclude.
func (f *Fake) mailOwner(mail string, exclude map[string]bool) (string, string) {
	if mail == "" {
		return "", ""
	}
	for _, u := range f.Users {
		if exclude[strings.ToLower(u.GUID)] {
			continue
		}
		if strings.EqualFold(u.Attrs["mail"], mail) {
			return u.DN, u.GUID
		}
		for _, p := range strings.Split(u.Attrs["proxyAddresses"], ";") {
			if strings.EqualFold(strings.TrimSpace(p), "smtp:"+mail) {
				return u.DN, u.GUID
			}
		}
	}
	if dn, ok := f.MailObjects[strings.ToLower(mail)]; ok {
		return dn, ""
	}
	return "", ""
}

func (f *Fake) CheckNames(_ context.Context, req NameCheckRequest) ([]TakenName, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, fmt.Sprintf("checknames(n=%d)", len(req.Names)))
	if f.NamesErr != nil {
		return nil, f.NamesErr
	}
	ex := map[string]bool{}
	for _, g := range req.Exclude {
		ex[strings.ToLower(g)] = true
	}
	guids := make([]string, 0, len(f.Users))
	for g := range f.Users {
		guids = append(guids, g)
	}
	sort.Strings(guids)
	var out []TakenName
	for _, q := range req.Names {
		switch q.Field {
		case NameMail:
			if dn, guid := f.mailOwner(q.Value, ex); dn != "" {
				out = append(out, TakenName{Field: q.Field, Value: q.Value, DN: dn, ObjectGUID: guid})
			}
			continue
		}
		for _, g := range guids {
			u := f.Users[g]
			if ex[strings.ToLower(u.GUID)] {
				continue
			}
			hit := false
			switch q.Field {
			case NameSam:
				hit = strings.EqualFold(u.Sam, q.Value)
			case NameUPN:
				hit = strings.EqualFold(u.UPN, q.Value)
			case NameCN:
				hit = strings.EqualFold(u.DN, "CN="+q.Value+","+q.OU)
			}
			if hit {
				out = append(out, TakenName{Field: q.Field, Value: q.Value, DN: u.DN, ObjectGUID: u.GUID})
				break
			}
		}
	}
	return out, nil
}

func (f *Fake) Link(_ context.Context, o ActionOpts, items []LinkItem) ([]Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.begin("link", o, len(items)); err != nil {
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
			out = append(out, Result{ChangeID: it.ChangeID, Status: StatusFailed, Error: "account not found"})
			continue
		}
		if !strings.EqualFold(u.External, it.ExpectedExternalID) {
			out = append(out, Result{ChangeID: it.ChangeID, Status: StatusFailed, Error: fmt.Sprintf("account has id attribute %q, expected %q", u.External, it.ExpectedExternalID)})
			continue
		}
		clash := false
		for _, other := range f.Users {
			if other != u && strings.EqualFold(other.External, it.ExternalID) {
				clash = true
			}
		}
		if clash {
			out = append(out, Result{ChangeID: it.ChangeID, Status: StatusFailed, Error: "another account already has id attribute " + it.ExternalID})
			continue
		}
		dn, sam, upn := u.DN, u.Sam, u.UPN
		if r := it.Rename; r != nil {
			ex := map[string]bool{strings.ToLower(u.GUID): true}
			for _, other := range f.Users {
				if other != u && (strings.EqualFold(other.Sam, r.Sam) || strings.EqualFold(other.UPN, r.UPN)) {
					clash = true
				}
			}
			if owner, _ := f.mailOwner(r.Mail, ex); owner != "" {
				clash = true
			}
			if clash {
				out = append(out, Result{ChangeID: it.ChangeID, Status: StatusFailed, Error: "a new name is already in use"})
				continue
			}
			dn, sam, upn = "CN="+r.CN+","+parentOf(u.DN), r.Sam, r.UPN
		}
		if o.DryRun {
			out = append(out, Result{ChangeID: it.ChangeID, Status: StatusWould, ObjectGUID: u.GUID, DN: dn, Sam: sam, UPN: upn})
			continue
		}
		u.External = it.ExternalID
		if r := it.Rename; r != nil {
			u.DN, u.Sam, u.UPN = dn, sam, upn
			if r.Mail != "" {
				u.Attrs["mail"] = r.Mail
			}
		}
		out = append(out, Result{ChangeID: it.ChangeID, Status: StatusApplied, ObjectGUID: u.GUID, DN: u.DN, Sam: u.Sam, UPN: u.UPN})
	}
	return out, nil
}
