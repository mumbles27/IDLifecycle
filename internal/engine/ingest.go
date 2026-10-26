package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"idlifecycle/internal/model"
	"idlifecycle/internal/sources"
)

func (e *Engine) finish(ctx context.Context, run *model.Run) {
	var fatal []string
	var notes runNotes
	e.execMu.Lock() // the executor also rewrites identity rows; never interleave with planning
	err := e.sync(ctx, run, &notes)
	e.execMu.Unlock()
	if err != nil {
		fatal = append(fatal, err.Error())
	}
	if res, err := e.executeCounted(ctx); err != nil {
		fatal = append(fatal, "execute: "+err.Error())
	} else {
		run.Counters["applied"], run.Counters["failed"] = res.applied, res.failed
		if res.deferred > 0 {
			run.Counters["deferred_deletes"] = res.deferred
		}
	}

	now := e.Now()
	run.FinishedAt = &now
	run.Status = model.RunOK
	if len(fatal) > 0 || run.Counters["source_errors"] > 0 {
		run.Status = model.RunError
	}
	// run.Error holds source/plan notes collected by sync; fatal errors are appended.
	run.Error = strings.Join(dedupe(append([]string{run.Error}, fatal...)), "; ")
	if err := e.Store.UpdateRun(ctx, *run); err != nil {
		e.Log.Error("update run", "run", run.ID, "error", err)
	}
	e.audit(ctx, "system", "sync.finish", "run", run.ID, map[string]any{"status": run.Status, "counters": run.Counters, "error": run.Error})
	e.Log.Info("sync finished", "run", run.ID, "status", run.Status, "counters", run.Counters, "error", run.Error)
	e.notifyRun(ctx, run, notes.pending, notes.thresholds)
	e.notifyIssues(ctx, run, notes.issues)
}

const (
	minPopulationForShrinkGuard = 10
	maxFeedShrinkPercent        = 50
)

func hashAttrs(a map[string]string) string {
	b, _ := json.Marshal(a)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func isTerminated(opt model.SourceOptions, attrs map[string]string) bool {
	if opt.StatusField == "" {
		return false
	}
	v := strings.TrimSpace(lookupAttr(attrs, opt.StatusField))
	for _, t := range opt.TerminatedValues {
		if strings.EqualFold(v, strings.TrimSpace(t)) {
			return true
		}
	}
	return false
}

// ingest fetches one source and upserts identities. The returned slice is the source's full identity set.
func (e *Engine) ingest(ctx context.Context, src model.Source, cnt map[string]int) ([]model.Identity, error) {
	built, err := sources.BuildFor(src, e.Cipher, e.Store.ListManualPeople)
	if err != nil {
		return nil, err
	}
	rows, err := built.Fetch(ctx)
	if err != nil {
		return nil, err
	}

	existing, err := e.Store.ListIdentitiesBySource(ctx, src.ID)
	if err != nil {
		return nil, err
	}
	// The feed guards protect against truncated HR exports; people typed into the console cannot be truncated.
	feed := src.Type != sources.TypeManual
	if feed && len(rows) == 0 && len(existing) > 0 {
		return nil, errors.New("source returned zero records although identities exist; refusing to treat that as 'everyone left'")
	}

	if feed && len(existing) > 0 {
		valid := 0
		for _, r := range rows {
			if strings.TrimSpace(lookupAttr(r, src.Options.ExternalIDField)) != "" {
				valid++
			}
		}
		if valid == 0 {
			return nil, fmt.Errorf("none of the %d records has a value in %q; is external_id_field correct?", len(rows), src.Options.ExternalIDField)
		}
	}

	all := make([]*model.Identity, 0, len(existing)+len(rows))
	byExt := make(map[string]*model.Identity, len(existing)+len(rows))
	for i := range existing {
		id := existing[i]
		all = append(all, &id)
		byExt[id.ExternalID] = &id
	}

	now := e.Now()
	idField := src.Options.ExternalIDField
	occurrences := map[string]int{}
	for _, r := range rows {
		occurrences[strings.TrimSpace(lookupAttr(r, idField))]++
	}

	// A feed that suddenly lacks most of the known population is far more likely truncated than real.
	// Checked before anything is written so a rejected feed leaves no trace.
	if feed && len(existing) >= minPopulationForShrinkGuard {
		active, absent := 0, 0
		for _, cur := range all {
			if cur.Active {
				active++
				if occurrences[cur.ExternalID] == 0 {
					absent++
				}
			}
		}
		if active > 0 && absent*100/active > maxFeedShrinkPercent {
			return nil, fmt.Errorf("%d of %d active identities are missing from the feed (over %d%%); refusing to process a feed that looks truncated", absent, active, maxFeedShrinkPercent)
		}
	}
	seen := map[string]bool{}
	for _, r := range rows {
		ext := strings.TrimSpace(lookupAttr(r, idField))
		if ext == "" {
			cnt["skipped_no_id"]++
			continue
		}
		seen[ext] = true
		if occurrences[ext] > 1 {
			// Ambiguous: leave any existing identity untouched (but present) rather than guess which row is right.
			cnt["skipped_duplicate_id"]++
			continue
		}

		attrs := map[string]string(r)
		active := !isTerminated(src.Options, attrs)
		hash := hashAttrs(attrs)
		display := displayName(attrs, ext)

		cur, ok := byExt[ext]
		if !ok {
			id := &model.Identity{
				ID: uuid.NewString(), SourceID: src.ID, ExternalID: ext, Display: display, Attrs: attrs, AttrsHash: hash,
				Active: active, FirstSeen: now, LastSeen: now, Applied: map[string]string{}, State: model.StateNew,
			}
			if err := e.Store.InsertIdentity(ctx, *id); err != nil {
				return nil, fmt.Errorf("insert identity %s: %w", ext, err)
			}
			cnt["new_identities"]++
			byExt[ext] = id
			all = append(all, id)
			continue
		}
		if cur.AttrsHash != hash || cur.Active != active || cur.MissingRuns != 0 || cur.Display != display {
			cur.Attrs, cur.AttrsHash, cur.Active, cur.MissingRuns, cur.Display, cur.LastSeen = attrs, hash, active, 0, display, now
			if err := e.Store.UpdateIdentity(ctx, *cur); err != nil {
				return nil, fmt.Errorf("update identity %s: %w", ext, err)
			}
			cnt["updated_identities"]++
		}
	}

	out := make([]model.Identity, 0, len(all))
	for _, cur := range all {
		if !seen[cur.ExternalID] {
			cur.MissingRuns++
			if src.Options.MissingMeansTerminated && cur.MissingRuns > src.Options.MissingGraceRuns && cur.Active {
				cur.Active = false
				cnt["terminated_by_absence"]++
			}
			if err := e.Store.UpdateIdentity(ctx, *cur); err != nil {
				return nil, err
			}
		}
		out = append(out, *cur)
	}
	return out, nil
}
