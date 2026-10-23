// Package engine holds the sync logic: templating, role resolution, planning, approvals, execution.
package engine

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"text/template"
	"text/template/parse"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

const maxTemplateOutput = 16 << 10

var funcs = template.FuncMap{
	"lower":   strings.ToLower,
	"upper":   strings.ToUpper,
	"trim":    strings.TrimSpace,
	"title":   titleCase,
	"first":   func(s string, n int) string { return headRunes(s, n) },
	"last":    func(s string, n int) string { return tailRunes(s, n) },
	"substr":  substr,
	"replace": func(s, old, repl string) string { return strings.ReplaceAll(s, old, repl) },
	"default": func(fallback, v string) string {
		if strings.TrimSpace(v) == "" {
			return fallback
		}
		return v
	},
	"coalesce": func(vals ...string) string {
		for _, v := range vals {
			if strings.TrimSpace(v) != "" {
				return v
			}
		}
		return ""
	},
	"join":   func(sep string, vals ...string) string { return strings.Join(nonEmpty(vals), sep) },
	"ascii":  stripDiacritics,
	"digits": func(s string) string { return keepRunes(s, unicode.IsDigit) },
	"alnum": func(s string) string {
		return keepRunes(stripDiacritics(s), func(r rune) bool { return r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) })
	},
	"split": strings.Split,
}

func nonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

func headRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if n >= len(r) {
		return s
	}
	return string(r[:n])
}

func tailRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if n >= len(r) {
		return s
	}
	return string(r[len(r)-n:])
}

func substr(s string, start, length int) string {
	r := []rune(s)
	if start < 0 {
		start = 0
	}
	if start >= len(r) || length <= 0 {
		return ""
	}
	end := start + length
	if end > len(r) {
		end = len(r)
	}
	return string(r[start:end])
}

func titleCase(s string) string {
	var b strings.Builder
	up := true
	for _, r := range strings.ToLower(s) {
		if up && unicode.IsLetter(r) {
			b.WriteRune(unicode.ToUpper(r))
			up = false
			continue
		}
		b.WriteRune(r)
		up = !unicode.IsLetter(r) && r != '\''
	}
	return b.String()
}

func keepRunes(s string, keep func(rune) bool) string {
	var b strings.Builder
	for _, r := range s {
		if keep(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

var diacritics = transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)

func stripDiacritics(s string) string {
	out, _, err := transform.String(diacritics, s)
	if err != nil {
		return s
	}
	return out
}

const maxCacheEntries = 2000

var (
	tmplCache sync.Map // string -> *template.Template (bounded: sources of templates include user-typed previews)
	tmplCount atomic.Int64
)

// parseTemplate parses and validates a template, rejecting constructs that could loop or recurse.
func parseTemplate(src string) (*template.Template, error) {
	if v, ok := tmplCache.Load(src); ok {
		return v.(*template.Template), nil
	}
	t, err := template.New("t").Funcs(funcs).Option("missingkey=zero").Parse(src)
	if err != nil {
		return nil, err
	}
	if len(t.Templates()) > 1 {
		return nil, errors.New("define/block are not allowed in templates")
	}
	if t.Tree != nil && t.Tree.Root != nil {
		if err := vet(t.Tree.Root); err != nil {
			return nil, err
		}
	}
	if tmplCount.Add(1) <= maxCacheEntries {
		tmplCache.Store(src, t)
	}
	return t, nil
}

func vet(n parse.Node) error {
	switch x := n.(type) {
	case *parse.ListNode:
		if x == nil {
			return nil
		}
		for _, c := range x.Nodes {
			if err := vet(c); err != nil {
				return err
			}
		}
	case *parse.RangeNode:
		return errors.New("range is not allowed in templates")
	case *parse.TemplateNode:
		return errors.New("template calls are not allowed in templates")
	case *parse.IfNode:
		if err := vet(x.List); err != nil {
			return err
		}
		if x.ElseList != nil {
			return vet(x.ElseList)
		}
	case *parse.WithNode:
		if err := vet(x.List); err != nil {
			return err
		}
		if x.ElseList != nil {
			return vet(x.ElseList)
		}
	}
	return nil
}

type limitedWriter struct {
	buf bytes.Buffer
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > maxTemplateOutput {
		return 0, errors.New("template output too large")
	}
	return w.buf.Write(p)
}

// Render evaluates src against the source attributes. Missing keys render as "".
func Render(src string, data map[string]string) (string, error) {
	t, err := parseTemplate(src)
	if err != nil {
		return "", fmt.Errorf("template: %w", err)
	}
	// text/template indexes map[string]string with missingkey=zero giving "" for absent keys.
	var w limitedWriter
	if err := t.Execute(&w, data); err != nil {
		return "", fmt.Errorf("template: %w", err)
	}
	return strings.TrimSpace(w.buf.String()), nil
}

// ValidateTemplate checks syntax without data.
func ValidateTemplate(src string) error {
	_, err := parseTemplate(src)
	if err != nil {
		return fmt.Errorf("template: %w", err)
	}
	return nil
}

// ---- sanitizers for directory naming ----------------------------------------

var samInvalid = regexp.MustCompile(`["/\\\[\]:;|=,+*?<>@\s]`)

// SanitizeSam produces a legal sAMAccountName (<= max chars, no illegal chars, no trailing dot).
func SanitizeSam(s string, max int) string {
	s = samInvalid.ReplaceAllString(stripDiacritics(s), "")
	s = strings.TrimRight(s, ".")
	return headRunes(s, max)
}

var upnInvalid = regexp.MustCompile(`[^A-Za-z0-9._\-]`)

// SanitizeUPNLocal keeps only characters that are safe in a UPN prefix.
func SanitizeUPNLocal(s string) string {
	s = upnInvalid.ReplaceAllString(stripDiacritics(s), "")
	return strings.Trim(s, ".")
}

// SanitizeCN removes control characters and surrounding whitespace; DN escaping is the script's job.
func SanitizeCN(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > 64 {
		s = headRunes(s, 64)
	}
	return strings.TrimSpace(s)
}
