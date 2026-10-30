package model

import "time"

// AccountNames are an account's names (a link rename and its undo). ProxyAddresses is the full list.
type AccountNames struct {
	Sam            string   `json:"sam"`
	UPN            string   `json:"upn"`
	CN             string   `json:"cn"`
	Mail           string   `json:"mail"`
	ProxyAddresses []string `json:"proxyAddresses,omitempty"`
}

// BeforeState is what an action script reports about an account before it touched it (the "before" of a result).
// Keys a script does not touch are left out; an attribute value of nil means the attribute was empty.
type BeforeState struct {
	Enabled       *bool              `json:"enabled,omitempty"`
	DN            string             `json:"dn,omitempty"`
	Attributes    map[string]*string `json:"attributes,omitempty"`
	GroupsAdded   []string           `json:"groupsAdded,omitempty"`
	GroupsRemoved []string           `json:"groupsRemoved,omitempty"`
	IDValue       *string            `json:"idValue,omitempty"`
	Names         *AccountNames      `json:"names,omitempty"`
	Created       bool               `json:"created,omitempty"`
	CloudAdded    []string           `json:"cloudAdded,omitempty"`
	CloudRemoved  []string           `json:"cloudRemoved,omitempty"`
}

// Expect is checked by a script before it changes anything (an undo must not overwrite later changes). Every key is
// optional; a mismatch fails the item with "drift: ...".
type Expect struct {
	Enabled     *bool              `json:"enabled,omitempty"`
	DN          string             `json:"dn,omitempty"`
	Attributes  map[string]*string `json:"attributes,omitempty"`
	MemberOf    []string           `json:"memberOf,omitempty"`
	NotMemberOf []string           `json:"notMemberOf,omitempty"`
	IDValue     *string            `json:"idValue,omitempty"`
	Names       *AccountNames      `json:"names,omitempty"`
}

// AppliedState is recorded when a change is applied: the account before the change, the account after it (object
// GUID, DN, names) and the attribute values the change wrote (nil = cleared), so the change can be rolled back.
type AppliedState struct {
	Before     BeforeState        `json:"before"`
	ObjectGUID string             `json:"object_guid,omitempty"`
	DN         string             `json:"dn,omitempty"`
	Sam        string             `json:"sam,omitempty"`
	UPN        string             `json:"upn,omitempty"`
	Wrote      map[string]*string `json:"wrote,omitempty"`
	At         time.Time          `json:"at"`
}

// Undo scripts (UndoPayload.Script): which action script runs the inverse.
const (
	UndoScriptSet     = "set"
	UndoScriptEnable  = "enable"
	UndoScriptDisable = "disable"
	UndoScriptLink    = "link"
	UndoScriptCloud   = "cloud"
)

// UndoPayload describes an undo change. The inverse operations themselves are in the ordinary payload fields
// (Set/Clear/AddGroups/RemoveGroups/MoveTo/CloudAdd/CloudRemove); a link undo uses LinkTo/Rename/ProxyAddresses.
type UndoPayload struct {
	Of     string  `json:"of"`
	OfType string  `json:"of_type"`
	Script string  `json:"script"`
	Expect *Expect `json:"expect,omitempty"`
	// Link: the id attribute goes from LinkFrom (its value now) to LinkTo ("" = cleared), names back to Rename,
	// proxyAddresses to exactly ProxyAddresses.
	LinkFrom       string     `json:"link_from,omitempty"`
	LinkTo         *string    `json:"link_to,omitempty"`
	Rename         *Candidate `json:"rename,omitempty"`
	ProxyAddresses []string   `json:"proxy_addresses,omitempty"`
	Force          bool       `json:"force,omitempty"`
	Reason         string     `json:"reason,omitempty"`
	Holds          []HoldSpec `json:"holds,omitempty"`
	RequestedBy    string     `json:"requested_by,omitempty"`
	ConfirmText    string     `json:"confirm_text,omitempty"`
	// Items are the undone parts of an update or cloud_groups change (UndoItem keys); empty = the whole change.
	Items []string `json:"items,omitempty"`
	// Batch groups the undo changes requested together for one run (approved together with UNDO <n>).
	Batch string `json:"batch,omitempty"`
}

// Hold kinds: what a hold stops the planner from changing for one identity.
const (
	HoldAttribute  = "attribute"   // Key = LDAP attribute name
	HoldGroup      = "group"       // Key = group DN (as the planner writes it)
	HoldCloudGroup = "cloud_group" // Key = Entra group object id
	HoldOU         = "ou"          // no key: no OU move
	HoldState      = "state"       // no key: no create, link, disable, enable or delete
	HoldAll        = "all"         // no key: the whole person is Manual: nothing is planned for them
)

// HoldSpec is a hold asked for with an undo.
type HoldSpec struct {
	Kind  string     `json:"kind"`
	Key   string     `json:"key"`
	Until *time.Time `json:"until,omitempty"`
}

// Hold keeps the planner from redoing something a person rolled back, until it is released or expires.
type Hold struct {
	ID         string     `json:"id"`
	IdentityID string     `json:"identity_id"`
	Kind       string     `json:"kind"`
	Key        string     `json:"key"`
	Until      *time.Time `json:"until"`
	Reason     string     `json:"reason"`
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	ChangeID   string     `json:"change_id"`
	// Suppressed is what the last real sync would have done had the hold not been there (empty = nothing).
	Suppressed *HoldSuppressed `json:"suppressed,omitempty"`
}

// HoldSuppressed says what a hold kept the planner from doing, and when that was last seen.
type HoldSuppressed struct {
	Summary string    `json:"summary"`
	At      time.Time `json:"at"`
}

// Active reports whether the hold still applies at now.
func (h Hold) Active(now time.Time) bool { return h.Until == nil || now.Before(*h.Until) }

// UndoSettings control rolling back applied changes. Undo changes are always approved by a person.
type UndoSettings struct {
	Enabled bool `json:"enabled"`
	// SecondPerson: the person who approves an undo must not be the one who requested it.
	SecondPerson bool `json:"second_person"`
}
