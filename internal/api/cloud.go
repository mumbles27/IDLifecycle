package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"idlifecycle/internal/cloud"
	"idlifecycle/internal/model"
	"idlifecycle/internal/secrets"
)

// prepareCloud encrypts the cloud client secret for storage. A secret sent as model.MaskedSecret or "" keeps the stored
// one, unless the tenant or client ID changed (a secret belongs to one app registration, so it must then be re-entered).
func prepareCloud(in, prev model.CloudSettings, c *secrets.Cipher) (model.CloudSettings, error) {
	out := in
	sameApp := normGUID(in.TenantID) == normGUID(prev.TenantID) && normGUID(in.ClientID) == normGUID(prev.ClientID)
	switch v := in.ClientSecret; {
	case v == model.MaskedSecret || v == "":
		out.ClientSecret = ""
		if sameApp {
			out.ClientSecret = prev.ClientSecret
		}
	case secrets.IsEncrypted(v):
		return out, fmt.Errorf("cloud.client_secret looks like an encrypted value; send the plain secret or %q", model.MaskedSecret)
	default:
		if c == nil {
			return out, errors.New("cloud: no cipher configured")
		}
		enc, err := c.Encrypt(v)
		if err != nil {
			return out, err
		}
		out.ClientSecret = enc
	}
	if out.ClientSecret != "" && !secrets.IsEncrypted(out.ClientSecret) && c != nil {
		enc, err := c.Encrypt(out.ClientSecret)
		if err != nil {
			return out, err
		}
		out.ClientSecret = enc
	}
	return out, nil
}

func maskCloud(c model.CloudSettings) model.CloudSettings {
	if c.ClientSecret != "" {
		c.ClientSecret = model.MaskedSecret
	}
	return c
}

// cloudGroupOut is a group as the API returns it: ref is what a role or lifecycle list stores.
type cloudGroupOut struct {
	cloud.Group
	Ref string `json:"ref"`
}

func cloudGroupsOut(groups []cloud.Group) []cloudGroupOut {
	out := make([]cloudGroupOut, 0, len(groups))
	for _, g := range groups {
		g.ID = strings.ToLower(g.ID)
		out = append(out, cloudGroupOut{Group: g, Ref: model.CloudGroupPrefix + g.ID})
	}
	return out
}

const cloudOffMsg = "the Microsoft 365 connection is not configured (Settings: Microsoft 365 / Entra ID)"

// cloudConn is the saved connection, or a message for the caller when there is none.
func (s *Server) cloudConn(r *http.Request) (cloud.Connection, string, error) {
	st, err := s.Store.GetSettings(r.Context())
	if err != nil {
		return cloud.Connection{}, "", err
	}
	if s.Engine.Cloud == nil || !st.Cloud.Enabled {
		return cloud.Connection{}, cloudOffMsg, nil
	}
	conn, err := cloud.ConnectionFrom(st.Cloud, s.Cipher)
	if err != nil {
		return cloud.Connection{}, err.Error(), nil
	}
	return conn, "", nil
}

// handleSearchCloudGroups: GET /api/cloud/groups?q=&max= - Entra ID groups whose name or mail contains q (Microsoft
// Graph), with their kind and whether this service can change their membership. Read-only.
func (s *Server) handleSearchCloudGroups(w http.ResponseWriter, r *http.Request) {
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
	conn, msg, err := s.cloudConn(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if msg != "" {
		writeJSON(w, http.StatusOK, map[string]any{"groups": []cloudGroupOut{}, "truncated": false, "error": msg})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	groups, truncated, err := s.Engine.Cloud.SearchGroups(ctx, conn, q, limit)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"groups": []cloudGroupOut{}, "truncated": false, "error": "searching Entra ID failed: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": cloudGroupsOut(groups), "truncated": truncated})
}

// maxResolve caps the ids one resolve request may name.
const maxResolve = 50

// handleResolveCloudGroups: GET /api/cloud/groups/resolve?ids=<id>,entra:<id> - names and kinds of cloud groups for the
// chips of a group list: from the last cloud snapshot, the rest from Microsoft Graph. Unknown ids are left out.
func (s *Server) handleResolveCloudGroups(w http.ResponseWriter, r *http.Request) {
	var ids []string
	seen := map[string]bool{}
	for _, raw := range strings.Split(r.URL.Query().Get("ids"), ",") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if !model.IsCloudGroupRef(raw) {
			raw = model.CloudGroupPrefix + raw
		}
		id, ok := model.CloudGroupID(raw)
		if !ok {
			badRequest(w, fmt.Sprintf("ids: %q is not a group object id", strings.TrimPrefix(raw, model.CloudGroupPrefix)))
			return
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 || len(ids) > maxResolve {
		badRequest(w, fmt.Sprintf("ids must name 1-%d group object ids", maxResolve))
		return
	}
	found := s.Engine.CachedCloudGroups(ids)
	var missing []string
	for _, id := range ids {
		if _, ok := found[id]; !ok {
			missing = append(missing, id)
		}
	}
	res := map[string]any{}
	if len(missing) > 0 {
		conn, msg, err := s.cloudConn(r)
		switch {
		case err != nil:
			s.fail(w, r, err)
			return
		case msg != "":
			res["error"] = msg
		default:
			ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
			more, err := s.Engine.Cloud.ResolveGroups(ctx, conn, missing)
			cancel()
			if err != nil {
				res["error"] = "looking up groups in Entra ID failed: " + err.Error()
			}
			for _, g := range more {
				found[strings.ToLower(g.ID)] = g
			}
		}
	}
	groups := make([]cloud.Group, 0, len(ids))
	for _, id := range ids {
		if g, ok := found[id]; ok {
			groups = append(groups, g)
		}
	}
	res["groups"] = cloudGroupsOut(groups)
	writeJSON(w, http.StatusOK, res)
}

// handleTestCloud: POST /api/cloud/test {cloud?} - signs in with the saved connection, or with the unsaved one in the
// body (a masked or empty secret means the saved one), and reports each check. Writes nothing.
func (s *Server) handleTestCloud(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Cloud *model.CloudSettings `json:"cloud"`
	}
	if !decode(w, r, &in) {
		return
	}
	st, err := s.Store.GetSettings(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	cfg := st.Cloud
	if in.Cloud != nil {
		if cfg, err = prepareCloud(*in.Cloud, st.Cloud, s.Cipher); err != nil {
			badRequest(w, err.Error())
			return
		}
	}
	cfg.Enabled = true // a test checks the connection whether or not it is switched on
	if err := cfg.Validate(); err != nil {
		badRequest(w, err.Error())
		return
	}
	res := map[string]any{"ok": false, "checks": []cloud.Check{}}
	var failed []string
	if s.Engine.Cloud == nil {
		res["error"] = "this service has no cloud client"
	} else if conn, cerr := cloud.ConnectionFrom(cfg, s.Cipher); cerr != nil {
		res["error"] = cerr.Error()
	} else {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		checks, terr := s.Engine.Cloud.Test(ctx, conn)
		cancel()
		if terr != nil {
			res["error"] = "the connection test could not run: " + terr.Error()
		} else {
			ok := len(checks) > 0
			for _, c := range checks {
				if !c.OK {
					ok = false
					failed = append(failed, c.Name)
				}
			}
			res["ok"], res["checks"] = ok, checks
		}
	}
	s.audit(r, "cloud.test", "settings", "cloud", map[string]any{
		"ok": res["ok"], "environment": cfg.Environment, "auth": cfg.Auth, "exchange": cfg.Exchange.Enabled, "failed": failed, "unsaved": in.Cloud != nil,
	})
	writeJSON(w, http.StatusOK, res)
}
