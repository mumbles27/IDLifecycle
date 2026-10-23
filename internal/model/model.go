// Package model holds the plain data types shared by store, engine and api.
// JSON field names are snake_case and form part of the HTTP API contract.
package model

import "time"

// Roles for GUI users.
const (
	RoleViewer   = "viewer"
	RoleApprover = "approver"
	RoleAdmin    = "admin"
)

// Attribute sync modes.
const (
	ModeCreateOnly = "create_only"
	ModeSync       = "sync"
	ModeImmutable  = "immutable"
)

// Change types.
const (
	ChangeCreate  = "create"
	ChangeUpdate  = "update"
	ChangeDisable = "disable"
	ChangeEnable  = "enable"
	ChangeDelete  = "delete"
)

// Change statuses.
const (
	StatusPending    = "pending"
	StatusApproved   = "approved"
	StatusApplying   = "applying"
	StatusApplied    = "applied"
	StatusFailed     = "failed"
	StatusRejected   = "rejected"
	StatusSuperseded = "superseded"
	StatusSimulated  = "simulated"
)

// Identity lifecycle states.
const (
	StateNew      = "new"
	StateLinked   = "linked"
	StateDisabled = "disabled"
	StateDeleted  = "deleted"
	StateError    = "error"
)

// Run statuses.
const (
	RunRunning = "running"
	RunOK      = "ok"
	RunError   = "error"
	RunAborted = "aborted"
)

// MaskedSecret is returned to API clients in place of stored secrets.
const MaskedSecret = "********"

type User struct {
	ID           string     `json:"id"`
	Username     string     `json:"username"`
	Role         string     `json:"role"`
	Disabled     bool       `json:"disabled"`
	CreatedAt    time.Time  `json:"created_at"`
	LastLoginAt  *time.Time `json:"last_login_at"`
	PasswordHash string     `json:"-"`
	FailedLogins int        `json:"-"`
	LockedUntil  *time.Time `json:"-"`
}

type Session struct {
	TokenHash  string
	UserID     string
	CSRFToken  string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
}

type SourceOptions struct {
	ExternalIDField        string   `json:"external_id_field"`
	StatusField            string   `json:"status_field"`
	TerminatedValues       []string `json:"terminated_values"`
	MissingMeansTerminated bool     `json:"missing_means_terminated"`
	MissingGraceRuns       int      `json:"missing_grace_runs"`
}

type Source struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Type       string         `json:"type"`
	Enabled    bool           `json:"enabled"`
	Options    SourceOptions  `json:"options"`
	Config     map[string]any `json:"config"`
	LastSyncAt *time.Time     `json:"last_sync_at"`
	LastStatus string         `json:"last_status"`
	LastError  string         `json:"last_error"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
}

// Mapping maps a source-derived template onto an AD attribute.
type Mapping struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	TargetAttr  string    `json:"target_attr"`
	Template    string    `json:"template"`
	Mode        string    `json:"mode"`
	OnEmpty     string    `json:"on_empty"`   // skip | clear
	ValueType   string    `json:"value_type"` // string | user_ref
	Enabled     bool      `json:"enabled"`
	SortOrder   int       `json:"sort_order"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// AttrGrant is an attribute mapping carried by a role rule.
type AttrGrant struct {
	TargetAttr string `json:"target_attr"`
	Template   string `json:"template"`
	Mode       string `json:"mode"`
	OnEmpty    string `json:"on_empty"`
	ValueType  string `json:"value_type"`
}

type Condition struct {
	Attr  string `json:"attr"`
	Op    string `json:"op"`
	Value string `json:"value"`
}

type Grants struct {
	Groups     []string    `json:"groups"`
	OU         string      `json:"ou"`
	Attributes []AttrGrant `json:"attributes"`
}

type RoleRule struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Enabled     bool        `json:"enabled"`
	Priority    int         `json:"priority"`
	Match       string      `json:"match"` // all | any
	Conditions  []Condition `json:"conditions"`
	Grants      Grants      `json:"grants"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

// Identity is a person known from a source, plus the link to their AD account.
type Identity struct {
	ID          string            `json:"id"`
	SourceID    string            `json:"source_id"`
	SourceName  string            `json:"source_name,omitempty"`
	ExternalID  string            `json:"external_id"`
	Display     string            `json:"display"`
	Attrs       map[string]string `json:"attrs"`
	AttrsHash   string            `json:"-"`
	Active      bool              `json:"active"`
	MissingRuns int               `json:"-"`
	FirstSeen   time.Time         `json:"first_seen"`
	LastSeen    time.Time         `json:"last_seen"`
	ADGUID      string            `json:"ad_guid"`
	ADDN        string            `json:"ad_dn"`
	ADSam       string            `json:"ad_sam"`
	Applied     map[string]string `json:"-"` // last values this service pushed per attribute
	DisabledAt  *time.Time        `json:"disabled_at"`
	State       string            `json:"state"`
}

type Candidate struct {
	Sam string `json:"sam"`
	UPN string `json:"upn"`
	CN  string `json:"cn"`
}

type AttrDiff struct {
	From string `json:"from"`
	To   string `json:"to"`
	Mode string `json:"mode"`
}

// ChangePayload carries everything the executor needs; it never contains passwords.
type ChangePayload struct {
	ObjectGUID   string              `json:"object_guid,omitempty"`
	DN           string              `json:"dn,omitempty"`
	OU           string              `json:"ou,omitempty"`
	Candidates   []Candidate         `json:"candidates,omitempty"`
	Attributes   map[string]string   `json:"attributes,omitempty"` // create
	Groups       []string            `json:"groups,omitempty"`     // create
	Set          map[string]AttrDiff `json:"set,omitempty"`        // update
	Clear        []string            `json:"clear,omitempty"`
	AddGroups    []string            `json:"add_groups,omitempty"`
	RemoveGroups []string            `json:"remove_groups,omitempty"`
	MoveTo       string              `json:"move_to,omitempty"`
	Description  string              `json:"description,omitempty"`
}

type Change struct {
	ID           string        `json:"id"`
	RunID        string        `json:"run_id"`
	IdentityID   string        `json:"identity_id"`
	ExternalID   string        `json:"external_id"`
	Display      string        `json:"display"`
	Type         string        `json:"type"`
	Status       string        `json:"status"`
	Payload      ChangePayload `json:"payload"`
	Summary      []string      `json:"summary"`
	PayloadHash  string        `json:"-"`
	CreatedAt    time.Time     `json:"created_at"`
	DecidedBy    string        `json:"decided_by"`
	DecidedAt    *time.Time    `json:"decided_at"`
	DecisionNote string        `json:"decision_note"`
	AppliedAt    *time.Time    `json:"applied_at"`
	Error        string        `json:"error"`
	Attempts     int           `json:"attempts"`
}

type Run struct {
	ID          string         `json:"id"`
	Trigger     string         `json:"trigger"`
	TriggeredBy string         `json:"triggered_by"`
	StartedAt   time.Time      `json:"started_at"`
	FinishedAt  *time.Time     `json:"finished_at"`
	Status      string         `json:"status"`
	Counters    map[string]int `json:"counters"`
	Error       string         `json:"error"`
}

type AuditEntry struct {
	Seq        int64          `json:"seq"`
	TS         time.Time      `json:"ts"`
	Actor      string         `json:"actor"`
	Action     string         `json:"action"`
	EntityType string         `json:"entity_type"`
	EntityID   string         `json:"entity_id"`
	Detail     map[string]any `json:"detail"`
	PrevHash   string         `json:"-"`
	Hash       string         `json:"hash"`
}
