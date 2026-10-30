// Package cloud changes Entra ID / Microsoft 365 / Exchange Online group memberships through the PowerShell scripts in
// /scripts (Microsoft Graph for security and Microsoft 365 groups, Exchange Online for mail-enabled security groups and
// distribution lists). The Client interface lets the engine be tested against an in-memory fake.
package cloud

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/model"
	"idlifecycle/internal/secrets"
)

// Group kinds (Group.Kind).
const (
	KindSecurity     = "security"      // cloud security group (Graph)
	KindM365         = "m365"          // Microsoft 365 group (Graph)
	KindMailSecurity = "mail_security" // mail-enabled security group (Exchange Online)
	KindDistribution = "distribution"  // distribution list (Exchange Online)
)

// Why a group cannot be changed (Group.Reason when Manageable is false).
const (
	ReasonDynamic        = "dynamic"         // membership comes from a rule
	ReasonRoleAssignable = "role_assignable" // can hold Entra ID roles: never changed by this service
	ReasonSynced         = "synced"          // mastered in AD: pick it from the AD groups
	ReasonExchangeOff    = "exchange_off"    // mail-enabled: needs the Exchange Online connection
)

// KindLabel names a group kind for people (change summaries).
func KindLabel(kind string) string {
	switch kind {
	case KindSecurity:
		return "security group"
	case KindM365:
		return "Microsoft 365 group"
	case KindMailSecurity:
		return "mail-enabled security group, Exchange Online"
	case KindDistribution:
		return "distribution list, Exchange Online"
	}
	return "cloud group"
}

// ReasonText explains a non-manageable group.
func ReasonText(reason string) string {
	switch reason {
	case ReasonDynamic:
		return "a dynamic group (membership comes from its rule)"
	case ReasonRoleAssignable:
		return "a role-assignable group (it can hold Entra ID admin roles; never changed by IDLifecycle)"
	case ReasonSynced:
		return "synced from AD (pick it under Active Directory)"
	case ReasonExchangeOff:
		return "mail-enabled (needs the Exchange Online connection)"
	}
	return "not manageable"
}

// Connection is what a script needs to sign in (sent on stdin only). ClientSecret is set only for auth = secret.
type Connection struct {
	Environment    string           `json:"environment"`
	TenantID       string           `json:"tenantId"`
	ClientID       string           `json:"clientId"`
	Auth           string           `json:"auth"`
	CertThumbprint string           `json:"certThumbprint"`
	ClientSecret   string           `json:"clientSecret,omitempty"`
	Exchange       ExchangeSettings `json:"exchange"`
}

type ExchangeSettings struct {
	Enabled      bool   `json:"enabled"`
	Organization string `json:"organization"`
}

// ConnectionFrom builds the connection from settings, decrypting the stored client secret.
func ConnectionFrom(s model.CloudSettings, c *secrets.Cipher) (Connection, error) {
	conn := Connection{
		Environment: s.Environment, TenantID: s.TenantID, ClientID: s.ClientID, Auth: s.Auth, CertThumbprint: s.CertThumbprint,
		Exchange: ExchangeSettings{Enabled: s.Exchange.Enabled, Organization: s.Exchange.Organization},
	}
	if s.Auth != model.CloudAuthSecret || s.ClientSecret == "" {
		return conn, nil
	}
	if !secrets.IsEncrypted(s.ClientSecret) {
		conn.ClientSecret = s.ClientSecret
		return conn, nil
	}
	if c == nil {
		return conn, errors.New("cloud: no cipher to decrypt the client secret")
	}
	plain, err := c.Decrypt(s.ClientSecret)
	if err != nil {
		return conn, fmt.Errorf("cloud: cannot decrypt the client secret: %w", err)
	}
	conn.ClientSecret = plain
	return conn, nil
}

// Check is one result of a connection test.
type Check struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Warning bool   `json:"warning,omitempty"` // ok, but worth a look (certificate expiring, broader permissions than needed)
	Detail  string `json:"detail"`
}

// Group is a cloud group as the search and the snapshot report it.
type Group struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Mail        string `json:"mail"`
	Kind        string `json:"kind"`
	Manageable  bool   `json:"manageable"`
	Reason      string `json:"reason"`
}

// User is an Entra ID user synced from AD.
type User struct {
	ID          string `json:"id"`
	UPN         string `json:"upn"`
	SID         string `json:"sid"`
	ImmutableID string `json:"immutableId"`
	Enabled     bool   `json:"enabled"`
}

// SnapshotGroup is a referenced group with its direct user members (Entra user ids).
type SnapshotGroup struct {
	Group
	Found   bool     `json:"found"`
	Members []string `json:"members"`
}

// SnapshotRequest names the groups to read; Match is the settings' user_match.
type SnapshotRequest struct {
	Groups       []string `json:"groups"`
	Match        string   `json:"match"`
	IncludeUsers bool     `json:"includeUsers"`
}

type Snapshot struct {
	Users  []User          `json:"users"`
	Groups []SnapshotGroup `json:"groups"`
}

// SetItem is one person's membership change. Add / Remove / Keep hold group object ids.
type SetItem struct {
	ChangeID   string   `json:"changeId"`
	ExternalID string   `json:"externalId"`
	UserID     string   `json:"userId"`
	Add        []string `json:"add"`
	Remove     []string `json:"remove"`
	RemoveAll  bool     `json:"removeAll"`
	Keep       []string `json:"keep"`
}

// Client is everything the service needs from the cloud.
type Client interface {
	// Test checks the connection (token, permissions, certificate, Exchange Online).
	Test(ctx context.Context, conn Connection) ([]Check, error)
	SearchGroups(ctx context.Context, conn Connection, query string, max int) (groups []Group, truncated bool, err error)
	// ResolveGroups looks groups up by object id (unknown ids are left out).
	ResolveGroups(ctx context.Context, conn Connection, ids []string) ([]Group, error)
	Snapshot(ctx context.Context, conn Connection, req SnapshotRequest) (*Snapshot, error)
	SetGroups(ctx context.Context, conn Connection, dryRun bool, items []SetItem) ([]ad.Result, error)
}

// Script names.
const (
	ScriptTest     = "Test-IDLCloudConnection.ps1"
	ScriptSearch   = "Search-IDLCloudGroups.ps1"
	ScriptSnapshot = "Get-IDLCloudSnapshot.ps1"
	ScriptSet      = "Set-IDLCloudGroups.ps1"
)

// PowerShellClient runs the cloud scripts with the AD client's PowerShell settings.
type PowerShellClient struct {
	PS *ad.PowerShellClient
}

func NewPowerShell(ps *ad.PowerShellClient) *PowerShellClient { return &PowerShellClient{PS: ps} }

func redact(conn Connection) []string {
	if conn.ClientSecret == "" {
		return nil
	}
	return []string{conn.ClientSecret}
}

// installed fails clearly when an optional script is missing (an older scripts folder).
func (c *PowerShellClient) installed(script string) error {
	if _, err := os.Stat(filepath.Join(c.PS.ScriptsDir, script)); err != nil {
		return fmt.Errorf("%s is not installed in %s", script, c.PS.ScriptsDir)
	}
	return nil
}

func (c *PowerShellClient) Test(ctx context.Context, conn Connection) ([]Check, error) {
	if err := c.installed(ScriptTest); err != nil {
		return nil, err
	}
	var resp struct {
		OK     bool    `json:"ok"`
		Error  string  `json:"error"`
		Checks []Check `json:"checks"`
	}
	if err := c.PS.RunScript(ctx, ScriptTest, false, struct {
		Cloud Connection `json:"cloud"`
	}{conn}, &resp, redact(conn)); err != nil {
		return nil, err
	}
	if resp.Checks == nil && resp.Error != "" {
		return nil, fmt.Errorf("%s: %s", ScriptTest, resp.Error)
	}
	if resp.Checks == nil {
		resp.Checks = []Check{}
	}
	return resp.Checks, nil
}

type searchResponse struct {
	OK        bool    `json:"ok"`
	Error     string  `json:"error"`
	Groups    []Group `json:"groups"`
	Truncated bool    `json:"truncated"`
}

func (c *PowerShellClient) search(ctx context.Context, conn Connection, req map[string]any) (*searchResponse, error) {
	if err := c.installed(ScriptSearch); err != nil {
		return nil, err
	}
	req["cloud"] = conn
	var resp searchResponse
	if err := c.PS.RunScript(ctx, ScriptSearch, false, req, &resp, redact(conn)); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("%s: %s", ScriptSearch, orUnknown(resp.Error))
	}
	if resp.Groups == nil {
		resp.Groups = []Group{}
	}
	return &resp, nil
}

func (c *PowerShellClient) SearchGroups(ctx context.Context, conn Connection, query string, max int) ([]Group, bool, error) {
	resp, err := c.search(ctx, conn, map[string]any{"query": query, "max": max})
	if err != nil {
		return nil, false, err
	}
	return resp.Groups, resp.Truncated, nil
}

func (c *PowerShellClient) ResolveGroups(ctx context.Context, conn Connection, ids []string) ([]Group, error) {
	if len(ids) == 0 {
		return []Group{}, nil
	}
	resp, err := c.search(ctx, conn, map[string]any{"ids": ids})
	if err != nil {
		return nil, err
	}
	return resp.Groups, nil
}

func (c *PowerShellClient) Snapshot(ctx context.Context, conn Connection, req SnapshotRequest) (*Snapshot, error) {
	if err := c.installed(ScriptSnapshot); err != nil {
		return nil, err
	}
	var resp struct {
		OK     bool            `json:"ok"`
		Error  string          `json:"error"`
		Users  []User          `json:"users"`
		Groups []SnapshotGroup `json:"groups"`
	}
	body := struct {
		Cloud Connection `json:"cloud"`
		SnapshotRequest
	}{conn, req}
	if err := c.PS.RunScript(ctx, ScriptSnapshot, false, body, &resp, redact(conn)); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("%s: %s", ScriptSnapshot, orUnknown(resp.Error))
	}
	return &Snapshot{Users: resp.Users, Groups: resp.Groups}, nil
}

func (c *PowerShellClient) SetGroups(ctx context.Context, conn Connection, dryRun bool, items []SetItem) ([]ad.Result, error) {
	if len(items) == 0 {
		return nil, nil
	}
	if err := c.installed(ScriptSet); err != nil {
		return nil, err
	}
	var resp struct {
		OK      bool        `json:"ok"`
		Error   string      `json:"error"`
		Results []ad.Result `json:"results"`
	}
	body := struct {
		Cloud Connection `json:"cloud"`
		Items []SetItem  `json:"items"`
	}{conn, items}
	err := c.PS.RunScript(ctx, ScriptSet, dryRun, body, &resp, redact(conn))
	var se *ad.ScriptError
	switch {
	case err == nil:
	case errors.As(err, &se) && se.ExitCode == 1:
		// Some items failed; the response still carries per-item results.
	default:
		return nil, err
	}
	if !resp.OK && len(resp.Results) == 0 {
		return nil, fmt.Errorf("%s: %s", ScriptSet, orUnknown(resp.Error))
	}
	byID := map[string]ad.Result{}
	for _, r := range resp.Results {
		byID[r.ChangeID] = r
	}
	out := make([]ad.Result, 0, len(items))
	for _, it := range items {
		if r, ok := byID[it.ChangeID]; ok {
			out = append(out, r)
			continue
		}
		out = append(out, ad.Result{ChangeID: it.ChangeID, Status: ad.StatusFailed, Error: "script returned no result for this change"})
	}
	return out, nil
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown error"
	}
	return s
}

var _ Client = (*PowerShellClient)(nil)
