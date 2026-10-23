package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"idlifecycle/internal/model"
	"idlifecycle/internal/store"
)

// BcryptCost is a variable so tests can lower it.
var BcryptCost = 12

const minPasswordLen = 12

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

func constantEq(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashPassword hashes a password with bcrypt.
func HashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), BcryptCost)
	return string(b), err
}

// ValidatePassword enforces the minimum policy (also bcrypt's 72-byte input limit).
func ValidatePassword(pw string) string {
	switch {
	case len(pw) < minPasswordLen:
		return "password must be at least 12 characters"
	case len(pw) > 72:
		return "password must be at most 72 bytes"
	}
	return ""
}

// dummyHash lets unknown-user logins cost the same as real ones (no user enumeration by timing).
var (
	dummyOnce sync.Once
	dummyHash []byte
)

func getDummyHash() []byte {
	dummyOnce.Do(func() { dummyHash, _ = bcrypt.GenerateFromPassword([]byte("not-a-real-password"), BcryptCost) })
	return dummyHash
}

type ipLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   map[string][]time.Time
}

func newIPLimiter(max int, window time.Duration) *ipLimiter {
	return &ipLimiter{max: max, window: window, hits: map[string][]time.Time{}}
}

// allow records an attempt and reports whether it is within the limit.
func (l *ipLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cut := now.Add(-l.window)
	kept := l.hits[ip][:0]
	for _, t := range l.hits[ip] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= l.max {
		l.hits[ip] = kept
		return false
	}
	l.hits[ip] = append(kept, now)
	if len(l.hits) > 10000 {
		for k, v := range l.hits {
			if len(v) == 0 || v[len(v)-1].Before(cut) {
				delete(l.hits, k)
			}
		}
	}
	return true
}

type userView struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

func view(u model.User) userView { return userView{u.ID, u.Username, u.Role} }

func (s *Server) setCookie(w http.ResponseWriter, token string, exp time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: token, Path: "/", Expires: exp, MaxAge: int(time.Until(exp).Seconds()),
		HttpOnly: true, Secure: s.TLS, SameSite: http.SameSiteStrictMode,
	})
}

func (s *Server) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.TLS, SameSite: http.SameSiteStrictMode})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.limiter.allow(clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "rate_limited", "too many sign-in attempts; try again later")
		return
	}
	if origin := r.Header.Get("Origin"); origin != "" && !sameHost(origin, r.Host) {
		writeErr(w, http.StatusForbidden, "csrf", "cross-origin request refused")
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	ctx := r.Context()
	generic := func() {
		writeErr(w, http.StatusUnauthorized, "bad_credentials", "invalid username or password (or the account is temporarily locked)")
	}

	u, err := s.Store.GetUserByName(ctx, strings.TrimSpace(in.Username))
	if err != nil {
		_ = bcrypt.CompareHashAndPassword(getDummyHash(), []byte(in.Password))
		s.auditAnon(r, "auth.login_failed", "", "unknown user") // never persist what was typed: it may be a password
		generic()
		return
	}
	if u.LockedUntil != nil && time.Now().Before(*u.LockedUntil) {
		s.auditAnon(r, "auth.login_blocked", u.Username, "account locked")
		generic() // same answer as a bad password so lockout state does not reveal that the account exists
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(in.Password)); err != nil || u.Disabled {
		if err != nil {
			_ = s.Store.RecordLoginFailure(ctx, u.ID, lockThreshold, lockDuration)
		}
		s.auditAnon(r, "auth.login_failed", u.Username, "bad password or disabled")
		generic()
		return
	}

	token, err := randomToken(32)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	csrf, err := randomToken(24)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	now := time.Now()
	if err := s.Store.CreateSession(ctx, model.Session{TokenHash: hashToken(token), UserID: u.ID, CSRFToken: csrf, CreatedAt: now, ExpiresAt: now.Add(sessionIdle), LastSeenAt: now}); err != nil {
		s.fail(w, r, err)
		return
	}
	_ = s.Store.RecordLoginSuccess(ctx, u.ID)
	_ = s.Store.DeleteExpiredSessions(ctx)
	s.setCookie(w, token, now.Add(sessionIdle))
	_ = s.Store.AppendAudit(ctx, u.Username, "auth.login", "user", u.ID, map[string]any{"remote": clientIP(r)})
	writeJSON(w, http.StatusOK, map[string]any{"user": view(u), "csrf_token": csrf})
}

func (s *Server) auditAnon(r *http.Request, action, name, reason string) {
	_ = s.Store.AppendAudit(r.Context(), "anonymous", action, "user", name, map[string]any{"remote": clientIP(r), "reason": reason})
}

func sameHost(origin, host string) bool {
	i := strings.Index(origin, "://")
	if i < 0 {
		return false
	}
	return strings.EqualFold(origin[i+3:], host)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	_ = s.Store.DeleteSession(r.Context(), p.TokenHash)
	s.clearCookie(w)
	s.audit(r, "auth.logout", "user", p.User.ID, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	writeJSON(w, http.StatusOK, map[string]any{"user": view(p.User), "csrf_token": p.CSRFToken})
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	p := principal(r)
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if !decode(w, r, &in) {
		return
	}
	if msg := ValidatePassword(in.New); msg != "" {
		badRequest(w, msg)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(p.User.PasswordHash), []byte(in.Current)) != nil {
		writeErr(w, http.StatusForbidden, "bad_credentials", "current password is incorrect")
		return
	}
	h, err := HashPassword(in.New)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	u := p.User
	u.PasswordHash = h
	if err := s.Store.UpdateUser(r.Context(), u); err != nil {
		s.fail(w, r, err)
		return
	}
	// Sign out every other session for this user; keep the current one.
	_ = s.Store.DeleteUserSessions(r.Context(), u.ID)
	token, _ := randomToken(32)
	csrf, _ := randomToken(24)
	now := time.Now()
	if err := s.Store.CreateSession(r.Context(), model.Session{TokenHash: hashToken(token), UserID: u.ID, CSRFToken: csrf, CreatedAt: now, ExpiresAt: now.Add(sessionIdle), LastSeenAt: now}); err != nil {
		s.fail(w, r, err)
		return
	}
	s.setCookie(w, token, now.Add(sessionIdle))
	s.audit(r, "auth.password_change", "user", u.ID, nil)
	writeJSON(w, http.StatusOK, map[string]any{"user": view(u), "csrf_token": csrf})
}

// ---- user administration --------------------------------------------------------------

type userInput struct {
	Username string `json:"username"`
	Role     string `json:"role"`
	Disabled *bool  `json:"disabled"`
	Password string `json:"password"`
}

func validRole(r string) bool { _, ok := roleRank[r]; return ok }

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.Store.ListUsers(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	list(w, users, len(users))
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var in userInput
	if !decode(w, r, &in) {
		return
	}
	in.Username = strings.TrimSpace(in.Username)
	switch {
	case in.Username == "" || len(in.Username) > 100:
		badRequest(w, "username is required (max 100 characters)")
		return
	case !validRole(in.Role):
		badRequest(w, "role must be viewer, approver or admin")
		return
	}
	if msg := ValidatePassword(in.Password); msg != "" {
		badRequest(w, msg)
		return
	}
	h, err := HashPassword(in.Password)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	u := model.User{ID: uuid.NewString(), Username: in.Username, PasswordHash: h, Role: in.Role, Disabled: in.Disabled != nil && *in.Disabled, CreatedAt: time.Now()}
	if err := s.Store.CreateUser(r.Context(), u); err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "user.create", "user", u.ID, map[string]any{"username": u.Username, "role": u.Role})
	writeJSON(w, http.StatusCreated, u)
}

func (s *Server) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cur, err := s.Store.GetUser(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var in userInput
	if !decode(w, r, &in) {
		return
	}
	next := cur
	if n := strings.TrimSpace(in.Username); n != "" {
		next.Username = n
	}
	if in.Role != "" {
		if !validRole(in.Role) {
			badRequest(w, "role must be viewer, approver or admin")
			return
		}
		next.Role = in.Role
	}
	if in.Disabled != nil {
		next.Disabled = *in.Disabled
	}
	if in.Password != "" {
		if msg := ValidatePassword(in.Password); msg != "" {
			badRequest(w, msg)
			return
		}
		if next.PasswordHash, err = HashPassword(in.Password); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	if err := s.guardLastAdmin(r, cur, next); err != nil {
		writeErr(w, http.StatusConflict, "last_admin", err.Error())
		return
	}
	if err := s.Store.UpdateUser(r.Context(), next); err != nil {
		s.fail(w, r, err)
		return
	}
	if next.Disabled || in.Password != "" || next.Role != cur.Role {
		_ = s.Store.DeleteUserSessions(r.Context(), id)
	}
	s.audit(r, "user.update", "user", id, map[string]any{"username": next.Username, "role": next.Role, "disabled": next.Disabled, "password_reset": in.Password != ""})
	writeJSON(w, http.StatusOK, next)
}

func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	cur, err := s.Store.GetUser(r.Context(), id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if id == principal(r).User.ID {
		writeErr(w, http.StatusConflict, "self", "you cannot delete your own account")
		return
	}
	gone := cur
	gone.Disabled, gone.Role = true, model.RoleViewer
	if err := s.guardLastAdmin(r, cur, gone); err != nil {
		writeErr(w, http.StatusConflict, "last_admin", err.Error())
		return
	}
	if err := s.Store.DeleteUser(r.Context(), id); err != nil {
		s.fail(w, r, err)
		return
	}
	s.audit(r, "user.delete", "user", id, map[string]any{"username": cur.Username})
	w.WriteHeader(http.StatusNoContent)
}

// guardLastAdmin refuses changes that would leave no enabled administrator.
func (s *Server) guardLastAdmin(r *http.Request, cur, next model.User) error {
	wasAdmin := cur.Role == model.RoleAdmin && !cur.Disabled
	stillAdmin := next.Role == model.RoleAdmin && !next.Disabled
	if !wasAdmin || stillAdmin {
		return nil
	}
	n, err := s.Store.CountActiveAdmins(r.Context())
	if err != nil {
		return err
	}
	if n <= 1 {
		return errf("this would leave no enabled administrator")
	}
	return nil
}

var _ = store.ErrNotFound
