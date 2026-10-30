package ad

import (
	"context"
	"strings"
	"testing"

	"idlifecycle/internal/model"
)

// The fake promises what the scripts promise for undo: "before" on applied and would results, expect checked before any
// change (drift fails the item untouched), force skips it, set/clear on enable and disable, unlink and proxyAddresses.
func TestFakeUndoContract(t *testing.T) {
	ctx := context.Background()
	f := NewFake()
	f.AddGroup("GG-A", "CN=GG-A,OU=G,DC=t")
	f.AddGroup("GG-B", "CN=GG-B,OU=G,DC=t")
	g := f.AddUser(FakeUser{DN: "CN=Jane,OU=Staff,DC=t", Sam: "jdoe", UPN: "jdoe@t", Enabled: true, External: "E1",
		Attrs: map[string]string{"title": "Analyst", "mail": "jane@t"}, MemberOf: []string{"CN=GG-A,OU=G,DC=t"}, ProxyAddresses: []string{"SMTP:jane@t"}})
	o := ActionOpts{}

	res, _ := f.Update(ctx, ActionOpts{DryRun: true}, []UpdateItem{{ChangeID: "c0", ExternalID: "E1", ObjectGUID: g, Set: map[string]string{"title": "Lead"}, AddGroups: []string{"GG-B"}}})
	if res[0].Status != StatusWould || res[0].Before == nil || *res[0].Before.Attributes["title"] != "Analyst" || len(res[0].Before.GroupsAdded) != 1 {
		t.Fatalf("dry run reports what it would find and change: %+v %+v", res[0], res[0].Before)
	}
	if f.Users[g].Attrs["title"] != "Analyst" {
		t.Fatal("dry run must not change the account")
	}

	res, _ = f.Update(ctx, o, []UpdateItem{{ChangeID: "c1", ExternalID: "E1", ObjectGUID: g, Set: map[string]string{"title": "Lead", "pager": "1"},
		AddGroups: []string{"GG-B", "CN=GG-B,OU=G,DC=t"}, RemoveGroups: []string{"CN=GG-A,OU=G,DC=t"}, MoveTo: "OU=Other,DC=t"}})
	b := res[0].Before
	if res[0].Status != StatusApplied || b.DN != "CN=Jane,OU=Staff,DC=t" || *b.Attributes["title"] != "Analyst" || b.Attributes["pager"] != nil {
		t.Fatalf("before: %+v", b)
	}
	if strings.Join(b.GroupsAdded, ",") != "CN=GG-B,OU=G,DC=t" || strings.Join(b.GroupsRemoved, ",") != "CN=GG-A,OU=G,DC=t" {
		t.Fatalf("only real membership changes are reported: %+v", b)
	}

	lead := "Lead"
	exp := &model.Expect{Attributes: map[string]*string{"title": &lead, "pager": nil}, MemberOf: []string{"GG-A"}}
	res, _ = f.Update(ctx, o, []UpdateItem{{ChangeID: "c2", ExternalID: "E1", ObjectGUID: g, Set: map[string]string{"title": "X"}, Expect: exp}})
	if res[0].Status != StatusFailed || res[0].Error != "drift: pager is '1', expected empty; no longer a member of GG-A" || f.Users[g].Attrs["title"] != "Lead" {
		t.Fatalf("drift must fail the item untouched: %+v", res[0])
	}
	res, _ = f.Update(ctx, o, []UpdateItem{{ChangeID: "c3", ExternalID: "E1", ObjectGUID: g, Set: map[string]string{"title": "X"}, Expect: exp, Force: true}})
	if res[0].Status != StatusApplied || f.Users[g].Attrs["title"] != "X" {
		t.Fatalf("force skips the check: %+v", res[0])
	}

	off := false
	res, _ = f.Disable(ctx, o, []DisableItem{{ChangeID: "c4", ExternalID: "E1", ObjectGUID: g, Set: map[string]string{"title": "Gone"}, Clear: []string{"pager"},
		Expect: &model.Expect{Enabled: &off}}})
	if res[0].Status != StatusFailed || !strings.Contains(res[0].Error, "account is enabled, expected disabled") {
		t.Fatalf("enabled expectation: %+v", res[0])
	}
	res, _ = f.Disable(ctx, o, []DisableItem{{ChangeID: "c5", ExternalID: "E1", ObjectGUID: g, Set: map[string]string{"title": "Gone"}, Clear: []string{"pager"}}})
	if u := f.Users[g]; u.Enabled || u.Attrs["title"] != "Gone" || u.Attrs["pager"] != "" || !*res[0].Before.Enabled {
		t.Fatalf("disable with set/clear: %+v %+v", u, res[0].Before)
	}
	res, _ = f.Enable(ctx, o, []EnableItem{{ChangeID: "c6", ExternalID: "E1", ObjectGUID: g, Set: map[string]string{"title": "Back"}}})
	if u := f.Users[g]; !u.Enabled || u.Attrs["title"] != "Back" || *res[0].Before.Enabled {
		t.Fatalf("enable with set: %+v %+v", u, res[0].Before)
	}

	res, _ = f.Link(ctx, o, []LinkItem{{ChangeID: "c7", ObjectGUID: g, ExternalID: "E9", ExpectedExternalID: "E1",
		Rename: &model.Candidate{Sam: "jdoe2", UPN: "jdoe2@t", CN: "Jane D", Mail: "jane.d@t"}}})
	if res[0].Status != StatusApplied || *res[0].Before.IDValue != "E1" || res[0].Before.Names.Sam != "jdoe" || res[0].Before.Names.ProxyAddresses[0] != "SMTP:jane@t" {
		t.Fatalf("link before: %+v %+v", res[0], res[0].Before)
	}
	if p := strings.Join(f.Users[g].ProxyAddresses, ","); p != "SMTP:jane.d@t,smtp:jane@t" {
		t.Fatalf("rename keeps the old address as an alias: %s", p)
	}
	res, _ = f.Link(ctx, o, []LinkItem{{ChangeID: "c8", ObjectGUID: g, ExternalID: "", ExpectedExternalID: "E9",
		Rename: &model.Candidate{Sam: "jdoe", UPN: "jdoe@t", CN: "Jane", Mail: "jane@t"}, ProxyAddresses: []string{"SMTP:jane@t"},
		Expect: &model.Expect{Names: &model.AccountNames{Sam: "jdoe2"}}}})
	if u := f.Users[g]; res[0].Status != StatusApplied || u.External != "" || u.Sam != "jdoe" || strings.Join(u.ProxyAddresses, ",") != "SMTP:jane@t" {
		t.Fatalf("unlink and restore names: %+v %+v", res[0], u)
	}
	res, _ = f.Link(ctx, o, []LinkItem{{ChangeID: "c9", ObjectGUID: g, ExternalID: "", ExpectedExternalID: ""}})
	if res[0].Status != StatusFailed {
		t.Fatalf("an empty id needs an expected id: %+v", res[0])
	}
}
