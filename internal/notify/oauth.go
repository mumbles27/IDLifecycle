package notify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"idlifecycle/internal/model"
)

const (
	defaultTokenURL = "https://login.microsoftonline.com/{tenant}/oauth2/v2.0/token"
	defaultGraphURL = "https://graph.microsoft.com"
	scopeSMTP       = "https://outlook.office365.com/.default"
	scopeGraph      = "https://graph.microsoft.com/.default"
)

type tokenKey struct {
	url, tenant, client, scope, secretHash string
}

type cachedToken struct {
	token string
	exp   time.Time
}

func (s *Service) httpClient() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: defaultTimeout}
}

func (s *Service) tokenURL(tenant string) string {
	u := s.TokenURL
	if u == "" {
		u = defaultTokenURL
	}
	return strings.ReplaceAll(u, "{tenant}", url.PathEscape(tenant))
}

// token returns an app-only access token (client credentials) for scope, cached until 60s before expiry.
func (s *Service) token(ctx context.Context, o model.OAuthSettings, scope string, sc *scrubber) (string, error) {
	endpoint := s.tokenURL(o.TenantID)
	h := sha256.Sum256([]byte(o.ClientSecret))
	key := tokenKey{endpoint, strings.ToLower(o.TenantID), strings.ToLower(o.ClientID), scope, hex.EncodeToString(h[:8])}

	s.mu.Lock()
	defer s.mu.Unlock() // held during the request: concurrent senders share one token fetch
	if t, ok := s.tokens[key]; ok && time.Now().Before(t.exp) {
		sc.add(t.token)
		return t.token, nil
	}

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {o.ClientID},
		"client_secret": {o.ClientSecret},
		"scope":         {scope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("oauth: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("oauth: token request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var tr struct {
		AccessToken string          `json:"access_token"`
		ExpiresIn   json.RawMessage `json:"expires_in"`
		Error       string          `json:"error"`
		Description string          `json:"error_description"`
	}
	_ = json.Unmarshal(raw, &tr)
	if tr.AccessToken != "" {
		sc.add(tr.AccessToken)
	}
	if resp.StatusCode != http.StatusOK || tr.AccessToken == "" {
		msg := tr.Error
		if tr.Description != "" {
			msg += ": " + firstLine(tr.Description)
		}
		if msg == "" {
			msg = snippet(raw)
		}
		return "", fmt.Errorf("oauth: token request failed (HTTP %d): %s", resp.StatusCode, sc.clean(msg))
	}
	secs := parseExpires(tr.ExpiresIn)
	if secs > 60 {
		if s.tokens == nil {
			s.tokens = map[tokenKey]cachedToken{}
		}
		s.tokens[key] = cachedToken{token: tr.AccessToken, exp: time.Now().Add(time.Duration(secs-60) * time.Second)}
	}
	return tr.AccessToken, nil
}

// parseExpires accepts expires_in as a JSON number or string (Entra has used both).
func parseExpires(raw json.RawMessage) int {
	var n int
	if json.Unmarshal(raw, &n) == nil {
		return n
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		fmt.Sscan(s, &n)
	}
	return n
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

func snippet(raw []byte) string {
	s := strings.Join(strings.Fields(string(raw)), " ")
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	if s == "" {
		s = "(empty response)"
	}
	return s
}

// ---- Microsoft Graph -------------------------------------------------------

func (s *Service) sendGraph(ctx context.Context, cfg model.NotifySettings, subject, body string, sc *scrubber) error {
	from, err := parseAddr("from", cfg.From)
	if err != nil {
		return err
	}
	type addr struct {
		EmailAddress struct {
			Address string `json:"address"`
			Name    string `json:"name,omitempty"`
		} `json:"emailAddress"`
	}
	var to []addr
	for _, t := range cfg.To {
		a, err := parseAddr("to", t)
		if err != nil {
			return err
		}
		var x addr
		x.EmailAddress.Address = a.Address
		x.EmailAddress.Name = a.Name
		to = append(to, x)
	}
	payload := map[string]any{
		"message": map[string]any{
			"subject":      subject,
			"body":         map[string]string{"contentType": "Text", "content": body},
			"toRecipients": to,
		},
		"saveToSentItems": false,
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	tok, err := s.token(ctx, cfg.OAuth, scopeGraph, sc)
	if err != nil {
		return err
	}
	base := strings.TrimRight(s.GraphURL, "/")
	if base == "" {
		base = defaultGraphURL
	}
	endpoint := base + "/v1.0/users/" + url.PathEscape(from.Address) + "/sendMail"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(buf)))
	if err != nil {
		return fmt.Errorf("graph: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("graph: sendMail: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusAccepted {
		return nil
	}
	var ge struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := snippet(raw)
	if json.Unmarshal(raw, &ge) == nil && ge.Error.Code != "" {
		msg = ge.Error.Code + ": " + firstLine(ge.Error.Message)
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		msg += " (check the Mail.Send application permission, admin consent and any application access policy for the sender mailbox)"
	}
	return errors.New("graph: sendMail failed (HTTP " + fmt.Sprint(resp.StatusCode) + "): " + sc.clean(msg))
}
