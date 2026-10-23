// Package api serves the JSON API and the embedded single-page GUI.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"idlifecycle/internal/engine"
	"idlifecycle/internal/model"
	"idlifecycle/internal/secrets"
	"idlifecycle/internal/store"
)

// Scheduler is what the API needs from the scheduler.
type Scheduler interface {
	Reload()
	Next() (time.Time, bool)
}

type noScheduler struct{}

func (noScheduler) Reload()                 {}
func (noScheduler) Next() (time.Time, bool) { return time.Time{}, false }

// Server wires handlers to the store and engine.
type Server struct {
	Store     *store.Store
	Engine    *engine.Engine
	Cipher    *secrets.Cipher
	Scheduler Scheduler
	Static    http.Handler
	Log       *slog.Logger
	Version   string
	TLS       bool
	// BaseCtx outlives individual requests; background sync runs use it.
	BaseCtx context.Context

	limiter *ipLimiter
	handler http.Handler
	once    sync.Once
}

const (
	maxBody       = 1 << 20
	sessionIdle   = 8 * time.Hour
	sessionMax    = 24 * time.Hour
	cookieName    = "idl_session"
	csrfHeader    = "X-CSRF-Token"
	lockThreshold = 5
	lockDuration  = 15 * time.Minute
)

// Handler returns the fully wrapped http.Handler.
func (s *Server) Handler() http.Handler {
	s.once.Do(func() {
		if s.Scheduler == nil {
			s.Scheduler = noScheduler{}
		}
		if s.Log == nil {
			s.Log = slog.Default()
		}
		if s.BaseCtx == nil {
			s.BaseCtx = context.Background()
		}
		s.limiter = newIPLimiter(30, 5*time.Minute)

		mux := http.NewServeMux()
		s.routes(mux)
		if s.Static != nil {
			mux.Handle("/", s.Static)
		}
		s.handler = s.secure(s.recoverer(mux))
	})
	return s.handler
}

// ---- middleware --------------------------------------------------------------------

func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		if s.TLS {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.Log.Error("panic in handler", "path", r.URL.Path, "panic", rec, "stack", string(debug.Stack()))
				writeErr(w, http.StatusInternalServerError, "internal", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type ctxKey int

const principalKey ctxKey = 1

// Principal is the authenticated caller.
type Principal struct {
	User      model.User
	CSRFToken string
	TokenHash string
}

func principal(r *http.Request) Principal {
	p, _ := r.Context().Value(principalKey).(Principal)
	return p
}

var roleRank = map[string]int{model.RoleViewer: 1, model.RoleApprover: 2, model.RoleAdmin: 3}

// route registers a handler guarded by authentication, CSRF (non-GET) and a minimum role.
func (s *Server) route(mux *http.ServeMux, pattern, minRole string, h http.HandlerFunc) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.authenticate(w, r)
		if !ok {
			return
		}
		if roleRank[p.User.Role] < roleRank[minRole] {
			_ = s.Store.AppendAudit(r.Context(), p.User.Username, "auth.denied", "route", r.Method+" "+r.URL.Path, map[string]any{"role": p.User.Role, "needs": minRole, "remote": clientIP(r)})
			writeErr(w, http.StatusForbidden, "forbidden", "your role does not allow this action")
			return
		}
		if !s.csrfOK(w, r, p) {
			_ = s.Store.AppendAudit(r.Context(), p.User.Username, "auth.csrf_rejected", "route", r.Method+" "+r.URL.Path, map[string]any{"remote": clientIP(r)})
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
	})
}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (Principal, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		writeErr(w, http.StatusUnauthorized, "unauthenticated", "sign in required")
		return Principal{}, false
	}
	hash := hashToken(c.Value)
	se, err := s.Store.GetSession(r.Context(), hash)
	now := time.Now()
	if err != nil || now.After(se.ExpiresAt) || now.After(se.CreatedAt.Add(sessionMax)) {
		if err == nil {
			_ = s.Store.DeleteSession(r.Context(), hash)
		}
		s.clearCookie(w)
		writeErr(w, http.StatusUnauthorized, "unauthenticated", "session expired; sign in again")
		return Principal{}, false
	}
	u, err := s.Store.GetUser(r.Context(), se.UserID)
	if err != nil || u.Disabled {
		_ = s.Store.DeleteSession(r.Context(), hash)
		s.clearCookie(w)
		writeErr(w, http.StatusUnauthorized, "unauthenticated", "account unavailable")
		return Principal{}, false
	}
	if now.Sub(se.LastSeenAt) > time.Minute {
		_ = s.Store.TouchSession(r.Context(), hash, now.Add(sessionIdle))
	}
	return Principal{User: u, CSRFToken: se.CSRFToken, TokenHash: hash}, true
}

// csrfOK enforces the double-submit header and a same-origin check on state-changing requests.
func (s *Server) csrfOK(w http.ResponseWriter, r *http.Request, p Principal) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || !strings.EqualFold(u.Host, r.Host) {
			writeErr(w, http.StatusForbidden, "csrf", "cross-origin request refused")
			return false
		}
	}
	if !constantEq(r.Header.Get(csrfHeader), p.CSRFToken) {
		writeErr(w, http.StatusForbidden, "csrf", "missing or invalid CSRF token")
		return false
	}
	return true
}

// ---- helpers -----------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	var e apiError
	e.Error.Code, e.Error.Message = code, msg
	writeJSON(w, status, e)
}

// fail maps domain errors to HTTP responses; unexpected errors are logged and not leaked.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, store.ErrConflict):
		writeErr(w, http.StatusConflict, "conflict", conflictMessage(err))
	case errors.Is(err, engine.ErrInvalid):
		writeErr(w, http.StatusBadRequest, "invalid", strings.TrimPrefix(err.Error(), engine.ErrInvalid.Error()+": "))
	case errors.Is(err, engine.ErrRunning):
		writeErr(w, http.StatusConflict, "running", err.Error())
	default:
		s.Log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		writeErr(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

func conflictMessage(err error) string {
	msg := err.Error()
	if msg == store.ErrConflict.Error() {
		return "the item already exists or is in use"
	}
	return strings.TrimPrefix(msg, store.ErrConflict.Error()+": ")
}

func badRequest(w http.ResponseWriter, msg string) {
	writeErr(w, http.StatusBadRequest, "invalid", msg)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeErr(w, http.StatusUnsupportedMediaType, "invalid", "Content-Type must be application/json")
		return false
	}
	body := http.MaxBytesReader(w, r.Body, maxBody)
	dec := json.NewDecoder(body)
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			badRequest(w, "request body is required")
			return false
		}
		badRequest(w, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

func pageOf(r *http.Request) store.Page {
	q := r.URL.Query()
	l, _ := strconv.Atoi(q.Get("limit"))
	o, _ := strconv.Atoi(q.Get("offset"))
	return store.Page{Limit: l, Offset: o}
}

func list[T any](w http.ResponseWriter, items []T, total int) {
	if items == nil {
		items = []T{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": total})
}

func (s *Server) audit(r *http.Request, action, entityType, entityID string, detail map[string]any) {
	actor := principal(r).User.Username
	if actor == "" {
		actor = "anonymous"
	}
	if detail == nil {
		detail = map[string]any{}
	}
	detail["remote"] = clientIP(r)
	if err := s.Store.AppendAudit(r.Context(), actor, action, entityType, entityID, detail); err != nil {
		s.Log.Error("audit append failed", "action", action, "error", err)
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func errf(format string, a ...any) error { return fmt.Errorf(format, a...) }
