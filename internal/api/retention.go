package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"idlifecycle/internal/engine"
	"idlifecycle/internal/model"
)

// handleListRetention: GET /api/retention-policies - every policy in evaluation order, with how many people the last
// sync resolved to each (the built-in Default is not in the list: it is engine.delete_after_days / ad.disabled_ou).
func (s *Server) handleListRetention(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.ListRetentionPolicies(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	counts, err := s.Store.RetentionCounts(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for i := range list {
		list[i].People = counts[list[i].ID]
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": list, "total": len(list), "default_people": counts[model.RetentionDefaultID]})
}

func (s *Server) handleGetRetention(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.GetRetentionPolicy(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if counts, err := s.Store.RetentionCounts(r.Context()); err == nil {
		p.People = counts[p.ID]
	}
	writeJSON(w, http.StatusOK, p)
}

// handleSaveRetention: POST /api/retention-policies (create) and PUT /api/retention-policies/{id} (update).
func (s *Server) handleSaveRetention(w http.ResponseWriter, r *http.Request) {
	var in model.RetentionPolicy
	if !decode(w, r, &in) {
		return
	}
	engine.NormalizeRetentionPolicy(&in)
	if err := engine.ValidateRetentionPolicy(in); err != nil {
		badRequest(w, err.Error())
		return
	}
	in.People = 0
	id := r.PathValue("id")
	if id == "" {
		now := time.Now()
		in.ID, in.CreatedAt, in.UpdatedAt = uuid.NewString(), now, now
		if err := s.Store.CreateRetentionPolicy(r.Context(), in); err != nil {
			s.fail(w, r, err)
			return
		}
		s.audit(r, "retention.create", "retention_policy", in.ID, map[string]any{"name": in.Name, "fields": auditSnapshot(in, in)})
		writeJSON(w, http.StatusCreated, in)
		return
	}
	prev, err := s.Store.GetRetentionPolicy(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	in.ID = id
	if err := s.Store.UpdateRetentionPolicy(r.Context(), in); err != nil {
		s.fail(w, r, err)
		return
	}
	out, _ := s.Store.GetRetentionPolicy(r.Context(), id)
	s.audit(r, "retention.update", "retention_policy", id, withChanges(map[string]any{"name": in.Name}, auditChanges(prev, out, prev, out)))
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDeleteRetention(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	prev, err := s.Store.GetRetentionPolicy(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.Store.DeleteRetentionPolicy(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "retention.delete", "retention_policy", id, map[string]any{"name": prev.Name, "fields": auditSnapshot(prev, prev)})
	w.WriteHeader(http.StatusNoContent)
}

// handleTestRetention: POST /api/retention-policies/test {policy, identity_id?, record?} - does an (unsaved) policy
// match this person, condition by condition.
func (s *Server) handleTestRetention(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Policy     model.RetentionPolicy `json:"policy"`
		IdentityID string                `json:"identity_id"`
		Record     map[string]string     `json:"record"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.IdentityID == "" && in.Record == nil {
		badRequest(w, "identity_id or record is required")
		return
	}
	out, err := s.Engine.TestRetentionPolicy(r.Context(), in.Policy, in.IdentityID, in.Record)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRetentionUpcoming: GET /api/retention/upcoming?days=30 - accounts due for deletion and people leaving soon.
func (s *Server) handleRetentionUpcoming(w http.ResponseWriter, r *http.Request) {
	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 3650 {
			badRequest(w, "days must be between 1 and 3650")
			return
		}
		days = n
	}
	kind := r.URL.Query().Get("kind")
	if kind != "" && kind != "delete" && kind != "leaving" {
		badRequest(w, "kind must be delete or leaving")
		return
	}
	all, err := s.Engine.RetentionUpcoming(r.Context(), days)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	items := all[:0:0]
	for _, it := range all {
		if kind == "" || it.Kind == kind {
			items = append(items, it)
		}
	}
	p := pageOf(r)
	if p.Limit <= 0 || p.Limit > 500 {
		p.Limit = min(max(p.Limit, 0), 500)
		if p.Limit == 0 {
			p.Limit = 50
		}
	}
	p.Offset = max(p.Offset, 0)
	total := len(items)
	start := min(p.Offset, total)
	end := min(start+p.Limit, total)
	list(w, items[start:end], total)
}

// handleSetIdentityRetention: PUT /api/identities/{id}/retention {policy_id ("" = automatic, "default", or an id), reason}.
func (s *Server) handleSetIdentityRetention(w http.ResponseWriter, r *http.Request) {
	var in struct {
		PolicyID string `json:"policy_id"`
		Reason   string `json:"reason"`
	}
	if !decode(w, r, &in) {
		return
	}
	id, err := s.Engine.SetIdentityRetention(r.Context(), r.PathValue("id"), in.PolicyID, in.Reason, principal(r).User.Username)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ret, err := s.Engine.IdentityRetentionInfo(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"identity_id": id.ID, "retention_policy_id": id.RetentionPolicyID, "retention": ret})
}
