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
	// Regex helpers take the value LAST so they work in a pipe: {{ .Phone | regexReplace "[^0-9]" "" }}.
	// Go regexps (RE2) run in linear time, so a pattern cannot hang a run.
	"regexMatch": func(pattern, s string) (bool, error) {
		re, err := compileRegex(pattern)
		if err != nil {
			return false, err
		}
		return re.MatchString(s), nil
	},
	"regexFind": func(pattern, s string) (string, error) {
		re, err := compileRegex(pattern)
		if err != nil {
			return "", err
		}
		m := re.FindStringSubmatch(s)
		switch {
		case m == nil:
			return "", nil
		case len(m) > 1:
			return m[1], nil // the first capture group when the pattern has one
		default:
			return m[0], nil
		}
	},
	"regexReplace": func(pattern, repl, s string) (string, error) {
		re, err := compileRegex(pattern)
		if err != nil {
			return "", err
		}
		return re.ReplaceAllString(s, repl), nil
	},
}

// passwordFuncs are funcs plus random characters, for the initial-password template only: account names and
// attributes must render the same on every run, a password must not.
var passwordFuncs = func() template.FuncMap {
	m := template.FuncMap{}
	for k, v := range funcs {
		m[k] = v
	}
	m["random"] = func(n int) (string, error) { return randomChars(pwUpper+pwLower+pwDigits, n) }
	m["randomDigits"] = func(n int) (string, error) { return randomChars(pwDigits, n) }
	m["randomSymbols"] = func(n int) (string, error) { return randomChars(pwSymbol, n) }
	return m
}()

const maxRegexLen = 256

var (
	regexCache sync.Map // pattern -> *regexp.Regexp (bounded like the template caches)
	regexCount atomic.Int64
)

func compileRegex(pattern string) (*regexp.Regexp, error) {
	if v, ok := regexCache.Load(pattern); ok {
		return v.(*regexp.Regexp), nil
	}
	if len(pattern) > maxRegexLen {
		return nil, fmt.Errorf("regex longer than %d characters", maxRegexLen)
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("regex %q: %w", pattern, err)
	}
	if regexCount.Add(1) <= maxCacheEntries {
		regexCache.Store(pattern, re)
	}
	return re, nil
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

// templateSet is a function map with its own parse cache (a source parsed with one map must not be served
// from the cache to the other).
type templateSet struct {
	funcs template.FuncMap
	cache sync.Map // string -> *template.Template (bounded: sources of templates include user-typed previews)
	count atomic.Int64
}

var (
	plainTemplates    = &templateSet{funcs: funcs}
	passwordTemplates = &templateSet{funcs: passwordFuncs}
)

// parseTemplate parses and validates a template, rejecting constructs that could loop or recurse.
func parseTemplate(src string) (*template.Template, error) {
	return plainTemplates.parse(src)
}

func (ts *templateSet) parse(src string) (*template.Template, error) {
	if v, ok := ts.cache.Load(src); ok {
		return v.(*template.Template), nil
	}
	t, err := template.New("t").Funcs(ts.funcs).Option("missingkey=zero").Parse(src)
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
	if ts.count.Add(1) <= maxCacheEntries {
		ts.cache.Store(src, t)
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
	return plainTemplates.render(src, data)
}

// RenderPassword evaluates an initial-password template (Render's functions plus random, randomDigits and
// randomSymbols). The result is a secret: callers never store or log it.
func RenderPassword(src string, data map[string]string) (string, error) {
	return passwordTemplates.render(src, data)
}

func (ts *templateSet) render(src string, data map[string]string) (string, error) {
	t, err := ts.parse(src)
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

// ValidatePasswordTemplate checks an initial-password template's syntax without data.
func ValidatePasswordTemplate(src string) error {
	if _, err := passwordTemplates.parse(src); err != nil {
		return fmt.Errorf("template: %w", err)
	}
	return nil
}

// MinInitialPassword is the shortest initial password a template may produce.
const MinInitialPassword = 8

// CheckInitialPassword reports why AD's default complexity policy would refuse pw, or nil. It needs 8-128
// characters, 3 of the 4 character classes, and must not contain the account name (sam, when 3+ characters).
// The error never contains the password.
func CheckInitialPassword(pw, sam string) error {
	n := utf8.RuneCountInString(pw)
	if n < MinInitialPassword {
		return fmt.Errorf("the initial password is %d characters; at least %d are needed", n, MinInitialPassword)
	}
	if n > 128 {
		return fmt.Errorf("the initial password is longer than 128 characters")
	}
	var up, low, dig, other bool
	for _, r := range pw {
		switch {
		case unicode.IsUpper(r):
			up = true
		case unicode.IsLower(r):
			low = true
		case unicode.IsDigit(r):
			dig = true
		default:
			other = true
		}
	}
	classes := 0
	for _, b := range []bool{up, low, dig, other} {
		if b {
			classes++
		}
	}
	if classes < 3 {
		return fmt.Errorf("the initial password uses %d of the 4 character kinds (uppercase, lowercase, digits, symbols); AD needs 3", classes)
	}
	if utf8.RuneCountInString(sam) >= 3 && strings.Contains(strings.ToLower(pw), strings.ToLower(sam)) {
		return fmt.Errorf("the initial password contains the account name, which AD refuses")
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
