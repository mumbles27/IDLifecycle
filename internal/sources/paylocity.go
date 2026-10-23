package sources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

func paylocityType() Type {
	return Type{
		Name:  "paylocity",
		Label: "Paylocity (API)",
		Fields: []Field{
			{Name: "base_url", Label: "API base URL", Type: "string", Default: "https://api.paylocity.com", Help: "Use https://apisandbox.paylocity.com for the sandbox. Must be https."},
			{Name: "company_id", Label: "Company ID", Type: "string", Required: true},
			{Name: "client_id", Label: "Client ID", Type: "string", Required: true},
			{Name: "client_secret", Label: "Client secret", Type: "password", Required: true, Help: "Stored encrypted; never shown again."},
			{Name: "scope", Label: "OAuth scope", Type: "string", Default: "WebLinkAPI"},
			{Name: "page_size", Label: "Page size", Type: "int", Default: 200},
			{Name: "fetch_details", Label: "Fetch per-employee details", Type: "bool", Default: false, Help: "One extra API call per employee; needed for fields the list endpoint omits (department, supervisor, ...)."},
		},
		New: newPaylocity,
	}
}

type paylocity struct {
	base      *url.URL
	company   string
	clientID  string
	secret    string
	scope     string
	pageSize  int
	details   bool
	client    *http.Client
	token     string
	tokenExp  time.Time
	tokenLock sync.Mutex
}

func newPaylocity(cfg map[string]any) (Source, error) {
	raw := cfgString(cfg, "base_url", "https://api.paylocity.com")
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("paylocity: base_url %q is not a valid URL", raw)
	}
	if u.Scheme != "https" && !isLoopbackHost(u.Hostname()) {
		return nil, errors.New("paylocity: base_url must use https")
	}
	p := &paylocity{
		base:     u,
		company:  cfgString(cfg, "company_id", ""),
		clientID: cfgString(cfg, "client_id", ""),
		secret:   cfgString(cfg, "client_secret", ""),
		scope:    cfgString(cfg, "scope", "WebLinkAPI"),
		pageSize: cfgInt(cfg, "page_size", 200),
		details:  cfgBool(cfg, "fetch_details"),
		client:   &http.Client{Timeout: 60 * time.Second},
	}
	if p.company == "" || p.clientID == "" || p.secret == "" {
		return nil, errors.New("paylocity: company_id, client_id and client_secret are required")
	}
	if p.pageSize < 1 || p.pageSize > 1000 {
		p.pageSize = 200
	}
	return p, nil
}

func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func (p *paylocity) Fetch(ctx context.Context) ([]Row, error) {
	var rows []Row
	var prevFirst Row
	for page := 0; ; page++ {
		path := fmt.Sprintf("/api/v2/companies/%s/employees?pagesize=%d&pagenumber=%d", url.PathEscape(p.company), p.pageSize, page)
		body, err := p.get(ctx, path)
		if err != nil {
			return nil, err
		}
		items, err := extractItems(body)
		if err != nil {
			return nil, fmt.Errorf("paylocity: page %d: %w", page, err)
		}
		if len(items) == 0 {
			break
		}
		first := Flatten(items[0])
		if page > 0 && sameRow(first, prevFirst) {
			break // the server ignored pagenumber and repeated the previous page
		}
		prevFirst = first
		for _, it := range items {
			rows = append(rows, Flatten(it))
		}
		if page > 10000 {
			return nil, errors.New("paylocity: pagination did not terminate")
		}
	}

	if p.details {
		if err := p.enrich(ctx, rows); err != nil {
			return nil, err
		}
	}
	return rows, nil
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

// enrich fetches /employees/{id} for each row (4 workers) and merges the flattened detail fields.
func (p *paylocity) enrich(ctx context.Context, rows []Row) error {
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	jobs := make(chan int)

	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				id := rows[i]["employeeId"]
				if id == "" {
					continue
				}
				body, err := p.get(ctx, fmt.Sprintf("/api/v2/companies/%s/employees/%s", url.PathEscape(p.company), url.PathEscape(id)))
				if err == nil {
					var obj any
					if err = json.Unmarshal(body, &obj); err == nil {
						detail := Flatten(obj)
						mu.Lock()
						for k, v := range detail {
							if _, exists := rows[i][k]; !exists {
								rows[i][k] = v
							}
						}
						mu.Unlock()
					}
				}
				if err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("paylocity: employee %s: %w", id, err)
						cancel()
					}
					mu.Unlock()
				}
			}
		}()
	}
loop:
	for i := range rows {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break loop
		}
	}
	close(jobs)
	wg.Wait()
	if firstErr == nil && parent.Err() != nil {
		return parent.Err() // cancelled mid-way: never hand half-enriched rows to the planner
	}
	return firstErr
}

func (p *paylocity) accessToken(ctx context.Context) (string, error) {
	p.tokenLock.Lock()
	defer p.tokenLock.Unlock()
	if p.token != "" && time.Now().Before(p.tokenExp.Add(-30*time.Second)) {
		return p.token, nil
	}

	form := url.Values{"grant_type": {"client_credentials"}, "scope": {p.scope}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base.String()+"/IdentityServer/connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(p.clientID, p.secret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("paylocity: token request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("paylocity: token request returned HTTP %d: %s", resp.StatusCode, snippet(body))
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tr); err != nil || tr.AccessToken == "" {
		return "", errors.New("paylocity: token response did not contain access_token")
	}
	if tr.ExpiresIn <= 0 {
		tr.ExpiresIn = 300
	}
	p.token = tr.AccessToken
	p.tokenExp = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	return p.token, nil
}

// get performs an authenticated GET with bounded retries for 429/5xx.
func (p *paylocity) get(ctx context.Context, path string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt*attempt) * 500 * time.Millisecond):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		tok, err := p.accessToken(ctx)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base.String()+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")

		resp, err := p.client.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("paylocity: request failed: %w", err)
			continue
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusUnauthorized:
			p.tokenLock.Lock()
			p.token = ""
			p.tokenLock.Unlock()
			lastErr = errors.New("paylocity: unauthorized (token rejected)")
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			lastErr = fmt.Errorf("paylocity: HTTP %d", resp.StatusCode)
		case resp.StatusCode != http.StatusOK:
			return nil, fmt.Errorf("paylocity: HTTP %d for %s: %s", resp.StatusCode, strings.SplitN(path, "?", 2)[0], snippet(body))
		case rerr != nil:
			lastErr = rerr
		default:
			return body, nil
		}
	}
	return nil, lastErr
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// extractItems accepts a bare array or an object wrapping the array.
func extractItems(body []byte) ([]any, error) {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	switch x := v.(type) {
	case []any:
		return x, nil
	case map[string]any:
		for _, k := range []string{"employees", "items", "data", "results"} {
			if arr, ok := x[k].([]any); ok {
				return arr, nil
			}
		}
	}
	return nil, errors.New("response did not contain an employee array")
}

// Flatten converts nested JSON into dotted keys: {"a":{"b":1}} -> {"a.b":"1"}.
// Arrays of scalars are comma-joined; arrays of objects use numeric indexes (a.0.b). Nulls become "".
func Flatten(v any) Row {
	out := Row{}
	flattenInto(out, "", v)
	return out
}

func flattenInto(out Row, prefix string, v any) {
	join := func(k string) string {
		if prefix == "" {
			return k
		}
		return prefix + "." + k
	}
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			flattenInto(out, join(k), child)
		}
	case []any:
		scalar := true
		for _, e := range x {
			switch e.(type) {
			case map[string]any, []any:
				scalar = false
			}
		}
		if scalar {
			parts := make([]string, len(x))
			for i, e := range x {
				parts[i] = scalarString(e)
			}
			out[prefix] = strings.Join(parts, ",")
			return
		}
		for i, e := range x {
			flattenInto(out, join(strconv.Itoa(i)), e)
		}
	default:
		out[prefix] = scalarString(v)
	}
}

func scalarString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	}
	return fmt.Sprint(v)
}
