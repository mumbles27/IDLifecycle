package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/cloud"
	"idlifecycle/internal/model"
	"idlifecycle/internal/secrets"
	"idlifecycle/internal/store"
)

var (
	ErrRunning = errors.New("a sync run is already in progress")
	ErrInvalid = errors.New("invalid request")
	// ErrForbidden: the request is valid but this person may not do it (an undo they requested themselves).
	ErrForbidden = errors.New("not allowed")
)

// Engine orchestrates sync runs and executes approved changes. Both are single-flight.
type Engine struct {
	Store *store.Store
	AD    ad.Client
	// Cloud changes Entra ID / Microsoft 365 group memberships (optional; nil = no cloud groups).
	Cloud  cloud.Client
	Cipher *secrets.Cipher
	Log    *slog.Logger
	Now    func() time.Time
	// Notifier receives operator notifications (optional).
	Notifier Notifier

	runMu        sync.Mutex
	execMu       sync.Mutex
	kick         chan struct{}
	lastDeferred int // deletes held back by max_deletes_per_day at the last executor pass (guarded by execMu)

	snapMu     sync.Mutex
	snapCache  *cachedSnapshot // the last AD snapshot (sync or impact preview), reused by previews
	cloudCache *cachedCloud    // the last cloud snapshot, likewise
	previewing atomic.Bool

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
	issues     []string // identity issues: names to choose, matches to verify, accounts nobody matches
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

	loc, err := time.LoadLocation(settings.Schedule.Timezone)
	if err != nil || settings.Schedule.Timezone == "" {
		loc = time.UTC
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
		list, err := e.ingest(ctx, s, cnt, loc, notes)
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

	// 2. AD snapshot (kept for the impact preview).
	holds, err := st.ActiveHolds(ctx, e.Now())
	if err != nil {
		return err
	}
	retention, err := st.ListRetentionPolicies(ctx)
	if err != nil {
		return err
	}
	cfg := planConfig{Settings: settings, Mappings: mappings, Rules: rules, Holds: holds, Retention: retention, Sources: srcs}
	req := snapshotRequestFor(cfg)
	snap, err := e.AD.Snapshot(ctx, req)
	if err != nil {
		return fmt.Errorf("AD snapshot: %w", err)
	}
	e.cacheSnapshot(req, snap)

	// 2b. Entra ID snapshot for cloud groups. A failure only stops cloud changes this run; AD work goes on.
	var cv *cloudView
	cloudFailed := false
	if needsCloud(cfg) {
		if cv, _, err = e.cloudSnapshot(ctx, settings, cloudGroupIDs(cfg), false); err != nil {
			cloudFailed, cv = true, nil
			cnt["warnings"]++
			cnt["cloud_errors"]++
			e.Log.Error("cloud snapshot failed; no cloud group changes this run", "error", err)
			if run.Error != "" {
				run.Error += "; "
			}
			run.Error += "Microsoft 365 groups: " + err.Error() + " (no cloud group changes this run)"
		}
	}

	// 3. correlate, link + plan (pure), then persist what changed on the identities.
	pass, err := planEveryone(cfg, snap, cv, idents, e.Now(), cnt, func(msg string, args ...any) { e.Log.Warn(msg, args...) })
	if err != nil {
		return err
	}
	for _, id := range pass.updates {
		if err := st.UpdateIdentity(ctx, id); err != nil {
			return fmt.Errorf("update identity %s: %w", id.ExternalID, err)
		}
	}
	for _, ev := range pass.events {
		e.audit(ctx, "system", ev.action, "identity", ev.identityID, ev.detail)
	}
	pc, plans, processed, decided, planErrs := pass.pc, pass.plans, pass.processed, pass.decided, pass.errList
	e.recordSuppressed(ctx, holds, pc.Suppressed)
	e.recordRetention(ctx, pc, pass.byID)
	if n := len(pc.ManualLeft); n > 0 {
		cnt["manual_not_disabled"] = n
		notes.issues = append(notes.issues, fmt.Sprintf("%d person(s) who left the source are Manual and were not disabled. Set them back to Auto on their page under Identities, or disable them by hand:", n))
		notes.issues = append(notes.issues, pc.ManualLeft...)
	}
	for _, w := range e.checkNames(ctx, settings, plans) {
		cnt["warnings"]++
		e.Log.Warn("name check", "warning", w)
	}
	plans = append(plans[:len(plans):len(plans)], pass.cloud...)

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
	autoLinks := 0
	for _, pl := range plans {
		if lp := pl.p.Payload.Link; pl.p.Type == model.ChangeLink && lp.Chosen != "" && !lp.Verify {
			autoLinks++
		}
	}
	heldLinks := settings.Correlate.MaxAutoLinksPerRun > 0 && autoLinks > settings.Correlate.MaxAutoLinksPerRun
	if heldLinks {
		cnt["link_guard_tripped"] = 1
		notes.thresholds = append(notes.thresholds, fmt.Sprintf("%d accounts were matched to people in one run, above correlation.max_auto_links_per_run (%d); all of these links need manual approval.", autoLinks, settings.Correlate.MaxAutoLinksPerRun))
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
		if held && (destructiveChange || (cloudRemovals(p) && !pl.id.Active)) {
			holds = append(holds, fmt.Sprintf("HELD for manual approval: %d disables/deletes in one run exceeds the limit of %d", destructive, settings.Engine.MaxDestructivePerRun))
		}
		if p.Payload.NeedsOU {
			holds = append(holds, "WAITING for an OU: choose one with Change OU, then approve")
		}
		if heldDeletes && p.Type == model.ChangeDelete {
			holds = append(holds, fmt.Sprintf("HELD for manual approval: %d deletes in one run exceeds the limit of %d", deletes, settings.Engine.MaxDeletesPerRun))
		}
		if p.manualPolicy != "" && p.Type == model.ChangeDelete {
			holds = append(holds, fmt.Sprintf("HELD for manual approval: retention policy %q requires a person to approve deletes", p.manualPolicy))
		}
		if p.Payload.NeedsName {
			holds = append(holds, "WAITING for a username: choose one with Set name, then approve")
			notes.issues = append(notes.issues, fmt.Sprintf("Username needed: %s (%s): %s", c.Display, c.ExternalID, conflictText(p.Payload.NameConflicts)))
		}
		if lp := p.Payload.Link; p.Type == model.ChangeLink {
			switch {
			case lp.Chosen == "":
				holds = append(holds, "WAITING for a decision: choose the matching account (or create a new one), then approve")
				notes.issues = append(notes.issues, fmt.Sprintf("Choose the account: %s (%s) matches %d existing account(s)", c.Display, c.ExternalID, len(lp.Candidates)))
			case lp.Verify:
				holds = append(holds, "VERIFY the match: approve only if the account belongs to this person")
				ch, _ := lp.ChosenCandidate()
				notes.issues = append(notes.issues, fmt.Sprintf("Verify the match: %s (%s) -> %s (%s)", c.Display, c.ExternalID, ch.Sam, matchText(ch)))
			case heldLinks:
				holds = append(holds, fmt.Sprintf("HELD for manual approval: %d links in one run exceeds the limit of %d", autoLinks, settings.Correlate.MaxAutoLinksPerRun))
			}
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
		e.audit(ctx, "system", "change.create", "change", c.ID, map[string]any{"type": c.Type, "status": c.Status, "external_id": c.ExternalID, "identity_id": c.IdentityID, "run": run.ID})

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
		if oc.Type == model.ChangeUndo {
			continue // an undo is a person's decision about one applied change, not a plan of this run
		}
		stale := oc.Status == model.StatusPending || oc.Status == model.StatusSimulated ||
			(oc.Status == model.StatusApproved && decided[oc.IdentityID]) // a plan error is not a verdict
		if oc.Type == model.ChangeCloudGroups && cloudFailed {
			continue // nothing was planned for the cloud this run: that is not a verdict either
		}
		if stale && processed[oc.IdentityID] && !keep[oc.ID] {
			e.supersede(ctx, oc, cnt)
		}
	}
	e.writeReport(ctx, run, settings, snap.Users, idents, pc.Links, plans, notes)
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
// snapshotBases are the search bases plus every OU disabled accounts are moved to (ad.disabled_ou and the retention
// policies' own), unless a search base already covers it: a disabled account must stay visible, or it would look
// deleted.
func snapshotBases(a model.ADSettings, extra ...string) []string {
	bases := append([]string(nil), a.SearchBases...)
	for _, ou := range append([]string{a.DisabledOU}, extra...) {
		ou = strings.TrimSpace(ou)
		if ou == "" {
			continue
		}
		covered := false
		for _, b := range bases {
			if strings.EqualFold(ou, b) || strings.HasSuffix(strings.ToLower(ou), ","+strings.ToLower(b)) {
				covered = true
				break
			}
		}
		if !covered {
			bases = append(bases, ou)
		}
	}
	return bases
}
