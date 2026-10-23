package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"idlifecycle/internal/model"
	"idlifecycle/internal/secrets"
)

func TestParseCSV(t *testing.T) {
	rows, err := ParseCSV([]byte("\xef\xbb\xbfEmployeeID, First Name ,Dept\r\nE1,Jane,Eng\r\n\r\nE2,Bob\r\n"), ',')
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows (blank lines skipped), got %d", len(rows))
	}
	if rows[0]["EmployeeID"] != "E1" || rows[0]["First Name"] != "Jane" {
		t.Fatalf("BOM/header trim failed: %+v", rows[0])
	}
	if v, ok := rows[1]["Dept"]; !ok || v != "" {
		t.Fatalf("short rows must be padded: %+v", rows[1])
	}
}

func TestParseCSVErrors(t *testing.T) {
	cases := map[string]string{
		"empty":            "",
		"duplicate header": "a,A\n1,2\n",
		"blank header":     "a,,c\n1,2,3\n",
		"too many fields":  "a,b\n1,2,3\n",
	}
	for name, in := range cases {
		if _, err := ParseCSV([]byte(in), ','); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestCSVSourceFetchAndTabDelimiter(t *testing.T) {
	p := filepath.Join(t.TempDir(), "e.tsv")
	if err := os.WriteFile(p, []byte("id\tname\nE1\tJane\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	typ, _ := Lookup("csv")
	src, err := typ.New(map[string]any{"path": p, "delimiter": `\t`})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := src.Fetch(context.Background())
	if err != nil || len(rows) != 1 || rows[0]["name"] != "Jane" {
		t.Fatalf("fetch: %v %+v", err, rows)
	}
	missing, _ := typ.New(map[string]any{"path": filepath.Join(t.TempDir(), "nope.csv")})
	if _, err := missing.Fetch(context.Background()); err == nil {
		t.Fatal("missing file must error (and so abort the sync, never read as an empty population)")
	}
	if _, err := typ.New(map[string]any{"path": p, "delimiter": "ab"}); err == nil {
		t.Fatal("multi-char delimiter must be rejected")
	}
}

func TestFlatten(t *testing.T) {
	var v any
	_ = json.Unmarshal([]byte(`{"id":7,"name":"Jane","active":true,"manager":null,
		"status":{"type":"A","dates":{"hire":"2020-01-01"}},"tags":["a","b"],"phones":[{"n":"1"},{"n":"2"}]}`), &v)
	got := Flatten(v)
	want := map[string]string{
		"id": "7", "name": "Jane", "active": "true", "manager": "", "status.type": "A",
		"status.dates.hire": "2020-01-01", "tags": "a,b", "phones.0.n": "1", "phones.1.n": "2",
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s: got %q want %q", k, got[k], w)
		}
	}
}

type fakePaylocity struct {
	tokenCalls   int32
	employees    []map[string]any
	rejectFirst  int32 // number of 401s to send before accepting
	detailCalls  int32
	sawBadScheme bool
}

func (f *fakePaylocity) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/IdentityServer/connect/token", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.tokenCalls, 1)
		u, p, _ := r.BasicAuth()
		if u != "cid" || p != "sek" || r.FormValue("grant_type") != "client_credentials" {
			http.Error(w, `{"error":"invalid_client"}`, 400)
			return
		}
		fmt.Fprint(w, `{"access_token":"tok","expires_in":3600}`)
	})
	mux.HandleFunc("/api/v2/companies/C1/employees", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", 401)
			return
		}
		if atomic.AddInt32(&f.rejectFirst, -1) >= 0 {
			http.Error(w, "expired", 401)
			return
		}
		size := 2
		if r.URL.Query().Get("pagesize") != "2" {
			t.Errorf("pagesize not passed: %s", r.URL.RawQuery)
		}
		page := 0
		fmt.Sscan(r.URL.Query().Get("pagenumber"), &page)
		lo, hi := page*size, page*size+size
		if lo > len(f.employees) {
			lo = len(f.employees)
		}
		if hi > len(f.employees) {
			hi = len(f.employees)
		}
		_ = json.NewEncoder(w).Encode(f.employees[lo:hi])
	})
	mux.HandleFunc("/api/v2/companies/C1/employees/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&f.detailCalls, 1)
		id := strings.TrimPrefix(r.URL.Path, "/api/v2/companies/C1/employees/")
		_ = json.NewEncoder(w).Encode(map[string]any{"employeeId": id, "department": map[string]any{"name": "Dept-" + id}})
	})
	return mux
}

func paylocityCfg(url string, extra map[string]any) map[string]any {
	cfg := map[string]any{"base_url": url, "company_id": "C1", "client_id": "cid", "client_secret": "sek", "page_size": 2}
	for k, v := range extra {
		cfg[k] = v
	}
	return cfg
}

func TestPaylocityPagingAuthAndRetry(t *testing.T) {
	f := &fakePaylocity{rejectFirst: 1}
	for i := 1; i <= 5; i++ {
		f.employees = append(f.employees, map[string]any{"employeeId": fmt.Sprintf("E%d", i), "firstName": "N", "currentStatus": map[string]any{"statusType": "A"}})
	}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	typ, _ := Lookup("paylocity")
	src, err := typ.New(paylocityCfg(srv.URL, nil))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 5 || rows[4]["employeeId"] != "E5" || rows[0]["currentStatus.statusType"] != "A" {
		t.Fatalf("unexpected rows (%d): %+v", len(rows), rows)
	}
	if n := atomic.LoadInt32(&f.tokenCalls); n != 2 { // initial + re-auth after the 401
		t.Fatalf("token calls = %d, want 2 (token cached, refreshed after 401)", n)
	}
}

func TestPaylocityDetailsAndErrors(t *testing.T) {
	f := &fakePaylocity{}
	for i := 1; i <= 3; i++ {
		f.employees = append(f.employees, map[string]any{"employeeId": fmt.Sprintf("E%d", i)})
	}
	srv := httptest.NewServer(f.handler(t))
	defer srv.Close()

	typ, _ := Lookup("paylocity")
	src, _ := typ.New(paylocityCfg(srv.URL, map[string]any{"fetch_details": true}))
	rows, err := src.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rows[2]["department.name"] != "Dept-E3" || atomic.LoadInt32(&f.detailCalls) != 3 {
		t.Fatalf("details not merged: %+v calls=%d", rows, f.detailCalls)
	}

	bad, _ := typ.New(paylocityCfg(srv.URL, map[string]any{"client_secret": "wrong"}))
	_, err = bad.Fetch(context.Background())
	if err == nil || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("auth failure must error without leaking the secret: %v", err)
	}
}

func TestPaylocityRequiresHTTPS(t *testing.T) {
	typ, _ := Lookup("paylocity")
	if _, err := typ.New(paylocityCfg("http://api.example.com", nil)); err == nil {
		t.Fatal("plain http to a non-loopback host must be refused")
	}
	if _, err := typ.New(paylocityCfg("https://api.paylocity.com", nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := typ.New(map[string]any{"company_id": "x"}); err == nil {
		t.Fatal("missing credentials must be rejected")
	}
}

func TestExtractItemsWrappedObject(t *testing.T) {
	items, err := extractItems([]byte(`{"employees":[{"a":1},{"a":2}]}`))
	if err != nil || len(items) != 2 {
		t.Fatalf("%v %v", err, items)
	}
	if _, err := extractItems([]byte(`{"nope":1}`)); err == nil {
		t.Fatal("unexpected shape must error")
	}
}

func TestSecretHandling(t *testing.T) {
	c, _ := secrets.New(make([]byte, 32))
	in := map[string]any{"company_id": "C1", "client_id": "cid", "client_secret": "sek"}

	stored, err := PrepareConfigForStorage("paylocity", in, nil, c)
	if err != nil {
		t.Fatal(err)
	}
	if !secrets.IsEncrypted(stored["client_secret"].(string)) {
		t.Fatal("secret must be encrypted at rest")
	}
	masked := MaskConfig("paylocity", stored)
	if masked["client_secret"] != model.MaskedSecret || masked["company_id"] != "C1" {
		t.Fatalf("mask: %+v", masked)
	}

	// Round trip from the GUI: masked value must keep the stored ciphertext.
	again, err := PrepareConfigForStorage("paylocity", map[string]any{"company_id": "C2", "client_secret": model.MaskedSecret}, stored, c)
	if err != nil || again["client_secret"] != stored["client_secret"] || again["company_id"] != "C2" {
		t.Fatalf("masked round trip: %v %+v", err, again)
	}
	// Omitted secret on update also keeps it.
	again, _ = PrepareConfigForStorage("paylocity", map[string]any{"company_id": "C2"}, stored, c)
	if again["client_secret"] != stored["client_secret"] {
		t.Fatal("omitted secret should keep previous")
	}
	// Submitting ciphertext directly is refused (prevents replaying/forging stored values).
	if _, err := PrepareConfigForStorage("paylocity", map[string]any{"client_secret": stored["client_secret"]}, nil, c); err == nil {
		t.Fatal("ciphertext input must be refused")
	}

	plain, err := DecryptConfig("paylocity", stored, c)
	if err != nil || plain["client_secret"] != "sek" {
		t.Fatalf("decrypt: %v %+v", err, plain)
	}
	if _, err := DecryptConfig("paylocity", map[string]any{"client_secret": "plaintext"}, c); err == nil {
		t.Fatal("unencrypted stored secret must be flagged")
	}
}

func TestValidate(t *testing.T) {
	ok := model.Source{Name: "x", Type: "csv", Options: model.SourceOptions{ExternalIDField: "id"}, Config: map[string]any{"path": "a.csv"}}
	if err := Validate(ok); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*model.Source){
		"no name":   func(s *model.Source) { s.Name = " " },
		"bad type":  func(s *model.Source) { s.Type = "ldap" },
		"no idfld":  func(s *model.Source) { s.Options.ExternalIDField = "" },
		"no path":   func(s *model.Source) { s.Config = map[string]any{} },
		"neg grace": func(s *model.Source) { s.Options.MissingGraceRuns = -1 },
	} {
		s := ok
		s.Config = map[string]any{"path": "a.csv"}
		mut(&s)
		if err := Validate(s); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}
}
