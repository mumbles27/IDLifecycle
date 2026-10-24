package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"idlifecycle/internal/engine"
	"idlifecycle/internal/model"
	"idlifecycle/internal/notify"
	"idlifecycle/internal/sources"
	"idlifecycle/internal/store"
)

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	s.route(mux, "POST /api/auth/logout", model.RoleViewer, s.handleLogout)
	s.route(mux, "GET /api/auth/me", model.RoleViewer, s.handleMe)
	s.route(mux, "POST /api/auth/password", model.RoleViewer, s.handleChangePassword)

	s.route(mux, "GET /api/status", model.RoleViewer, s.handleStatus)
	s.route(mux, "GET /api/source-types", model.RoleViewer, s.handleSourceTypes)

	s.route(mux, "GET /api/sources", model.RoleViewer, s.handleListSources)
	s.route(mux, "POST /api/sources", model.RoleAdmin, s.handleSaveSource)
	s.route(mux, "GET /api/sources/{id}", model.RoleViewer, s.handleGetSource)
	s.route(mux, "PUT /api/sources/{id}", model.RoleAdmin, s.handleSaveSource)
	s.route(mux, "DELETE /api/sources/{id}", model.RoleAdmin, s.handleDeleteSource)
	s.route(mux, "POST /api/sources/{id}/test", model.RoleAdmin, s.handleTestSource)

	s.route(mux, "GET /api/mappings", model.RoleViewer, s.handleListMappings)
	s.route(mux, "POST /api/mappings", model.RoleAdmin, s.handleSaveMapping)
	s.route(mux, "GET /api/mappings/{id}", model.RoleViewer, s.handleGetMapping)
	s.route(mux, "PUT /api/mappings/{id}", model.RoleAdmin, s.handleSaveMapping)
	s.route(mux, "DELETE /api/mappings/{id}", model.RoleAdmin, s.handleDeleteMapping)
	s.route(mux, "POST /api/templates/preview", model.RoleAdmin, s.handleTemplatePreview)

	s.route(mux, "GET /api/roles", model.RoleViewer, s.handleListRoles)
	s.route(mux, "POST /api/roles", model.RoleAdmin, s.handleSaveRole)
	s.route(mux, "GET /api/roles/{id}", model.RoleViewer, s.handleGetRole)
	s.route(mux, "PUT /api/roles/{id}", model.RoleAdmin, s.handleSaveRole)
	s.route(mux, "DELETE /api/roles/{id}", model.RoleAdmin, s.handleDeleteRole)
	s.route(mux, "POST /api/roles/preview", model.RoleAdmin, s.handleRolePreview)

	s.route(mux, "GET /api/settings", model.RoleViewer, s.handleGetSettings)
	s.route(mux, "PUT /api/settings", model.RoleAdmin, s.handlePutSettings)
	s.route(mux, "POST /api/notifications/test", model.RoleAdmin, s.handleTestNotification)

	s.route(mux, "GET /api/changes", model.RoleViewer, s.handleListChanges)
	s.route(mux, "POST /api/changes/bulk", model.RoleApprover, s.handleBulkChanges)
	s.route(mux, "GET /api/changes/{id}", model.RoleViewer, s.handleGetChange)
	s.route(mux, "POST /api/changes/{id}/approve", model.RoleApprover, s.decision("approve"))
	s.route(mux, "POST /api/changes/{id}/reject", model.RoleApprover, s.decision("reject"))
	s.route(mux, "POST /api/changes/{id}/retry", model.RoleApprover, s.decision("retry"))

	s.route(mux, "GET /api/runs", model.RoleViewer, s.handleListRuns)
	s.route(mux, "POST /api/runs", model.RoleAdmin, s.handleStartRun)
	s.route(mux, "GET /api/runs/{id}", model.RoleViewer, s.handleGetRun)

	s.route(mux, "GET /api/identities", model.RoleViewer, s.handleListIdentities)
	s.route(mux, "GET /api/identities/{id}", model.RoleViewer, s.handleGetIdentity)

	s.route(mux, "GET /api/audit", model.RoleViewer, s.handleListAudit)
	s.route(mux, "GET /api/audit/verify", model.RoleViewer, s.handleVerifyAudit)

	s.route(mux, "GET /api/tables", model.RoleViewer, s.handleTables)
	s.route(mux, "GET /api/tables/{name}", model.RoleViewer, s.handleBrowseTable)

	s.route(mux, "GET /api/users", model.RoleAdmin, s.handleListUsers)
	s.route(mux, "POST /api/users", model.RoleAdmin, s.handleCreateUser)
	s.route(mux, "PUT /api/users/{id}", model.RoleAdmin, s.handleUpdateUser)
	s.route(mux, "DELETE /api/users/{id}", model.RoleAdmin, s.handleDeleteUser)

	// Unknown API paths must 404 as JSON rather than fall through to the SPA.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "not_found", "no such endpoint")
	})
}

// ---- status ------------------------------------------------------------------------

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	settings, err := s.Store.GetSettings(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	pending, err := s.Store.CountChanges(ctx, model.StatusPending)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	mode := "live"
	if settings.Engine.DryRun {
		mode = "dry_run"
	}
	sched := map[string]any{"enabled": settings.Schedule.Enabled, "next_run": nil}
	if t, ok := s.Scheduler.Next(); ok && settings.Schedule.Enabled {
		sched["next_run"] = t.UTC()
	}

	out := map[string]any{
		"version": s.Version, "db_driver": s.Store.Driver(), "mode": mode, "approval": settings.Approval,
		"scheduler": sched, "pending_changes": pending, "running_run": nil, "last_run": nil,
	}
	if last, err := s.Store.LatestRun(ctx); err == nil {
		out["last_run"] = last
		if last.Status == model.RunRunning {
			out["running_run"] = last
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- sources -----------------------------------------------------------------------

func (s *Server) handleSourceTypes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, sources.Types())
}

func (s *Server) maskSource(src model.Source) model.Source {
	src.Config = sources.MaskConfig(src.Type, src.Config)
	return src
}

func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.ListSources(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	for i := range items {
		items[i] = s.maskSource(items[i])
	}
	list(w, items, len(items))
}

func (s *Server) handleGetSource(w http.ResponseWriter, r *http.Request) {
	src, err := s.Store.GetSource(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, s.maskSource(src))
}

func (s *Server) handleSaveSource(w http.ResponseWriter, r *http.Request) {
	var in model.Source
	if !decode(w, r, &in) {
		return
	}
	id := r.PathValue("id")
	var prev *model.Source
	if id != "" {
		cur, err := s.Store.GetSource(r.Context(), id)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		prev = &cur
		in.Type = cur.Type // the type of an existing source cannot change
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Config == nil {
		in.Config = map[string]any{}
	}
	if in.Options.TerminatedValues == nil {
		in.Options.TerminatedValues = []string{}
	}
	if err := sources.Validate(in); err != nil {
		badRequest(w, err.Error())
		return
	}
	var prevCfg map[string]any
	if prev != nil {
		prevCfg = prev.Config
	}
	if prev != nil && sources.EndpointChangedWithoutSecret(in.Type, prevCfg, in.Config) {
		badRequest(w, "re-enter the secret when changing the endpoint URL (a stored secret is never re-sent to a new address)")
		return
	}
	cfg, err := sources.PrepareConfigForStorage(in.Type, in.Config, prevCfg, s.Cipher)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	in.Config = cfg

	now := time.Now()
	if prev == nil {
		in.ID, in.CreatedAt, in.UpdatedAt, in.LastStatus = uuid.NewString(), now, now, "never"
		if err := s.Store.CreateSource(r.Context(), in); err != nil {
			s.fail(w, r, err)
			return
		}
		s.audit(r, "source.create", "source", in.ID, map[string]any{"name": in.Name, "type": in.Type})
		writeJSON(w, http.StatusCreated, s.maskSource(in))
		return
	}
	in.ID = prev.ID
	if err := s.Store.UpdateSource(r.Context(), in); err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "source.update", "source", in.ID, map[string]any{"name": in.Name})
	out, _ := s.Store.GetSource(r.Context(), in.ID)
	writeJSON(w, http.StatusOK, s.maskSource(out))
}

func (s *Server) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Store.DeleteSource(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeErr(w, http.StatusConflict, "in_use", "this source still has identities; disable it instead of deleting it")
			return
		}
		s.fail(w, r, err)
		return
	}
	s.audit(r, "source.delete", "source", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTestSource(w http.ResponseWriter, r *http.Request) {
	src, err := s.Store.GetSource(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	res := map[string]any{"ok": false, "count": 0, "attributes": []string{}, "sample": []map[string]string{}, "error": ""}
	built, err := sources.Build(src, s.Cipher)
	var rows []sources.Row
	if err == nil {
		rows, err = built.Fetch(ctx)
	}
	if err != nil {
		res["error"] = err.Error()
		s.audit(r, "source.test", "source", src.ID, map[string]any{"ok": false})
		writeJSON(w, http.StatusOK, res)
		return
	}
	attrs := map[string]bool{}
	for _, row := range rows {
		for k := range row {
			attrs[k] = true
		}
	}
	names := make([]string, 0, len(attrs))
	for k := range attrs {
		names = append(names, k)
	}
	sort.Strings(names)
	sample := make([]map[string]string, 0, 5)
	for i := 0; i < len(rows) && i < 5; i++ {
		sample = append(sample, rows[i])
	}
	idField := src.Options.ExternalIDField
	if idField != "" && len(rows) > 0 {
		if _, ok := rows[0][idField]; !ok {
			res["error"] = "the external id field " + idField + " is not present in the records"
		}
	}
	res["ok"], res["count"], res["attributes"], res["sample"] = res["error"] == "", len(rows), names, sample
	s.audit(r, "source.test", "source", src.ID, map[string]any{"ok": res["ok"], "count": len(rows)})
	writeJSON(w, http.StatusOK, res)
}

// ---- mappings ----------------------------------------------------------------------

func (s *Server) handleListMappings(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.ListMappings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	list(w, items, len(items))
}

func (s *Server) handleGetMapping(w http.ResponseWriter, r *http.Request) {
	m, err := s.Store.GetMapping(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (s *Server) handleSaveMapping(w http.ResponseWriter, r *http.Request) {
	var in model.Mapping
	if !decode(w, r, &in) {
		return
	}
	in.Name, in.TargetAttr = strings.TrimSpace(in.Name), strings.TrimSpace(in.TargetAttr)
	if in.OnEmpty == "" {
		in.OnEmpty = "skip"
	}
	if in.ValueType == "" {
		in.ValueType = "string"
	}
	if err := engine.ValidateMapping(in); err != nil {
		badRequest(w, err.Error())
		return
	}
	id := r.PathValue("id")
	if msg, err := s.mappingConflict(r, in, id); err != nil {
		s.fail(w, r, err)
		return
	} else if msg != "" {
		badRequest(w, msg)
		return
	}
	if id == "" {
		now := time.Now()
		in.ID, in.CreatedAt, in.UpdatedAt = uuid.NewString(), now, now
		if err := s.Store.CreateMapping(r.Context(), in); err != nil {
			s.fail(w, r, err)
			return
		}
		s.audit(r, "mapping.create", "mapping", in.ID, map[string]any{"name": in.Name, "target": in.TargetAttr, "mode": in.Mode})
		writeJSON(w, http.StatusCreated, in)
		return
	}
	in.ID = id
	if err := s.Store.UpdateMapping(r.Context(), in); err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "mapping.update", "mapping", id, map[string]any{"name": in.Name, "target": in.TargetAttr, "mode": in.Mode})
	out, _ := s.Store.GetMapping(r.Context(), id)
	writeJSON(w, http.StatusOK, out)
}

// linkKeyMessage refuses targeting ad.id_attribute: the service sets it from the external id and matches
// accounts by it, so a mapping could re-point an account at another person.
func (s *Server) linkKeyMessage(r *http.Request, attr string) (string, error) {
	st, err := s.Store.GetSettings(r.Context())
	if err != nil {
		return "", err
	}
	if strings.EqualFold(strings.TrimSpace(attr), st.AD.IDAttribute) {
		return attr + " is the link key (ad.id_attribute); the service sets it from the source's external id and it cannot be mapped", nil
	}
	return "", nil
}

// mappingConflict returns a user-facing reason why the mapping cannot be saved ("" when it can).
func (s *Server) mappingConflict(r *http.Request, in model.Mapping, selfID string) (string, error) {
	if msg, err := s.linkKeyMessage(r, in.TargetAttr); msg != "" || err != nil {
		return msg, err
	}
	if !in.Enabled {
		return "", nil
	}
	all, err := s.Store.ListMappings(r.Context())
	if err != nil {
		return "", err
	}
	for _, m := range all {
		if m.ID != selfID && m.Enabled && strings.EqualFold(m.TargetAttr, in.TargetAttr) {
			return fmt.Sprintf("mapping %q already writes %s; disable or edit it instead (two enabled mappings for one attribute would fight each other)", m.Name, m.TargetAttr), nil
		}
	}
	return "", nil
}

func (s *Server) handleDeleteMapping(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Store.DeleteMapping(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "mapping.delete", "mapping", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleTemplatePreview(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Template string            `json:"template"`
		Record   map[string]string `json:"record"`
	}
	if !decode(w, r, &in) {
		return
	}
	out, err := engine.Render(in.Template, in.Record)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "output": "", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "output": out, "error": ""})
}

// ---- roles -------------------------------------------------------------------------

func (s *Server) handleListRoles(w http.ResponseWriter, r *http.Request) {
	items, err := s.Store.ListRoles(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	list(w, items, len(items))
}

func (s *Server) handleGetRole(w http.ResponseWriter, r *http.Request) {
	x, err := s.Store.GetRole(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, x)
}

func (s *Server) handleSaveRole(w http.ResponseWriter, r *http.Request) {
	var in model.RoleRule
	if !decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	if in.Match == "" {
		in.Match = "all"
	}
	if in.Conditions == nil {
		in.Conditions = []model.Condition{}
	}
	if in.Grants.Groups == nil {
		in.Grants.Groups = []string{}
	}
	if in.Grants.Attributes == nil {
		in.Grants.Attributes = []model.AttrGrant{}
	}
	for i := range in.Grants.Attributes {
		a := &in.Grants.Attributes[i]
		a.TargetAttr = strings.TrimSpace(a.TargetAttr)
		a.Mode, a.OnEmpty, a.ValueType = orDefault(a.Mode, model.ModeSync), orDefault(a.OnEmpty, "skip"), orDefault(a.ValueType, "string")
	}
	if err := engine.ValidateRole(in); err != nil {
		badRequest(w, err.Error())
		return
	}
	for _, a := range in.Grants.Attributes {
		if msg, err := s.linkKeyMessage(r, a.TargetAttr); err != nil {
			s.fail(w, r, err)
			return
		} else if msg != "" {
			badRequest(w, "grants.attributes: "+msg)
			return
		}
	}
	id := r.PathValue("id")
	if id == "" {
		now := time.Now()
		in.ID, in.CreatedAt, in.UpdatedAt = uuid.NewString(), now, now
		if err := s.Store.CreateRole(r.Context(), in); err != nil {
			s.fail(w, r, err)
			return
		}
		s.audit(r, "role.create", "role", in.ID, map[string]any{"name": in.Name, "groups": in.Grants.Groups})
		writeJSON(w, http.StatusCreated, in)
		return
	}
	in.ID = id
	if err := s.Store.UpdateRole(r.Context(), in); err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "role.update", "role", id, map[string]any{"name": in.Name, "groups": in.Grants.Groups})
	out, _ := s.Store.GetRole(r.Context(), id)
	writeJSON(w, http.StatusOK, out)
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func (s *Server) handleDeleteRole(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Store.DeleteRole(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "role.delete", "role", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRolePreview(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Record map[string]string `json:"record"`
	}
	if !decode(w, r, &in) {
		return
	}
	rules, err := s.Store.ListRoles(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	mappings, err := s.Store.ListMappings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	res := engine.ResolveRoles(rules, in.Record)
	matched := make([]map[string]string, 0, len(res.Matched))
	for _, m := range res.Matched {
		matched = append(matched, map[string]string{"id": m.ID, "name": m.Name})
	}
	attrs, errs := engine.PreviewAttributes(mappings, res.Attrs, in.Record)
	groups := res.Groups
	if groups == nil {
		groups = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"matched": matched, "groups": groups, "ou": res.OU, "attributes": attrs, "errors": errs})
}

// ---- settings ----------------------------------------------------------------------

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	st, err := s.Store.GetSettings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	st.Notify = notify.Mask(st.Notify)
	writeJSON(w, http.StatusOK, st)
}

// handleTestNotification sends a test message with the (unsaved) notification settings from the form.
// Masked secrets fall back to the stored ones, so a test never needs the secret re-typed.
func (s *Server) handleTestNotification(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Notifications model.NotifySettings `json:"notifications"`
	}
	if !decode(w, r, &in) {
		return
	}
	stored, err := s.Store.GetSettings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	res := map[string]any{"ok": true}
	cfg, err := notify.PrepareForStorage(in.Notifications, stored.Notify, s.Cipher)
	if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
		defer cancel()
		err = s.notifier().Send(ctx, cfg, model.Notification{
			Event: model.EventTest, Subject: "Test message",
			Lines: []string{"This is a test message from IDLifecycle.", "Requested by " + principal(r).User.Username + "."},
		})
	}
	if err != nil {
		res = map[string]any{"ok": false, "error": err.Error()}
	}
	s.audit(r, "notifications.test", "settings", "notifications", map[string]any{"ok": err == nil, "provider": cfg.Provider})
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) notifier() *notify.Service {
	if s.Notifier != nil {
		return s.Notifier
	}
	return &notify.Service{Settings: s.Store.GetSettings, Cipher: s.Cipher, Log: s.Log}
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var in model.Settings
	if !decode(w, r, &in) {
		return
	}
	if err := in.Validate(); err != nil {
		badRequest(w, err.Error())
		return
	}
	for name, tpl := range map[string]string{"naming.sam_template": in.Naming.SamTemplate, "naming.upn_template": in.Naming.UPNTemplate, "naming.cn_template": in.Naming.CNTemplate} {
		if err := engine.ValidateTemplate(tpl); err != nil {
			badRequest(w, name+": "+err.Error())
			return
		}
	}
	if !in.Engine.DryRun {
		if msg := in.ReadyForLive(); msg != "" {
			badRequest(w, "cannot leave dry-run mode: "+msg)
			return
		}
	}
	old, err := s.Store.GetSettings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if in.Notify.To == nil {
		in.Notify.To = []string{}
	}
	if in.Notify, err = notify.PrepareForStorage(in.Notify, old.Notify, s.Cipher); err != nil {
		badRequest(w, "notifications: "+err.Error())
		return
	}
	if err := notify.Validate(in.Notify); err != nil {
		badRequest(w, "notifications: "+err.Error())
		return
	}
	if err := s.Store.SaveSettings(r.Context(), in, principal(r).User.Username); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Scheduler.Reload()
	// diffPaths sees ciphertext for secrets, so a changed secret shows up as a path - never as a value.
	s.audit(r, "settings.update", "settings", "settings", map[string]any{"changed": diffPaths(old, in)})
	in.Notify = notify.Mask(in.Notify)
	writeJSON(w, http.StatusOK, in)
}

// diffPaths lists dotted JSON paths whose values differ (for the audit log).
func diffPaths(a, b any) []string {
	var ma, mb map[string]any
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	_ = json.Unmarshal(ja, &ma)
	_ = json.Unmarshal(jb, &mb)
	var out []string
	var walk func(prefix string, x, y any)
	walk = func(prefix string, x, y any) {
		xm, xok := x.(map[string]any)
		ym, yok := y.(map[string]any)
		if xok && yok {
			keys := map[string]bool{}
			for k := range xm {
				keys[k] = true
			}
			for k := range ym {
				keys[k] = true
			}
			for k := range keys {
				p := k
				if prefix != "" {
					p = prefix + "." + k
				}
				walk(p, xm[k], ym[k])
			}
			return
		}
		xb, _ := json.Marshal(x)
		yb, _ := json.Marshal(y)
		if string(xb) != string(yb) {
			out = append(out, prefix)
		}
	}
	walk("", ma, mb)
	sort.Strings(out)
	return out
}

// ---- changes -----------------------------------------------------------------------

func (s *Server) handleListChanges(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	items, total, err := s.Store.ListChanges(r.Context(), store.ChangeFilter{
		Status: q.Get("status"), Type: q.Get("type"), Q: q.Get("q"), IdentityID: q.Get("identity_id"), RunID: q.Get("run_id"),
	}, pageOf(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	list(w, items, total)
}

func (s *Server) handleGetChange(w http.ResponseWriter, r *http.Request) {
	c, err := s.Store.GetChange(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) decide(r *http.Request, action, id, note string) (model.Change, error) {
	by := principal(r).User.Username
	ctx := r.Context()
	switch action {
	case "approve":
		return s.Engine.Approve(ctx, id, by, note)
	case "reject":
		return s.Engine.Reject(ctx, id, by, note)
	case "retry":
		return s.Engine.Retry(ctx, id, by, note)
	}
	return model.Change{}, errors.New("unknown action")
}

func (s *Server) decision(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Note string `json:"note"`
		}
		if r.ContentLength != 0 && !decode(w, r, &in) {
			return
		}
		c, err := s.decide(r, action, r.PathValue("id"), strings.TrimSpace(in.Note))
		if err != nil {
			s.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, c)
	}
}

func (s *Server) handleBulkChanges(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"`
		Note   string   `json:"note"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Action != "approve" && in.Action != "reject" {
		badRequest(w, "action must be approve or reject")
		return
	}
	if len(in.IDs) == 0 || len(in.IDs) > 500 {
		badRequest(w, "ids must contain between 1 and 500 change ids")
		return
	}
	type result struct {
		ID    string `json:"id"`
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	results := make([]result, 0, len(in.IDs))
	for _, id := range in.IDs {
		_, err := s.decide(r, in.Action, id, strings.TrimSpace(in.Note))
		res := result{ID: id, OK: err == nil}
		if err != nil {
			res.Error = err.Error()
			if errors.Is(err, store.ErrNotFound) {
				res.Error = "not found"
			}
		}
		results = append(results, res)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// ---- runs --------------------------------------------------------------------------

func (s *Server) handleListRuns(w http.ResponseWriter, r *http.Request) {
	items, total, err := s.Store.ListRuns(r.Context(), pageOf(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	list(w, items, total)
}

func (s *Server) handleGetRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.Store.GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleStartRun(w http.ResponseWriter, r *http.Request) {
	run, _, err := s.Engine.Start(s.BaseCtx, "manual", principal(r).User.Username)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"id": run.ID})
}

// ---- identities, audit, tables ---------------------------------------------------------

func (s *Server) handleListIdentities(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	items, total, err := s.Store.ListIdentities(r.Context(), store.IdentityFilter{Q: q.Get("q"), State: q.Get("state")}, pageOf(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	list(w, items, total)
}

func (s *Server) handleGetIdentity(w http.ResponseWriter, r *http.Request) {
	x, err := s.Store.GetIdentity(r.Context(), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, x)
}

func (s *Server) handleListAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	items, total, err := s.Store.ListAudit(r.Context(), store.AuditFilter{
		Actor: q.Get("actor"), Action: q.Get("action"), EntityType: q.Get("entity_type"), EntityID: q.Get("entity_id"),
	}, pageOf(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	list(w, items, total)
}

func (s *Server) handleVerifyAudit(w http.ResponseWriter, r *http.Request) {
	res, err := s.Store.VerifyAudit(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleTables(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"items": store.BrowsableTables()})
}

func (s *Server) handleBrowseTable(w http.ResponseWriter, r *http.Request) {
	v, err := s.Store.BrowseTable(r.Context(), r.PathValue("name"), r.URL.Query().Get("q"), pageOf(r))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}
