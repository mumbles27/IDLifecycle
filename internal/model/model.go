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
	// ChangeLink hands an existing AD account to an identity (correlation): sets the id attribute, optionally renames.
	ChangeLink = "link"
	// ChangeCloudGroups changes a person's Entra ID / Microsoft 365 / Exchange Online group memberships (separate from
	// the AD change of the same person, so the two are approved and fail independently).
	ChangeCloudGroups = "cloud_groups"
	// ChangeUndo rolls back one applied change (always approved by a person; see UndoPayload).
	ChangeUndo = "undo"
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
	// StateMerged: the identity's account was handed to another identity (MergedInto); the planner skips it.
	StateMerged = "merged"
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
	AuthSource   string     `json:"auth_source"` // local | oidc
	ExternalID   string     `json:"external_id"` // oidc: "<tenant id>:<object id>"
	PasswordHash string     `json:"-"`
	FailedLogins int        `json:"-"`
	LockedUntil  *time.Time `json:"-"`
}

// User sign-in sources.
const (
	AuthLocal = "local"
	AuthOIDC  = "oidc"
)

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
	// Termination date: a person counts as terminated once the date in TerminationDateField (plus TerminationAfterDays)
	// has passed, in schedule.timezone. TerminationDateFormat is "" or "auto" (common formats) or a Go layout.
	TerminationDateField  string `json:"termination_date_field"`
	TerminationDateFormat string `json:"termination_date_format"`
	TerminationAfterDays  int    `json:"termination_after_days"`
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
	// Conditions are the children of a group (op all_of / any_of).
	Conditions []Condition `json:"conditions,omitempty"`
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
	// OUOverride is an OU chosen by a person for this identity (Change OU on an approval). It applies while the
	// placement field (e.g. Department) still has the value OUOverrideValue; a department change ends it.
	OUOverride      string `json:"ou_override,omitempty"`
	OUOverrideValue string `json:"ou_override_value,omitempty"`
	// NameOverride are account names a person chose for this identity (Set name on a create); used in every
	// naming.conflict_mode instead of the templates.
	NameOverride *Candidate `json:"name_override,omitempty"`
	// Correlation: MergedInto is the identity that took over this identity's account; LinkRejects are objectGUIDs a
	// person said are not this identity; LinkDecision is "" | "new" (always create) | "guid:<objectGUID>" (link to it).
	MergedInto   string   `json:"merged_into,omitempty"`
	LinkRejects  []string `json:"link_rejects,omitempty"`
	LinkDecision string   `json:"link_decision,omitempty"`
	// RetentionPolicyID is a retention policy a person assigned to this identity ("" = automatic, "default" = the
	// default policy); RetentionAssignedBy is who did. Set only by SetIdentityRetention, never by a sync.
	RetentionPolicyID   string `json:"retention_policy_id,omitempty"`
	RetentionAssignedBy string `json:"retention_assigned_by,omitempty"`
}

// Identity.LinkDecision values.
const (
	LinkDecisionNew  = "new"
	LinkDecisionGUID = "guid:"
)

// ManualPerson is a person typed into the console (source type "manual"), e.g. a contractor.
type ManualPerson struct {
	ID         string            `json:"id"`
	SourceID   string            `json:"source_id"`
	ExternalID string            `json:"external_id"`
	Attrs      map[string]string `json:"attrs"`
	Ended      bool              `json:"ended"`
	CreatedBy  string            `json:"created_by"`
	CreatedAt  time.Time         `json:"created_at"`
	UpdatedBy  string            `json:"updated_by"`
	UpdatedAt  time.Time         `json:"updated_at"`
	// Filled in by the API from the identity (read-only).
	State string `json:"state,omitempty"`
	ADSam string `json:"ad_sam,omitempty"`
}

// NameConflict is a generated account name that is already in use.
type NameConflict struct {
	Field string `json:"field"`          // sam | upn | mail | cn
	Value string `json:"value"`          //
	DN    string `json:"dn,omitempty"`   // the AD object that has it
	With  string `json:"with,omitempty"` // "Display (external id)" of another person planned in the same run
}

// FieldMatch is one correlation rule evaluated against one AD account.
type FieldMatch struct {
	Field   string `json:"field"`
	ADAttr  string `json:"ad_attr"`
	Value   string `json:"value"`
	ADValue string `json:"ad_value"`
	Matched bool   `json:"matched"`
}

// LinkCandidate is an existing AD account that may belong to an identity.
type LinkCandidate struct {
	ObjectGUID      string       `json:"object_guid"`
	DN              string       `json:"dn"`
	Sam             string       `json:"sam"`
	UPN             string       `json:"upn"`
	Enabled         bool         `json:"enabled"`
	ExternalID      string       `json:"external_id"`           // current id attribute value ("" = none)
	IdentityID      string       `json:"identity_id,omitempty"` // identity the account belongs to now (a transfer)
	IdentityDisplay string       `json:"identity_display,omitempty"`
	SourceName      string       `json:"source_name,omitempty"`
	Fields          []FieldMatch `json:"fields"`
	Full            bool         `json:"full"`
	Manual          bool         `json:"manual,omitempty"` // chosen by a person (link_decision)
}

// LinkPayload is the correlation part of a link change.
type LinkPayload struct {
	Candidates []LinkCandidate `json:"candidates"`
	Chosen     string          `json:"chosen"` // object_guid or ""
	Verify     bool            `json:"verify"` // a person must confirm (partial or several candidates)
	Rename     *Candidate      `json:"rename,omitempty"`
	RenameNote string          `json:"rename_note,omitempty"`
}

// ChosenCandidate returns the chosen candidate, if any.
func (l *LinkPayload) ChosenCandidate() (LinkCandidate, bool) {
	if l == nil || l.Chosen == "" {
		return LinkCandidate{}, false
	}
	for _, c := range l.Candidates {
		if c.ObjectGUID == l.Chosen {
			return c, true
		}
	}
	return LinkCandidate{}, false
}

type Candidate struct {
	Sam  string `json:"sam"`
	UPN  string `json:"upn"`
	CN   string `json:"cn"`
	Mail string `json:"mail,omitempty"` // only when naming.mail_template is set and no mapping writes mail
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
	// OU placement: NeedsOU marks a create/move that waits for a person to choose the OU (no mapping for the
	// department); Placement is the placement field's value the plan was made for (recorded when applied).
	NeedsOU   bool   `json:"needs_ou,omitempty"`
	OUReason  string `json:"ou_reason,omitempty"`
	Placement string `json:"placement,omitempty"`
	// Naming conflicts: NeedsName marks a create whose names are all taken; a person sets one (Set name).
	NeedsName     bool           `json:"needs_name,omitempty"`
	NameConflicts []NameConflict `json:"name_conflicts,omitempty"`
	// Link (change type link): ObjectGUID/DN are the chosen account.
	Link *LinkPayload `json:"link,omitempty"`
	// Cloud groups (change type cloud_groups): the Entra user and group object ids. CloudRemoveAll removes every
	// direct cloud membership the user has except CloudKeep (a disable with lifecycle_groups.disabled_remove all_except).
	CloudUserID    string   `json:"cloud_user_id,omitempty"`
	CloudAdd       []string `json:"cloud_add,omitempty"`
	CloudRemove    []string `json:"cloud_remove,omitempty"`
	CloudRemoveAll bool     `json:"cloud_remove_all,omitempty"`
	CloudKeep      []string `json:"cloud_keep,omitempty"`
	// Undo (change type undo): what is rolled back and how; the inverse operations use the fields above.
	Undo *UndoPayload `json:"undo,omitempty"`
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
	// Undo bookkeeping (filled by the store when a change is read, never part of the payload hash):
	// AppliedState is what the account looked like before this change, UndoneBy the undo change that rolled it back,
	// UndoPending an undo waiting for approval or running, UndoOf the change an undo change rolls back.
	AppliedState *AppliedState `json:"applied_state,omitempty"`
	UndoneBy     string        `json:"undone_by,omitempty"`
	UndoPending  string        `json:"undo_pending,omitempty"`
	UndoOf       string        `json:"undo_of,omitempty"`
	// Per-item undo of an update / cloud_groups change: the parts already undone and the parts an open undo covers
	// (item key -> undo change id). UndoneBy is set once every part is undone; UndoPending only for a whole-change undo.
	UndoneItems      map[string]string `json:"undone_items,omitempty"`
	UndoPendingItems map[string]string `json:"undo_pending_items,omitempty"`
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
