package api

import (
	"errors"
	"fmt"
	"strings"

	"idlifecycle/internal/model"
	"idlifecycle/internal/secrets"
)

// prepareAuth encrypts the OIDC client secret for storage. A secret sent as model.MaskedSecret or "" keeps the
// stored one, unless the tenant or client ID changed: a secret belongs to one app registration, so it must
// then be re-entered (or is dropped when sign-in is off).
func prepareAuth(in, prev model.AuthSettings, c *secrets.Cipher) (model.AuthSettings, error) {
	out := in
	o, p := &out.OIDC, prev.OIDC
	sameApp := normGUID(o.TenantID) == normGUID(p.TenantID) && normGUID(o.ClientID) == normGUID(p.ClientID)
	switch v := o.ClientSecret; {
	case v == model.MaskedSecret || v == "":
		switch {
		case p.ClientSecret == "":
			o.ClientSecret = ""
		case !sameApp && o.Enabled:
			return out, errors.New("auth.oidc.client_secret: the tenant or client ID changed; enter the client secret of the new app registration")
		case !sameApp:
			o.ClientSecret = ""
		default:
			o.ClientSecret = p.ClientSecret
		}
	case secrets.IsEncrypted(v):
		return out, fmt.Errorf("auth.oidc.client_secret looks like an encrypted value; send the plain secret or %q", model.MaskedSecret)
	default:
		if c == nil {
			return out, errors.New("auth: no cipher configured")
		}
		enc, err := c.Encrypt(v)
		if err != nil {
			return out, err
		}
		o.ClientSecret = enc
	}
	if o.ClientSecret != "" && !secrets.IsEncrypted(o.ClientSecret) && c != nil {
		enc, err := c.Encrypt(o.ClientSecret)
		if err != nil {
			return out, err
		}
		o.ClientSecret = enc
	}
	if o.Enabled && o.ClientSecret == "" {
		return out, errors.New("auth.oidc.client_secret is required to enable Entra ID sign-in")
	}
	return out, nil
}

func maskAuth(a model.AuthSettings) model.AuthSettings {
	if a.OIDC.ClientSecret != "" {
		a.OIDC.ClientSecret = model.MaskedSecret
	}
	return a
}

func normGUID(s string) string { return strings.ToLower(strings.TrimSpace(s)) }
