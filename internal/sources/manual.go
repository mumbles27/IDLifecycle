package sources

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"idlifecycle/internal/model"
	"idlifecycle/internal/secrets"
)

// TypeManual is the source of people typed into the console (contractors, out-of-band requests).
const TypeManual = "manual"

// Fields every manual row carries in addition to the form fields; a form cannot define them.
const (
	ManualIDField     = "ExternalID"
	ManualStatusField = "Status"
	ManualActive      = "Active"
	ManualTerminated  = "Terminated"
)

// DefaultManualFields is the form a new manual source starts with.
const DefaultManualFields = `FirstName | First name | required
LastName | Last name | required
Department | Department | required
Title | Job title
Company | Company
Manager | Manager (employee ID)
Email | Contact e-mail | email
StartDate | Start date | date
EndDate | End date | date
Notes | Notes`

// MaxManualValue caps one typed value (characters).
const MaxManualValue = 256

// FormField is one input of a manual source's form.
type FormField struct {
	Name     string   `json:"name"`
	Label    string   `json:"label"`
	Required bool     `json:"required"`
	Kind     string   `json:"kind"` // text | date | email | select
	Options  []string `json:"options,omitempty"`
	Default  string   `json:"default,omitempty"`
}

func manualType() Type {
	return Type{
		Name:  TypeManual,
		Label: "Manual entry (people added in the console)",
		Fields: []Field{
			{Name: "id_prefix", Label: "ID prefix", Type: "string", Default: "C-", Help: "New people get this prefix plus 6 random letters/digits as their ID (written to the AD id attribute), e.g. C-7K2Q9D."},
			{Name: "form_fields", Label: "Form fields", Type: "text", Default: DefaultManualFields,
				Help: "One field per line: Name | Label | flags. Flags (comma-separated): required, date, email, select: A; B; C, default: value. Use the same names as your HR source (FirstName, LastName, Department...) so mappings, roles and OU placement apply."},
			{Name: "end_date_field", Label: "End date field", Type: "string", Default: "EndDate", Help: "When this date has passed, the person counts as terminated (their account is disabled through the normal approval flow). Empty = no end date."},
		},
		New: func(map[string]any) (Source, error) {
			return nil, errors.New("manual: people are read from the console's store (BuildFor)")
		},
	}
}

// PeopleLoader returns a manual source's people.
type PeopleLoader func(ctx context.Context, sourceID string) ([]model.ManualPerson, error)

// BuildFor is Build for every source type, including manual sources (whose people come from load).
func BuildFor(src model.Source, c *secrets.Cipher, load PeopleLoader) (Source, error) {
	if src.Type != TypeManual {
		return Build(src, c)
	}
	if load == nil {
		return nil, errors.New("manual: no people store")
	}
	fields, err := ParseFormFields(cfgString(src.Config, "form_fields", DefaultManualFields))
	if err != nil {
		return nil, err
	}
	return &manualSource{id: src.ID, endField: cfgString(src.Config, "end_date_field", "EndDate"), fields: fields, load: load, now: time.Now}, nil
}

type manualSource struct {
	id       string
	endField string
	fields   []FormField
	load     PeopleLoader
	now      func() time.Time
}

func (m *manualSource) Fetch(ctx context.Context) ([]Row, error) {
	people, err := m.load(ctx, m.id)
	if err != nil {
		return nil, err
	}
	today := m.now().Format(time.DateOnly)
	rows := make([]Row, 0, len(people))
	for _, p := range people {
		r := Row{}
		for _, f := range m.fields { // every form field is present (empty when not typed) so templates see it
			r[f.Name] = ""
		}
		for k, v := range p.Attrs {
			r[k] = v
		}
		r[ManualIDField] = p.ExternalID
		r["Source"] = TypeManual
		r[ManualStatusField] = ManualActive
		if p.Ended || PastEndDate(r[m.endField], today) {
			r[ManualStatusField] = ManualTerminated
		}
		rows = append(rows, r)
	}
	return rows, nil
}

// PastEndDate reports whether end (YYYY-MM-DD) is before today (YYYY-MM-DD). Anything else is not an end date.
func PastEndDate(end, today string) bool {
	end = strings.TrimSpace(end)
	if _, err := time.Parse(time.DateOnly, end); err != nil {
		return false
	}
	return end < today
}

// NormalizeManual fixes the options of a manual source (its rows always use these fields) and fills config defaults.
func NormalizeManual(src *model.Source) {
	if src.Type != TypeManual {
		return
	}
	src.Options.ExternalIDField = ManualIDField
	src.Options.StatusField = ManualStatusField
	src.Options.TerminatedValues = []string{ManualTerminated}
	// A person removed from the list is gone on purpose; ending them is the normal way, deleting is only allowed
	// before an account exists.
	src.Options.MissingMeansTerminated = true
	src.Options.MissingGraceRuns = 0
	if src.Config == nil {
		src.Config = map[string]any{}
	}
	for _, f := range manualType().Fields {
		if _, ok := src.Config[f.Name]; !ok {
			src.Config[f.Name] = f.Default
		}
	}
}

func validateManualConfig(cfg map[string]any) error {
	p := strings.TrimSpace(asString(cfg["id_prefix"]))
	if len(p) > 10 || strings.ContainsAny(p, " \t\r\n\"'<>&;,") {
		return fmt.Errorf("config.id_prefix must be at most 10 characters without spaces or punctuation such as ; , \" '")
	}
	fields, err := ParseFormFields(cfgString(cfg, "form_fields", DefaultManualFields))
	if err != nil {
		return err
	}
	if end := strings.TrimSpace(asString(cfg["end_date_field"])); end != "" {
		for _, f := range fields {
			if strings.EqualFold(f.Name, end) {
				return nil
			}
		}
		return fmt.Errorf("config.end_date_field %q is not one of the form fields", end)
	}
	return nil
}

// ParseFormFields reads the "Name | Label | flags" lines of a manual source.
func ParseFormFields(text string) ([]FormField, error) {
	var out []FormField
	seen := map[string]bool{}
	for i, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "|")
		f := FormField{Name: strings.TrimSpace(parts[0]), Kind: "text"}
		if !fieldNameRe(f.Name) {
			return nil, fmt.Errorf("form_fields line %d: %q is not a field name (letters, digits, _ and ., starting with a letter)", i+1, f.Name)
		}
		switch strings.ToLower(f.Name) {
		case strings.ToLower(ManualIDField), strings.ToLower(ManualStatusField), "source":
			return nil, fmt.Errorf("form_fields line %d: %s is set by the service and cannot be a form field", i+1, f.Name)
		}
		if seen[strings.ToLower(f.Name)] {
			return nil, fmt.Errorf("form_fields line %d: %s is listed twice", i+1, f.Name)
		}
		seen[strings.ToLower(f.Name)] = true
		f.Label = f.Name
		if len(parts) > 1 && strings.TrimSpace(parts[1]) != "" {
			f.Label = strings.TrimSpace(parts[1])
		}
		if len(parts) > 2 {
			for _, flag := range strings.Split(strings.Join(parts[2:], "|"), ",") {
				flag = strings.TrimSpace(flag)
				low := strings.ToLower(flag)
				switch {
				case flag == "":
				case low == "required":
					f.Required = true
				case low == "date", low == "email", low == "text":
					f.Kind = low
				case strings.HasPrefix(low, "select:"):
					f.Kind = "select"
					for _, o := range strings.Split(flag[len("select:"):], ";") {
						if o = strings.TrimSpace(o); o != "" {
							f.Options = append(f.Options, o)
						}
					}
					if len(f.Options) == 0 {
						return nil, fmt.Errorf("form_fields line %d: select needs options, e.g. select: Finance; Sales", i+1)
					}
				case strings.HasPrefix(low, "default:"):
					f.Default = strings.TrimSpace(flag[len("default:"):])
				default:
					return nil, fmt.Errorf("form_fields line %d: unknown flag %q (use required, date, email, select: A; B, default: value)", i+1, flag)
				}
			}
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil, errors.New("form_fields: define at least one field")
	}
	if len(out) > 50 {
		return nil, errors.New("form_fields: at most 50 fields")
	}
	return out, nil
}

func fieldNameRe(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case i > 0 && (r >= '0' && r <= '9' || r == '_' || r == '.'):
		default:
			return false
		}
	}
	return true
}

// CheckManualAttrs validates typed values against the form: unknown fields are refused, required ones must be set,
// dates are YYYY-MM-DD, e-mails valid, selects one of the options. It returns the cleaned values (trimmed, every
// form field present).
func CheckManualAttrs(fields []FormField, in map[string]string) (map[string]string, error) {
	byName := map[string]FormField{}
	for _, f := range fields {
		byName[strings.ToLower(f.Name)] = f
	}
	for k := range in {
		if _, ok := byName[strings.ToLower(k)]; !ok {
			return nil, fmt.Errorf("%s is not a field of this form", k)
		}
	}
	out := make(map[string]string, len(fields))
	for _, f := range fields {
		v := ""
		for k, x := range in {
			if strings.EqualFold(k, f.Name) {
				v = strings.TrimSpace(x)
			}
		}
		if strings.ContainsAny(v, "\r\n\x00") {
			return nil, fmt.Errorf("%s: line breaks are not allowed", f.Label)
		}
		if len([]rune(v)) > MaxManualValue {
			return nil, fmt.Errorf("%s: at most %d characters", f.Label, MaxManualValue)
		}
		if v == "" {
			if f.Required {
				return nil, fmt.Errorf("%s is required", f.Label)
			}
			out[f.Name] = ""
			continue
		}
		switch f.Kind {
		case "date":
			if _, err := time.Parse(time.DateOnly, v); err != nil {
				return nil, fmt.Errorf("%s must be a date like 2026-12-31", f.Label)
			}
		case "email":
			if a, err := mail.ParseAddress(v); err != nil || a.Address != v {
				return nil, fmt.Errorf("%s must be an e-mail address", f.Label)
			}
		case "select":
			ok := false
			for _, o := range f.Options {
				if strings.EqualFold(o, v) {
					v, ok = o, true
				}
			}
			if !ok {
				return nil, fmt.Errorf("%s must be one of: %s", f.Label, strings.Join(f.Options, ", "))
			}
		}
		out[f.Name] = v
	}
	return out, nil
}
