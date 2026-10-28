package sources

// A complete dedicated connector, as a template. It lives in a _test.go file so it is compiled only by the tests and
// never registered in the product. To add a real one: copy exampleType/exampleSource into a new file (e.g. acmehr.go),
// rename them, add register(acmeType()) to init in sources.go, and keep a test like TestExampleConnector next to it.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func exampleType() Type {
	return Type{
		Name:  "example_hr", // stored with every source of this type: never rename it later
		Label: "Example HR (dedicated connector)",
		Fields: []Field{
			{Name: "base_url", Label: "Base URL", Type: "string", Required: true},
			{Name: "api_key", Label: "API key", Type: "password", Required: true, Help: "Stored encrypted."},
		},
		New: func(cfg map[string]any) (Source, error) {
			base, err := parseHTTPURL(cfgString(cfg, "base_url", ""), "base_url") // https only (http for localhost)
			if err != nil {
				return nil, err
			}
			key := cfgString(cfg, "api_key", "")
			if key == "" {
				return nil, errors.New("example_hr: api_key is required")
			}
			return &exampleSource{base: base, key: key, http: NewHTTPClient(30*time.Second, key)}, nil
		},
	}
}

type exampleSource struct {
	base *url.URL
	key  string
	http *HTTPClient // retries, Retry-After, size cap, same-host redirects, secrets kept out of errors
}

// Fetch returns everyone, or an error: never a partial list.
func (s *exampleSource) Fetch(ctx context.Context) ([]Row, error) {
	u := s.base.JoinPath("/people")
	resp, err := s.http.Do(ctx, func() (*http.Request, error) {
		req, err := http.NewRequest(http.MethodGet, u.String(), nil)
		if err == nil {
			req.Header.Set("X-Api-Key", s.key)
		}
		return req, err
	})
	if err != nil {
		return nil, err
	}
	doc, err := s.http.DecodeJSON(resp, SafeURL(u))
	if err != nil {
		return nil, err
	}
	items, err := RecordsAt(doc, "people")
	if err != nil {
		return nil, err
	}
	rows := make([]Row, 0, len(items))
	for _, it := range items {
		rows = append(rows, Flatten(it)) // {"name":{"first":"Jane"}} -> "name.first": "Jane"
	}
	return rows, nil
}

func TestExampleConnector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "k1" {
			http.Error(w, "denied", http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"people": []any{map[string]any{"id": "E1", "name": map[string]any{"first": "Jane"}}}})
	}))
	defer srv.Close()

	src, err := exampleType().New(map[string]any{"base_url": srv.URL, "api_key": "k1"})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := src.Fetch(context.Background())
	if err != nil || len(rows) != 1 || rows[0]["name.first"] != "Jane" {
		t.Fatalf("%v %+v", err, rows)
	}
	bad, _ := exampleType().New(map[string]any{"base_url": srv.URL, "api_key": "wrong-key"})
	if _, err := bad.Fetch(context.Background()); err == nil {
		t.Fatal("a refused request is an error, never an empty population")
	}
}
