package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/model"
)

// plannedFor is one identity's plan inside a sync run.
type plannedFor struct {
	id model.Identity
	p  *Planned
}

// correlate proposes existing accounts for people who have none (by id attribute). It returns the proposals by
// identity id and the objectGUIDs of accounts that are being handed over (their current owner gets no plan this run).
func correlate(s model.Settings, users []ad.User, idents []model.Identity, byExt map[string][]*ad.User) (map[string]*model.LinkPayload, map[string]bool, []string) {
	owners := map[string]*model.Identity{}
	for i := range idents {
		if _, dup := owners[idents[i].ExternalID]; !dup {
			owners[idents[i].ExternalID] = &idents[i]
		}
	}
	c := newCorrelator(s, users, owners)
	links := map[string]*model.LinkPayload{}
	var warns []string
	for _, id := range idents {
		if !id.Active || id.State == model.StateMerged || len(byExt[id.ExternalID]) > 0 || id.LinkDecision == model.LinkDecisionNew {
			continue
		}
		lp, w := c.find(id)
		for _, x := range w {
			warns = append(warns, id.ExternalID+": "+x)
		}
		if lp != nil {
			links[id.ID] = lp
		}
	}
	// One account proposed as the pick for several people: nobody gets it automatically.
	picks := map[string]int{}
	for _, lp := range links {
		if lp.Chosen != "" {
			picks[strings.ToLower(lp.Chosen)]++
		}
	}
	for _, lp := range links {
		if lp.Chosen != "" && picks[strings.ToLower(lp.Chosen)] > 1 {
			if ch, _ := lp.ChosenCandidate(); !ch.Manual {
				lp.Chosen, lp.Verify = "", true
			}
		}
	}
	transferring := map[string]bool{}
	for _, lp := range links {
		for _, cand := range lp.Candidates {
			if cand.IdentityID != "" && (lp.Chosen == "" || strings.EqualFold(lp.Chosen, cand.ObjectGUID)) {
				transferring[strings.ToLower(cand.ObjectGUID)] = true
			}
		}
	}
	return links, transferring, warns
}

// checkNames drops taken names from create plans (and marks creates that have none left), and picks a free name for
// renames on link plans. Names claimed by earlier plans of the same run count as taken. It returns warnings; when AD
// cannot be asked, plans are left as they are (the create script refuses taken names anyway).
func (e *Engine) checkNames(ctx context.Context, s model.Settings, plans []plannedFor) []string {
	var creates, links []plannedFor
	for _, pl := range plans {
		switch {
		case pl.p.Type == model.ChangeCreate:
			creates = append(creates, pl)
		case pl.p.Type == model.ChangeLink && len(pl.p.renames) > 0:
			links = append(links, pl)
		}
	}
	if len(creates)+len(links) == 0 {
		return nil
	}
	sort.SliceStable(creates, func(i, j int) bool { return creates[i].id.ExternalID < creates[j].id.ExternalID })

	var warns []string
	claimed := map[string]string{} // nameKey.id() -> "Display (external id)"
	taken := func(req ad.NameCheckRequest) (map[string]ad.TakenName, bool) {
		if len(req.Names) == 0 {
			return map[string]ad.TakenName{}, true
		}
		res, err := e.AD.CheckNames(ctx, req)
		if err != nil {
			warns = append(warns, "could not check account names in AD ("+err.Error()+"); taken names are refused when the account is created")
			return nil, false
		}
		out := map[string]ad.TakenName{}
		for _, t := range res {
			k := nameKey{t.Field, t.Value, ""}
			if t.Field == ad.NameCN {
				k.ou = parentDN(t.DN)
			}
			out[k.id()] = t
		}
		return out, true
	}
	lookup := func(m map[string]ad.TakenName, k nameKey) (ad.TakenName, bool) {
		t, ok := m[k.id()]
		return t, ok
	}

	// Creates: one query for every candidate of every create.
	var req ad.NameCheckRequest
	req.Server = s.AD.Server
	seen := map[string]bool{}
	for _, pl := range creates {
		ou := pl.p.Payload.OU
		if pl.p.Payload.NeedsOU {
			ou = ""
		}
		for _, c := range pl.p.Payload.Candidates {
			for _, k := range CandidateQueries(c, ou) {
				if !seen[k.id()] {
					seen[k.id()] = true
					req.Names = append(req.Names, ad.NameQuery{Field: k.field, Value: k.value, OU: k.ou})
				}
			}
		}
	}
	if inAD, ok := taken(req); ok {
		for _, pl := range creates {
			pay := &pl.p.Payload
			ou := pay.OU
			if pay.NeedsOU {
				ou = ""
			}
			var free []model.Candidate
			var first []model.NameConflict
			for i, c := range pay.Candidates {
				var conflicts []model.NameConflict
				for _, k := range CandidateQueries(c, ou) {
					if t, hit := lookup(inAD, k); hit {
						conflicts = append(conflicts, model.NameConflict{Field: k.field, Value: k.value, DN: t.DN})
					} else if who, hit := claimed[k.id()]; hit {
						conflicts = append(conflicts, model.NameConflict{Field: k.field, Value: k.value, With: who})
					}
				}
				if i == 0 {
					first = conflicts
				}
				if len(conflicts) == 0 {
					free = append(free, c)
				}
			}
			if len(free) == 0 {
				pay.NeedsName, pay.NameConflicts = true, first
				pay.Candidates = pay.Candidates[:1] // the names the templates give; a person replaces them
			} else {
				pay.Candidates = free
				for _, k := range CandidateQueries(free[0], ou) {
					claimed[k.id()] = fmt.Sprintf("%s (%s)", pl.id.Display, pl.id.ExternalID)
				}
			}
			pl.p.summarizeCreate()
		}
	}

	// Renames: the account itself does not count as taken.
	for _, pl := range links {
		lp := pl.p.Payload.Link
		ch, _ := lp.ChosenCandidate()
		var rq ad.NameCheckRequest
		rq.Server, rq.Exclude = s.AD.Server, []string{ch.ObjectGUID}
		ou := parentDN(ch.DN)
		for _, c := range pl.p.renames {
			for _, k := range CandidateQueries(c, ou) {
				rq.Names = append(rq.Names, ad.NameQuery{Field: k.field, Value: k.value, OU: k.ou})
			}
		}
		inAD, ok := taken(rq)
		if !ok {
			lp.RenameNote = "Not renamed: account names could not be checked in AD"
			pl.p.Summary = LinkSummary(pl.p.Payload, s.AD.IDAttribute, pl.id.ExternalID)
			continue
		}
		for _, c := range pl.p.renames {
			clash := ""
			for _, k := range CandidateQueries(c, ou) {
				if t, hit := lookup(inAD, k); hit {
					clash = fmt.Sprintf("%s %q is used by %s", nameFieldLabel(k.field), k.value, t.DN)
				} else if who, hit := claimed[k.id()]; hit {
					clash = fmt.Sprintf("%s %q is planned for %s", nameFieldLabel(k.field), k.value, who)
				}
				if clash != "" {
					break
				}
			}
			if clash == "" {
				r := c
				lp.Rename = &r
				for _, k := range CandidateQueries(c, ou) {
					claimed[k.id()] = fmt.Sprintf("%s (%s)", pl.id.Display, pl.id.ExternalID)
				}
				break
			}
			if lp.RenameNote == "" {
				lp.RenameNote = "Not renamed to the new naming: " + clash
			}
		}
		if lp.Rename != nil {
			lp.RenameNote = ""
		}
		pl.p.Summary = LinkSummary(pl.p.Payload, s.AD.IDAttribute, pl.id.ExternalID)
	}
	return warns
}

// ---- correlation report -------------------------------------------------------------

// ReportAccount is an AD account in the correlation report.
type ReportAccount struct {
	ObjectGUID string `json:"object_guid"`
	DN         string `json:"dn"`
	Sam        string `json:"sam"`
	UPN        string `json:"upn"`
	Display    string `json:"display"`
	Enabled    bool   `json:"enabled"`
	ExternalID string `json:"external_id"`
}

// ReportChange points at a change that needs a person.
type ReportChange struct {
	ChangeID   string `json:"change_id"`
	IdentityID string `json:"identity_id"`
	Display    string `json:"display"`
	ExternalID string `json:"external_id"`
	Candidates int    `json:"candidates,omitempty"`
}

// CorrelationReport is written by every sync run (store state "correlation_report").
type CorrelationReport struct {
	RunID           string          `json:"run_id"`
	GeneratedAt     time.Time       `json:"generated_at"`
	Enabled         bool            `json:"enabled"`
	Uncorrelated    []ReportAccount `json:"uncorrelated"`
	Verify          []ReportChange  `json:"verify"`
	NeedsName       []ReportChange  `json:"needs_name"`
	UnmatchedPeople int             `json:"unmatched_people"`
}

// ReportState is the store state name of the correlation report.
const ReportState = "correlation_report"

// uncorrelatedAccounts lists enabled accounts that no person in any source accounts for: no id (or an id no source
// knows) and not proposed for anybody in this run. Ignored accounts are left out.
func uncorrelatedAccounts(s model.Settings, users []ad.User, idents []model.Identity, links map[string]*model.LinkPayload) []ReportAccount {
	known := map[string]bool{}
	for _, id := range idents {
		if id.State != model.StateMerged {
			known[id.ExternalID] = true
		}
	}
	proposed := map[string]bool{}
	for _, lp := range links {
		for _, c := range lp.Candidates {
			proposed[strings.ToLower(c.ObjectGUID)] = true
		}
	}
	out := []ReportAccount{}
	for _, u := range users {
		if !u.Enabled || Privileged(&u) || (u.ExternalID != "" && known[u.ExternalID]) || proposed[strings.ToLower(u.ObjectGUID)] ||
			IgnoredAccount(s.Correlate.IgnoreAccounts, u.Sam, u.DN) {
			continue
		}
		out = append(out, ReportAccount{ObjectGUID: u.ObjectGUID, DN: u.DN, Sam: u.Sam, UPN: u.UPN, Display: shortCN(u.DN), Enabled: u.Enabled, ExternalID: u.ExternalID})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Sam) < strings.ToLower(out[j].Sam) })
	return out
}

// writeReport stores this run's correlation report and adds newly uncorrelated accounts to the run's issues.
func (e *Engine) writeReport(ctx context.Context, run *model.Run, s model.Settings, users []ad.User, idents []model.Identity, links map[string]*model.LinkPayload, plans []plannedFor, notes *runNotes) {
	var prev CorrelationReport
	hadPrev, err := e.Store.GetState(ctx, ReportState, &prev)
	if err != nil {
		e.Log.Warn("load correlation report", "error", err)
	}
	rep := CorrelationReport{RunID: run.ID, GeneratedAt: e.Now(), Enabled: s.Correlate.Enabled, Uncorrelated: []ReportAccount{}, Verify: []ReportChange{}, NeedsName: []ReportChange{}}
	if s.Correlate.Enabled {
		if s.Correlate.ReportUncorrelated {
			rep.Uncorrelated = uncorrelatedAccounts(s, users, idents, links)
		}
		for _, pl := range plans {
			if pl.p.Type == model.ChangeCreate {
				rep.UnmatchedPeople++
			}
		}
	}
	open, err := e.Store.ListChangesWithStatus(ctx, model.StatusPending, model.StatusSimulated)
	if err != nil {
		e.Log.Warn("list open changes for the correlation report", "error", err)
	}
	for _, c := range open {
		rc := ReportChange{ChangeID: c.ID, IdentityID: c.IdentityID, Display: c.Display, ExternalID: c.ExternalID}
		switch {
		case c.Payload.NeedsName:
			rep.NeedsName = append(rep.NeedsName, rc)
		case c.Type == model.ChangeLink && c.Payload.Link != nil && (c.Payload.Link.Chosen == "" || c.Payload.Link.Verify):
			rc.Candidates = len(c.Payload.Link.Candidates)
			rep.Verify = append(rep.Verify, rc)
		}
	}
	if len(rep.Uncorrelated) > 0 {
		before := map[string]bool{}
		if hadPrev && prev.Enabled {
			for _, a := range prev.Uncorrelated {
				before[strings.ToLower(a.ObjectGUID)] = true
			}
		}
		var fresh []string
		for _, a := range rep.Uncorrelated {
			if !before[strings.ToLower(a.ObjectGUID)] {
				fresh = append(fresh, fmt.Sprintf("Not matched to anybody: %s (%s)", a.Sam, a.DN))
			}
		}
		if len(fresh) > 0 {
			notes.issues = append(notes.issues, fmt.Sprintf("%d enabled AD account(s) are not matched to anybody in a source (%d new since the last run). Link them to a person, or ignore service accounts, under Matching.", len(rep.Uncorrelated), len(fresh)))
			notes.issues = append(notes.issues, fresh...)
		}
	}
	run.Counters["uncorrelated_accounts"] = len(rep.Uncorrelated)
	if err := e.Store.PutState(ctx, ReportState, rep); err != nil {
		e.Log.Warn("save correlation report", "error", err)
	}
}

// notifyIssues sends the identity_issues message for one run.
func (e *Engine) notifyIssues(ctx context.Context, run *model.Run, issues []string) {
	if len(issues) == 0 {
		return
	}
	e.notify(ctx, model.Notification{
		Event: model.EventIdentityIssues, RunID: run.ID,
		Subject: fmt.Sprintf("%d identity issue(s) need a person", len(issues)),
		Lines:   append([]string{"The last sync found usernames to choose, account matches to verify or AD accounts nobody matches. Open Approvals or Matching in the console:", ""}, capped(issues)...),
	})
}
