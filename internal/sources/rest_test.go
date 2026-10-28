package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"idlifecycle/internal/model"
)

// people returns n records shaped like a typical HR API.
func people(from, n int) []map[string]any {
	out := make([]map[string]any, 0, n)
	for i := from; i < from+n; i++ {
		out = append(out, map[string]any{"id": fmt.Sprintf("E%d", i), "name": map[string]any{"first": "P", "last": strconv.Itoa(i)}, "tags": []any{"a", "b"}, "active": true})
	}
	return out
}

func newRESTForTest(t *testing.T, cfg map[string]any) *restSource {
	t.Helper()
	s, err := newREST(cfg)
	if err != nil {
		t.Fatal(err)
	}
	rs := s.(*restSource)
	rs.http.BaseDelay = time.Millisecond
	return rs
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func ids(rows []Row) string {
	var s []string
	for _, r := range rows {
		s = append(s, r["id"])
	}
	return strings.Join(s, ",")
}

func TestRESTNoPaginationRecordsPathFlattenAndInclude(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/employees" || r.URL.Query().Get("status") != "all" || r.Header.Get("Accept-Version") != "2" {
			http.Error(w, "bad request "+r.URL.String(), 400)
			return
		}
		writeJSON(w, map[string]any{"result": map[string]any{"employees": people(1, 3)}})
	}))
	defer srv.Close()
	s := newRESTForTest(t, map[string]any{"base_url": srv.URL, "path": "/v1/employees", "query": "status=all", "headers": "Accept-Version: 2",
		"records_path": "result.employees"})
	rows, err := s.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ids(rows) != "E1,E2,E3" || rows[0]["name.first"] != "P" || rows[0]["tags"] != "a,b" || rows[0]["active"] != "true" {
		t.Fatalf("rows: %+v", rows)
	}
	s = newRESTForTest(t, map[string]any{"base_url": srv.URL, "path": "/v1/employees", "query": "status=all", "headers": "Accept-Version: 2",
		"records_path": "result.employees", "include_fields": "id, name.last"})
	rows, _ = s.Fetch(context.Background())
	if len(rows[0]) != 2 || rows[0]["name.last"] != "1" {
		t.Fatalf("include_fields: %+v", rows[0])
	}
}

func TestRESTRecordsPathVariants(t *testing.T) {
	for name, tc := range map[string]struct {
		body    string
		path    string
		want    int
		wantErr string
	}{
		"bare array":     {`[{"id":"1"},{"id":"2"}]`, "", 2, ""},
		"nested":         {`{"data":{"items":[{"id":"1"}]}}`, "data.items", 1, ""},
		"null page":      {`{"data":null}`, "data", 0, ""},
		"missing path":   {`{"data":[]}`, "records", 0, "does not exist"},
		"not an array":   {`{"data":{"id":"1"}}`, "data", 0, "not a JSON array"},
		"object no path": {`{"data":[]}`, "", 0, "not a JSON array"},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(tc.body)) }))
			defer srv.Close()
			rows, err := newRESTForTest(t, map[string]any{"base_url": srv.URL, "records_path": tc.path}).Fetch(context.Background())
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("want error %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil || len(rows) != tc.want {
				t.Fatalf("%v %+v", err, rows)
			}
		})
	}
}

func TestRESTAuthModes(t *testing.T) {
	check := func(t *testing.T, cfg map[string]any, ok func(r *http.Request) bool) {
		t.Helper()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !ok(r) {
				http.Error(w, "denied", 401)
				return
			}
			writeJSON(w, people(1, 1))
		}))
		defer srv.Close()
		cfg["base_url"] = srv.URL
		rows, err := newRESTForTest(t, cfg).Fetch(context.Background())
		if err != nil || len(rows) != 1 {
			t.Fatalf("%v %+v", err, rows)
		}
	}
	t.Run("none", func(t *testing.T) {
		check(t, map[string]any{}, func(r *http.Request) bool { return r.Header.Get("Authorization") == "" })
	})
	t.Run("api key header", func(t *testing.T) {
		check(t, map[string]any{"auth": "api_key", "api_key_name": "X-Key", "api_key": "k-123"}, func(r *http.Request) bool { return r.Header.Get("X-Key") == "k-123" })
	})
	t.Run("api key query", func(t *testing.T) {
		check(t, map[string]any{"auth": "api_key", "api_key_in": "query", "api_key_name": "apikey", "api_key": "k-456"},
			func(r *http.Request) bool { return r.URL.Query().Get("apikey") == "k-456" })
	})
	t.Run("basic", func(t *testing.T) {
		check(t, map[string]any{"auth": "basic", "username": "u", "password": "p-789"}, func(r *http.Request) bool {
			u, p, ok := r.BasicAuth()
			return ok && u == "u" && p == "p-789"
		})
	})
	t.Run("bearer", func(t *testing.T) {
		check(t, map[string]any{"auth": "bearer", "bearer_token": "tok-1"}, func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer tok-1" })
	})
	t.Run("missing secret", func(t *testing.T) {
		if _, err := newREST(map[string]any{"base_url": "https://hr.example.com", "auth": "bearer"}); err == nil || !strings.Contains(err.Error(), "bearer_token is required") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestRESTOAuth2TokenCachingAndRefreshOn401(t *testing.T) {
	var tokenCalls, apiCalls int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/token":
			n := atomic.AddInt32(&tokenCalls, 1)
			_ = r.ParseForm()
			if r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("scope") != "people.read" || r.Form.Get("audience") != "hr-api" {
				http.Error(w, "bad form", 400)
				return
			}
			if r.Header.Get("Authorization") != "" { // body style
				if id, sec, _ := r.BasicAuth(); id != "cid" || sec != "csecret-xyz" {
					http.Error(w, "bad client", 401)
					return
				}
			} else if r.Form.Get("client_id") != "cid" || r.Form.Get("client_secret") != "csecret-xyz" {
				http.Error(w, "bad client", 401)
				return
			}
			writeJSON(w, map[string]any{"access_token": fmt.Sprintf("tok%d", n), "expires_in": 3600})
		default:
			n := atomic.AddInt32(&apiCalls, 1)
			if n == 2 && r.Header.Get("Authorization") == "Bearer tok1" { // the first token is revoked after one call
				http.Error(w, "expired", 401)
				return
			}
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer tok") {
				http.Error(w, "no token", 401)
				return
			}
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if page > 3 {
				writeJSON(w, []any{})
				return
			}
			writeJSON(w, people(page, 1))
		}
	}))
	defer srv.Close()
	for _, style := range []string{"header", "body"} {
		atomic.StoreInt32(&tokenCalls, 0)
		atomic.StoreInt32(&apiCalls, 0)
		s := newRESTForTest(t, map[string]any{"base_url": srv.URL, "auth": "oauth2_client_credentials", "token_url": srv.URL + "/oauth/token",
			"client_id": "cid", "client_secret": "csecret-xyz", "scope": "people.read", "audience": "hr-api", "token_auth": style,
			"pagination": "page", "page_size": 1})
		rows, err := s.Fetch(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", style, err)
		}
		if ids(rows) != "E1,E2,E3" {
			t.Fatalf("%s: %s", style, ids(rows))
		}
		if got := atomic.LoadInt32(&tokenCalls); got != 2 {
			t.Errorf("%s: token cached, renewed once after the 401: %d token calls", style, got)
		}
	}
}

func TestRESTPaginationModes(t *testing.T) {
	const total = 7
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch r.URL.Path {
		case "/page": // page numbers from 0, size 3
			p, _ := strconv.Atoi(q.Get("pg"))
			size, _ := strconv.Atoi(q.Get("per"))
			from := p * size
			writeJSON(w, map[string]any{"data": people(from+1, max(0, min(size, total-from)))})
		case "/offset":
			off, _ := strconv.Atoi(q.Get("offset"))
			lim, _ := strconv.Atoi(q.Get("limit"))
			writeJSON(w, people(off+1, max(0, min(lim, total-off))))
		case "/cursor":
			c, _ := strconv.Atoi(q.Get("after"))
			n := min(3, total-c)
			next := ""
			if c+n < total {
				next = strconv.Itoa(c + n)
			}
			writeJSON(w, map[string]any{"items": people(c+1, n), "meta": map[string]any{"next": next}})
		case "/next":
			c, _ := strconv.Atoi(q.Get("from"))
			n := min(3, total-c)
			body := map[string]any{"items": people(c+1, n)}
			if c+n < total {
				body["links"] = map[string]any{"next": fmt.Sprintf("/next?from=%d", c+n)}
			}
			writeJSON(w, body)
		case "/linkheader":
			c, _ := strconv.Atoi(q.Get("from"))
			n := min(3, total-c)
			if c+n < total {
				w.Header().Set("Link", fmt.Sprintf(`<http://%s/linkheader?from=%d>; rel="next", <http://%s/x>; rel="last"`, r.Host, c+n, r.Host))
			}
			writeJSON(w, people(c+1, n))
		case "/ignores":
			writeJSON(w, people(1, 3)) // ignores the page parameter
		}
	}))
	defer srv.Close()
	want := "E1,E2,E3,E4,E5,E6,E7"
	for name, cfg := range map[string]map[string]any{
		"page":        {"path": "/page", "records_path": "data", "pagination": "page", "page_param": "pg", "page_start": 0, "size_param": "per", "page_size": 3},
		"offset":      {"path": "/offset", "pagination": "offset", "page_size": 3},
		"cursor":      {"path": "/cursor", "records_path": "items", "pagination": "cursor", "cursor_param": "after", "cursor_path": "meta.next"},
		"next path":   {"path": "/next", "records_path": "items", "pagination": "next_link", "next_path": "links.next"},
		"link header": {"path": "/linkheader", "pagination": "next_link"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg["base_url"] = srv.URL
			rows, err := newRESTForTest(t, cfg).Fetch(context.Background())
			if err != nil || ids(rows) != want {
				t.Fatalf("%v %s", err, ids(rows))
			}
		})
	}
	t.Run("page ignored by the API", func(t *testing.T) {
		rows, err := newRESTForTest(t, map[string]any{"base_url": srv.URL, "path": "/ignores", "pagination": "page", "page_size": 3}).Fetch(context.Background())
		if err != nil || ids(rows) != "E1,E2,E3" {
			t.Fatalf("a repeated page ends paging without duplicates: %v %s", err, ids(rows))
		}
	})
	t.Run("max_pages", func(t *testing.T) {
		_, err := newRESTForTest(t, map[string]any{"base_url": srv.URL, "path": "/offset", "pagination": "offset", "page_size": 1, "max_pages": 3}).Fetch(context.Background())
		if err == nil || !strings.Contains(err.Error(), "max_pages (3)") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("max_records", func(t *testing.T) {
		_, err := newRESTForTest(t, map[string]any{"base_url": srv.URL, "path": "/offset", "pagination": "offset", "page_size": 3, "max_records": 5}).Fetch(context.Background())
		if err == nil || !strings.Contains(err.Error(), "max_records (5)") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestRESTRefusesOtherHosts(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("credentials reached another host")
		}
		writeJSON(w, people(1, 1))
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/next":
			writeJSON(w, map[string]any{"items": people(1, 1), "next": other.URL + "/steal"})
		case "/redirect":
			http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
		}
	}))
	defer srv.Close()
	_, err := newRESTForTest(t, map[string]any{"base_url": srv.URL, "path": "/next", "records_path": "items", "pagination": "next_link", "next_path": "next",
		"auth": "bearer", "bearer_token": "tok-secret"}).Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "another host") {
		t.Fatalf("next link to another host: %v", err)
	}
	_, err = newRESTForTest(t, map[string]any{"base_url": srv.URL, "path": "/redirect", "auth": "bearer", "bearer_token": "tok-secret"}).Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "another host") {
		t.Fatalf("redirect to another host: %v", err)
	}
}

func TestRESTRetriesAndErrorsWithoutSecrets(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/flaky":
			if atomic.AddInt32(&calls, 1) < 3 {
				w.Header().Set("Retry-After", "0")
				http.Error(w, "slow down", 429)
				return
			}
			writeJSON(w, people(1, 2))
		case "/down":
			http.Error(w, "down for maintenance", 503)
		case "/html":
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>login page with key-SECRET-1 echoed</html>"))
		case "/denied":
			http.Error(w, "bad key key-SECRET-1", 403)
		}
	}))
	defer srv.Close()
	rows, err := newRESTForTest(t, map[string]any{"base_url": srv.URL, "path": "/flaky"}).Fetch(context.Background())
	if err != nil || len(rows) != 2 || atomic.LoadInt32(&calls) != 3 {
		t.Fatalf("retried 429 twice: %v %d calls", err, calls)
	}
	_, err = newRESTForTest(t, map[string]any{"base_url": srv.URL, "path": "/down"}).Fetch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "HTTP 503") {
		t.Fatalf("503 after retries: %v", err)
	}
	for _, path := range []string{"/html", "/denied"} {
		_, err = newRESTForTest(t, map[string]any{"base_url": srv.URL, "path": path, "auth": "api_key", "api_key_in": "query", "api_key_name": "key", "api_key": "key-SECRET-1"}).Fetch(context.Background())
		if err == nil || strings.Contains(err.Error(), "key-SECRET-1") || strings.Contains(err.Error(), "key=") {
			t.Fatalf("%s: the error must not carry the key or the query string: %v", path, err)
		}
	}
	if !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("403 named: %v", err)
	}
	// A request that cannot connect names the URL without the secret query parameter.
	_, err = newRESTForTest(t, map[string]any{"base_url": "http://127.0.0.1:1", "auth": "api_key", "api_key_in": "query", "api_key_name": "key", "api_key": "key-SECRET-1"}).Fetch(context.Background())
	if err == nil || strings.Contains(err.Error(), "key-SECRET-1") {
		t.Fatalf("network error leaks the key: %v", err)
	}
}

func TestRESTContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "busy", 503) }))
	defer srv.Close()
	s := newRESTForTest(t, map[string]any{"base_url": srv.URL})
	s.http.BaseDelay = time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := s.Fetch(ctx); err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("cancel must stop waiting: %v after %s", err, time.Since(start))
	}
}

func TestRESTPOSTBody(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			http.Error(w, "busy", 503)
			return
		}
		var body map[string]any
		if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" || json.NewDecoder(r.Body).Decode(&body) != nil || body["report"] != "staff" {
			http.Error(w, "bad", 400)
			return
		}
		writeJSON(w, people(1, 1))
	}))
	defer srv.Close()
	// Retries re-send the body: the first call fails.
	rows, err := newRESTForTest(t, map[string]any{"base_url": srv.URL, "method": "POST", "body": `{"report":"staff"}`}).Fetch(context.Background())
	if err != nil || len(rows) != 1 || atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("%v (%d calls)", err, calls)
	}
}

func TestRESTValidation(t *testing.T) {
	base := func(extra map[string]any) model.Source {
		cfg := map[string]any{"base_url": "https://hr.example.com"}
		for k, v := range extra {
			cfg[k] = v
		}
		return model.Source{Name: "hr", Type: TypeREST, Options: model.SourceOptions{ExternalIDField: "id"}, Config: cfg}
	}
	if err := Validate(base(nil)); err != nil {
		t.Fatal(err)
	}
	for want, extra := range map[string]map[string]any{
		"must use https":               {"base_url": "http://hr.example.com"},
		"must not contain a user name": {"base_url": "https://u:p@hr.example.com"},
		"must not contain a query":     {"base_url": "https://hr.example.com?x=1"},
		"not a URL":                    {"path": "https://other.example.com/x"},
		"must not contain ? or #":      {"path": "/x?y=1"},
		"method must be one of":        {"method": "DELETE"},
		"body must be valid JSON":      {"method": "POST", "body": "{nope"},
		"query line 1":                 {"query": "novalue"},
		"headers line 2":               {"headers": "A: b\nbad header"},
		"Authorization is set by":      {"headers": "Authorization: Bearer x"},
		"auth must be one of":          {"auth": "kerberos"},
		"api_key_in must be one of":    {"auth": "api_key", "api_key_in": "cookie"},
		"token_url":                    {"auth": "oauth2_client_credentials", "client_id": "c"},
		"client_id is required":        {"auth": "oauth2_client_credentials", "token_url": "https://login.example.com/t"},
		"username is required":         {"auth": "basic"},
		"pagination must be one of":    {"pagination": "infinite"},
		"cursor_param and cursor_path": {"pagination": "cursor"},
		"page_start must be 0 or 1":    {"pagination": "page", "page_start": 5},
		"page_size must be between":    {"page_size": 0},
		"max_pages and max_records":    {"max_records": 0},
		"timeout_seconds must be":      {"timeout_seconds": 0},
		"config.base_url is required":  {"base_url": ""},
		"token_url must use https":     {"auth": "oauth2_client_credentials", "client_id": "c", "token_url": "http://login.example.com/t"},
	} {
		if err := Validate(base(extra)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: want %q, got %v", extra, want, err)
		}
	}
}

func TestUnknownTypeIsRefusedAndItsSecretsStayMasked(t *testing.T) {
	src := model.Source{Name: "old", Type: "retired", Options: model.SourceOptions{ExternalIDField: "employeeId"}, Config: map[string]any{"client_secret": "enc:v1:abc"}}
	if _, err := Build(src, nil); err == nil || !strings.Contains(err.Error(), "unknown source type") {
		t.Fatalf("build: %v", err)
	}
	if err := Validate(src); err == nil || !strings.Contains(err.Error(), "unknown source type") {
		t.Fatalf("validate: %v", err)
	}
	if MaskConfig("retired", src.Config)["client_secret"] != model.MaskedSecret {
		t.Fatal("the stored secret of a type this build does not know stays masked")
	}
}

func TestEndpointChangeNeedsStoredSecretsOnly(t *testing.T) {
	prev := map[string]any{"base_url": "https://hr.example.com", "auth": "bearer", "bearer_token": "enc:v1:x"}
	if !EndpointChangedWithoutSecret(TypeREST, prev, map[string]any{"base_url": "https://evil.example.com", "bearer_token": model.MaskedSecret}) {
		t.Fatal("a stored token must not follow a new base_url")
	}
	if EndpointChangedWithoutSecret(TypeREST, prev, map[string]any{"base_url": "https://evil.example.com", "bearer_token": "fresh"}) {
		t.Fatal("a re-entered token may")
	}
	prev = map[string]any{"base_url": "https://hr.example.com", "token_url": "https://login.example.com/t", "client_secret": "enc:v1:x"}
	if !EndpointChangedWithoutSecret(TypeREST, prev, map[string]any{"base_url": "https://hr.example.com", "token_url": "https://evil.example.com/t", "client_secret": model.MaskedSecret}) {
		t.Fatal("token_url counts as an endpoint")
	}
	if EndpointChangedWithoutSecret(TypeREST, map[string]any{"base_url": "https://a.example.com"}, map[string]any{"base_url": "https://b.example.com"}) {
		t.Fatal("no stored secrets: nothing to leak")
	}
}
