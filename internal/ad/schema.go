package ad

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// SchemaAttribute is one attribute a user object may carry, as Get-IDLSchema.ps1 reports it.
// Syntax is one of string, dn, integer, boolean, time, binary, oid, other.
type SchemaAttribute struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Syntax       string `json:"syntax"`
	SingleValued bool   `json:"singleValued"`
	SystemOnly   bool   `json:"systemOnly"`
	Constructed  bool   `json:"constructed"`
	LinkID       int    `json:"linkId"`     // 0 when not linked; odd = back-link (maintained by AD)
	RangeUpper   int    `json:"rangeUpper"` // 0 when unbounded
}

// BackLink reports a linked attribute that AD maintains itself (memberOf, directReports, ...).
func (a SchemaAttribute) BackLink() bool { return a.LinkID != 0 && a.LinkID%2 != 0 }

// SchemaReader is implemented by clients that can read the AD schema. It is optional: the GUI falls back
// to BuiltinUserAttributes when the client does not implement it or the read fails.
type SchemaReader interface {
	Schema(ctx context.Context, server string) ([]SchemaAttribute, error)
}

const scriptSchema = "Get-IDLSchema.ps1"

// Schema runs Get-IDLSchema.ps1, which lists the attributes of the user class (inherited and auxiliary
// classes included).
func (c *PowerShellClient) Schema(ctx context.Context, server string) ([]SchemaAttribute, error) {
	if _, err := os.Stat(filepath.Join(c.ScriptsDir, scriptSchema)); err != nil {
		return nil, fmt.Errorf("%s is not installed in %s", scriptSchema, c.ScriptsDir)
	}
	var resp struct {
		OK         bool              `json:"ok"`
		Error      string            `json:"error"`
		Attributes []SchemaAttribute `json:"attributes"`
	}
	if err := c.run(ctx, scriptSchema, false, map[string]any{"server": server, "className": "user"}, &resp, nil); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("%s: %s", scriptSchema, orUnknown(resp.Error))
	}
	return resp.Attributes, nil
}

// Schema returns the fake's SchemaAttrs (BuiltinUserAttributes when unset) or SchemaErr.
func (f *Fake) Schema(_ context.Context, _ string) ([]SchemaAttribute, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "schema")
	if f.SchemaErr != nil {
		return nil, f.SchemaErr
	}
	if f.SchemaAttrs != nil {
		return append([]SchemaAttribute(nil), f.SchemaAttrs...), nil
	}
	return BuiltinUserAttributes(), nil
}

func str(name, desc string) SchemaAttribute {
	return SchemaAttribute{Name: name, Description: desc, Syntax: "string", SingleValued: true}
}

// BuiltinUserAttributes is the commonly mapped part of the user class, used when the live schema is not
// available. It also lists a few well-known attributes that cannot be mapped, so the GUI can say why.
func BuiltinUserAttributes() []SchemaAttribute {
	out := []SchemaAttribute{
		str("givenName", "First name"),
		str("sn", "Last name (surname)"),
		str("initials", "Initials"),
		str("middleName", "Middle name"),
		str("displayName", "Display name shown in address lists"),
		str("displayNamePrintable", "Printable display name"),
		str("personalTitle", "Personal title (Mr, Ms, Dr)"),
		str("generationQualifier", "Generation suffix (Jr, III)"),
		str("description", "Description"),
		str("title", "Job title"),
		str("department", "Department"),
		str("company", "Company"),
		str("division", "Division"),
		str("employeeID", "Employee ID (the default link key)"),
		str("employeeNumber", "Employee number"),
		str("employeeType", "Employee type"),
		str("physicalDeliveryOfficeName", "Office"),
		str("telephoneNumber", "Office phone"),
		str("mobile", "Mobile phone"),
		str("homePhone", "Home phone"),
		str("ipPhone", "IP phone"),
		str("pager", "Pager"),
		str("facsimileTelephoneNumber", "Fax"),
		str("mail", "Email address"),
		str("wWWHomePage", "Web page"),
		str("streetAddress", "Street"),
		str("l", "City"),
		str("st", "State or province"),
		str("postalCode", "ZIP or postal code"),
		str("c", "Country code, 2 letters (ISO 3166)"),
		str("co", "Country name"),
		{Name: "countryCode", Description: "Country code, numeric (ISO 3166)", Syntax: "integer", SingleValued: true},
		{Name: "manager", Description: "Manager (use value type user_ref)", Syntax: "dn", SingleValued: true, LinkID: 42},
		str("info", "Notes"),
		str("comment", "Comment"),
		{Name: "sAMAccountName", Description: "Logon name (set by the naming templates)", Syntax: "string", SingleValued: true},
		{Name: "userPrincipalName", Description: "UPN (set by the naming templates)", Syntax: "string", SingleValued: true},
		{Name: "cn", Description: "Common name (set by the naming templates)", Syntax: "string", SingleValued: true, SystemOnly: true},
		{Name: "proxyAddresses", Description: "Email aliases", Syntax: "string"},
		{Name: "memberOf", Description: "Group memberships (use role rules)", Syntax: "dn", LinkID: 3, SystemOnly: true},
		{Name: "directReports", Description: "People who report to this user", Syntax: "dn", LinkID: 43, SystemOnly: true},
	}
	for i := 1; i <= 15; i++ {
		out = append(out, str(fmt.Sprintf("extensionAttribute%d", i), fmt.Sprintf("Custom attribute %d (needs the Exchange schema)", i)))
	}
	return out
}
