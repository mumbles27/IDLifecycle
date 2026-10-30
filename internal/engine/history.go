package engine

import (
	"context"

	"idlifecycle/internal/model"
)

// Identity history: audit rows on entity "identity" that, with the rows of the identity's changes, tell a person's
// story (GET /api/identities/{id}/history). They are written only when something actually changed, never on every sync.
const (
	ActFirstSeen        = "identity.first_seen"        // the identity was created from a source record
	ActSourceChanged    = "identity.source_changed"    // the source record's fields changed
	ActInactive         = "identity.inactive"          // terminated in the source, or missing from it
	ActReactivated      = "identity.reactivated"       // active again after being inactive
	ActState            = "identity.state"             // lifecycle_state changed (linked, disabled, new...)
	ActAccountMissing   = "identity.account_missing"   // the linked AD account is gone (deleted outside this service)
	ActAccountDeleted   = "identity.account_deleted"   // a delete change removed the account; detail is the last record of it
	ActRetentionChanged = "identity.retention_changed" // a sync resolved a different retention policy for the person
	ActRetentionSet     = "identity.retention_set"     // a person assigned a retention policy (or set it back to automatic)
)

// Origins of an identity (identity.first_seen detail "origin").
const (
	OriginSource = "source" // an HR feed (CSV, SQL, API)
	OriginManual = "manual" // typed into the console (Add person)
)

// identityEvent is an identity history row collected while planning (written by the sync once the rows are saved).
type identityEvent struct {
	identityID string
	action     string
	detail     map[string]any
}

func (e *Engine) identityAudit(ctx context.Context, actor, action string, id model.Identity, detail map[string]any) {
	if detail == nil {
		detail = map[string]any{}
	}
	detail["external_id"] = id.ExternalID
	if _, ok := detail["source_id"]; !ok && id.SourceID != "" {
		detail["source_id"] = id.SourceID
	}
	e.audit(ctx, actor, action, "identity", id.ID, detail)
}

// attrChanges lists the fields that differ between two source records: {field: {from, to}} (nil = absent).
func attrChanges(old, cur map[string]string) map[string]any {
	out := map[string]any{}
	for k, v := range cur {
		if ov, ok := old[k]; !ok || ov != v {
			var from any
			if ok {
				from = ov
			}
			out[k] = map[string]any{"from": from, "to": v}
		}
	}
	for k, ov := range old {
		if _, ok := cur[k]; !ok {
			out[k] = map[string]any{"from": ov, "to": nil}
		}
	}
	return out
}

// stateDetail describes a lifecycle_state change; the account fields are those after the change.
func stateDetail(from, to model.Identity, changeID, changeType string) map[string]any {
	d := map[string]any{"from": from.State, "to": to.State, "ad_guid": to.ADGUID, "ad_dn": to.ADDN, "ad_sam": to.ADSam}
	if to.ADGUID == "" && from.ADGUID != "" {
		d["previous_guid"], d["previous_dn"], d["previous_sam"] = from.ADGUID, from.ADDN, from.ADSam
	}
	if changeID != "" {
		d["change"], d["change_type"] = changeID, changeType
	}
	return d
}

// noteState writes identity.state when the lifecycle state changed between prev and cur.
func (e *Engine) noteState(ctx context.Context, prev, cur model.Identity, changeID, changeType string) {
	if prev.State == cur.State {
		return
	}
	e.identityAudit(ctx, "system", ActState, cur, stateDetail(prev, cur, changeID, changeType))
}
