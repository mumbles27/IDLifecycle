// Package sources fetches people from HR systems and normalizes them to flat string maps.
package sources

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"idlifecycle/internal/model"
	"idlifecycle/internal/secrets"
)

// Row is one person as a flat attribute map. Nested API payloads are flattened to dotted keys.
type Row map[string]string

// Source fetches the complete current population from one system.
type Source interface {
	Fetch(ctx context.Context) ([]Row, error)
}

// Field describes one config input so the GUI can render forms generically.
type Field struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Type     string `json:"type"` // string | password | int | bool | text
	Required bool   `json:"required"`
	Default  any    `json:"default,omitempty"`
	Help     string `json:"help,omitempty"`
}

type Type struct {
	Name   string                                   `json:"type"`
	Label  string                                   `json:"label"`
	Fields []Field                                  `json:"fields"`
	New    func(cfg map[string]any) (Source, error) `json:"-"`
}

var registry = map[string]Type{}

func register(t Type) { registry[t.Name] = t }

func init() {
	register(csvType())
	register(paylocityType())
	register(sqlType())
	register(manualType())
}

// Types lists registered source types sorted by name (New funcs are not serialized).
func Types() []Type {
	out := make([]Type, 0, len(registry))
	for _, t := range registry {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func Lookup(name string) (Type, bool) {
	t, ok := registry[name]
	return t, ok
}

// Validate checks a source definition before it is stored.
func Validate(src model.Source) error {
	t, ok := Lookup(src.Type)
	if !ok {
		return fmt.Errorf("unknown source type %q", src.Type)
	}
	if strings.TrimSpace(src.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if strings.TrimSpace(src.Options.ExternalIDField) == "" {
		return fmt.Errorf("options.external_id_field is required")
	}
	if src.Options.MissingGraceRuns < 0 {
		return fmt.Errorf("options.missing_grace_runs must be >= 0")
	}
	for _, f := range t.Fields {
		if f.Required && strings.TrimSpace(asString(src.Config[f.Name])) == "" {
			return fmt.Errorf("config.%s is required", f.Name)
		}
	}
	if src.Type == "csv" && PathPolicy != nil {
		if err := PathPolicy(asString(src.Config["path"])); err != nil {
			return fmt.Errorf("config.path: %w", err)
		}
	}
	if src.Type == "sql" {
		if err := validateSQLConfig(src.Config); err != nil {
			return err
		}
	}
	if src.Type == TypeManual {
		if err := validateManualConfig(src.Config); err != nil {
			return err
		}
	}
	return nil
}

// Build decrypts secrets and instantiates the source.
func Build(src model.Source, c *secrets.Cipher) (Source, error) {
	t, ok := Lookup(src.Type)
	if !ok {
		return nil, fmt.Errorf("unknown source type %q", src.Type)
	}
	cfg, err := DecryptConfig(src.Type, src.Config, c)
	if err != nil {
		return nil, err
	}
	return t.New(cfg)
}

func passwordFields(typeName string) map[string]bool {
	out := map[string]bool{}
	if t, ok := Lookup(typeName); ok {
		for _, f := range t.Fields {
			if f.Type == "password" {
				out[f.Name] = true
			}
		}
	}
	return out
}

// PrepareConfigForStorage encrypts password fields. A masked or missing value keeps the previously
// stored (encrypted) value so the GUI can round-trip a source without exposing its secret.
func PrepareConfigForStorage(typeName string, in, previous map[string]any, c *secrets.Cipher) (map[string]any, error) {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	for name := range passwordFields(typeName) {
		v := asString(out[name])
		switch {
		case v == model.MaskedSecret || (v == "" && previous != nil && asString(previous[name]) != ""):
			if prev, ok := previous[name]; ok {
				out[name] = prev
			} else {
				delete(out, name)
			}
		case v == "":
			delete(out, name)
		case secrets.IsEncrypted(v):
			return nil, fmt.Errorf("config.%s looks like an encrypted value; send the plain secret or %q", name, model.MaskedSecret)
		default:
			enc, err := c.Encrypt(v)
			if err != nil {
				return nil, err
			}
			out[name] = enc
		}
	}
	return out, nil
}

// EndpointChangedWithoutSecret reports an attempt to redirect a stored secret: base_url changed while
// a password field was left masked/empty (so the old ciphertext would be kept and sent to the new host).
func EndpointChangedWithoutSecret(typeName string, prev, in map[string]any) bool {
	if strings.TrimRight(strings.TrimSpace(asString(prev["base_url"])), "/") == strings.TrimRight(strings.TrimSpace(asString(in["base_url"])), "/") {
		return false
	}
	for name := range passwordFields(typeName) {
		if v := asString(in[name]); v == "" || v == model.MaskedSecret {
			return true
		}
	}
	return false
}

// MaskConfig returns a copy safe to send to the browser.
func MaskConfig(typeName string, cfg map[string]any) map[string]any {
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		out[k] = v
	}
	for name := range passwordFields(typeName) {
		if asString(out[name]) != "" {
			out[name] = model.MaskedSecret
		}
	}
	return out
}

func DecryptConfig(typeName string, cfg map[string]any, c *secrets.Cipher) (map[string]any, error) {
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		out[k] = v
	}
	for name := range passwordFields(typeName) {
		v := asString(out[name])
		if v == "" {
			continue
		}
		if !secrets.IsEncrypted(v) {
			return nil, fmt.Errorf("config.%s is stored unencrypted", name)
		}
		plain, err := c.Decrypt(v)
		if err != nil {
			return nil, fmt.Errorf("config.%s: %w", name, err)
		}
		out[name] = plain
	}
	return out, nil
}

// ---- config value helpers --------------------------------------------------

func asString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case bool:
		return strconv.FormatBool(x)
	}
	return fmt.Sprint(v)
}

func cfgString(cfg map[string]any, key, def string) string {
	if s := strings.TrimSpace(asString(cfg[key])); s != "" {
		return s
	}
	return def
}

func cfgInt(cfg map[string]any, key string, def int) int {
	switch x := cfg[key].(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(x)); err == nil {
			return n
		}
	}
	return def
}

func cfgBool(cfg map[string]any, key string) bool {
	switch x := cfg[key].(type) {
	case bool:
		return x
	case string:
		b, _ := strconv.ParseBool(x)
		return b
	}
	return false
}
