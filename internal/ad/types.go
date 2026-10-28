// Package ad talks to Active Directory exclusively through the PowerShell scripts in /scripts.
// The Client interface lets the engine be tested against an in-memory fake.
package ad

import (
	"context"
	"errors"
	"fmt"

	"idlifecycle/internal/model"
)

// Result statuses returned by the scripts.
const (
	StatusApplied = "applied"
	StatusWould   = "would"
	StatusSkipped = "skipped"
	StatusFailed  = "failed"
)

type SnapshotRequest struct {
	Server      string   `json:"server"`
	SearchBases []string `json:"searchBases"`
	IDAttribute string   `json:"idAttribute"`
	Properties  []string `json:"properties"`
	Groups      []string `json:"groups"`
	// IncludeUnlinked also returns accounts whose id attribute is empty (ExternalID ""), for correlation.
	IncludeUnlinked bool `json:"includeUnlinked,omitempty"`
}

type User struct {
	ObjectGUID string             `json:"objectGuid"`
	DN         string             `json:"dn"`
	Sam        string             `json:"sam"`
	UPN        string             `json:"upn"`
	Enabled    bool               `json:"enabled"`
	ParentOU   string             `json:"parentOu"`
	ExternalID string             `json:"externalId"`
	Attributes map[string]*string `json:"attributes"`
	MemberOf   []string           `json:"memberOf"`
}

// Attr returns an attribute value ("" when unset).
func (u User) Attr(name string) string {
	for k, v := range u.Attributes {
		if equalFold(k, name) && v != nil {
			return *v
		}
	}
	return ""
}

type GroupInfo struct {
	Identity string `json:"identity"`
	DN       string `json:"dn"`
	Found    bool   `json:"found"`
}

type Snapshot struct {
	Users  []User      `json:"users"`
	Groups []GroupInfo `json:"groups"`
}

// ActionOpts are common to every action script.
type ActionOpts struct {
	Server      string
	IDAttribute string
	DryRun      bool
}

type CreateItem struct {
	ChangeID              string            `json:"changeId"`
	ExternalID            string            `json:"externalId"`
	OU                    string            `json:"ou"`
	Candidates            []model.Candidate `json:"candidates"`
	Attributes            map[string]string `json:"attributes"`
	Groups                []string          `json:"groups"`
	Password              string            `json:"password"`
	Enabled               bool              `json:"enabled"`
	ChangePasswordAtLogon bool              `json:"changePasswordAtLogon"`
}

type UpdateItem struct {
	ChangeID     string            `json:"changeId"`
	ExternalID   string            `json:"externalId"`
	ObjectGUID   string            `json:"objectGuid"`
	Set          map[string]string `json:"set"`
	Clear        []string          `json:"clear"`
	AddGroups    []string          `json:"addGroups"`
	RemoveGroups []string          `json:"removeGroups"`
	MoveTo       string            `json:"moveTo"`
}

type DisableItem struct {
	ChangeID     string   `json:"changeId"`
	ExternalID   string   `json:"externalId"`
	ObjectGUID   string   `json:"objectGuid"`
	MoveTo       string   `json:"moveTo"`
	RemoveGroups []string `json:"removeGroups"`
	AddGroups    []string `json:"addGroups"` // lifecycle_groups.disabled_add, applied after the account is disabled
	Description  string   `json:"description"`
}

type EnableItem struct {
	ChangeID     string   `json:"changeId"`
	ExternalID   string   `json:"externalId"`
	ObjectGUID   string   `json:"objectGuid"`
	MoveTo       string   `json:"moveTo"`
	AddGroups    []string `json:"addGroups"`
	RemoveGroups []string `json:"removeGroups"` // lifecycle_groups.disabled_add groups the account still holds
}

type RemoveItem struct {
	ChangeID   string `json:"changeId"`
	ExternalID string `json:"externalId"`
	ObjectGUID string `json:"objectGuid"`
}

type Result struct {
	ChangeID   string `json:"changeId"`
	Status     string `json:"status"`
	Message    string `json:"message"`
	ObjectGUID string `json:"objectGuid"`
	DN         string `json:"dn"`
	Sam        string `json:"sam"`
	UPN        string `json:"upn"`
	Error      string `json:"error"`
}

// OK reports whether the item reached its intended end state (or would, in dry-run).
func (r Result) OK() bool {
	return r.Status == StatusApplied || r.Status == StatusWould || r.Status == StatusSkipped
}

// Client is everything the engine needs from AD.
type Client interface {
	Snapshot(ctx context.Context, req SnapshotRequest) (*Snapshot, error)
	Create(ctx context.Context, o ActionOpts, items []CreateItem) ([]Result, error)
	Update(ctx context.Context, o ActionOpts, items []UpdateItem) ([]Result, error)
	Disable(ctx context.Context, o ActionOpts, items []DisableItem) ([]Result, error)
	Enable(ctx context.Context, o ActionOpts, items []EnableItem) ([]Result, error)
	Remove(ctx context.Context, o ActionOpts, items []RemoveItem) ([]Result, error)
	// CheckNames reports which account names are in use (read-only).
	CheckNames(ctx context.Context, req NameCheckRequest) ([]TakenName, error)
	// Link hands existing accounts to identities (id attribute, optional rename).
	Link(ctx context.Context, o ActionOpts, items []LinkItem) ([]Result, error)
}

// Error kinds, mapped from script exit codes.
var (
	ErrInvalidInput = errors.New("ad: script rejected the request (exit 2)")
	ErrDependency   = errors.New("ad: PowerShell dependency missing, e.g. ActiveDirectory module (exit 3)")
	ErrConnection   = errors.New("ad: cannot reach or authenticate to Active Directory (exit 4)")
	ErrFatal        = errors.New("ad: script failed unexpectedly (exit 5)")
)

// ScriptError carries the exit code and message of a failed script run.
type ScriptError struct {
	Script   string
	ExitCode int
	Message  string
	kind     error
}

func (e *ScriptError) Error() string {
	return fmt.Sprintf("%s exited with code %d: %s", e.Script, e.ExitCode, e.Message)
}

func (e *ScriptError) Unwrap() error { return e.kind }

// reconcile guarantees one result per requested change id; anything the script did not report is a failure.
func reconcile(ids []string, got []Result) []Result {
	byID := make(map[string]Result, len(got))
	for _, r := range got {
		byID[r.ChangeID] = r
	}
	out := make([]Result, 0, len(ids))
	for _, id := range ids {
		if r, ok := byID[id]; ok {
			out = append(out, r)
			continue
		}
		out = append(out, Result{ChangeID: id, Status: StatusFailed, Error: "script returned no result for this change"})
	}
	return out
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}
