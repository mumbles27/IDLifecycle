package sources

import (
	"context"
	"strings"
	"testing"

	"idlifecycle/internal/model"
)

func TestParseFormFields(t *testing.T) {
	fields, err := ParseFormFields(DefaultManualFields)
	if err != nil || len(fields) != 10 || !fields[0].Required || fields[7].Kind != "date" || fields[6].Kind != "email" {
		t.Fatalf("default form: %+v %v", fields, err)
	}
	f, err := ParseFormFields("Dept | Department | required, select: Finance; Sales, default: Sales\n# comment\n\nCompany")
	if err != nil || len(f) != 2 || f[0].Kind != "select" || strings.Join(f[0].Options, ",") != "Finance,Sales" || f[0].Default != "Sales" || f[1].Label != "Company" {
		t.Fatalf("custom form: %+v %v", f, err)
	}
	for _, bad := range []string{"", "ExternalID", "First Name", "A\nA", "A | a | colour", "A | a | select:"} {
		if _, err := ParseFormFields(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestCheckManualAttrs(t *testing.T) {
	fields, _ := ParseFormFields("FirstName | First name | required\nEndDate | End | date\nEmail | Mail | email\nDept | Dept | select: Finance; Sales")
	got, err := CheckManualAttrs(fields, map[string]string{"firstname": " Jane ", "dept": "sales"})
	if err != nil || got["FirstName"] != "Jane" || got["Dept"] != "Sales" || got["EndDate"] != "" {
		t.Fatalf("cleaned: %+v %v", got, err)
	}
	for _, in := range []map[string]string{
		{},
		{"FirstName": "J", "EndDate": "31/12/2026"},
		{"FirstName": "J", "Email": "not mail"},
		{"FirstName": "J", "Dept": "IT"},
		{"FirstName": "J", "Other": "x"},
		{"FirstName": "J\nK"},
		{"FirstName": strings.Repeat("x", MaxManualValue+1)},
	} {
		if _, err := CheckManualAttrs(fields, in); err == nil {
			t.Errorf("%v must be refused", in)
		}
	}
}

func TestManualFetch(t *testing.T) {
	src := model.Source{ID: "s", Name: "Contractors", Type: TypeManual, Config: map[string]any{}}
	NormalizeManual(&src)
	if src.Options.ExternalIDField != ManualIDField || src.Options.StatusField != ManualStatusField || src.Config["id_prefix"] != "C-" {
		t.Fatalf("normalized: %+v", src)
	}
	if err := Validate(src); err != nil {
		t.Fatal(err)
	}
	load := func(context.Context, string) ([]model.ManualPerson, error) {
		return []model.ManualPerson{
			{ExternalID: "C-1", Attrs: map[string]string{"FirstName": "A", "EndDate": "2999-01-01"}},
			{ExternalID: "C-2", Attrs: map[string]string{"FirstName": "B", "EndDate": "2000-01-01"}},
			{ExternalID: "C-3", Ended: true, Attrs: map[string]string{"FirstName": "C"}},
		}, nil
	}
	b, err := BuildFor(src, nil, load)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := b.Fetch(context.Background())
	if err != nil || len(rows) != 3 {
		t.Fatal(rows, err)
	}
	if rows[0]["Status"] != ManualActive || rows[1]["Status"] != ManualTerminated || rows[2]["Status"] != ManualTerminated {
		t.Errorf("status: %v %v %v", rows[0]["Status"], rows[1]["Status"], rows[2]["Status"])
	}
	if _, ok := rows[0]["LastName"]; !ok || rows[0]["ExternalID"] != "C-1" || rows[0]["Source"] != "manual" {
		t.Errorf("row: %+v", rows[0])
	}
	src.Config["end_date_field"] = "Nope"
	if err := Validate(src); err == nil {
		t.Error("an end date field that is not a form field must be refused")
	}
}
