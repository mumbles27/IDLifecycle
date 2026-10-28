package sources

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

func TestSecretHandling(t *testing.T) {
	c, _ := secrets.New(make([]byte, 32))
	in := map[string]any{"base_url": "https://hr.example.com", "company_id": "C1", "client_id": "cid", "client_secret": "sek"}

	stored, err := PrepareConfigForStorage("rest", in, nil, c)
	if err != nil {
		t.Fatal(err)
	}
	if !secrets.IsEncrypted(stored["client_secret"].(string)) {
		t.Fatal("secret must be encrypted at rest")
	}
	masked := MaskConfig("rest", stored)
	if masked["client_secret"] != model.MaskedSecret || masked["company_id"] != "C1" {
		t.Fatalf("mask: %+v", masked)
	}

	// Round trip from the GUI: masked value must keep the stored ciphertext.
	again, err := PrepareConfigForStorage("rest", map[string]any{"company_id": "C2", "client_secret": model.MaskedSecret}, stored, c)
	if err != nil || again["client_secret"] != stored["client_secret"] || again["company_id"] != "C2" {
		t.Fatalf("masked round trip: %v %+v", err, again)
	}
	// Omitted secret on update also keeps it.
	again, _ = PrepareConfigForStorage("rest", map[string]any{"company_id": "C2"}, stored, c)
	if again["client_secret"] != stored["client_secret"] {
		t.Fatal("omitted secret should keep previous")
	}
	// Submitting ciphertext directly is refused (prevents replaying/forging stored values).
	if _, err := PrepareConfigForStorage("rest", map[string]any{"client_secret": stored["client_secret"]}, nil, c); err == nil {
		t.Fatal("ciphertext input must be refused")
	}

	plain, err := DecryptConfig("rest", stored, c)
	if err != nil || plain["client_secret"] != "sek" {
		t.Fatalf("decrypt: %v %+v", err, plain)
	}
	if _, err := DecryptConfig("rest", map[string]any{"client_secret": "plaintext"}, c); err == nil {
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
