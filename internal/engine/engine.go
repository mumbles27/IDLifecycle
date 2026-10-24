package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/model"
	"idlifecycle/internal/secrets"
	"idlifecycle/internal/store"
)

var (
	ErrRunning = errors.New("a sync run is already in progress")
	ErrInvalid = errors.New("invalid request")
)

// Engine orchestrates sync runs and executes approved changes. Both are single-flight.
type Engine struct {
	Store  *store.Store
	AD     ad.Client
	Cipher *secrets.Cipher
	Log    *slog.Logger
	Now    func() time.Time
	// Notifier receives operator notifications (optional).
	Notifier Notifier

	runMu        sync.Mutex
	execMu       sync.Mutex
	kick         chan struct{}
	lastDeferred int // deletes held back by max_deletes_per_day at the last executor pass (guarded by execMu)

	runCtx    context.Context
	runCancel context.CancelFunc
	wg        sync.WaitGroup
}

func New(st *store.Store, client ad.Client, c *secrets.Cipher, log *slog.Logger) *Engine {
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Engine{Store: st, AD: client, Cipher: c, Log: log, Now: time.Now, kick: make(chan struct{}, 1), runCtx: ctx, runCancel: cancel}
}

// Shutdown waits up to grace for in-flight runs to finish, then cancels them and waits briefly.
// Anything still unfinished is cleaned up by Recover on the next start.
func (e *Engine) Shutdown(grace time.Duration) {
	done := make(chan struct{})
	go func() { e.wg.Wait(); close(done) }()
	select {
	case <-done:
		return
	case <-time.After(grace):
	}
	e.Log.Warn("shutdown grace period elapsed; cancelling in-flight work")
	e.runCancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
}

// Kick asks the worker to look for approved changes soon.
func (e *Engine) Kick() {
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

// RunWorker applies approved changes when kicked and on a slow ticker until ctx ends.
func (e *Engine) RunWorker(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.kick:
		case <-t.C:
		}
		e.wg.Add(1)
		_, err := e.Execute(e.runCtx) // in-flight AD work gets the same shutdown grace as a sync
		e.wg.Done()
		if err != nil && ctx.Err() == nil {
			e.Log.Error("executor failed", "error", err)
		}
	}
}

// Recover cleans up state left by a crash. Call once at startup.
func (e *Engine) Recover(ctx context.Context) error {
	n, err := e.Store.MarkStaleRunsAborted(ctx)
	if err != nil {
		return err
	}
	m, err := e.Store.FailInterruptedChanges(ctx)
	if err != nil {
		return err
	}
	if n > 0 || m > 0 {
		e.Log.Warn("recovered from unclean shutdown", "aborted_runs", n, "interrupted_changes", m)
		e.audit(ctx, "system", "service.recover", "service", "", map[string]any{"aborted_runs": n, "interrupted_changes": m})
	}
	return nil
}

// Start launches a sync run in the background and returns immediately with the run record.
// The returned channel closes when the run (including execution of auto-approved changes) ends.
func (e *Engine) Start(ctx context.Context, trigger, by string) (model.Run, <-chan struct{}, error) {
	if !e.runMu.TryLock() {
		return model.Run{}, nil, ErrRunning
	}
	run := model.Run{ID: uuid.NewString(), Trigger: trigger, TriggeredBy: by, StartedAt: e.Now(), Status: model.RunRunning, Counters: map[string]int{}}
	if err := e.Store.CreateRun(ctx, run); err != nil {
		e.runMu.Unlock()
		return model.Run{}, nil, err
	}
	e.audit(ctx, by, "sync.start", "run", run.ID, map[string]any{"trigger": trigger})

	done := make(chan struct{})
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer close(done)
		defer e.runMu.Unlock()
		e.finish(e.runCtx, &run)
	}()
	return run, done, nil
}

// RunNow runs a sync synchronously (used by tests and the CLI).
func (e *Engine) RunNow(ctx context.Context, trigger, by string) (model.Run, error) {
	run, done, err := e.Start(ctx, trigger, by)
	if err != nil {
		return run, err
	}
	<-done
	return e.Store.GetRun(ctx, run.ID)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// ---- sync --------------------------------------------------------------------

type openKey struct{ identity, typ string }

// runNotes collects what a run should tell operators about.
type runNotes struct {
	pending    []string // "type display (external id)" of changes waiting for a human
	thresholds []string // safety limits that held changes back
}

func (e *Engine) sync(ctx context.Context, run *model.Run, notes *runNotes) error {
	cnt := run.Counters
	st := e.Store

	settings, err := st.GetSettings(ctx)
	if err != nil {
		return err
	}
	if len(settings.AD.SearchBases) == 0 {
		return errors.New("ad.search_bases is not configured")
	}
	if !settings.Engine.DryRun {
		if msg := settings.ReadyForLive(); msg != "" {
			return errors.New(msg)
		}
	}
	mappings, err := st.ListMappings(ctx)
	if err != nil {
		return err
	}
	rules, err := st.ListRoles(ctx)
	if err != nil {
		return err
	}
	srcs, err := st.ListSources(ctx)
	if err != nil {
		return err
	}

	// 1. ingest every enabled source (failures are isolated per source).
	var idents []model.Identity
	var srcErrs []string
	enabled := 0
	for _, s := range srcs {
		if !s.Enabled {
			continue
		}
		enabled++
		list, err := e.ingest(ctx, s, cnt)
		if err != nil {
			cnt["source_errors"]++
			srcErrs = append(srcErrs, fmt.Sprintf("source %q: %v", s.Name, err))
			_ = st.SetSourceStatus(ctx, s.ID, "error", err.Error())
			e.Log.Error("source failed", "source", s.Name, "error", err)
			continue
		}
		_ = st.SetSourceStatus(ctx, s.ID, "ok", "")
		idents = append(idents, list...)
	}
	if len(srcErrs) > 0 {
		run.Error = strings.Join(srcErrs, "; ")
	}
	if enabled == 0 {
		return errors.New("no enabled sources")
	}
	if len(idents) == 0 {
		return nil
	}
	cnt["identities"] = len(idents)

	// 2. AD snapshot.
	props := attributeNames(mappings, rules)
	groupIDs := AllRuleGroups(rules)
	snap, err := e.AD.Snapshot(ctx, ad.SnapshotRequest{
		Server: settings.AD.Server, SearchBases: snapshotBases(settings.AD), IDAttribute: settings.AD.IDAttribute,
		Properties: props, Groups: groupIDs,
	})
	if err != nil {
		return fmt.Errorf("AD snapshot: %w", err)
	}

	byExt := map[string][]*ad.User{}
	for i := range snap.Users {
		u := &snap.Users[i]
		byExt[u.ExternalID] = append(byExt[u.ExternalID], u)
	}
	if len(snap.Users) == 0 {
		for _, id := range idents {
			if id.ADGUID != "" {
				return errors.New("AD snapshot returned no users although accounts are linked; refusing to continue (check ad.search_bases and the service account's read rights)")
			}
		}
	}

	pc := &PlanContext{
		Settings: settings, Mappings: mappings, Rules: rules, Now: e.Now(),
		GroupDN: map[string]string{}, ManagedDNs: map[string]bool{}, UserDNByExt: map[string]string{},
	}
	for _, g := range snap.Groups {
		if g.Found {
			pc.GroupDN[strings.ToLower(g.Identity)] = g.DN
			pc.ManagedDNs[strings.ToLower(g.DN)] = true
		} else {
			cnt["warnings"]++
			e.Log.Warn("role group not found in AD", "group", g.Identity)
		}
	}
	for ext, list := range byExt {
		if len(list) == 1 {
			pc.UserDNByExt[ext] = list[0].DN
		}
	}

	// 3. link + plan.
	type plan struct {
		id model.Identity
		p  *Planned
	}
	var plans []plan
	processed := map[string]bool{}
	decided := map[string]bool{} // identities the planner reached a verdict for (with or without a change)
	var planErrs []string

	extCount := map[string]int{}
	for _, id := range idents {
		extCount[id.ExternalID]++
	}
	for _, id := range idents {
		processed[id.ID] = true
		if extCount[id.ExternalID] > 1 {
			cnt["plan_errors"]++
			planErrs = append(planErrs, fmt.Sprintf("%s: the same external id appears in more than one source; they would map to one AD account, so it is skipped", id.ExternalID))
			continue
		}
		var acct *ad.User
		if list := byExt[id.ExternalID]; len(list) > 1 {
			cnt["plan_errors"]++
			planErrs = append(planErrs, fmt.Sprintf("%s: %d AD accounts share %s=%s; resolve the duplicate", id.ExternalID, len(list), settings.AD.IDAttribute, id.ExternalID))
			continue
		} else if len(list) == 1 {
			acct = list[0]
		}

		if e.link(ctx, &id, acct) {
			if err := st.UpdateIdentity(ctx, id); err != nil {
				return fmt.Errorf("update identity %s: %w", id.ExternalID, err)
			}
		}
		if id.Active {
			cnt["active"]++
		} else {
			cnt["inactive"]++
		}

		p, warns, err := PlanIdentity(pc, id, acct)
		cnt["warnings"] += len(warns)
		for _, w := range warns {
			e.Log.Warn("plan warning", "identity", id.ExternalID, "warning", w)
		}
		if err != nil {
			cnt["plan_errors"]++
			planErrs = append(planErrs, fmt.Sprintf("%s: %v", id.ExternalID, err))
			e.Log.Error("plan failed", "identity", id.ExternalID, "error", err)
			continue
		}
		decided[id.ID] = true
		if p != nil && len(p.Observed) > 0 {
			if id.Applied == nil {
				id.Applied = map[string]string{}
			}
			for k, v := range p.Observed {
				setApplied(id.Applied, k, v)
			}
			if err := st.UpdateIdentity(ctx, id); err != nil {
				return fmt.Errorf("update identity %s: %w", id.ExternalID, err)
			}
		}
		if p == nil || p.Type == "" {
			cnt["unchanged"]++
			continue
		}
		plans = append(plans, plan{id, p})
	}
	if len(planErrs) > 0 {
		shown := planErrs
		if len(shown) > 5 {
			shown = append(append([]string{}, shown[:5]...), fmt.Sprintf("... and %d more", len(planErrs)-5))
		}
		if run.Error != "" {
			run.Error += "; "
		}
		run.Error += strings.Join(shown, "; ")
	}

	// 4. persist changes with approval policy.
	open, err := st.ListChangesWithStatus(ctx, model.StatusPending, model.StatusSimulated, model.StatusApproved, model.StatusApplying, model.StatusFailed)
	if err != nil {
		return err
	}
	openBy := map[openKey][]model.Change{}
	for _, c := range open {
		k := openKey{c.IdentityID, c.Type}
		openBy[k] = append(openBy[k], c)
	}

	destructive, deletes := 0, 0
	for _, pl := range plans {
		switch pl.p.Type {
		case model.ChangeDelete:
			deletes++
			destructive++
		case model.ChangeDisable:
			destructive++
		}
	}
	held := settings.Engine.MaxDestructivePerRun > 0 && destructive > settings.Engine.MaxDestructivePerRun
	if held {
		e.Log.Warn("bulk-destructive guard tripped; disables/deletes need manual approval", "count", destructive, "limit", settings.Engine.MaxDestructivePerRun)
		cnt["guard_tripped"] = 1
		notes.thresholds = append(notes.thresholds, fmt.Sprintf("%d disables/deletes were planned in one run, above engine.max_destructive_per_run (%d); all of them need manual approval.", destructive, settings.Engine.MaxDestructivePerRun))
	}
	heldDeletes := settings.Engine.MaxDeletesPerRun > 0 && deletes > settings.Engine.MaxDeletesPerRun
	if heldDeletes {
		e.Log.Warn("delete limit per run exceeded; deletes need manual approval", "count", deletes, "limit", settings.Engine.MaxDeletesPerRun)
		cnt["delete_guard_tripped"] = 1
		notes.thresholds = append(notes.thresholds, fmt.Sprintf("%d deletes were planned in one run, above engine.max_deletes_per_run (%d); all of them need manual approval.", deletes, settings.Engine.MaxDeletesPerRun))
	}

	keep := map[string]bool{} // ids of open changes still valid
	for _, pl := range plans {
		p := pl.p
		hash := PayloadHash(p.Type, p.Payload)
		k := openKey{pl.id.ID, p.Type}
		skip := false
		for _, oc := range openBy[k] {
			switch oc.Status {
			case model.StatusApplying:
				keep[oc.ID], skip = true, true
			case model.StatusApproved, model.StatusFailed:
				// Same plan already queued/failed: keep it (a failed one waits for a human to retry).
				if oc.PayloadHash == hash {
					keep[oc.ID], skip = true, true
				}
			case model.StatusPending, model.StatusSimulated:
				// A simulated plan only counts while in dry-run (and a pending one only when live), so
				// switching modes produces fresh proposals instead of silently reusing the old ones.
				sameMode := (oc.Status == model.StatusSimulated) == settings.Engine.DryRun
				if oc.PayloadHash == hash && sameMode {
					keep[oc.ID], skip = true, true
				}
			}
		}
		if skip {
			cnt["already_queued"]++
			continue
		}

		c := model.Change{
			ID: uuid.NewString(), RunID: run.ID, IdentityID: pl.id.ID, ExternalID: pl.id.ExternalID, Display: pl.id.Display,
			Type: p.Type, Payload: p.Payload, Summary: p.Summary, PayloadHash: hash, CreatedAt: e.Now(),
		}
		destructiveChange := p.Type == model.ChangeDisable || p.Type == model.ChangeDelete
		var holds []string
		if held && destructiveChange {
			holds = append(holds, fmt.Sprintf("HELD for manual approval: %d disables/deletes in one run exceeds the limit of %d", destructive, settings.Engine.MaxDestructivePerRun))
		}
		if heldDeletes && p.Type == model.ChangeDelete {
			holds = append(holds, fmt.Sprintf("HELD for manual approval: %d deletes in one run exceeds the limit of %d", deletes, settings.Engine.MaxDeletesPerRun))
		}
		switch {
		case settings.Engine.DryRun:
			c.Status = model.StatusSimulated
			cnt["simulated"]++
		case settings.Approval.For(p.Type) == "auto" && len(holds) == 0:
			c.Status = model.StatusApproved
			now := e.Now()
			c.DecidedBy, c.DecidedAt, c.DecisionNote = "system:auto", &now, "auto-approved by policy"
			cnt["auto_approved"]++
		default:
			c.Status = model.StatusPending
			cnt["pending"]++
			notes.pending = append(notes.pending, fmt.Sprintf("%s %s (%s)", c.Type, c.Display, c.ExternalID))
			if len(holds) > 0 && settings.Approval.For(p.Type) == "auto" {
				c.Summary = append(holds, c.Summary...)
				cnt["held"]++
			}
		}
		if err := st.InsertChange(ctx, c); err != nil {
			return fmt.Errorf("insert change: %w", err)
		}
		cnt["planned_"+p.Type]++
		keep[c.ID] = true
		e.audit(ctx, "system", "change.create", "change", c.ID, map[string]any{"type": c.Type, "status": c.Status, "external_id": c.ExternalID, "run": run.ID})

		// A newer plan for the same identity+type replaces earlier pending/simulated ones.
		for _, oc := range openBy[k] {
			if (oc.Status == model.StatusPending || oc.Status == model.StatusSimulated || oc.Status == model.StatusApproved) && !keep[oc.ID] {
				e.supersede(ctx, oc, cnt)
			}
		}
	}

	// Plans that disappeared (data reverted, person rehired, someone fixed AD by hand...) make open proposals
	// obsolete - including approved ones that have not run yet (e.g. approved during dry-run): applying a
	// decision about a state that no longer exists is how a rehire gets disabled.
	for _, oc := range open {
		stale := oc.Status == model.StatusPending || oc.Status == model.StatusSimulated ||
			(oc.Status == model.StatusApproved && decided[oc.IdentityID]) // a plan error is not a verdict
		if stale && processed[oc.IdentityID] && !keep[oc.ID] {
			e.supersede(ctx, oc, cnt)
		}
	}
	return nil
}

func (e *Engine) supersede(ctx context.Context, c model.Change, cnt map[string]int) {
	if err := e.Store.TransitionChange(ctx, c.ID, c.Status, model.StatusSuperseded, "system", ""); err != nil {
		if !errors.Is(err, store.ErrConflict) {
			e.Log.Error("supersede change", "change", c.ID, "error", err)
		}
		return
	}
	cnt["superseded"]++
	e.audit(ctx, "system", "change.supersede", "change", c.ID, map[string]any{"from": c.Status})
}

// link reconciles an identity with its AD account and reports whether the row changed.
func (e *Engine) link(ctx context.Context, id *model.Identity, acct *ad.User) bool {
	before := *id
	now := e.Now()
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
				id.DisabledAt = &now
			case id.Active:
				id.DisabledAt = nil
			}
		}
	} else if id.ADGUID != "" || id.State == model.StateLinked || id.State == model.StateDisabled {
		// Account vanished outside this service (or was deleted by us earlier).
		if id.State != model.StateDeleted {
			e.Log.Warn("linked AD account no longer exists", "identity", id.ExternalID, "guid", id.ADGUID)
		}
		id.ADGUID, id.ADDN, id.ADSam, id.DisabledAt = "", "", "", nil
		if id.State != model.StateDeleted {
			id.State = model.StateNew
		}
		id.Applied = map[string]string{}
	}
	return before.ADGUID != id.ADGUID || before.ADDN != id.ADDN || before.ADSam != id.ADSam || before.State != id.State ||
		(before.DisabledAt == nil) != (id.DisabledAt == nil)
}

// attributeNames lists every AD attribute any mapping or role grant touches.
func attributeNames(mappings []model.Mapping, rules []model.RoleRule) []string {
	seen := map[string]bool{}
	var out []string
	add := func(a string) {
		if a != "" && !seen[strings.ToLower(a)] {
			seen[strings.ToLower(a)] = true
			out = append(out, a)
		}
	}
	for _, m := range mappings {
		if m.Enabled {
			add(m.TargetAttr)
		}
	}
	for _, r := range rules {
		if r.Enabled {
			for _, a := range r.Grants.Attributes {
				add(a.TargetAttr)
			}
		}
	}
	sort.Strings(out)
	return out
}

// ---- ingest --------------------------------------------------------------------

func displayName(attrs map[string]string, fallback string) string {
	for _, k := range []string{"displayName", "DisplayName", "Name", "name", "FullName"} {
		if v := strings.TrimSpace(attrs[k]); v != "" {
			return v
		}
	}
	first := lookupAttr(attrs, "FirstName")
	if first == "" {
		first = lookupAttr(attrs, "firstName")
	}
	last := lookupAttr(attrs, "LastName")
	if last == "" {
		last = lookupAttr(attrs, "lastName")
	}
	if n := strings.TrimSpace(first + " " + last); n != "" {
		return n
	}
	return fallback
}

// snapshotBases returns the search bases plus the disabled-accounts OU (when it lies outside them),
// otherwise a disabled account would drop out of view the moment it is moved there.
func snapshotBases(a model.ADSettings) []string {
	bases := append([]string(nil), a.SearchBases...)
	if a.DisabledOU == "" {
		return bases
	}
	for _, b := range bases {
		if strings.EqualFold(a.DisabledOU, b) || strings.HasSuffix(strings.ToLower(a.DisabledOU), ","+strings.ToLower(b)) {
			return bases
		}
	}
	return append(bases, a.DisabledOU)
}
