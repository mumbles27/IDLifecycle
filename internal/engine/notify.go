package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"idlifecycle/internal/model"
)

// Notifier delivers operator notifications (e-mail). Implementations must not block: Notify is called
// from the sync and executor paths. A nil Notifier disables notifications.
type Notifier interface {
	Notify(ctx context.Context, n model.Notification)
}

const maxListed = 25 // lines of detail per message; the GUI has the full list

func (e *Engine) notify(ctx context.Context, n model.Notification) {
	if e.Notifier != nil {
		e.Notifier.Notify(context.WithoutCancel(ctx), n)
	}
}

func capped(lines []string) []string {
	if len(lines) <= maxListed {
		return lines
	}
	return append(append([]string{}, lines[:maxListed]...), fmt.Sprintf("... and %d more", len(lines)-maxListed))
}

// notifyExecution reports failures and delete-limit deferrals of one executor pass. Called with execMu held.
func (e *Engine) notifyExecution(ctx context.Context, r execResult) {
	if len(r.failures) > 0 {
		e.notify(ctx, model.Notification{
			Event:   model.EventChangesFailed,
			Subject: fmt.Sprintf("%d change(s) failed in Active Directory", len(r.failures)),
			Lines:   append([]string{"These approved changes could not be applied. Review them under Approvals and retry once the cause is fixed:", ""}, capped(r.failures)...),
		})
	}
	// The worker re-runs every 30 seconds: only speak up when the backlog changes.
	if r.deferred != e.lastDeferred {
		e.lastDeferred = r.deferred
		if r.deferred > 0 {
			e.notify(ctx, model.Notification{
				Event:   model.EventThreshold,
				Subject: fmt.Sprintf("Daily delete limit reached: %d approved delete(s) deferred", r.deferred),
				Lines: []string{
					fmt.Sprintf("engine.max_deletes_per_day stops more deletes in the current 24 hours; %d approved delete(s) wait and are applied automatically when the window allows.", r.deferred),
					"Raise the limit under Settings if this volume is expected.",
				},
			})
		}
	}
}

// notifyRun sends the end-of-run messages: failure, waiting approvals, thresholds and the optional summary.
func (e *Engine) notifyRun(ctx context.Context, run *model.Run, pending []string, thresholds []string) {
	cnt := run.Counters
	if run.Status != model.RunOK {
		e.notify(ctx, model.Notification{
			Event: model.EventRunFailed, RunID: run.ID,
			Subject: "Sync run finished with errors",
			Lines:   append([]string{"Error: " + orStr(run.Error, "(none recorded)"), ""}, counterLines(cnt)...),
		})
	}
	if len(pending) > 0 {
		e.notify(ctx, model.Notification{
			Event: model.EventApprovalsPending, RunID: run.ID,
			Subject: fmt.Sprintf("%d change(s) waiting for approval", len(pending)),
			Lines:   append([]string{"The last sync proposed changes that need a decision under Approvals:", ""}, capped(pending)...),
		})
	}
	if len(thresholds) > 0 {
		e.notify(ctx, model.Notification{
			Event: model.EventThreshold, RunID: run.ID,
			Subject: "Safety threshold reached: changes held for manual approval",
			Lines:   thresholds,
		})
	}
	if cnt["applied"]+cnt["failed"]+cnt["pending"]+cnt["auto_approved"]+cnt["simulated"] > 0 {
		e.notify(ctx, model.Notification{
			Event: model.EventRunSummary, RunID: run.ID,
			Subject: "Sync run " + run.Status,
			Lines:   counterLines(cnt),
		})
	}
}

func counterLines(cnt map[string]int) []string {
	keys := make([]string, 0, len(cnt))
	for k, v := range cnt {
		if v != 0 {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, fmt.Sprintf("%-22s %d", strings.ReplaceAll(k, "_", " "), cnt[k]))
	}
	return out
}
