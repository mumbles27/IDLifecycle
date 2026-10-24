package notify

import (
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"strings"

	"idlifecycle/internal/model"
	"idlifecycle/internal/secrets"
)

// Exchange Online SMTP AUTH defaults (m365_smtp).
const (
	m365SMTPHost = "smtp.office365.com"
	m365SMTPPort = 587
)

// MaxRecipients caps the To list of one message.
const MaxRecipients = 50

var (
	guidRe   = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	domainRe = regexp.MustCompile(`^(?i)[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)
)

// normalize trims values, cleans the recipient list and fills provider defaults. It never touches secrets.
func normalize(cfg model.NotifySettings) model.NotifySettings {
	cfg.Provider = strings.ToLower(strings.TrimSpace(cfg.Provider))
	if cfg.Provider == "" {
		cfg.Provider = model.NotifySMTP
	}
	cfg.From = strings.TrimSpace(cfg.From)
	cfg.SubjectPrefix = strings.TrimSpace(cfg.SubjectPrefix)
	cfg.GUIBaseURL = strings.TrimRight(strings.TrimSpace(cfg.GUIBaseURL), "/")
	to := make([]string, 0, len(cfg.To))
	seen := map[string]bool{}
	for _, t := range cfg.To {
		for _, part := range splitRecipients(t) {
			if k := strings.ToLower(part); !seen[k] {
				seen[k] = true
				to = append(to, part)
			}
		}
	}
	cfg.To = to

	s := &cfg.SMTP
	s.Host = strings.TrimSpace(s.Host)
	s.Security = strings.ToLower(strings.TrimSpace(s.Security))
	s.Auth = strings.ToLower(strings.TrimSpace(s.Auth))
	s.Username = strings.TrimSpace(s.Username)
	if cfg.Provider == model.NotifyM365SMTP {
		if s.Host == "" {
			s.Host = m365SMTPHost
		}
		if s.Port == 0 {
			s.Port = m365SMTPPort
		}
		if s.Security == "" {
			s.Security = "starttls"
		}
		if s.Auth == "" || s.Auth == "none" {
			s.Auth = "oauth2"
		}
	}
	if s.Security == "" {
		s.Security = "starttls"
	}
	if s.Auth == "" {
		s.Auth = "none"
	}
	if s.Port == 0 {
		switch s.Security {
		case "tls":
			s.Port = 465
		case "none":
			s.Port = 25
		default:
			s.Port = 587
		}
	}
	cfg.OAuth.TenantID = strings.TrimSpace(cfg.OAuth.TenantID)
	cfg.OAuth.ClientID = strings.TrimSpace(cfg.OAuth.ClientID)
	return cfg
}

// splitRecipients splits "a@x, b@y" or newline separated lists. Display names containing commas are not supported.
func splitRecipients(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' || r == '\n' || r == '\r' }) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// usesPassword reports whether the SMTP password is used by cfg (normalized).
func usesPassword(cfg model.NotifySettings) bool {
	return (cfg.Provider == model.NotifySMTP || cfg.Provider == model.NotifyM365SMTP) && cfg.SMTP.Auth == "login"
}

// usesOAuth reports whether the Entra app registration (client secret) is used by cfg (normalized).
func usesOAuth(cfg model.NotifySettings) bool {
	return cfg.Provider == model.NotifyM365Graph || (cfg.Provider == model.NotifyM365SMTP && cfg.SMTP.Auth == "oauth2")
}

func hasCRLF(s string) bool { return strings.ContainsAny(s, "\r\n") }

// parseAddr parses one mailbox and rejects header-injection characters.
func parseAddr(field, s string) (*mail.Address, error) {
	if hasCRLF(s) {
		return nil, fmt.Errorf("%s must not contain line breaks", field)
	}
	a, err := mail.ParseAddress(s)
	if err != nil {
		return nil, fmt.Errorf("%s %q is not a valid email address", field, s)
	}
	if strings.ContainsAny(a.Address, "\r\n<> ") || !strings.Contains(a.Address, "@") {
		return nil, fmt.Errorf("%s %q is not a valid email address", field, s)
	}
	return a, nil
}

// Validate checks a settings block. When cfg.Enabled is false only obviously malformed values are rejected,
// so an admin can save a half-finished configuration with notifications switched off.
func Validate(cfg model.NotifySettings) error {
	return validate(normalize(cfg), cfg.Enabled)
}

func validate(cfg model.NotifySettings, strict bool) error {
	switch cfg.Provider {
	case model.NotifySMTP, model.NotifyM365SMTP, model.NotifyM365Graph:
	default:
		return fmt.Errorf("notifications.provider must be smtp, m365_smtp or m365_graph")
	}
	for name, v := range map[string]string{
		"from": cfg.From, "subject_prefix": cfg.SubjectPrefix, "gui_base_url": cfg.GUIBaseURL,
		"smtp.host": cfg.SMTP.Host, "smtp.username": cfg.SMTP.Username, "smtp.password": cfg.SMTP.Password,
		"oauth.tenant_id": cfg.OAuth.TenantID, "oauth.client_id": cfg.OAuth.ClientID, "oauth.client_secret": cfg.OAuth.ClientSecret,
	} {
		if hasCRLF(v) {
			return fmt.Errorf("notifications.%s must not contain line breaks", name)
		}
	}
	if len(cfg.To) > MaxRecipients {
		return fmt.Errorf("notifications.to: at most %d recipients", MaxRecipients)
	}
	if cfg.SMTP.Port < 0 || cfg.SMTP.Port > 65535 {
		return fmt.Errorf("notifications.smtp.port must be between 1 and 65535")
	}
	switch cfg.SMTP.Security {
	case "starttls", "tls", "none":
	default:
		return fmt.Errorf("notifications.smtp.security must be starttls, tls or none")
	}
	switch cfg.SMTP.Auth {
	case "none", "login":
	case "oauth2":
		if cfg.Provider == model.NotifySMTP {
			return fmt.Errorf("notifications.smtp.auth oauth2 is only supported with provider m365_smtp")
		}
	default:
		return fmt.Errorf("notifications.smtp.auth must be none, login or oauth2")
	}
	if cfg.GUIBaseURL != "" {
		u, err := url.Parse(cfg.GUIBaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("notifications.gui_base_url must be an http(s) URL")
		}
	}
	if !strict {
		return nil
	}

	if cfg.From == "" {
		return errors.New("notifications.from is required")
	}
	if _, err := parseAddr("notifications.from", cfg.From); err != nil {
		return err
	}
	if len(cfg.To) == 0 {
		return errors.New("notifications.to needs at least one recipient")
	}
	for _, t := range cfg.To {
		if _, err := parseAddr("notifications.to", t); err != nil {
			return err
		}
	}

	if cfg.Provider != model.NotifyM365Graph {
		if cfg.SMTP.Host == "" {
			return errors.New("notifications.smtp.host is required")
		}
		if cfg.SMTP.Port < 1 {
			return fmt.Errorf("notifications.smtp.port must be between 1 and 65535")
		}
		if cfg.Provider == model.NotifyM365SMTP && cfg.SMTP.Security == "none" {
			return errors.New("notifications.smtp.security none is not allowed for Microsoft 365 (use starttls)")
		}
		if cfg.SMTP.Auth == "login" {
			if cfg.SMTP.Username == "" || cfg.SMTP.Password == "" {
				return errors.New("notifications.smtp.username and password are required for auth login")
			}
		}
	}
	if usesOAuth(cfg) {
		if cfg.OAuth.TenantID == "" || !(guidRe.MatchString(cfg.OAuth.TenantID) || domainRe.MatchString(cfg.OAuth.TenantID)) {
			return errors.New("notifications.oauth.tenant_id must be a tenant GUID or domain (contoso.onmicrosoft.com)")
		}
		if !guidRe.MatchString(cfg.OAuth.ClientID) {
			return errors.New("notifications.oauth.client_id must be the application (client) ID GUID")
		}
		if cfg.OAuth.ClientSecret == "" {
			return errors.New("notifications.oauth.client_secret is required")
		}
	}
	return nil
}

// PrepareForStorage normalizes cfg and encrypts its secrets. A secret sent as model.MaskedSecret or "" keeps
// prev's stored (encrypted) value. A stored secret is never kept when the endpoint it authenticates to changed
// (provider, SMTP host, OAuth tenant or client ID): if the secret is still in use an error asks the admin to
// re-enter it; if the new configuration no longer uses it, it is dropped.
func PrepareForStorage(in, prev model.NotifySettings, c *secrets.Cipher) (model.NotifySettings, error) {
	out := normalize(in)
	pn := normalize(prev)
	providerChanged := out.Provider != pn.Provider
	hostChanged := !strings.EqualFold(out.SMTP.Host, pn.SMTP.Host)
	oauthChanged := out.OAuth.TenantID != pn.OAuth.TenantID || !strings.EqualFold(out.OAuth.ClientID, pn.OAuth.ClientID)

	pw, err := prepareSecret("smtp.password", out.SMTP.Password, prev.SMTP.Password,
		providerChanged || hostChanged, usesPassword(out), c)
	if err != nil {
		return model.NotifySettings{}, err
	}
	out.SMTP.Password = pw
	// The bearer token obtained with the client secret goes to the SMTP host for m365_smtp.
	oauthEndpoint := providerChanged || oauthChanged || (out.Provider == model.NotifyM365SMTP && hostChanged)
	cs, err := prepareSecret("oauth.client_secret", out.OAuth.ClientSecret, prev.OAuth.ClientSecret,
		oauthEndpoint, usesOAuth(out), c)
	if err != nil {
		return model.NotifySettings{}, err
	}
	out.OAuth.ClientSecret = cs
	return out, nil
}

func prepareSecret(name, v, prev string, endpointChanged, inUse bool, c *secrets.Cipher) (string, error) {
	switch {
	case v == model.MaskedSecret || v == "":
		if prev == "" {
			return "", nil
		}
		if endpointChanged {
			if inUse {
				return "", fmt.Errorf("notifications.%s: the server or app registration changed; re-enter the secret", name)
			}
			return "", nil
		}
		if !secrets.IsEncrypted(prev) {
			if c == nil {
				return "", errors.New("notify: no cipher configured")
			}
			return c.Encrypt(prev)
		}
		return prev, nil
	case secrets.IsEncrypted(v):
		return "", fmt.Errorf("notifications.%s looks like an encrypted value; send the plain secret or %q", name, model.MaskedSecret)
	default:
		if c == nil {
			return "", errors.New("notify: no cipher configured")
		}
		return c.Encrypt(v)
	}
}

// Mask replaces non-empty secrets with model.MaskedSecret for API responses.
func Mask(cfg model.NotifySettings) model.NotifySettings {
	if cfg.SMTP.Password != "" {
		cfg.SMTP.Password = model.MaskedSecret
	}
	if cfg.OAuth.ClientSecret != "" {
		cfg.OAuth.ClientSecret = model.MaskedSecret
	}
	if cfg.To != nil {
		cfg.To = append([]string(nil), cfg.To...)
	}
	return cfg
}

// decryptSecrets returns cfg with secrets in plain text (values that are not encrypted are used as-is).
func decryptSecrets(cfg model.NotifySettings, c *secrets.Cipher) (model.NotifySettings, error) {
	dec := func(name, v string) (string, error) {
		if v == model.MaskedSecret {
			return "", fmt.Errorf("notifications.%s is masked; the stored secret was not resolved", name)
		}
		if !secrets.IsEncrypted(v) {
			return v, nil
		}
		if c == nil {
			return "", errors.New("notify: no cipher configured to decrypt secrets")
		}
		p, err := c.Decrypt(v)
		if err != nil {
			return "", fmt.Errorf("notifications.%s: %w", name, err)
		}
		return p, nil
	}
	var err error
	if cfg.SMTP.Password, err = dec("smtp.password", cfg.SMTP.Password); err != nil {
		return cfg, err
	}
	if cfg.OAuth.ClientSecret, err = dec("oauth.client_secret", cfg.OAuth.ClientSecret); err != nil {
		return cfg, err
	}
	return cfg, nil
}
