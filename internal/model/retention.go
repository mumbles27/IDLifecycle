package model

import "time"

// RetentionPolicy decides what happens to a leaver's account: how long it stays disabled before it is deleted, where
// it is moved and which groups it keeps. The first enabled policy (by priority) whose conditions match a person
// applies, unless a person assigned one; otherwise the default policy (engine.delete_after_days, ad.disabled_ou).
// A policy without conditions applies only to people assigned to it.
type RetentionPolicy struct {
	ID          string      `json:"id"`
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Enabled     bool        `json:"enabled"`
	Priority    int         `json:"priority"`
	Match       string      `json:"match_mode"` // all | any
	Conditions  []Condition `json:"conditions"`
	// DeleteAfterDays counts from the day the account was disabled; 0 = never delete automatically.
	DeleteAfterDays int `json:"delete_after_days"`
	// DisabledOU replaces ad.disabled_ou for people under this policy ("" = ad.disabled_ou).
	DisabledOU string `json:"disabled_ou"`
	// ExtraKeepGroups are kept on disable in addition to lifecycle_groups.disabled_keep.
	ExtraKeepGroups []string `json:"extra_keep_groups"`
	// Approval "" = the normal delete approval; "manual" = deletes under this policy always wait for a person.
	Approval  string    `json:"approval"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// People is how many identities resolved to this policy at the last sync (read-only).
	People int `json:"people"`
}

// The default policy's id (also the value that pins a person to it) and approval values.
const (
	RetentionDefaultID      = "default"
	RetentionApprovalManual = "manual"
)

// Why a person is under a policy.
const (
	RetentionReasonAssigned = "assigned"
	RetentionReasonRule     = "rule"
	RetentionReasonDefault  = "default"
)

// IdentityRetention is the policy a sync resolved for one identity, and when the account will be deleted.
type IdentityRetention struct {
	IdentityID string     `json:"-"`
	PolicyID   string     `json:"policy_id"`
	PolicyName string     `json:"policy_name"`
	Reason     string     `json:"reason"` // assigned | rule | default
	AssignedBy string     `json:"assigned_by,omitempty"`
	DeleteDue  *time.Time `json:"delete_due,omitempty"`
	// DeleteAfterDays is the policy's retention (0 = never deleted automatically).
	DeleteAfterDays int `json:"delete_after_days"`
	// LeaveDate is the termination date from the source record (YYYY-MM-DD), past or future.
	LeaveDate  string    `json:"leave_date,omitempty"`
	ResolvedAt time.Time `json:"resolved_at"`
}
