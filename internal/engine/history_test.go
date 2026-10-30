package engine

import (
	"strings"
	"testing"
	"time"

	"idlifecycle/internal/model"
	"idlifecycle/internal/sources"
	"idlifecycle/internal/store"
)

// events returns the identity's history rows (newest first), optionally only one action.
func (e *env) events(identityID, action string) []model.AuditEntry {
	e.t.Helper()
	list, _, err := e.st.ListAudit(e.ctx, store.AuditFilter{EntityType: "identity", EntityID: identityID, Action: action}, store.Page{Limit: 500})
	e.must(err)
	return list
}

func one(t *testing.T, what string, list []model.AuditEntry) model.AuditEntry {
	t.Helper()
	if len(list) != 1 {
		t.Fatalf("%s: want exactly 1 row, got %d: %+v", what, len(list), list)
	}
	return list[0]
}

func TestHistoryRecordsFirstSeenSourceChangesAndStates(t *testing.T) {
	e := newEnv(t)
	e.provision()
	jane := e.identity("E1")

	first := one(t, "first seen", e.events(jane.ID, ActFirstSeen))
	eq(t, "origin", first.Detail["origin"], any(OriginSource))
	eq(t, "source name", first.Detail["source"], any("HR CSV"))
	if f, _ := first.Detail["fields"].(map[string]any); f["Title"] != "Designer" {
		t.Fatalf("first seen keeps the record's fields: %+v", first.Detail)
	}
	linked := one(t, "linked", e.events(jane.ID, ActState))
	eq(t, "from", linked.Detail["from"], any(model.StateNew))
	eq(t, "to", linked.Detail["to"], any(model.StateLinked))
	eq(t, "account", linked.Detail["ad_sam"], any("jdoe"))
	eq(t, "caused by the create", linked.Detail["change_type"], any(model.ChangeCreate))

	// Unchanged syncs write nothing to the history.
	before := len(e.events(jane.ID, ""))
	e.run()
	e.run()
	eq(t, "no rows for unchanged records", len(e.events(jane.ID, "")), before)

	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Lead Designer", "A", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	e.run()
	ch := one(t, "source changed", e.events(jane.ID, ActSourceChanged))
	changes, _ := ch.Detail["changes"].(map[string]any)
	title, _ := changes["Title"].(map[string]any)
	if len(changes) != 1 || title["from"] != "Designer" || title["to"] != "Lead Designer" {
		t.Fatalf("only the changed field with from/to: %+v", ch.Detail)
	}
	eq(t, "bob untouched", len(e.events(e.identity("E2").ID, ActSourceChanged)), 0)

	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Lead Designer", "T", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	e.run()
	in := one(t, "inactive", e.events(jane.ID, ActInactive))
	eq(t, "reason", in.Detail["reason"], any("terminated"))
	eq(t, "status value", in.Detail["status"], any("T"))
	e.approveAll(model.ChangeDisable)
	states := e.events(jane.ID, ActState)
	eq(t, "disabled", states[0].Detail["to"], any(model.StateDisabled))
	eq(t, "by the disable", states[0].Detail["change_type"], any(model.ChangeDisable))

	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Lead Designer", "A", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	e.run()
	one(t, "reactivated", e.events(jane.ID, ActReactivated))
	e.approveAll(model.ChangeEnable)
	eq(t, "enabled again", e.events(jane.ID, ActState)[0].Detail["to"], any(model.StateLinked))
}

func TestHistoryMissingFromFeedIsRecordedOnce(t *testing.T) {
	e := newEnv(t)
	e.provision()
	e.src.Options.MissingMeansTerminated, e.src.Options.MissingGraceRuns = true, 1
	e.must(e.st.UpdateSource(e.ctx, e.src))
	e.writeCSV(row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	e.run()
	jane := e.identity("E1")
	eq(t, "within grace: nothing yet", len(e.events(jane.ID, ActInactive)), 0)
	e.run()
	e.run()
	in := one(t, "inactive once", e.events(jane.ID, ActInactive))
	eq(t, "reason", in.Detail["reason"], any("missing"))
}

func TestHistoryManualPersonRecordsWhoAddedThem(t *testing.T) {
	e := newEnv(t)
	man := model.Source{ID: "src-man", Name: "Contractors", Type: sources.TypeManual, Enabled: true, CreatedAt: e.clock, UpdatedAt: e.clock, Config: map[string]any{}}
	sources.NormalizeManual(&man)
	e.must(e.st.CreateSource(e.ctx, man))
	e.must(e.st.CreateManualPerson(e.ctx, model.ManualPerson{ID: "p1", SourceID: man.ID, ExternalID: "C-1", CreatedBy: "alice", CreatedAt: e.clock, UpdatedAt: e.clock,
		Attrs: map[string]string{"FirstName": "Carl", "LastName": "Contract", "Department": "IT"}}))
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""))
	e.run()
	ids, err := e.st.ListIdentitiesBySource(e.ctx, man.ID)
	e.must(err)
	first := one(t, "first seen", e.events(ids[0].ID, ActFirstSeen))
	eq(t, "origin", first.Detail["origin"], any(OriginManual))
	eq(t, "created by", first.Detail["created_by"], any("alice"))
	eq(t, "hr person is from the feed", one(t, "jane", e.events(e.identity("E1").ID, ActFirstSeen)).Detail["origin"], any(OriginSource))
}

func TestHistoryAccountRemovedOutsideIsRecordedOnce(t *testing.T) {
	e := newEnv(t)
	e.provision()
	bob := e.identity("E2")
	u := e.user("E2")
	delete(e.fake.Users, u.GUID)
	e.run()
	e.run()
	gone := one(t, "account missing", e.events(bob.ID, ActAccountMissing))
	eq(t, "last known sam", gone.Detail["ad_sam"], any("bsmith"))
	eq(t, "last known guid", gone.Detail["ad_guid"], any(u.GUID))
	eq(t, "previous state", gone.Detail["previous_state"], any(model.StateLinked))
}

func TestHistoryDeletedAccountIsKeptAsARecord(t *testing.T) {
	e := newEnv(t)
	e.provision()
	jane := e.identity("E1")
	guid := jane.ADGUID
	e.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "T", ""), row("E2", "Bob", "Smith", "Engineering", "Developer", "A", ""))
	e.run()
	e.approveAll(model.ChangeDisable)
	e.clock = e.clock.Add(31 * 24 * time.Hour)
	e.run()
	eq(t, "delete applied", e.approveAll(model.ChangeDelete), 1)

	kept := e.identity("E1") // the identity row is never removed
	eq(t, "state", kept.State, model.StateDeleted)
	del := one(t, "account deleted", e.events(jane.ID, ActAccountDeleted))
	eq(t, "guid", del.Detail["ad_guid"], any(guid))
	eq(t, "sam", del.Detail["ad_sam"], any("jdoe"))
	if d, _ := del.Detail["ad_dn"].(string); !strings.Contains(d, "Jane Doe") || !strings.HasSuffix(d, disabledOU) {
		t.Fatalf("last known DN: %q", d)
	}
	if attrs, _ := del.Detail["attributes"].(map[string]any); attrs["givenName"] != "Jane" {
		t.Fatalf("the last values set are kept: %+v", del.Detail["attributes"])
	}

	// The whole story is still there: identity rows and the rows of every change.
	all, err := e.st.ListIdentityHistory(e.ctx, jane.ID, jane.SourceID, jane.ExternalID, 0, 200)
	e.must(err)
	seen := map[string]bool{}
	for _, a := range all {
		seen[a.Action] = true
	}
	for _, want := range []string{ActFirstSeen, ActInactive, ActAccountDeleted, "change.create", "change.approve", "change.applied"} {
		if !seen[want] {
			t.Errorf("history lacks %s: %v", want, seen)
		}
	}
	// A deleted person's record survives further syncs.
	e.run()
	eq(t, "still there", e.identity("E1").State, model.StateDeleted)
}
