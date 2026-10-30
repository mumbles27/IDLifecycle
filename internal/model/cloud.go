package model

import (
	"fmt"
	"regexp"
	"strings"
)

// CloudGroupPrefix marks a group list entry as an Entra ID group (entra:<group object id>); every other entry is an AD
// group (sAMAccountName or DN). Role grants and the lifecycle lists may mix both.
const CloudGroupPrefix = "entra:"

// IsCloudGroupRef reports whether a group list entry names an Entra ID group.
func IsCloudGroupRef(g string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(g)), CloudGroupPrefix)
}

// CloudGroupID returns the lower-case group object id of an entra:<id> entry; ok is false for anything else.
func CloudGroupID(g string) (string, bool) {
	g = strings.TrimSpace(g)
	if !IsCloudGroupRef(g) {
		return "", false
	}
	id := strings.ToLower(strings.TrimSpace(g[len(CloudGroupPrefix):]))
	if !guidRe.MatchString(id) {
		return "", false
	}
	return id, true
}

// Microsoft cloud environments (CloudSettings.Environment).
const (
	CloudGlobal   = "global"
	CloudUSGov    = "usgov"    // GCC High
	CloudUSGovDoD = "usgovdod" // DoD
	CloudChina    = "china"    // operated by 21Vianet
)

// Cloud sign-in methods (CloudSettings.Auth).
const (
	CloudAuthCertificate = "certificate" // a certificate in LocalMachine\My (required for Exchange Online)
	CloudAuthSecret      = "secret"      // a client secret (Microsoft Graph only)
)

// How an AD account is matched to its Entra ID user (CloudSettings.UserMatch).
const (
	CloudMatchSID         = "sid"          // onPremisesSecurityIdentifier = the account's objectSid
	CloudMatchImmutableID = "immutable_id" // onPremisesImmutableId = base64 of the account's objectGUID
	CloudMatchUPN         = "upn"          // userPrincipalName
)

// CloudExchange is the Exchange Online connection used for mail-enabled security groups and distribution lists.
type CloudExchange struct {
	Enabled      bool   `json:"enabled"`
	Organization string `json:"organization"` // the tenant's initial domain, e.g. contoso.onmicrosoft.com
}

// CloudSettings is the Entra ID app registration the service uses to change cloud group memberships (Microsoft Graph,
// and Exchange Online for mail-enabled groups). The scripts sign in with it; the service never holds a token.
type CloudSettings struct {
	Enabled        bool          `json:"enabled"`
	Environment    string        `json:"environment"`
	TenantID       string        `json:"tenant_id"`
	ClientID       string        `json:"client_id"`
	Auth           string        `json:"auth"`
	CertThumbprint string        `json:"cert_thumbprint"`
	ClientSecret   string        `json:"client_secret"` // secret: encrypted at rest, masked in API responses
	UserMatch      string        `json:"user_match"`
	Exchange       CloudExchange `json:"exchange"`
}

// DefaultCloud is a switched-off connection with the recommended choices.
func DefaultCloud() CloudSettings {
	return CloudSettings{Environment: CloudGlobal, Auth: CloudAuthCertificate, UserMatch: CloudMatchSID}
}

func (c *CloudSettings) applyDefaults() {
	d := DefaultCloud()
	if c.Environment == "" {
		c.Environment = d.Environment
	}
	if c.Auth == "" {
		c.Auth = d.Auth
	}
	if c.UserMatch == "" {
		c.UserMatch = d.UserMatch
	}
}

var thumbprintRe = regexp.MustCompile(`^[0-9A-F]{40}$`)

// Validate normalizes and checks the connection. Field formats are always checked; what a working connection needs
// (ids, a credential) only when it is enabled. The client secret is only checked for presence (the API stores it).
func (c *CloudSettings) Validate() error {
	c.applyDefaults()
	c.TenantID = strings.ToLower(strings.TrimSpace(c.TenantID))
	c.ClientID = strings.ToLower(strings.TrimSpace(c.ClientID))
	c.CertThumbprint = strings.ToUpper(strings.NewReplacer(" ", "", ":", "", "\u200e", "").Replace(strings.TrimSpace(c.CertThumbprint)))
	c.Exchange.Organization = strings.ToLower(strings.TrimSpace(c.Exchange.Organization))
	switch c.Environment {
	case CloudGlobal, CloudUSGov, CloudUSGovDoD, CloudChina:
	default:
		return fmt.Errorf("cloud.environment must be global, usgov, usgovdod or china")
	}
	switch c.Auth {
	case CloudAuthCertificate, CloudAuthSecret:
	default:
		return fmt.Errorf("cloud.auth must be certificate or secret")
	}
	switch c.UserMatch {
	case CloudMatchSID, CloudMatchImmutableID, CloudMatchUPN:
	default:
		return fmt.Errorf("cloud.user_match must be sid, immutable_id or upn")
	}
	if c.TenantID != "" && !guidRe.MatchString(c.TenantID) && !domainRe.MatchString(c.TenantID) {
		return fmt.Errorf("cloud.tenant_id must be the directory (tenant) ID, a GUID, or the tenant's domain (contoso.onmicrosoft.com)")
	}
	if c.ClientID != "" && !guidRe.MatchString(c.ClientID) {
		return fmt.Errorf("cloud.client_id must be the application (client) ID, a GUID")
	}
	if c.CertThumbprint != "" && !thumbprintRe.MatchString(c.CertThumbprint) {
		return fmt.Errorf("cloud.cert_thumbprint must be the certificate's thumbprint: 40 hexadecimal characters")
	}
	if c.Exchange.Organization != "" && !domainRe.MatchString(c.Exchange.Organization) {
		return fmt.Errorf("cloud.exchange.organization must be the tenant's initial domain, e.g. contoso.onmicrosoft.com")
	}
	if !c.Enabled {
		return nil
	}
	return c.CheckComplete()
}

// CheckComplete reports what a connection still needs before it can be used (also for an unsaved "test connection").
func (c CloudSettings) CheckComplete() error {
	switch {
	case c.TenantID == "":
		return fmt.Errorf("cloud.tenant_id is required")
	case c.ClientID == "":
		return fmt.Errorf("cloud.client_id is required")
	case c.Auth == CloudAuthCertificate && c.CertThumbprint == "":
		return fmt.Errorf("cloud.cert_thumbprint is required when cloud.auth is certificate")
	case c.Auth == CloudAuthSecret && c.ClientSecret == "":
		return fmt.Errorf("cloud.client_secret is required when cloud.auth is secret")
	case c.Exchange.Enabled && c.Auth != CloudAuthCertificate:
		return fmt.Errorf("cloud.exchange needs cloud.auth = certificate: Exchange Online does not accept client secrets for app-only sign-in")
	case c.Exchange.Enabled && c.Exchange.Organization == "":
		return fmt.Errorf("cloud.exchange.organization is required when Exchange Online is enabled (the tenant's initial domain, e.g. contoso.onmicrosoft.com)")
	}
	return nil
}
