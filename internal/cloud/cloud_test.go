package cloud

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/model"
	"idlifecycle/internal/secrets"
)

func TestConnectionFromDecryptsOnlyWhatItNeeds(t *testing.T) {
	c, err := secrets.New(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := c.Encrypt("plain-secret")
	s := model.CloudSettings{Environment: model.CloudUSGov, TenantID: "t", ClientID: "c", Auth: model.CloudAuthSecret, ClientSecret: enc,
		Exchange: model.CloudExchange{Organization: "contoso.onmicrosoft.us"}}
	conn, err := ConnectionFrom(s, c)
	if err != nil || conn.ClientSecret != "plain-secret" || conn.Environment != model.CloudUSGov || conn.Exchange.Organization != "contoso.onmicrosoft.us" {
		t.Fatalf("secret auth: %+v %v", conn, err)
	}
	if _, err := ConnectionFrom(s, nil); err == nil {
		t.Error("no cipher, no secret")
	}
	s.Auth = model.CloudAuthCertificate
	conn, _ = ConnectionFrom(s, c)
	if conn.ClientSecret != "" {
		t.Error("a certificate connection never carries the secret")
	}
	b, _ := json.Marshal(conn)
	if strings.Contains(string(b), "clientSecret") {
		t.Errorf("an empty secret is left out of the request: %s", b)
	}
}

func TestPowerShellClientNamesAMissingScript(t *testing.T) {
	ps := NewPowerShell(&ad.PowerShellClient{Exe: "pwsh", ScriptsDir: t.TempDir()})
	_, _, err := ps.SearchGroups(context.Background(), Connection{}, "sales", 5)
	if err == nil || !strings.Contains(err.Error(), ScriptSearch) {
		t.Errorf("missing script: %v", err)
	}
	if _, err := ps.Test(context.Background(), Connection{}); err == nil || !strings.Contains(err.Error(), ScriptTest) {
		t.Errorf("missing script: %v", err)
	}
	if res, err := ps.SetGroups(context.Background(), Connection{}, false, nil); err != nil || res != nil {
		t.Errorf("nothing to do: %v %v", res, err)
	}
	if got := redact(Connection{ClientSecret: "x"}); len(got) != 1 {
		t.Error("the secret is scrubbed from script messages")
	}
}

func TestFakeRefusesWhatTheScriptsRefuse(t *testing.T) {
	f := NewFake()
	f.AddGroup("g1", "Sales", KindM365, "u1")
	f.AddGroup("g2", "Admins", KindSecurity, "u1").RoleAssignable = true
	f.AddGroup("g3", "Leavers", KindDistribution)
	f.Exchange = false
	res, _ := f.SetGroups(context.Background(), Connection{}, false, []SetItem{{ChangeID: "c1", UserID: "u1", Add: []string{"g3"}}})
	if res[0].Status != ad.StatusFailed || !strings.Contains(res[0].Error, ReasonExchangeOff) {
		t.Errorf("exchange off: %+v", res)
	}
	res, _ = f.SetGroups(context.Background(), Connection{}, false, []SetItem{{ChangeID: "c2", UserID: "u1", RemoveAll: true}})
	if res[0].Status != ad.StatusApplied || f.IsMember("g1", "u1") || !f.IsMember("g2", "u1") {
		t.Errorf("remove all keeps role-assignable groups: %+v", res)
	}
	if KindLabel(KindMailSecurity) == "" || ReasonText("x") == "" {
		t.Error("labels")
	}
}
