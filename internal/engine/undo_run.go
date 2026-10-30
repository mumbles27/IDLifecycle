package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/model"
	"idlifecycle/internal/store"
)

// Undo across a run: a person picks changes (and parts of changes) the run applied, previews them together, and
// queues one undo change per picked change, all in one batch. A batch is approved together with "UNDO <n>".

// MaxUndoBatch caps the changes one run undo can queue.
const MaxUndoBatch = 200

// RunUndoEntry is one applied change of a run, as the run undo dialog lists it.
type RunUndoEntry struct {
	Change    model.Change `json:"change"`
	Undoable  bool         `json:"undoable"`
	Reason    string       `json:"reason"`
	WholeOnly bool         `json:"whole_only"`
	Items     []UndoItem   `json:"items"`
}

// RunUndoSelection maps a change id to the parts to undo (["*"] or none = every part not undone yet).
type RunUndoSelection map[string][]string

// RunUndoPreviewEntry is the preview of one change of a run undo.
type RunUndoPreviewEntry struct {
	ChangeID    string           `json:"change_id"`
	Type        string           `json:"type"`
	Display     string           `json:"display"`
	ExternalID  string           `json:"external_id"`
	IdentityID  string           `json:"identity_id"`
	ConfirmText string           `json:"confirm_text"`
	Items       []string         `json:"items"`
	Steps       []string         `json:"steps"`
	DryRun      UndoDryRun       `json:"dry_run"`
	Drift       []string         `json:"drift"`
	Holds       []UndoHoldOption `json:"holds"`
	Warnings    []string         `json:"warnings"`
	Forced      bool             `json:"forced,omitempty"` // batch check: the request confirmed overwriting later changes
}

// RunUndoPreview is the whole selection, previewed with one dry run per script.
type RunUndoPreview struct {
	RunID       string                `json:"run_id"`
	People      int                   `json:"people"`
	Count       int                   `json:"count"` // undo changes the request creates (one per selected change): the n of "UNDO n"
	Changes     []RunUndoPreviewEntry `json:"changes"`
	DriftCount  int                   `json:"drift_count"`
	FailedCount int                   `json:"failed_count"`
	Warnings    []string              `json:"warnings"`
	ConfirmText string                `json:"confirm_text"`
	Approval    string                `json:"approval"`
	Hash        string                `json:"preview_hash"`

	previews []UndoPreview
}

// RunUndoRequest queues a run undo. Manual (default true) pins every undone part as Manual (the holds each undo offers).
type RunUndoRequest struct {
	Selection   RunUndoSelection `json:"selection"`
	Reason      string           `json:"reason"`
	Confirm     string           `json:"confirm"`
	Force       bool             `json:"force"`
	Manual      *bool            `json:"manual"`
	PreviewHash string           `json:"preview_hash"`
}

// undoBatchConfirm is what a person types to request or approve n undos at once.
func undoBatchConfirm(n int) string { return fmt.Sprintf("UNDO %d", n) }

// allChanges reads every change matching f (paged).
func (e *Engine) allChanges(ctx context.Context, f store.ChangeFilter) ([]model.Change, error) {
	var out []model.Change
	for off := 0; ; off += 500 {
		page, total, err := e.Store.ListChanges(ctx, f, store.Page{Limit: 500, Offset: off})
		if err != nil {
			return nil, err
		}
		out = append(out, page...)
		if len(page) == 0 || off+len(page) >= total {
			return out, nil
		}
	}
}

// RunUndoList lists the applied changes of run runID with what can be undone.
func (e *Engine) RunUndoList(ctx context.Context, runID string) (model.Run, []RunUndoEntry, error) {
	run, err := e.Store.GetRun(ctx, runID)
	if err != nil {
		return run, nil, err
	}
	changes, err := e.allChanges(ctx, store.ChangeFilter{RunID: runID, Status: model.StatusApplied})
	if err != nil {
		return run, nil, err
	}
	sort.SliceStable(changes, func(i, j int) bool {
		if changes[i].Display != changes[j].Display {
			return changes[i].Display < changes[j].Display
		}
		return changes[i].ID < changes[j].ID
	})
	out := make([]RunUndoEntry, 0, len(changes))
	for _, c := range changes {
		plan, err := e.PlanUndo(ctx, c.ID)
		if err != nil {
			out = append(out, RunUndoEntry{Change: c, Reason: err.Error(), Items: []UndoItem{}})
			continue
		}
		items := plan.Items
		if items == nil {
			items = []UndoItem{}
		}
		out = append(out, RunUndoEntry{Change: c, Undoable: plan.Undoable, Reason: plan.Reason,
			WholeOnly: c.Type != model.ChangeUpdate && c.Type != model.ChangeCloudGroups, Items: items})
	}
	return run, out, nil
}

// PreviewRunUndo plans and dry-runs the selected undos of run runID.
func (e *Engine) PreviewRunUndo(ctx context.Context, runID string, sel RunUndoSelection) (RunUndoPreview, error) {
	out := RunUndoPreview{RunID: runID, Changes: []RunUndoPreviewEntry{}, Warnings: []string{}, Approval: "manual"}
	if _, err := e.Store.GetRun(ctx, runID); err != nil {
		return out, err
	}
	switch {
	case len(sel) == 0:
		return out, fmt.Errorf("%w: choose at least one change to undo", ErrInvalid)
	case len(sel) > MaxUndoBatch:
		return out, fmt.Errorf("%w: at most %d changes can be undone at once (%d chosen)", ErrInvalid, MaxUndoBatch, len(sel))
	}
	s, err := e.Store.GetSettings(ctx)
	if err != nil {
		return out, err
	}
	if s.Undo.SecondPerson {
		out.Approval = "second_person"
	}
	ids := make([]string, 0, len(sel))
	for id := range sel {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	people := map[string]bool{}
	var probes []model.Change
	h := sha256.New()
	for _, id := range ids {
		c, err := e.Store.GetChange(ctx, id)
		if err != nil {
			return out, fmt.Errorf("%w: change %s: %v", ErrInvalid, id, err)
		}
		if c.RunID != runID {
			return out, fmt.Errorf("%w: change %s was not made by this run", ErrInvalid, id)
		}
		var want []string
		for _, k := range sel[id] {
			if k != "*" {
				want = append(want, k)
			}
		}
		plan, err := e.PlanUndoItems(ctx, id, want)
		if err != nil {
			return out, about(err, c.Display+" ("+c.Type+")")
		}
		if !plan.Undoable {
			return out, fmt.Errorf("%w: %s (%s) cannot be undone: %s", store.ErrConflict, c.Display, c.Type, plan.Reason)
		}
		out.previews = append(out.previews, UndoPreview{UndoPlan: plan, Approval: out.Approval})
		probes = append(probes, model.Change{ID: "undo-preview-" + c.ID, IdentityID: c.IdentityID, ExternalID: c.ExternalID, Display: c.Display,
			Type: model.ChangeUndo, Payload: plan.Payload})
		people[c.IdentityID] = true
		h.Write([]byte(id + "\x00" + plan.Hash + "\x00"))
	}
	// One dry run per script for the whole selection, in executor-sized chunks.
	results := make([]ad.Result, 0, len(probes))
	for i := 0; i < len(probes); i += ExecuteChunk {
		end := min(i+ExecuteChunk, len(probes))
		results = append(results, e.runUndo(ctx, s, probes[i:end], true)...)
	}
	for i := range out.previews {
		pv := &out.previews[i]
		r := results[i]
		pv.DryRun = UndoDryRun{Status: r.Status, Message: r.Message, Error: r.Error}
		pv.Drift = driftOf(r.Error)
		switch {
		case len(pv.Drift) > 0:
			out.DriftCount++
		case r.Status == ad.StatusFailed:
			out.FailedCount++
		}
		c := pv.Change
		out.Changes = append(out.Changes, RunUndoPreviewEntry{
			ChangeID: c.ID, Type: c.Type, Display: c.Display, ExternalID: c.ExternalID, IdentityID: c.IdentityID,
			ConfirmText: pv.ConfirmText, Items: orNone(pv.Payload.Undo.Items), Steps: orNone(pv.Steps), DryRun: pv.DryRun,
			Drift: orNone(pv.Drift), Holds: orNoneHolds(pv.Holds), Warnings: orNone(pv.Warnings),
		})
	}
	out.People, out.Count = len(people), len(out.Changes)
	out.ConfirmText = undoBatchConfirm(len(out.Changes))
	out.Hash = hex.EncodeToString(h.Sum(nil))
	if out.DriftCount > 0 {
		out.Warnings = append(out.Warnings, fmt.Sprintf("%d undo(s) would overwrite later changes; confirm overwriting them or leave those changes out.", out.DriftCount))
	}
	if out.FailedCount > 0 {
		out.Warnings = append(out.Warnings, fmt.Sprintf("%d undo(s) failed in the dry run for another reason; leave them out or fix the cause.", out.FailedCount))
	}
	if s.Undo.SecondPerson {
		out.Warnings = append(out.Warnings, "Someone other than you must approve these undos (undo.second_person).")
	}
	return out, nil
}

func driftOf(errText string) []string {
	msg, ok := strings.CutPrefix(errText, "drift: ")
	if !ok {
		return nil
	}
	var out []string
	for _, d := range strings.Split(msg, "; ") {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, d)
		}
	}
	return out
}

func orNone(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func orNoneHolds(v []UndoHoldOption) []UndoHoldOption {
	if v == nil {
		return []UndoHoldOption{}
	}
	return v
}

// RequestRunUndo queues the selected undos of run runID as one batch. Every undo still waits for approval.
func (e *Engine) RequestRunUndo(ctx context.Context, runID, by string, in RunUndoRequest) (string, []model.Change, error) {
	pv, err := e.PreviewRunUndo(ctx, runID, in.Selection)
	if err != nil {
		return "", nil, err
	}
	if in.PreviewHash != pv.Hash {
		return "", nil, fmt.Errorf("%w: the undo changed since you previewed it; preview again", store.ErrConflict)
	}
	if !strings.EqualFold(strings.Join(strings.Fields(in.Confirm), " "), pv.ConfirmText) {
		return "", nil, fmt.Errorf("%w: type %s to confirm the undo", ErrInvalid, pv.ConfirmText)
	}
	reason := strings.TrimSpace(in.Reason)
	if len([]rune(reason)) < MinUndoReason {
		return "", nil, fmt.Errorf("%w: give a reason of at least %d characters", ErrInvalid, MinUndoReason)
	}
	switch {
	case pv.DriftCount > 0 && !in.Force:
		return "", nil, fmt.Errorf("%w: %d undo(s) would overwrite later changes; preview again and confirm overwriting them", store.ErrConflict, pv.DriftCount)
	case pv.FailedCount > 0:
		var why []string
		for _, p := range pv.previews {
			if len(p.Drift) == 0 && p.DryRun.Status == ad.StatusFailed {
				why = append(why, fmt.Sprintf("%s: %s", p.Change.Display, firstNonEmpty(p.DryRun.Error, p.DryRun.Message, "unknown error")))
			}
		}
		return "", nil, fmt.Errorf("%w: the dry run failed for %s; leave them out or fix the cause", store.ErrConflict, strings.Join(why, "; "))
	}
	manual := in.Manual == nil || *in.Manual
	batch := uuid.NewString()
	var out []model.Change
	for _, p := range pv.previews {
		var holds []model.HoldSpec
		if manual {
			for _, o := range p.Holds {
				holds = append(holds, model.HoldSpec{Kind: o.Kind, Key: o.Key})
			}
		}
		c, err := e.queueUndo(ctx, p, by, reason, in.Force && len(p.Drift) > 0, holds, batch)
		if err != nil {
			return batch, out, err
		}
		out = append(out, c)
	}
	ids := make([]string, len(out))
	for i, c := range out {
		ids[i] = c.ID
	}
	e.audit(ctx, by, "undo.batch_requested", "undo_batch", batch, map[string]any{
		"run": runID, "count": len(out), "people": pv.People, "reason": reason, "force": in.Force && pv.DriftCount > 0, "manual": manual, "changes": ids,
	})
	return batch, out, nil
}

// UndoBatchEntry is one undo of a batch; pending ones carry a fresh dry run.
type UndoBatchEntry struct {
	ChangeID    string       `json:"change_id"`
	Change      model.Change `json:"change"`
	Display     string       `json:"display"`
	ExternalID  string       `json:"external_id"`
	IdentityID  string       `json:"identity_id"`
	Status      string       `json:"status"`
	ConfirmText string       `json:"confirm_text"`
	Items       []string     `json:"items"`
	Steps       []string     `json:"steps"`
	DryRun      *UndoDryRun  `json:"dry_run"` // nil when the undo is no longer pending
	Drift       []string     `json:"drift"`
	Forced      bool         `json:"forced,omitempty"` // the request confirmed overwriting later changes
}

// UndoBatchView is a batch of undos as the approval dialog shows it, with a fresh dry run of the pending ones.
type UndoBatchView struct {
	Batch       string           `json:"batch"`
	RunID       string           `json:"run_id"`
	RequestedBy string           `json:"requested_by"`
	Reason      string           `json:"reason"`
	CreatedAt   time.Time        `json:"created_at"`
	Force       bool             `json:"force"`
	Total       int              `json:"total"`
	Pending     int              `json:"pending"`
	Changes     []UndoBatchEntry `json:"changes"`
	DriftCount  int              `json:"drift_count"`
	FailedCount int              `json:"failed_count"`
	ConfirmText string           `json:"confirm_text"` // "UNDO <pending>"
	Approval    string           `json:"approval"`
}

// undoBatch returns every undo change of batch, oldest first.
func (e *Engine) undoBatch(ctx context.Context, batch string) ([]model.Change, error) {
	all, err := e.allChanges(ctx, store.ChangeFilter{Type: model.ChangeUndo})
	if err != nil {
		return nil, err
	}
	var out []model.Change
	for _, c := range all {
		if c.Payload.Undo != nil && c.Payload.Undo.Batch == batch && batch != "" {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil, store.ErrNotFound
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// CheckUndoBatch shows a batch and dry-runs its pending undos again.
func (e *Engine) CheckUndoBatch(ctx context.Context, batch string) (UndoBatchView, error) {
	v := UndoBatchView{Batch: batch, Changes: []UndoBatchEntry{}, Approval: "manual"}
	list, err := e.undoBatch(ctx, batch)
	if err != nil {
		return v, err
	}
	s, err := e.Store.GetSettings(ctx)
	if err != nil {
		return v, err
	}
	if s.Undo.SecondPerson {
		v.Approval = "second_person"
	}
	v.Total, v.CreatedAt = len(list), list[0].CreatedAt
	var pending []model.Change
	at := map[string]int{}
	for _, c := range list {
		u := c.Payload.Undo
		v.RequestedBy, v.Reason = u.RequestedBy, u.Reason
		v.Force = v.Force || u.Force
		at[c.ID] = len(v.Changes)
		v.Changes = append(v.Changes, UndoBatchEntry{
			ChangeID: c.ID, Change: c, Display: c.Display, ExternalID: c.ExternalID, IdentityID: c.IdentityID, Status: c.Status,
			ConfirmText: u.ConfirmText, Items: orNone(u.Items), Steps: orNone(undoSteps(c.Payload, u.ConfirmText, s.AD.IDAttribute)),
			Drift: []string{}, Forced: u.Force,
		})
		if c.Status == model.StatusPending {
			pending = append(pending, c)
		}
	}
	if orig, err := e.Store.GetChange(ctx, list[0].Payload.Undo.Of); err == nil {
		v.RunID = orig.RunID
	}
	v.Pending = len(pending)
	v.ConfirmText = undoBatchConfirm(len(pending))
	for i := 0; i < len(pending); i += ExecuteChunk {
		end := min(i+ExecuteChunk, len(pending))
		res := e.runUndo(ctx, s, pending[i:end], true)
		for j, c := range pending[i:end] {
			r := res[j]
			ent := &v.Changes[at[c.ID]]
			ent.DryRun = &UndoDryRun{Status: r.Status, Message: r.Message, Error: r.Error}
			ent.Drift = orNone(driftOf(r.Error))
			switch {
			case len(ent.Drift) > 0 && !ent.Forced:
				v.DriftCount++
			case len(ent.Drift) == 0 && r.Status == ad.StatusFailed:
				v.FailedCount++
			}
		}
	}
	return v, nil
}

// ApproveUndoBatch approves every pending undo of a batch at once. confirm must be "UNDO <n>" (n = pending undos);
// with undo.second_person the approver must not be the requester; an undo that would now overwrite later changes
// (and was not forced), or whose dry run fails, blocks the batch (approve or reject those one at a time).
func (e *Engine) ApproveUndoBatch(ctx context.Context, batch, by, note, confirm string) ([]model.Change, error) {
	v, err := e.CheckUndoBatch(ctx, batch)
	if err != nil {
		return nil, err
	}
	s, err := e.Store.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	switch {
	case !s.Undo.Enabled:
		return nil, fmt.Errorf("%w: undo is turned off in Settings", ErrInvalid)
	case v.Pending == 0:
		return nil, fmt.Errorf("%w: nothing in this batch is waiting for approval", store.ErrConflict)
	case !strings.EqualFold(strings.Join(strings.Fields(confirm), " "), v.ConfirmText):
		return nil, fmt.Errorf("%w: type %s to approve this batch", ErrInvalid, v.ConfirmText)
	case s.Undo.SecondPerson && strings.EqualFold(by, v.RequestedBy):
		return nil, fmt.Errorf("%w: undo.second_person is on: someone other than %s must approve these undos", ErrForbidden, v.RequestedBy)
	case v.DriftCount > 0 || v.FailedCount > 0:
		var why []string
		for _, ck := range v.Changes {
			if ck.DryRun != nil && ck.DryRun.Status == ad.StatusFailed && !(ck.Forced && len(ck.Drift) > 0) {
				why = append(why, ck.Display+": "+firstNonEmpty(ck.DryRun.Error, ck.DryRun.Message))
			}
		}
		if len(why) > 0 {
			return nil, fmt.Errorf("%w: %s; approve or reject those undos one at a time", store.ErrConflict, strings.Join(why, "; "))
		}
	}
	var out []model.Change
	for _, ck := range v.Changes {
		if ck.Status != model.StatusPending {
			continue
		}
		c, err := e.decide(ctx, ck.ChangeID, model.StatusPending, model.StatusApproved, by, note, "change.approve")
		if err != nil {
			return out, err
		}
		out = append(out, c)
	}
	e.audit(ctx, by, "undo.batch_approved", "undo_batch", batch, map[string]any{"count": len(out), "note": note, "run": v.RunID})
	return out, nil
}

// RejectUndoBatch rejects every pending undo of a batch (a note is required).
func (e *Engine) RejectUndoBatch(ctx context.Context, batch, by, note string) ([]model.Change, error) {
	if strings.TrimSpace(note) == "" {
		return nil, fmt.Errorf("%w: a note is required when rejecting", ErrInvalid)
	}
	list, err := e.undoBatch(ctx, batch)
	if err != nil {
		return nil, err
	}
	var out []model.Change
	for _, c := range list {
		if c.Status != model.StatusPending {
			continue
		}
		rc, err := e.Reject(ctx, c.ID, by, note)
		if err != nil {
			return out, err
		}
		out = append(out, rc)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: nothing in this batch is waiting for approval", store.ErrConflict)
	}
	e.audit(ctx, by, "undo.batch_rejected", "undo_batch", batch, map[string]any{"count": len(out), "note": note})
	return out, nil
}

// about puts what an error is about after its kind (ErrInvalid / ErrConflict stay first, so the API maps them).
func about(err error, what string) error {
	for _, kind := range []error{ErrInvalid, store.ErrConflict, ErrForbidden} {
		if errors.Is(err, kind) {
			return fmt.Errorf("%w: %s: %s", kind, what, strings.TrimPrefix(err.Error(), kind.Error()+": "))
		}
	}
	return fmt.Errorf("%s: %w", what, err)
}
