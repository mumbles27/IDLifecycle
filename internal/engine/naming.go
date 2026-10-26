package engine

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"

	"idlifecycle/internal/model"
)

// baseNames are the account names the naming templates produce for one person, before any conflict suffix.
type baseNames struct {
	sam, upn, cn, mail, mailDomain, upnSuffix string
}

func renderBaseNames(s model.Settings, attrs map[string]string, withMail bool) (baseNames, error) {
	n := s.Naming
	b := baseNames{mailDomain: firstNonEmpty(n.MailDomain, s.AD.UPNSuffix), upnSuffix: s.AD.UPNSuffix}
	if withMail && strings.TrimSpace(n.MailTemplate) != "" {
		m, err := Render(n.MailTemplate, attrs)
		if err != nil {
			return b, fmt.Errorf("naming.mail_template: %w", err)
		}
		if b.mail = SanitizeUPNLocal(m); b.mail == "" {
			return b, fmt.Errorf("cannot generate the mail address: naming.mail_template rendered empty; check the source has the attributes it uses")
		}
	}
	sam, err := Render(n.SamTemplate, attrs)
	if err != nil {
		return b, fmt.Errorf("naming.sam_template: %w", err)
	}
	upn, err := Render(n.UPNTemplate, attrs)
	if err != nil {
		return b, fmt.Errorf("naming.upn_template: %w", err)
	}
	cn, err := Render(n.CNTemplate, attrs)
	if err != nil {
		return b, fmt.Errorf("naming.cn_template: %w", err)
	}
	b.sam, b.upn, b.cn = SanitizeSam(sam, 64), SanitizeUPNLocal(upn), SanitizeCN(cn)
	if b.sam == "" || b.upn == "" || b.cn == "" {
		return b, fmt.Errorf("cannot generate account names (sam=%q upn=%q cn=%q): the naming templates rendered empty; check the source has the attributes they use", b.sam, b.upn, b.cn)
	}
	return b, nil
}

// with returns the candidate for one suffix ("" = the base names). The sAMAccountName is shortened so that it
// stays within 20 characters with the suffix; the CN gets " <suffix>".
func (b baseNames) with(suffix string) model.Candidate {
	cnSuffix := ""
	if suffix != "" {
		cnSuffix = " " + suffix
	}
	c := model.Candidate{
		Sam: headRunes(b.sam, 20-len(suffix)) + suffix,
		UPN: b.upn + suffix + "@" + b.upnSuffix,
		CN:  headRunes(b.cn, 64-len(cnSuffix)) + cnSuffix,
	}
	if b.mail != "" {
		c.Mail = b.mail + suffix + "@" + b.mailDomain
	}
	return c
}

// suffixes returns the conflict suffixes a mode tries after the base names (seed keeps random suffixes stable for
// one person, so the same plan is proposed run after run).
func suffixes(s model.Settings, mode string, attrs map[string]string, seed string) ([]string, error) {
	n := s.Naming
	switch mode {
	case model.ConflictNumber:
		var out []string
		for i := 2; i <= n.MaxCandidates; i++ {
			out = append(out, strconv.Itoa(i))
		}
		return out, nil
	case model.ConflictRandom:
		digits := min(max(n.ConflictRandomDigits, 2), 6)
		mod := uint64(1)
		for i := 0; i < digits; i++ {
			mod *= 10
		}
		var out []string
		seen := map[string]bool{}
		for i := 1; len(out) < max(n.MaxCandidates-1, 1) && i < 100; i++ {
			sum := sha256.Sum256([]byte(seed + ":" + strconv.Itoa(i)))
			v := fmt.Sprintf("%0*d", digits, binary.BigEndian.Uint64(sum[:8])%mod)
			if !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
		return out, nil
	case model.ConflictTemplate:
		v, err := Render(n.ConflictSuffix, attrs)
		if err != nil {
			return nil, fmt.Errorf("naming.conflict_suffix_template: %w", err)
		}
		v = alnumOnly(v)
		if v == "" {
			return nil, nil // nothing to append for this person: only the base names (then a person chooses)
		}
		return []string{v}, nil
	}
	return nil, nil // manual
}

func alnumOnly(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 128 && (r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return r
		}
		return -1
	}, stripDiacritics(s))
}

// namingCandidates lists the names to try for a new account, in order, for the configured conflict mode.
func namingCandidates(s model.Settings, attrs map[string]string, withMail bool) ([]model.Candidate, error) {
	return modeCandidates(s, s.Naming.ConflictMode, attrs, "", withMail)
}

func modeCandidates(s model.Settings, mode string, attrs map[string]string, seed string, withMail bool) ([]model.Candidate, error) {
	b, err := renderBaseNames(s, attrs, withMail)
	if err != nil {
		return nil, err
	}
	sfx, err := suffixes(s, mode, attrs, seed)
	if err != nil {
		return nil, err
	}
	out := []model.Candidate{b.with("")}
	for _, x := range sfx {
		out = append(out, b.with(x))
	}
	return out, nil
}

// identityCandidates is namingCandidates for one identity: names a person chose win over the templates.
func identityCandidates(s model.Settings, id model.Identity, withMail bool) ([]model.Candidate, error) {
	if o := id.NameOverride; o != nil && o.Sam != "" {
		c := *o
		if !withMail {
			c.Mail = "" // a mapping owns mail
		}
		return []model.Candidate{c}, nil
	}
	return modeCandidates(s, s.Naming.ConflictMode, id.Attrs, firstNonEmpty(id.ID, id.ExternalID), withMail)
}

// NameSuggestions offers alternative names for a person in every automatic mode (number, random, template), for a
// person choosing a name by hand. The base names come first. Nothing is checked against AD here.
func NameSuggestions(s model.Settings, id model.Identity, withMail bool) []NameSuggestion {
	var out []NameSuggestion
	seen := map[string]bool{}
	for _, mode := range []string{model.ConflictNumber, model.ConflictRandom, model.ConflictTemplate} {
		if mode == model.ConflictTemplate && strings.TrimSpace(s.Naming.ConflictSuffix) == "" {
			continue
		}
		cands, err := modeCandidates(s, mode, id.Attrs, firstNonEmpty(id.ID, id.ExternalID), withMail)
		if err != nil {
			continue
		}
		for _, c := range cands {
			if k := strings.ToLower(c.Sam); !seen[k] {
				seen[k] = true
				out = append(out, NameSuggestion{Mode: mode, Candidate: c})
			}
		}
	}
	return out
}

// NameSuggestion is one alternative name and the mode that produces it.
type NameSuggestion struct {
	Mode      string          `json:"mode"`
	Candidate model.Candidate `json:"candidate"`
}

// CandidateQueries lists the AD look-ups for a candidate (ou "" skips the CN, whose uniqueness is per OU).
func CandidateQueries(c model.Candidate, ou string) []nameKey {
	keys := []nameKey{{"sam", c.Sam, ""}, {"upn", c.UPN, ""}}
	if c.Mail != "" {
		keys = append(keys, nameKey{"mail", c.Mail, ""})
	}
	if ou != "" {
		keys = append(keys, nameKey{"cn", c.CN, ou})
	}
	return keys
}

// nameKey identifies one name; CN keys include the OU.
type nameKey struct{ field, value, ou string }

func (k nameKey) id() string {
	return k.field + "\x00" + strings.ToLower(k.value) + "\x00" + strings.ToLower(k.ou)
}

var samCheck = func(s string) bool {
	if s == "" || len([]rune(s)) > 20 || strings.HasSuffix(s, ".") {
		return false
	}
	return SanitizeSam(s, 20) == s
}

// ValidateCandidate checks names typed by a person (format only; AD is asked separately).
func ValidateCandidate(c model.Candidate, upnSuffix string, needMail bool) error {
	if !samCheck(c.Sam) {
		return fmt.Errorf("sAMAccountName %q: 1-20 characters, no spaces or \" / \\ [ ] : ; | = , + * ? < > @, not ending with a dot", c.Sam)
	}
	local, domain, ok := strings.Cut(c.UPN, "@")
	if !ok || local == "" || SanitizeUPNLocal(local) != local || !strings.EqualFold(domain, upnSuffix) {
		return fmt.Errorf("UPN %q must be name@%s (letters, digits, . _ - before the @)", c.UPN, upnSuffix)
	}
	if strings.TrimSpace(c.CN) == "" || SanitizeCN(c.CN) != c.CN {
		return fmt.Errorf("the display name (CN) must not be empty, at most 64 characters, without control characters or surrounding spaces")
	}
	if c.Mail == "" {
		if needMail {
			return fmt.Errorf("the mail address is required (naming.mail_template is set)")
		}
		return nil
	}
	ml, md, ok := strings.Cut(c.Mail, "@")
	if !ok || ml == "" || SanitizeUPNLocal(ml) != ml || md == "" || strings.ContainsAny(md, " @") {
		return fmt.Errorf("mail %q is not an address like jane.doe@corp.example.com", c.Mail)
	}
	return nil
}
