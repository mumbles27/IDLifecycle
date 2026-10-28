package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/engine"
	"idlifecycle/internal/model"
)

// handleSearchGroups: GET /api/ad/groups?q=&max= - groups in AD whose name, sAMAccountName or description contains q
// (for the group picker). q is 2-64 characters; '*' is a wildcard.
func (s *Server) handleSearchGroups(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if n := len([]rune(q)); n < 2 || n > 64 {
		badRequest(w, "q must be 2-64 characters")
		return
	}
	if strings.ContainsAny(q, "\r\n\x00") {
		badRequest(w, "q must be one line")
		return
	}
	limit := 25
	if v := r.URL.Query().Get("max"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			badRequest(w, "max must be 1-100")
			return
		}
		limit = n
	}
	searcher, _ := s.Engine.AD.(ad.GroupSearcher)
	if searcher == nil {
		writeJSON(w, http.StatusOK, map[string]any{"groups": []ad.GroupHit{}, "truncated": false, "error": "the AD client cannot search groups"})
		return
	}
	settings, err := s.Store.GetSettings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	groups, truncated, err := searcher.SearchGroups(ctx, ad.GroupSearchRequest{Server: settings.AD.Server, Query: q, Max: limit})
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"groups": []ad.GroupHit{}, "truncated": false, "error": "searching groups in AD failed: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": groups, "truncated": truncated})
}

// handlePreviewImpact: POST /api/preview/impact - what a sync would change with the proposed roles / mappings / settings
// compared to the saved ones (see engine.PreviewImpact). Every part of the body is optional.
func (s *Server) handlePreviewImpact(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Roles       *[]model.RoleRule `json:"roles"`
		Mappings    *[]model.Mapping  `json:"mappings"`
		Settings    json.RawMessage   `json:"settings"`
		Refresh     bool              `json:"refresh"`
		Q           string            `json:"q"`
		OnlyChanged *bool             `json:"only_changed"`
		Type        string            `json:"type"`
		Offset      int               `json:"offset"`
		Limit       int               `json:"limit"`
	}
	if !decode(w, r, &in) {
		return
	}
	req := engine.ImpactRequest{Roles: in.Roles, Mappings: in.Mappings, Refresh: in.Refresh, Q: in.Q, OnlyChanged: true,
		Type: in.Type, Offset: in.Offset, Limit: in.Limit, By: principal(r).User.Username}
	if in.OnlyChanged != nil {
		req.OnlyChanged = *in.OnlyChanged
	}
	if len(in.Settings) > 0 && string(in.Settings) != "null" {
		// The proposed document is read over the saved one, so a partial document changes only what it names.
		saved, err := s.Store.GetSettings(r.Context())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if err := json.Unmarshal(in.Settings, &saved); err != nil {
			badRequest(w, "settings: "+err.Error())
			return
		}
		req.Settings = &saved
	}
	res, err := s.Engine.PreviewImpact(r.Context(), req)
	switch {
	case errors.Is(err, engine.ErrPreviewBusy):
		writeErr(w, http.StatusConflict, "busy", err.Error())
		return
	case errors.Is(err, engine.ErrRunning):
		writeErr(w, http.StatusConflict, "running", "a sync is running; preview again when it has finished")
		return
	case err != nil:
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
