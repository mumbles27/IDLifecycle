package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"idlifecycle/internal/engine"
	"idlifecycle/internal/model"
)

// undoPreviewResponse is GET /api/changes/{id}/undo.
type undoPreviewResponse struct {
	Undoable    bool                    `json:"undoable"`
	Reason      string                  `json:"reason"`
	Change      model.Change            `json:"change"`
	Script      string                  `json:"script"`
	Steps       []string                `json:"steps"`
	DryRun      *engine.UndoDryRun      `json:"dry_run"`
	Drift       []string                `json:"drift"`
	Holds       []engine.UndoHoldOption `json:"holds"`
	ConfirmText string                  `json:"confirm_text"`
	Approval    string                  `json:"approval"`
	Warnings    []string                `json:"warnings"`
	PreviewHash string                  `json:"preview_hash"`
	// Items are every part of an update / cloud_groups change; Selected the parts this preview undoes.
	Items     []engine.UndoItem `json:"items"`
	Selected  []string          `json:"selected"`
	WholeOnly bool              `json:"whole_only"`
}

func orEmpty[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

// handlePreviewUndo plans the undo of an applied change and runs it with -DryRun against AD / the tenant.
func (s *Server) handlePreviewUndo(w http.ResponseWriter, r *http.Request) {
	pv, err := s.Engine.PreviewUndoItems(r.Context(), r.PathValue("id"), itemsParam(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	out := undoPreviewResponse{
		Undoable: pv.Undoable, Reason: pv.Reason, Change: pv.Change, Script: pv.Script, Steps: orEmpty(pv.Steps),
		Drift: orEmpty(pv.Drift), Holds: orEmpty(pv.Holds), ConfirmText: pv.ConfirmText, Approval: pv.Approval,
		Warnings: orEmpty(pv.Warnings), PreviewHash: pv.Hash, Items: orEmpty(pv.Items), Selected: orEmpty(pv.Selected), WholeOnly: pv.WholeOnly,
	}
	if pv.Undoable {
		out.DryRun = &pv.DryRun
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRequestUndo queues an undo for approval (body: reason, confirm, force, holds, preview_hash).
func (s *Server) handleRequestUndo(w http.ResponseWriter, r *http.Request) {
	var in engine.UndoRequest
	if !decode(w, r, &in) {
		return
	}
	c, err := s.Engine.RequestUndo(r.Context(), r.PathValue("id"), principal(r).User.Username, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

// handleUndoCheck re-runs an undo change with -DryRun for the approval dialog.
func (s *Server) handleUndoCheck(w http.ResponseWriter, r *http.Request) {
	dr, drift, steps, confirm, err := s.Engine.UndoCheck(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"dry_run": dr, "drift": orEmpty(drift), "steps": orEmpty(steps), "confirm_text": confirm})
}

func (s *Server) handleReleaseHold(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Note string `json:"note"`
	}
	if r.ContentLength > 0 && !decode(w, r, &in) {
		return
	}
	note := firstNonBlank(in.Note, r.URL.Query().Get("note"))
	h, err := s.Engine.ReleaseHoldNote(r.Context(), r.PathValue("id"), r.PathValue("hid"), principal(r).User.Username, note)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

// itemsParam reads the parts of a change to preview: repeated ?item=<key> (keys may contain commas: group DNs), or
// ?items= as a JSON array (or one key).
func itemsParam(r *http.Request) []string {
	q := r.URL.Query()
	out := append([]string{}, q["item"]...)
	for _, v := range q["items"] {
		v = strings.TrimSpace(v)
		var list []string
		if strings.HasPrefix(v, "[") && json.Unmarshal([]byte(v), &list) == nil {
			out = append(out, list...)
		} else if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// ---- undo across a run ---------------------------------------------------------------------------------------

func (s *Server) handleRunUndoList(w http.ResponseWriter, r *http.Request) {
	run, list, err := s.Engine.RunUndoList(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run, "changes": orEmpty(list)})
}

func (s *Server) handleRunUndoPreview(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Selection engine.RunUndoSelection `json:"selection"`
	}
	if !decode(w, r, &in) {
		return
	}
	pv, err := s.Engine.PreviewRunUndo(r.Context(), r.PathValue("id"), in.Selection)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, pv)
}

func (s *Server) handleRunUndoRequest(w http.ResponseWriter, r *http.Request) {
	var in engine.RunUndoRequest
	if !decode(w, r, &in) {
		return
	}
	batch, list, err := s.Engine.RequestRunUndo(r.Context(), r.PathValue("id"), principal(r).User.Username, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"batch": batch, "changes": orEmpty(list)})
}

func (s *Server) handleUndoBatch(w http.ResponseWriter, r *http.Request) {
	v, err := s.Engine.CheckUndoBatch(r.Context(), r.PathValue("batch"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) handleUndoBatchDecision(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Note    string `json:"note"`
			Confirm string `json:"confirm"`
		}
		if r.ContentLength != 0 && !decode(w, r, &in) {
			return
		}
		by, batch := principal(r).User.Username, r.PathValue("batch")
		var list []model.Change
		var err error
		if approve {
			list, err = s.Engine.ApproveUndoBatch(r.Context(), batch, by, strings.TrimSpace(in.Note), in.Confirm)
		} else {
			list, err = s.Engine.RejectUndoBatch(r.Context(), batch, by, strings.TrimSpace(in.Note))
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		key := "rejected"
		if approve {
			key = "approved"
		}
		writeJSON(w, http.StatusOK, map[string]any{"batch": batch, key: orEmpty(list)})
	}
}

// ---- Manual pins -------------------------------------------------------------------------------------------------

func (s *Server) handlePinManual(w http.ResponseWriter, r *http.Request) {
	var in engine.PinRequest
	if !decode(w, r, &in) {
		return
	}
	h, err := s.Engine.PinManual(r.Context(), r.PathValue("id"), principal(r).User.Username, in)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, h)
}

func (s *Server) handleManaged(w http.ResponseWriter, r *http.Request) {
	v, err := s.Engine.Managed(r.Context(), r.PathValue("id"), r.URL.Query().Get("refresh") == "1")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func firstNonBlank(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
