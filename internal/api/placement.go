package api

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/engine"
	"idlifecycle/internal/model"
	"idlifecycle/internal/store"
)

// ouCache keeps the OU list for the picker (reading AD takes a PowerShell start).
type ouCache struct {
	mu  sync.Mutex
	key string
	at  time.Time
	ous []ad.OU
}

// handleListOUs: the OUs below ad.search_bases (whole domain when none), cached for 30 minutes; refresh=1 re-reads.
func (s *Server) handleListOUs(w http.ResponseWriter, r *http.Request) {
	settings, err := s.Store.GetSettings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	reader, _ := s.Engine.AD.(ad.OUReader)
	if reader == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ous": []ad.OU{}, "error": "the AD client cannot list OUs"})
		return
	}
	key := settings.AD.Server + "|" + strings.Join(settings.AD.SearchBases, ";")
	c := &s.ous
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.URL.Query().Get("refresh") != "1" && c.key == key && time.Since(c.at) < schemaCacheTTL {
		writeJSON(w, http.StatusOK, map[string]any{"ous": c.ous, "read_at": c.at})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	ous, err := reader.OUs(ctx, settings.AD.Server, settings.AD.SearchBases)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ous": []ad.OU{}, "error": "reading OUs from AD failed: " + err.Error()})
		return
	}
	if ous == nil {
		ous = []ad.OU{}
	}
	c.key, c.at, c.ous = key, time.Now(), ous
	writeJSON(w, http.StatusOK, map[string]any{"ous": ous, "read_at": c.at})
}

// placementValue is one value of the placement field across the active identities.
type placementValue struct {
	Value  string `json:"value"`
	People int    `json:"people"`
	OU     string `json:"ou"` // from the placement rules ("" = unmapped)
}

// handlePlacementValues lists every value of the placement field (department) among active people with how many
// have it and the OU the rules give, unmapped values first - the to-do list for the placement rules.
func (s *Server) handlePlacementValues(w http.ResponseWriter, r *http.Request) {
	settings, err := s.Store.GetSettings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	field := r.URL.Query().Get("field")
	if field == "" {
		field = settings.Placement.Field
	}
	counts := map[string]int{}
	shown := map[string]string{}
	const pageSize = 1000
	for offset := 0; ; offset += pageSize {
		ids, total, err := s.Store.ListIdentities(r.Context(), store.IdentityFilter{}, store.Page{Limit: pageSize, Offset: offset})
		if err != nil {
			s.fail(w, r, err)
			return
		}
		for _, id := range ids {
			if !id.Active {
				continue
			}
			v := ""
			for k, val := range id.Attrs {
				if strings.EqualFold(k, field) {
					v = strings.TrimSpace(val)
				}
			}
			key := strings.ToLower(v)
			counts[key]++
			if _, ok := shown[key]; !ok {
				shown[key] = v
			}
		}
		if offset+pageSize >= total {
			break
		}
	}
	out := make([]placementValue, 0, len(counts))
	for key, n := range counts {
		ou, _ := engine.MatchPlacementRule(settings.Placement.Rules, shown[key])
		out = append(out, placementValue{Value: shown[key], People: n, OU: ou})
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].OU == "") != (out[j].OU == "") {
			return out[i].OU == ""
		}
		return strings.ToLower(out[i].Value) < strings.ToLower(out[j].Value)
	})
	writeJSON(w, http.StatusOK, map[string]any{"field": field, "values": out})
}

// handleSetChangeOU: {ou, add_rule} on a pending create/update/enable. Approvers choose the OU for one person;
// adding a placement rule for the whole department changes settings, so that part is for admins.
func (s *Server) handleSetChangeOU(w http.ResponseWriter, r *http.Request) {
	var in struct {
		OU      string `json:"ou"`
		AddRule bool   `json:"add_rule"`
	}
	if !decode(w, r, &in) {
		return
	}
	p := principal(r)
	if in.AddRule && p.User.Role != model.RoleAdmin {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "only administrators can add placement rules; choose the OU for this person only"})
		return
	}
	c, err := s.Engine.SetChangeOU(r.Context(), r.PathValue("id"), in.OU, p.User.Username, in.AddRule)
	switch {
	case errors.Is(err, engine.ErrInvalid):
		badRequest(w, strings.TrimPrefix(err.Error(), engine.ErrInvalid.Error()+": "))
		return
	case err != nil:
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}
