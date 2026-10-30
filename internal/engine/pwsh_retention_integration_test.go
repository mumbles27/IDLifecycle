package engine

import (
	"os"
	"strings"
	"testing"

	"idlifecycle/internal/model"
)

// Retention through the REAL scripts against the stub AD: a leaver under a policy is disabled into the policy's own
// OU (outside the search bases), keeps the policy's extra group, and is still found by the next snapshot.
func TestRealScriptsRetentionPolicyOUAndKeptGroup(t *testing.T) {
	p := newPwshEnv(t)
	b, _ := os.ReadFile(p.state)
	withOU := strings.Replace(string(b), `"ous":[`, `"ous":["`+legalOU+`",`, 1)
	if withOU == string(b) {
		t.Fatalf("could not add the policy OU to the stub state: %s", b)
	}
	p.must(os.WriteFile(p.state, []byte(withOU), 0o600))

	p.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "A", ""))
	p.run()
	eq(t, "created", p.approveAll(model.ChangeCreate), 1)
	p.policy(model.RetentionPolicy{Name: "Executives", Priority: 10, DeleteAfterDays: 365, DisabledOU: legalOU, ExtraKeepGroups: []string{"GG-Mkt"},
		Conditions: []model.Condition{{Attr: RetFieldMemberOf, Op: "contains", Value: "GG-Mkt"}}})

	p.writeCSV(row("E1", "Jane", "Doe", "Marketing", "Designer", "T", ""))
	p.run()
	eq(t, "disabled", p.approveAll(model.ChangeDisable), 1)
	i, s := p.account("jdoe")
	jane := s.Users[i]
	eq(t, "in the policy's OU", jane.DN, "CN=Jane Doe,"+legalOU)
	eq(t, "disabled", jane.Enabled, false)
	if len(jane.MemberOf) != 1 || !strings.EqualFold(jane.MemberOf[0], gMkt) {
		t.Fatalf("only the kept group is left: %v", jane.MemberOf)
	}

	run := p.run()
	eq(t, "still found: nothing planned", run.Counters["planned_create"]+run.Counters["planned_link"], 0)
	eq(t, "still disabled", p.identity("E1").State, model.StateDisabled)
	eq(t, "under the policy", p.retention("E1").PolicyName, "Executives")
	for _, c := range p.changes(model.StatusFailed, "") {
		t.Errorf("change failed: %s %s", c.Type, c.Error)
	}
}
