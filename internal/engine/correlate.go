package engine

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/model"
)

// Correlation: a person without an AD account (by id attribute) may already have one - from before this service
// (first rollout: the account has no id yet), as a contractor in another source, or under a former record of the same
// source (a rehire with a new id). The rules in settings.correlation compare source fields with AD attributes.

// CompareValues applies one correlation compare kind. Empty values never match.
func CompareValues(kind, a, b string) bool {
	a, b = normCompare(kind, a), normCompare(kind, b)
	return a != "" && a == b
}

func normCompare(kind, v string) string {
	switch kind {
	case model.CompareExact:
		return strings.TrimSpace(v)
	case model.CompareName:
		return strings.Map(func(r rune) rune {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				return unicode.ToLower(r)
			}
			return -1
		}, stripDiacritics(v))
	default: // ci
		return strings.ToLower(strings.Join(strings.Fields(v), " "))
	}
}

// IgnoredAccount reports whether settings.correlation.ignore_accounts lists the account: a sAMAccountName wildcard
// (svc_*) or, when the entry contains "=", a DN suffix (OU=Service Accounts,DC=corp,DC=example,DC=com).
func IgnoredAccount(patterns []string, sam, dn string) bool {
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		switch {
		case p == "":
		case strings.Contains(p, "="):
			d, b := strings.ToLower(dn), strings.ToLower(p)
			if d == b || strings.HasSuffix(d, ","+b) {
				return true
			}
		case wildcard(p).MatchString(sam):
			return true
		}
	}
	return false
}

// corrAccount is an AD account correlation may hand to a person, with the identity it belongs to now (if any).
type corrAccount struct {
	u     *ad.User
	owner *model.Identity
}

// correlator finds candidate accounts for people without one.
type correlator struct {
	s        model.Settings
	accounts []corrAccount
	byGUID   map[string]*corrAccount
}

// newCorrelator indexes the snapshot. owners maps an id attribute value to the identity with that external id.
func newCorrelator(s model.Settings, users []ad.User, owners map[string]*model.Identity) *correlator {
	c := &correlator{s: s, byGUID: map[string]*corrAccount{}}
	for i := range users {
		u := &users[i]
		a := corrAccount{u: u}
		if u.ExternalID != "" {
			if o := owners[u.ExternalID]; o != nil && o.State != model.StateMerged {
				a.owner = o
			}
		}
		c.accounts = append(c.accounts, a)
	}
	for i := range c.accounts {
		c.byGUID[strings.ToLower(c.accounts[i].u.ObjectGUID)] = &c.accounts[i]
	}
	return c
}

// Privileged reports whether AD protects the account (adminCount = 1: a member of an administrative group, now
// or before). Such accounts are never matched to people: linking would let HR data rename, regroup or disable them.
func Privileged(u *ad.User) bool { return strings.TrimSpace(u.Attr("adminCount")) == "1" }

// usable reports whether an account may be handed to id at all.
func (c *correlator) usable(id model.Identity, a *corrAccount) bool {
	if Privileged(a.u) {
		return false
	}
	if a.owner == nil {
		return true
	}
	if a.owner.ID == id.ID {
		return false
	}
	// Another active record of the same source is a different person; an inactive one may be a rehire.
	return a.owner.SourceID != id.SourceID || !a.owner.Active
}

func (c *correlator) candidate(id model.Identity, a *corrAccount) model.LinkCandidate {
	u := a.u
	lc := model.LinkCandidate{ObjectGUID: u.ObjectGUID, DN: u.DN, Sam: u.Sam, UPN: u.UPN, Enabled: u.Enabled, ExternalID: u.ExternalID, Fields: []model.FieldMatch{}}
	if a.owner != nil {
		lc.IdentityID, lc.IdentityDisplay, lc.SourceName = a.owner.ID, a.owner.Display, a.owner.SourceName
	}
	full := len(c.s.Correlate.Rules) > 0
	for _, r := range c.s.Correlate.Rules {
		fm := model.FieldMatch{Field: r.Field, ADAttr: r.ADAttr, Value: fieldValue(id.Attrs, r.Field), ADValue: u.Attr(r.ADAttr)}
		fm.Matched = CompareValues(r.Compare, fm.Value, fm.ADValue)
		full = full && fm.Matched
		lc.Fields = append(lc.Fields, fm)
	}
	lc.Full = full
	return lc
}

// requiredOK reports whether every required rule matched.
func (c *correlator) requiredOK(lc model.LinkCandidate) bool {
	for i, r := range c.s.Correlate.Rules {
		if r.Required && !lc.Fields[i].Matched {
			return false
		}
	}
	return true
}

// find returns the link proposal for one person, or nil when no account qualifies (then a create is planned).
func (c *correlator) find(id model.Identity) (*model.LinkPayload, []string) {
	var warns []string
	if guid, ok := strings.CutPrefix(id.LinkDecision, model.LinkDecisionGUID); ok {
		if a := c.byGUID[strings.ToLower(guid)]; a != nil && c.usable(id, a) {
			lc := c.candidate(id, a)
			lc.Manual = true
			return &model.LinkPayload{Candidates: []model.LinkCandidate{lc}, Chosen: lc.ObjectGUID}, nil
		}
		warns = append(warns, fmt.Sprintf("the AD account chosen for this person (%s) is not in the search bases or belongs to someone else now; the matching rules are used instead", guid))
	}
	rejected := map[string]bool{}
	for _, g := range id.LinkRejects {
		rejected[strings.ToLower(g)] = true
	}
	var cands []model.LinkCandidate
	for i := range c.accounts {
		a := &c.accounts[i]
		if rejected[strings.ToLower(a.u.ObjectGUID)] || !c.usable(id, a) || IgnoredAccount(c.s.Correlate.IgnoreAccounts, a.u.Sam, a.u.DN) {
			continue
		}
		lc := c.candidate(id, a)
		if c.requiredOK(lc) {
			cands = append(cands, lc)
		}
	}
	if len(cands) == 0 {
		return nil, warns
	}
	// Full matches first, then accounts without an id (the usual first-rollout case), then by name.
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].Full != cands[j].Full {
			return cands[i].Full
		}
		if mi, mj := matchedCount(cands[i]), matchedCount(cands[j]); mi != mj {
			return mi > mj
		}
		return strings.ToLower(cands[i].Sam) < strings.ToLower(cands[j].Sam)
	})
	if len(cands) > 10 {
		warns = append(warns, fmt.Sprintf("%d accounts match the required correlation rules; only the best 10 are listed (add rules to narrow it down)", len(cands)))
		cands = cands[:10]
	}
	lp := &model.LinkPayload{Candidates: cands}
	full := 0
	for _, x := range cands {
		if x.Full {
			full++
		}
	}
	switch {
	case full == 1 && len(cands) == 1:
		lp.Chosen = cands[0].ObjectGUID
	case full == 1:
		lp.Chosen, lp.Verify = cands[0].ObjectGUID, true // the full match is the obvious pick, but others are close
	case len(cands) == 1:
		lp.Chosen, lp.Verify = cands[0].ObjectGUID, true // partial match: a person confirms
	default:
		lp.Verify = true // several: a person chooses
	}
	return lp, warns
}

func matchedCount(c model.LinkCandidate) int {
	n := 0
	for _, f := range c.Fields {
		if f.Matched {
			n++
		}
	}
	return n
}

// planLink proposes handing an existing account to id. renames are the names to try when the account is renamed.
func planLink(pc *PlanContext, id model.Identity, maps []effective, lp *model.LinkPayload) (*Planned, error) {
	p := &Planned{Type: model.ChangeLink, Payload: model.ChangePayload{Link: lp}}
	if ch, ok := lp.ChosenCandidate(); ok {
		p.Payload.ObjectGUID, p.Payload.DN = ch.ObjectGUID, ch.DN
		if wantRename(pc.Settings, ch) {
			mailMapped := false
			for _, m := range maps {
				if strings.EqualFold(m.Attr, "mail") {
					mailMapped = true
				}
			}
			cands, err := identityCandidates(pc.Settings, id, !mailMapped)
			if err != nil {
				return nil, err
			}
			if !alreadyNamed(ch, cands) {
				p.renames = cands
			}
		}
	}
	p.Summary = LinkSummary(p.Payload, pc.Settings.AD.IDAttribute, id.ExternalID)
	return p, nil
}

func wantRename(s model.Settings, ch model.LinkCandidate) bool {
	switch s.Correlate.Rename {
	case model.RenameAlways:
		return true
	case model.RenameTransitions:
		return ch.IdentityID != ""
	}
	return false
}

// alreadyNamed: the account already has one of the names the person would get (jdoe2 stays jdoe2).
func alreadyNamed(ch model.LinkCandidate, cands []model.Candidate) bool {
	for _, c := range cands {
		if strings.EqualFold(c.Sam, ch.Sam) && strings.EqualFold(c.UPN, ch.UPN) {
			return true
		}
	}
	return false
}

// LinkSummary describes a link change.
func LinkSummary(pl model.ChangePayload, idAttr, ext string) []string {
	lp := pl.Link
	if lp == nil {
		return nil
	}
	var s []string
	ch, ok := lp.ChosenCandidate()
	if !ok {
		s = append(s, fmt.Sprintf("Choose the existing AD account of this person: %d possible match(es), or create a new account", len(lp.Candidates)))
		for _, c := range lp.Candidates {
			s = append(s, "  Possible: "+candidateLine(c))
		}
		return s
	}
	line := fmt.Sprintf("Link existing account %q (%s) to this person: set %s = %s", ch.Sam, ch.DN, idAttr, ext)
	if ch.ExternalID != "" {
		line += fmt.Sprintf(" (was %s)", ch.ExternalID)
	}
	s = append(s, line)
	if ch.IdentityID != "" {
		s = append(s, fmt.Sprintf("The account belonged to %s (%s); that record is retired (merged) once this is applied", ch.IdentityDisplay, orStr(ch.SourceName, "another source")))
	}
	switch {
	case ch.Manual:
		s = append(s, "Chosen by a person")
	default:
		s = append(s, "Match: "+matchText(ch))
	}
	if lp.Verify {
		s = append(s, "Approve only after checking that this account belongs to this person; reject if it does not (the account is then never proposed for this person again)")
	}
	if !ch.Enabled {
		s = append(s, "The account is disabled; it is re-enabled by the next sync after linking")
	}
	if r := lp.Rename; r != nil {
		s = append(s, fmt.Sprintf("Rename %q -> %q, UPN %s -> %s, CN -> %q", ch.Sam, r.Sam, ch.UPN, r.UPN, r.CN))
		if r.Mail != "" {
			s = append(s, "Mail -> "+r.Mail+" (the old address stays as an alias)")
		}
	}
	if lp.RenameNote != "" {
		s = append(s, lp.RenameNote)
	}
	if len(lp.Candidates) > 1 {
		s = append(s, fmt.Sprintf("%d other possible match(es) are listed; choose another one if this is the wrong account", len(lp.Candidates)-1))
	}
	return s
}

func candidateLine(c model.LinkCandidate) string {
	out := fmt.Sprintf("%s (%s)", c.Sam, c.DN)
	if c.ExternalID != "" {
		out += ", id " + c.ExternalID
	}
	if c.IdentityDisplay != "" {
		out += ", belongs to " + c.IdentityDisplay
	}
	return out + " - " + matchText(c)
}

func matchText(c model.LinkCandidate) string {
	var same, diff []string
	for _, f := range c.Fields {
		if f.Matched {
			same = append(same, f.Field)
		} else {
			diff = append(diff, fmt.Sprintf("%s (%q vs AD %q)", f.Field, f.Value, f.ADValue))
		}
	}
	out := "full match"
	if !c.Full {
		out = "partial match"
	}
	if len(same) > 0 {
		out += "; same: " + strings.Join(same, ", ")
	}
	if len(diff) > 0 {
		out += "; differs: " + strings.Join(diff, ", ")
	}
	return out
}

// withCorrelationAttrs adds the AD attributes the correlation rules read to the snapshot properties.
func withCorrelationAttrs(props []string, rules []model.CorrelationRule) []string {
	seen := map[string]bool{}
	for _, p := range props {
		seen[strings.ToLower(p)] = true
	}
	for _, r := range append(rules, model.CorrelationRule{ADAttr: "adminCount"}) {
		if !seen[strings.ToLower(r.ADAttr)] {
			seen[strings.ToLower(r.ADAttr)] = true
			props = append(props, r.ADAttr)
		}
	}
	sort.Strings(props)
	return props
}
