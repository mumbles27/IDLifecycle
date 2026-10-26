package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/engine"
	"idlifecycle/internal/sources"
)

// The field mapper shows a source's fields next to the AD user attributes and flags mappings that read
// fields the source does not send. Both endpoints are admin-only: one fetches live HR data, the other
// runs PowerShell.

const (
	schemaCacheTTL = 30 * time.Minute
	maxSamples     = 3
	maxSampleLen   = 60
)

type schemaCache struct {
	server  string
	at      time.Time
	attrs   []ad.SchemaAttribute
	liveErr string
}

type adAttribute struct {
	Name         string `json:"name"`
	Description  string `json:"description"`
	Syntax       string `json:"syntax"`
	SingleValued bool   `json:"single_valued"`
	Mappable     bool   `json:"mappable"`
	Reason       string `json:"reason,omitempty"`
	Hint         string `json:"hint,omitempty"`
}

// handleADAttributes lists the user-class attributes: from the live schema when the AD client can read it,
// otherwise the built-in catalog (live_error says why). ?refresh=1 skips the 30-minute cache.
func (s *Server) handleADAttributes(w http.ResponseWriter, r *http.Request) {
	settings, err := s.Store.GetSettings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	c := s.schema(r.Context(), settings.AD.Server, r.URL.Query().Get("refresh") == "1")

	builtin := map[string]string{}
	for _, a := range ad.BuiltinUserAttributes() {
		builtin[strings.ToLower(a.Name)] = a.Description
	}
	source, attrs := "live", c.attrs
	if c.liveErr != "" {
		source, attrs = "builtin", ad.BuiltinUserAttributes()
	}
	out := make([]adAttribute, 0, len(attrs))
	for _, a := range attrs {
		ok, reason, hint := engine.ClassifyAttribute(a)
		desc := a.Description
		if d := builtin[strings.ToLower(a.Name)]; d != "" {
			desc = d // friendlier than the schema's adminDescription
		}
		out = append(out, adAttribute{Name: a.Name, Description: desc, Syntax: a.Syntax, SingleValued: a.SingleValued, Mappable: ok, Reason: reason, Hint: hint})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	writeJSON(w, http.StatusOK, map[string]any{"source": source, "live_error": c.liveErr, "fetched_at": c.at.UTC(), "attributes": out})
}

func (s *Server) schema(ctx context.Context, server string, refresh bool) schemaCache {
	s.schemaMu.Lock()
	defer s.schemaMu.Unlock()
	if !refresh && s.schemaCached != nil && s.schemaCached.server == server && time.Since(s.schemaCached.at) < schemaCacheTTL {
		return *s.schemaCached
	}
	c := schemaCache{server: server, at: time.Now()}
	var reader ad.SchemaReader
	if s.Engine != nil {
		reader, _ = s.Engine.AD.(ad.SchemaReader)
	}
	if reader == nil {
		c.liveErr = "the AD client cannot read the schema"
	} else {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		attrs, err := reader.Schema(ctx, server)
		switch {
		case err != nil:
			c.liveErr = err.Error()
			var se *ad.ScriptError
			if errors.As(err, &se) && se.Message != "" {
				c.liveErr = se.Script + ": " + se.Message
			}
		case len(attrs) == 0:
			c.liveErr = "the schema read returned no attributes"
		default:
			c.attrs = attrs
		}
	}
	if c.liveErr != "" {
		s.Log.Warn("AD schema not available; using the built-in attribute list", "error", c.liveErr)
	}
	s.schemaCached = &c
	return c
}

// handleFieldReferences lists the source fields every mapping and role rule reads, without fetching a
// source; the GUI joins it with a field list it already has after each save.
func (s *Server) handleFieldReferences(w http.ResponseWriter, r *http.Request) {
	mappings, err := s.Store.ListMappings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	roles, err := s.Store.ListRoles(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"references": engine.FieldReferences(mappings, roles)})
}

type sourceField struct {
	Name    string            `json:"name"`
	Filled  int               `json:"filled"`
	Samples []string          `json:"samples"`
	UsedBy  []engine.FieldRef `json:"used_by"`
	Role    string            `json:"role,omitempty"` // external_id | status
}

type fieldRefStatus struct {
	engine.FieldRef
	Missing     []string          `json:"missing"`
	Suggestions map[string]string `json:"suggestions"` // missing field -> same name in another case
}

// handleSourceFields fetches the source once and reports every field (fill count and a few sample
// values), which mappings and role rules read it, and which referenced fields the source does not send.
func (s *Server) handleSourceFields(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	src, err := s.Store.GetSource(ctx, r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	mappings, err := s.Store.ListMappings(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	roles, err := s.Store.ListRoles(ctx)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	res := map[string]any{"ok": false, "error": "", "count": 0, "id_field": src.Options.ExternalIDField, "status_field": src.Options.StatusField,
		"fields": []sourceField{}, "references": []fieldRefStatus{}}

	fctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	built, err := sources.BuildFor(src, s.Cipher, s.Store.ListManualPeople)
	var rows []sources.Row
	if err == nil {
		rows, err = built.Fetch(fctx)
	}
	if err != nil {
		res["error"] = err.Error()
		s.audit(r, "source.fields", "source", src.ID, map[string]any{"ok": false})
		writeJSON(w, http.StatusOK, res)
		return
	}

	byName := map[string]*sourceField{}
	for _, row := range rows {
		for k, v := range row {
			f := byName[k]
			if f == nil {
				f = &sourceField{Name: k, Samples: []string{}, UsedBy: []engine.FieldRef{}}
				byName[k] = f
			}
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			f.Filled++
			if len(f.Samples) < maxSamples && !contains(f.Samples, clip(v)) {
				f.Samples = append(f.Samples, clip(v))
			}
		}
	}
	lower := map[string]string{}
	for k := range byName {
		lower[strings.ToLower(k)] = k
	}
	if f := byName[src.Options.ExternalIDField]; f != nil {
		f.Role = "external_id"
	}
	if f := byName[src.Options.StatusField]; f != nil && f.Role == "" {
		f.Role = "status"
	}

	refs := engine.FieldReferences(mappings, roles)
	status := make([]fieldRefStatus, 0, len(refs))
	for _, ref := range refs {
		st := fieldRefStatus{FieldRef: ref, Missing: []string{}, Suggestions: map[string]string{}}
		for _, name := range ref.Fields {
			f := byName[name]
			if f == nil {
				st.Missing = append(st.Missing, name)
				if other, ok := lower[strings.ToLower(name)]; ok {
					st.Suggestions[name] = other
				}
				continue
			}
			f.UsedBy = append(f.UsedBy, engine.FieldRef{Kind: ref.Kind, ID: ref.ID, Name: ref.Name, TargetAttr: ref.TargetAttr, Enabled: ref.Enabled, Fields: []string{}})
		}
		status = append(status, st)
	}

	fields := make([]sourceField, 0, len(byName))
	for _, f := range byName {
		fields = append(fields, *f)
	}
	sort.Slice(fields, func(i, j int) bool { return strings.ToLower(fields[i].Name) < strings.ToLower(fields[j].Name) })

	res["ok"], res["count"], res["fields"], res["references"] = true, len(rows), fields, status
	if len(rows) == 0 {
		res["ok"], res["error"] = false, "the source returned no records, so it has no fields to show"
	}
	s.audit(r, "source.fields", "source", src.ID, map[string]any{"ok": res["ok"], "count": len(rows)})
	writeJSON(w, http.StatusOK, res)
}

func clip(v string) string {
	if utf8.RuneCountInString(v) <= maxSampleLen {
		return v
	}
	return string([]rune(v)[:maxSampleLen-1]) + "…"
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
