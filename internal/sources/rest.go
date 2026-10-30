package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TypeREST is the generic HR system API connector: any REST API that returns people as JSON.
const TypeREST = "rest"

// Auth modes (config "auth").
const (
	AuthNone   = "none"
	AuthAPIKey = "api_key"
	AuthBasic  = "basic"
	AuthBearer = "bearer"
	AuthOAuth2 = "oauth2_client_credentials"
)

// Pagination modes (config "pagination").
const (
	PageNone     = "none"
	PageNumber   = "page"
	PageOffset   = "offset"
	PageCursor   = "cursor"
	PageNextLink = "next_link"
)

func restType() Type {
	on := func(field string, values ...string) *ShowIf { return &ShowIf{Field: field, Values: values} }
	return Type{
		Name:  TypeREST,
		Label: "HR system API (REST/JSON)",
		Fields: []Field{
			{Name: "base_url", Label: "Base URL", Type: "string", Required: true, Help: "Scheme and host of the API, e.g. https://api.example-hr.com. Must be https (http only for localhost)."},
			{Name: "path", Label: "Path", Type: "string", Help: "The endpoint that lists people, e.g. /v1/employees. Combined with the base URL."},
			{Name: "method", Label: "Method", Type: "select", Options: []string{"GET", "POST"}, Default: "GET"},
			{Name: "body", Label: "Request body (JSON)", Type: "text", ShowIf: on("method", "POST"), Help: "Sent with POST requests, e.g. a report or filter definition."},
			{Name: "query", Label: "Query parameters", Type: "text", Help: "One per line: name=value, e.g. status=all. Not for secrets (use the API key fields)."},
			{Name: "headers", Label: "Headers", Type: "text", Help: "One per line: Name: value, e.g. Accept-Version: 2. Not for secrets (use the authentication fields)."},

			{Name: "auth", Label: "Authentication", Type: "select", Options: []string{AuthNone, AuthAPIKey, AuthBasic, AuthBearer, AuthOAuth2}, Default: AuthNone,
				Help: "api_key: a key in a header or query parameter. basic: user name + password. bearer: a fixed token. oauth2_client_credentials: a client ID + secret exchanged for a token at the token URL."},
			{Name: "api_key_in", Label: "Send the API key in", Type: "select", Options: []string{"header", "query"}, Default: "header", ShowIf: on("auth", AuthAPIKey)},
			{Name: "api_key_name", Label: "API key header / parameter name", Type: "string", Default: "X-API-Key", ShowIf: on("auth", AuthAPIKey)},
			{Name: "api_key", Label: "API key", Type: "password", ShowIf: on("auth", AuthAPIKey), Help: "Stored encrypted; never shown again."},
			{Name: "username", Label: "User name", Type: "string", ShowIf: on("auth", AuthBasic)},
			{Name: "password", Label: "Password", Type: "password", ShowIf: on("auth", AuthBasic), Help: "Stored encrypted; never shown again."},
			{Name: "bearer_token", Label: "Bearer token", Type: "password", ShowIf: on("auth", AuthBearer), Help: "Stored encrypted; never shown again."},
			{Name: "token_url", Label: "Token URL", Type: "string", ShowIf: on("auth", AuthOAuth2), Help: "OAuth2 token endpoint, e.g. https://login.example-hr.com/oauth/token. Must be https."},
			{Name: "client_id", Label: "Client ID", Type: "string", ShowIf: on("auth", AuthOAuth2)},
			{Name: "client_secret", Label: "Client secret", Type: "password", ShowIf: on("auth", AuthOAuth2), Help: "Stored encrypted; never shown again."},
			{Name: "scope", Label: "Scope", Type: "string", ShowIf: on("auth", AuthOAuth2), Help: "Optional, space-separated."},
			{Name: "audience", Label: "Audience", Type: "string", ShowIf: on("auth", AuthOAuth2), Help: "Optional; some providers (e.g. Auth0-based APIs) need it."},
			{Name: "token_auth", Label: "Send the client credentials", Type: "select", Options: []string{"header", "body"}, Default: "header", ShowIf: on("auth", AuthOAuth2),
				Help: "header: HTTP Basic (the OAuth2 default). body: client_id and client_secret as form fields."},

			{Name: "records_path", Label: "Records path", Type: "string", Help: "Dotted path to the array of people in the response, e.g. data or result.employees. Empty when the response itself is the array."},
			{Name: "include_fields", Label: "Only these fields", Type: "string", Help: "Optional, comma-separated flattened field names to keep (e.g. id, name.first, department.name). Empty keeps all."},

			{Name: "pagination", Label: "Pagination", Type: "select", Options: []string{PageNone, PageNumber, PageOffset, PageCursor, PageNextLink}, Default: PageNone,
				Help: "page: page number + page size parameters. offset: offset + limit parameters. cursor: a cursor from each response. next_link: the next page's URL from the response (or the Link header)."},
			{Name: "page_param", Label: "Page number parameter", Type: "string", Default: "page", ShowIf: on("pagination", PageNumber)},
			{Name: "page_start", Label: "First page number", Type: "int", Default: 1, ShowIf: on("pagination", PageNumber), Help: "0 or 1, as the API counts."},
			{Name: "size_param", Label: "Page size parameter", Type: "string", Default: "page_size", ShowIf: on("pagination", PageNumber), Help: "Empty if the API has a fixed page size."},
			{Name: "offset_param", Label: "Offset parameter", Type: "string", Default: "offset", ShowIf: on("pagination", PageOffset)},
			{Name: "limit_param", Label: "Limit parameter", Type: "string", Default: "limit", ShowIf: on("pagination", PageOffset)},
			{Name: "page_size", Label: "Page size", Type: "int", Default: 100, ShowIf: on("pagination", PageNumber, PageOffset, PageCursor),
				Help: "Records per page. A page with fewer records than this is taken as the last page (page/offset)."},
			{Name: "cursor_param", Label: "Cursor parameter", Type: "string", Default: "cursor", ShowIf: on("pagination", PageCursor)},
			{Name: "cursor_size_param", Label: "Page size parameter (cursor)", Type: "string", ShowIf: on("pagination", PageCursor), Help: "Optional, e.g. limit."},
			{Name: "cursor_path", Label: "Next cursor path", Type: "string", ShowIf: on("pagination", PageCursor), Help: "Dotted path to the next cursor in the response, e.g. meta.next_cursor. Paging stops when it is empty."},
			{Name: "next_path", Label: "Next link path", Type: "string", ShowIf: on("pagination", PageNextLink), Help: "Dotted path to the next page's full URL, e.g. links.next. Empty = the Link: <...>; rel=\"next\" header."},

			{Name: "max_pages", Label: "Maximum pages", Type: "int", Default: 1000, Help: "Safety stop. Reaching it is an error, never a silently shorter list."},
			{Name: "max_records", Label: "Maximum records", Type: "int", Default: 200000, Help: "Safety stop. Reaching it is an error, never a silently shorter list."},
			{Name: "timeout_seconds", Label: "Timeout per request (seconds)", Type: "int", Default: 60},
		},
		New: newREST,
	}
}

type restConfig struct {
	base, endpoint, tokenURL *url.URL
	method                   string
	body                     []byte
	query                    url.Values
	headers                  http.Header
	auth                     string
	apiKeyIn, apiKeyName     string
	apiKey                   string
	user, pass               string
	bearer                   string
	clientID, clientSecret   string
	scope, audience          string
	tokenAuth                string
	recordsPath              string
	include                  map[string]bool
	pagination               string
	pageParam, sizeParam     string
	pageStart, pageSize      int
	offsetParam, limitParam  string
	cursorParam, cursorPath  string
	cursorSizeParam          string
	nextPath                 string
	maxPages, maxRecords     int
	timeout                  time.Duration
}

var headerNameRe = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_` + "`" + `|~-]+$`)

// forbiddenHeaders are set by the connector (or must not be set by configuration).
var forbiddenHeaders = map[string]bool{"authorization": true, "host": true, "content-length": true, "cookie": true, "proxy-authorization": true}

func parseHTTPURL(raw, what string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("%s %q must be an absolute http(s) URL", what, raw)
	}
	if u.User != nil {
		return nil, fmt.Errorf("%s must not contain a user name or password (use the authentication fields)", what)
	}
	if u.Scheme != "https" && !IsLoopbackHost(u.Hostname()) {
		return nil, fmt.Errorf("%s must use https (http is allowed only for localhost)", what)
	}
	return u, nil
}

// parseRESTConfig checks everything except that secrets are present (they are checked when the source is built, after
// decryption; a saved source's secrets arrive masked).
func parseRESTConfig(cfg map[string]any) (*restConfig, error) {
	c := &restConfig{
		method:          strings.ToUpper(cfgString(cfg, "method", "GET")),
		auth:            cfgString(cfg, "auth", AuthNone),
		apiKeyIn:        cfgString(cfg, "api_key_in", "header"),
		apiKeyName:      cfgString(cfg, "api_key_name", "X-API-Key"),
		apiKey:          cfgString(cfg, "api_key", ""),
		user:            cfgString(cfg, "username", ""),
		pass:            cfgString(cfg, "password", ""),
		bearer:          cfgString(cfg, "bearer_token", ""),
		clientID:        cfgString(cfg, "client_id", ""),
		clientSecret:    cfgString(cfg, "client_secret", ""),
		scope:           cfgString(cfg, "scope", ""),
		audience:        cfgString(cfg, "audience", ""),
		tokenAuth:       cfgString(cfg, "token_auth", "header"),
		recordsPath:     cfgString(cfg, "records_path", ""),
		pagination:      cfgString(cfg, "pagination", PageNone),
		pageParam:       cfgString(cfg, "page_param", "page"),
		sizeParam:       strings.TrimSpace(asString(cfg["size_param"])),
		pageStart:       cfgInt(cfg, "page_start", 1),
		pageSize:        cfgInt(cfg, "page_size", 100),
		offsetParam:     cfgString(cfg, "offset_param", "offset"),
		limitParam:      cfgString(cfg, "limit_param", "limit"),
		cursorParam:     cfgString(cfg, "cursor_param", "cursor"),
		cursorPath:      cfgString(cfg, "cursor_path", ""),
		cursorSizeParam: cfgString(cfg, "cursor_size_param", ""),
		nextPath:        cfgString(cfg, "next_path", ""),
		maxPages:        cfgInt(cfg, "max_pages", 1000),
		maxRecords:      cfgInt(cfg, "max_records", 200000),
		timeout:         time.Duration(cfgInt(cfg, "timeout_seconds", 60)) * time.Second,
		query:           url.Values{},
		headers:         http.Header{},
	}
	if _, set := cfg["size_param"]; !set {
		c.sizeParam = "page_size"
	}
	var err error
	if c.base, err = parseHTTPURL(cfgString(cfg, "base_url", ""), "base_url"); err != nil {
		return nil, err
	}
	if c.base.RawQuery != "" {
		return nil, errors.New("base_url must not contain a query string (use query parameters)")
	}
	path := cfgString(cfg, "path", "")
	if strings.Contains(path, "://") || strings.HasPrefix(path, "//") {
		return nil, errors.New("path must be a path such as /v1/employees, not a URL")
	}
	if strings.ContainsAny(path, "?#") {
		return nil, errors.New("path must not contain ? or # (use query parameters)")
	}
	c.endpoint = c.base.JoinPath(path) // #nosec G125 -- the path an admin configured, on the configured base URL (same host enforced by NewHTTPClient)

	switch c.method {
	case "GET":
	case "POST":
		if b := strings.TrimSpace(asString(cfg["body"])); b != "" {
			if !json.Valid([]byte(b)) {
				return nil, errors.New("body must be valid JSON")
			}
			c.body = []byte(b)
		}
	default:
		return nil, errors.New("method must be GET or POST")
	}

	for i, line := range lines(asString(cfg["query"])) {
		k, v, ok := strings.Cut(line, "=")
		if k = strings.TrimSpace(k); !ok || k == "" {
			return nil, fmt.Errorf("query line %d: use name=value", i+1)
		}
		c.query.Add(k, strings.TrimSpace(v))
	}
	for i, line := range lines(asString(cfg["headers"])) {
		k, v, ok := strings.Cut(line, ":")
		k = strings.TrimSpace(k)
		if !ok || !headerNameRe.MatchString(k) {
			return nil, fmt.Errorf("headers line %d: use Name: value", i+1)
		}
		if forbiddenHeaders[strings.ToLower(k)] {
			return nil, fmt.Errorf("headers line %d: %s is set by the connector (use the authentication fields for credentials)", i+1, k)
		}
		if strings.ContainsAny(v, "\r\n") {
			return nil, fmt.Errorf("headers line %d: line breaks are not allowed", i+1)
		}
		c.headers.Add(k, strings.TrimSpace(v))
	}

	switch c.auth {
	case AuthNone, AuthBasic, AuthBearer:
	case AuthAPIKey:
		if c.apiKeyIn != "header" && c.apiKeyIn != "query" {
			return nil, errors.New("api_key_in must be header or query")
		}
		if c.apiKeyIn == "header" && (!headerNameRe.MatchString(c.apiKeyName) || forbiddenHeaders[strings.ToLower(c.apiKeyName)] && !strings.EqualFold(c.apiKeyName, "authorization")) {
			return nil, fmt.Errorf("api_key_name %q is not a usable header name", c.apiKeyName)
		}
		if c.apiKeyName == "" {
			return nil, errors.New("api_key_name is required")
		}
	case AuthOAuth2:
		if c.tokenURL, err = parseHTTPURL(cfgString(cfg, "token_url", ""), "token_url"); err != nil {
			return nil, err
		}
		if c.clientID == "" {
			return nil, errors.New("client_id is required for oauth2_client_credentials")
		}
		if c.tokenAuth != "header" && c.tokenAuth != "body" {
			return nil, errors.New("token_auth must be header or body")
		}
	default:
		return nil, fmt.Errorf("auth must be one of %s, %s, %s, %s, %s", AuthNone, AuthAPIKey, AuthBasic, AuthBearer, AuthOAuth2)
	}
	if c.auth == AuthBasic && c.user == "" {
		return nil, errors.New("username is required for basic authentication")
	}

	if f := strings.TrimSpace(asString(cfg["include_fields"])); f != "" {
		c.include = map[string]bool{}
		for _, name := range strings.Split(f, ",") {
			if name = strings.TrimSpace(name); name != "" {
				c.include[name] = true
			}
		}
	}

	switch c.pagination {
	case PageNone, PageNextLink:
	case PageNumber:
		if c.pageParam == "" {
			return nil, errors.New("page_param is required for page pagination")
		}
		if c.pageStart != 0 && c.pageStart != 1 {
			return nil, errors.New("page_start must be 0 or 1")
		}
	case PageOffset:
		if c.offsetParam == "" || c.limitParam == "" {
			return nil, errors.New("offset_param and limit_param are required for offset pagination")
		}
	case PageCursor:
		if c.cursorParam == "" || c.cursorPath == "" {
			return nil, errors.New("cursor_param and cursor_path are required for cursor pagination")
		}
	default:
		return nil, fmt.Errorf("pagination must be one of %s, %s, %s, %s, %s", PageNone, PageNumber, PageOffset, PageCursor, PageNextLink)
	}
	if c.pageSize < 1 || c.pageSize > 10000 {
		return nil, errors.New("page_size must be between 1 and 10000")
	}
	if c.maxPages < 1 || c.maxRecords < 1 {
		return nil, errors.New("max_pages and max_records must be at least 1")
	}
	if c.timeout < time.Second || c.timeout > 10*time.Minute {
		return nil, errors.New("timeout_seconds must be between 1 and 600")
	}
	return c, nil
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

func validateRESTConfig(cfg map[string]any) error {
	_, err := parseRESTConfig(cfg)
	return err
}

type restSource struct {
	c    *restConfig
	http *HTTPClient

	tokenMu  sync.Mutex
	token    string
	tokenExp time.Time
}

func newREST(cfg map[string]any) (Source, error) {
	c, err := parseRESTConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("rest: %w", err)
	}
	missing := ""
	switch c.auth {
	case AuthAPIKey:
		if c.apiKey == "" {
			missing = "api_key"
		}
	case AuthBasic:
		if c.pass == "" {
			missing = "password"
		}
	case AuthBearer:
		if c.bearer == "" {
			missing = "bearer_token"
		}
	case AuthOAuth2:
		if c.clientSecret == "" {
			missing = "client_secret"
		}
	}
	if missing != "" {
		return nil, fmt.Errorf("rest: %s is required for %s authentication", missing, c.auth)
	}
	return &restSource{c: c, http: NewHTTPClient(c.timeout, c.apiKey, c.pass, c.bearer, c.clientSecret)}, nil
}

func (s *restSource) Fetch(ctx context.Context) ([]Row, error) {
	c := s.c
	var rows []Row
	var prevFirst Row
	next := c.endpoint // next_link mode: the URL of the next page
	cursor := ""
	seenCursors := map[string]bool{}
	for page := 0; ; page++ {
		if page >= c.maxPages {
			return nil, fmt.Errorf("rest: more than max_pages (%d) pages; raise max_pages or check the pagination settings (a shorter list would look like people leaving)", c.maxPages)
		}
		u := *next
		q := u.Query()
		if c.pagination != PageNextLink || page == 0 {
			for k, vs := range c.query {
				q[k] = append([]string(nil), vs...)
			}
		}
		switch c.pagination {
		case PageNumber:
			q.Set(c.pageParam, strconv.Itoa(c.pageStart+page))
			if c.sizeParam != "" {
				q.Set(c.sizeParam, strconv.Itoa(c.pageSize))
			}
		case PageOffset:
			q.Set(c.offsetParam, strconv.Itoa(len(rows)))
			q.Set(c.limitParam, strconv.Itoa(c.pageSize))
		case PageCursor:
			if cursor != "" {
				q.Set(c.cursorParam, cursor)
			}
			if c.cursorSizeParam != "" {
				q.Set(c.cursorSizeParam, strconv.Itoa(c.pageSize))
			}
		}
		if c.auth == AuthAPIKey && c.apiKeyIn == "query" {
			q.Set(c.apiKeyName, c.apiKey)
		}
		u.RawQuery = q.Encode()

		resp, err := s.request(ctx, &u)
		if err != nil {
			return nil, fmt.Errorf("rest: page %d: %w", page+1, err)
		}
		doc, err := s.http.DecodeJSON(resp, SafeURL(&u))
		if err != nil {
			return nil, fmt.Errorf("rest: page %d: %w", page+1, err)
		}
		items, err := RecordsAt(doc, c.recordsPath)
		if err != nil {
			return nil, fmt.Errorf("rest: page %d: %w", page+1, err)
		}
		if len(items) > 0 {
			first := s.row(items[0])
			if page > 0 && (c.pagination == PageNumber || c.pagination == PageOffset) && sameRow(first, prevFirst) {
				break // the API ignored the page parameter and repeated the previous page
			}
			prevFirst = first
		}
		if len(rows)+len(items) > c.maxRecords {
			return nil, fmt.Errorf("rest: more than max_records (%d) records; raise max_records if this is expected", c.maxRecords)
		}
		for _, it := range items {
			rows = append(rows, s.row(it))
		}

		switch c.pagination {
		case PageNone:
			return rows, nil
		case PageNumber, PageOffset:
			if len(items) == 0 || len(items) < c.pageSize {
				return rows, nil
			}
		case PageCursor:
			v, _ := ValueAt(doc, c.cursorPath)
			cursor = strings.TrimSpace(scalarString(v))
			if v == nil || cursor == "" || len(items) == 0 {
				return rows, nil
			}
			if seenCursors[cursor] {
				return nil, errors.New("rest: the API returned the same cursor twice; check cursor_path")
			}
			seenCursors[cursor] = true
		case PageNextLink:
			link := s.nextLink(doc, resp.Header)
			if link == "" || len(items) == 0 {
				return rows, nil
			}
			nu, err := next.Parse(link)
			if err != nil {
				return nil, errors.New("rest: the next page link is not a valid URL")
			}
			if !strings.EqualFold(nu.Scheme, c.base.Scheme) || !strings.EqualFold(nu.Host, c.base.Host) {
				return nil, fmt.Errorf("rest: refusing a next page link to another host (%s://%s); next links must stay on %s", nu.Scheme, nu.Host, SafeURL(c.base))
			}
			next = nu
		}
	}
	return rows, nil
}

func (s *restSource) row(item any) Row {
	r := Flatten(item)
	if s.c.include != nil {
		for k := range r {
			if !s.c.include[k] {
				delete(r, k)
			}
		}
	}
	return r
}

func sameRow(a, b Row) bool {
	if len(a) != len(b) || len(a) == 0 {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

var linkNextRe = regexp.MustCompile(`<([^>]+)>\s*;[^,]*\brel="?next"?`)

func (s *restSource) nextLink(doc any, h http.Header) string {
	if s.c.nextPath != "" {
		v, ok := ValueAt(doc, s.c.nextPath)
		if !ok || v == nil {
			return ""
		}
		return strings.TrimSpace(scalarString(v))
	}
	for _, l := range h.Values("Link") {
		if m := linkNextRe.FindStringSubmatch(l); m != nil {
			return strings.TrimSpace(m[1])
		}
	}
	return ""
}

// request sends one page request with the configured method, body, headers and credentials. With OAuth2, a 401 drops
// the cached token and retries once with a fresh one.
func (s *restSource) request(ctx context.Context, u *url.URL) (*Response, error) {
	for attempt := 0; ; attempt++ {
		var token string
		if s.c.auth == AuthOAuth2 {
			t, err := s.accessToken(ctx)
			if err != nil {
				return nil, err
			}
			token = t
		}
		resp, err := s.http.Do(ctx, func() (*http.Request, error) {
			var body *bytes.Reader
			if s.c.body != nil {
				body = bytes.NewReader(s.c.body)
			}
			var req *http.Request
			var err error
			if body != nil {
				req, err = http.NewRequest(s.c.method, u.String(), body)
			} else {
				req, err = http.NewRequest(s.c.method, u.String(), nil)
			}
			if err != nil {
				return nil, errors.New("cannot build the request")
			}
			for k, vs := range s.c.headers {
				req.Header[k] = append([]string(nil), vs...)
			}
			req.Header.Set("Accept", "application/json")
			if body != nil {
				req.Header.Set("Content-Type", "application/json")
			}
			switch s.c.auth {
			case AuthAPIKey:
				if s.c.apiKeyIn == "header" {
					req.Header.Set(s.c.apiKeyName, s.c.apiKey)
				}
			case AuthBasic:
				req.SetBasicAuth(s.c.user, s.c.pass)
			case AuthBearer:
				req.Header.Set("Authorization", "Bearer "+s.c.bearer)
			case AuthOAuth2:
				req.Header.Set("Authorization", "Bearer "+token)
			}
			return req, nil
		})
		var herr *HTTPError
		if s.c.auth == AuthOAuth2 && attempt == 0 && errors.As(err, &herr) && herr.Status == http.StatusUnauthorized {
			s.tokenMu.Lock()
			s.token = ""
			s.tokenMu.Unlock()
			continue
		}
		return resp, err
	}
}

func (s *restSource) accessToken(ctx context.Context) (string, error) {
	s.tokenMu.Lock()
	defer s.tokenMu.Unlock()
	if s.token != "" && time.Now().Before(s.tokenExp) {
		return s.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}}
	if s.c.scope != "" {
		form.Set("scope", s.c.scope)
	}
	if s.c.audience != "" {
		form.Set("audience", s.c.audience)
	}
	if s.c.tokenAuth == "body" {
		form.Set("client_id", s.c.clientID)
		form.Set("client_secret", s.c.clientSecret)
	}
	encoded := form.Encode()
	resp, err := s.http.Do(ctx, func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodPost, s.c.tokenURL.String(), strings.NewReader(encoded))
		if err != nil {
			return nil, errors.New("cannot build the token request")
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		if s.c.tokenAuth == "header" {
			req.SetBasicAuth(url.QueryEscape(s.c.clientID), url.QueryEscape(s.c.clientSecret))
		}
		return req, nil
	})
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	var tr struct {
		AccessToken string      `json:"access_token"`
		ExpiresIn   json.Number `json:"expires_in"`
	}
	if err := json.Unmarshal(resp.Body, &tr); err != nil || tr.AccessToken == "" {
		return "", errors.New("token request: the response has no access_token")
	}
	s.http.AddSecret(tr.AccessToken)
	secs, _ := tr.ExpiresIn.Int64()
	if secs <= 0 {
		secs = 300
	}
	s.token = tr.AccessToken
	// Renew a little early so a page request never carries a token that expires in flight.
	s.tokenExp = time.Now().Add(time.Duration(secs)*time.Second - time.Duration(min(secs/10, 60))*time.Second)
	return s.token, nil
}
