package engine

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/model"
)

// planConfig is the configuration one planning pass uses.
type planConfig struct {
	Settings model.Settings
	Mappings []model.Mapping
	Rules    []model.RoleRule
}

// lifecycleGroupIDs lists every group the lifecycle settings name (as typed).
func lifecycleGroupIDs(s model.Settings) []string {
	l := s.Lifecycle
	return append(append(append([]string{}, l.ActiveAdd...), l.DisabledAdd...), l.DisabledKeep...)
}

// managedGroupIDs are the groups this service manages (lower-cased identities): role groups plus the lifecycle groups
// it adds. disabled_keep groups are resolved but never managed (remove_stale_groups must not touch them).
func managedGroupIDs(cfg planConfig) map[string]bool {
	out := map[string]bool{}
	for _, g := range AllRuleGroups(cfg.Rules) {
		out[strings.ToLower(strings.TrimSpace(g))] = true
	}
	for _, g := range append(append([]string{}, cfg.Settings.Lifecycle.ActiveAdd...), cfg.Settings.Lifecycle.DisabledAdd...) {
		out[strings.ToLower(strings.TrimSpace(g))] = true
	}
	return out
}

// snapshotRequestFor is the AD snapshot a configuration needs.
func snapshotRequestFor(cfg planConfig) ad.SnapshotRequest {
	s := cfg.Settings
	props := attributeNames(cfg.Mappings, cfg.Rules)
	if s.Correlate.Enabled {
		props = withCorrelationAttrs(props, s.Correlate.Rules)
	}
	return ad.SnapshotRequest{
		Server: s.AD.Server, SearchBases: snapshotBases(s.AD), IDAttribute: s.AD.IDAttribute,
		Properties: props, Groups: unionFold(AllRuleGroups(cfg.Rules), lifecycleGroupIDs(s)), IncludeUnlinked: s.Correlate.Enabled,
	}
}

// unionFold merges lists, dropping blanks and case-insensitive duplicates (first spelling wins).
func unionFold(lists ...[]string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, l := range lists {
		for _, v := range l {
			v = strings.TrimSpace(v)
			if v != "" && !seen[strings.ToLower(v)] {
				seen[strings.ToLower(v)] = true
				out = append(out, v)
			}
		}
	}
	return out
}

// linkedByExt indexes snapshot users by id attribute (accounts without one are only there for correlation).
func linkedByExt(snap *ad.Snapshot) map[string][]*ad.User {
	byExt := map[string][]*ad.User{}
	for i := range snap.Users {
		u := &snap.Users[i]
		if u.ExternalID != "" {
			byExt[u.ExternalID] = append(byExt[u.ExternalID], u)
		}
	}
	return byExt
}

// planPass is the outcome of planning every identity once. It writes nothing.
type planPass struct {
	pc        *PlanContext
	plans     []plannedFor
	processed map[string]bool   // every identity looked at
	decided   map[string]bool   // identities the planner reached a verdict for (with or without a change)
	errs      map[string]string // identity id -> why it could not be planned
	errList   []string          // the same, for run.Error
	updates   []model.Identity  // identities whose stored row the caller should update (link state, observed values)
	byID      map[string]model.Identity
}

// planEveryone plans every identity against snap without writing anything: link state and observed values are applied
// to copies (returned in updates for a sync to persist). cnt receives the run counters; warn (optional) the warnings.
func planEveryone(cfg planConfig, snap *ad.Snapshot, idents []model.Identity, now time.Time, cnt map[string]int,
	warn func(msg string, args ...any)) (*planPass, error) {
	if warn == nil {
		warn = func(string, ...any) {}
	}
	s := cfg.Settings
	byExt := linkedByExt(snap)
	if len(byExt) == 0 {
		for _, id := range idents {
			if id.ADGUID != "" {
				return nil, errors.New("AD snapshot returned no users although accounts are linked; refusing to continue (check ad.search_bases and the service account's read rights)")
			}
		}
	}

	pc := &PlanContext{
		Settings: s, Mappings: cfg.Mappings, Rules: cfg.Rules, Now: now,
		GroupDN: map[string]string{}, ManagedDNs: map[string]bool{}, UserDNByExt: map[string]string{},
	}
	managed := managedGroupIDs(cfg)
	for _, g := range snap.Groups {
		if g.Found {
			pc.GroupDN[strings.ToLower(g.Identity)] = g.DN
			if managed[strings.ToLower(strings.TrimSpace(g.Identity))] {
				pc.ManagedDNs[strings.ToLower(g.DN)] = true
			}
		} else {
			cnt["warnings"]++
			warn("group not found in AD", "group", g.Identity)
		}
	}
	for ext, list := range byExt {
		if len(list) == 1 {
			pc.UserDNByExt[ext] = list[0].DN
		}
	}

	var transferring map[string]bool
	if s.Correlate.Enabled {
		var warns []string
		pc.Links, transferring, warns = correlate(s, snap.Users, idents, byExt)
		cnt["warnings"] += len(warns)
		for _, w := range warns {
			warn("correlation warning", "warning", w)
		}
	}

	pass := &planPass{pc: pc, processed: map[string]bool{}, decided: map[string]bool{}, errs: map[string]string{}, byID: map[string]model.Identity{}}
	fail := func(id model.Identity, msg string) {
		cnt["plan_errors"]++
		pass.errs[id.ID] = msg
		pass.errList = append(pass.errList, fmt.Sprintf("%s: %s", id.ExternalID, msg))
	}
	extCount := map[string]int{}
	for _, id := range idents {
		extCount[id.ExternalID]++
	}
	for _, id := range idents {
		pass.processed[id.ID] = true
		pass.byID[id.ID] = id
		if id.State == model.StateMerged {
			// Its account was handed to another person record (MergedInto); that record manages it now.
			pass.decided[id.ID] = true
			cnt["merged"]++
			continue
		}
		if extCount[id.ExternalID] > 1 {
			fail(id, "the same external id appears in more than one source; they would map to one AD account, so it is skipped")
			continue
		}
		var acct *ad.User
		if list := byExt[id.ExternalID]; len(list) > 1 {
			fail(id, fmt.Sprintf("%d AD accounts share %s=%s; resolve the duplicate", len(list), s.AD.IDAttribute, id.ExternalID))
			continue
		} else if len(list) == 1 {
			acct = list[0]
		}

		changed, vanished := linkIdentity(&id, acct, now)
		if vanished {
			warn("linked AD account no longer exists", "identity", id.ExternalID)
		}
		pass.byID[id.ID] = id
		if id.Active {
			cnt["active"]++
		} else {
			cnt["inactive"]++
		}

		p, warns, err := PlanIdentity(pc, id, acct)
		cnt["warnings"] += len(warns)
		for _, w := range warns {
			warn("plan warning", "identity", id.ExternalID, "warning", w)
		}
		if err != nil {
			warn("plan failed", "identity", id.ExternalID, "error", err)
			fail(id, err.Error())
			if changed {
				pass.updates = append(pass.updates, id)
			}
			continue
		}
		pass.decided[id.ID] = true
		if p != nil && len(p.Observed) > 0 {
			applied := make(map[string]string, len(id.Applied)+len(p.Observed))
			for k, v := range id.Applied {
				applied[k] = v
			}
			for k, v := range p.Observed {
				setApplied(applied, k, v)
			}
			id.Applied = applied
			changed = true
		}
		if changed {
			pass.updates = append(pass.updates, id)
			pass.byID[id.ID] = id
		}
		if p != nil && p.Type != "" && acct != nil && transferring[strings.ToLower(acct.ObjectGUID)] {
			// The account is proposed for another person record (contractor -> employee, rehire): no changes from the
			// old record until a person decides about the link.
			cnt["held_for_link"]++
			continue
		}
		if p == nil || p.Type == "" {
			cnt["unchanged"]++
			continue
		}
		pass.plans = append(pass.plans, plannedFor{id, p})
	}
	return pass, nil
}

// linkIdentity reconciles an identity with its AD account in memory. changed: the stored row must be updated;
// vanished: a linked account is gone from AD.
func linkIdentity(id *model.Identity, acct *ad.User, now time.Time) (changed, vanished bool) {
	before := *id
	if acct != nil {
		id.ADGUID, id.ADDN, id.ADSam = acct.ObjectGUID, acct.DN, acct.Sam
		if acct.Enabled {
			id.State, id.DisabledAt = model.StateLinked, nil
		} else {
			id.State = model.StateDisabled
			// The retention clock only runs for people who are gone from the source; an account
			// disabled for other reasons (leave of absence...) must not count down towards deletion.
			switch {
			case !id.Active && id.DisabledAt == nil:
				t := now
				id.DisabledAt = &t
			case id.Active:
				id.DisabledAt = nil
			}
		}
	} else if id.ADGUID != "" || id.State == model.StateLinked || id.State == model.StateDisabled {
		// Account vanished outside this service (or was deleted by us earlier).
		vanished = id.State != model.StateDeleted
		id.ADGUID, id.ADDN, id.ADSam, id.DisabledAt = "", "", "", nil
		if id.State != model.StateDeleted {
			id.State = model.StateNew
		}
		id.Applied = map[string]string{}
	}
	changed = before.ADGUID != id.ADGUID || before.ADDN != id.ADDN || before.ADSam != id.ADSam || before.State != id.State ||
		(before.DisabledAt == nil) != (id.DisabledAt == nil)
	return changed, vanished
}
