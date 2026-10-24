// Package notify sends operator email notifications (run failures, pending approvals, safety thresholds, ...)
// through a generic SMTP server, Exchange Online SMTP AUTH (OAuth2 XOAUTH2 or basic) or Microsoft Graph sendMail.
package notify

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"idlifecycle/internal/model"
	"idlifecycle/internal/secrets"
)

const (
	asyncTimeout   = 60 * time.Second
	defaultTimeout = 30 * time.Second
)

// Service delivers notifications. The zero value of the optional fields uses production defaults.
type Service struct {
	Settings  func(ctx context.Context) (model.Settings, error) // reads current settings from the DB
	Cipher    *secrets.Cipher
	Log       *slog.Logger
	HTTP      *http.Client                                             // nil => default with 30s timeout (OAuth token + Graph)
	Dial      func(ctx context.Context, addr string) (net.Conn, error) // nil => net.Dialer
	TokenURL  string                                                   // "" => https://login.microsoftonline.com/{tenant}/oauth2/v2.0/token
	GraphURL  string                                                   // "" => https://graph.microsoft.com
	TLSConfig *tls.Config                                              // nil => default (ServerName from host, TLS 1.2+)

	mu     sync.Mutex
	tokens map[tokenKey]cachedToken
}

func (s *Service) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// Notify sends n in the background when notifications are enabled and n.Event is switched on.
// It only blocks for reading the settings; delivery errors are logged, never returned, and never panic.
func (s *Service) Notify(ctx context.Context, n model.Notification) {
	if s == nil || s.Settings == nil {
		return
	}
	st, err := s.Settings(ctx)
	if err != nil {
		s.log().Warn("notify: read settings", "err", err)
		return
	}
	cfg := st.Notify
	if !cfg.Enabled || !cfg.Events.On(n.Event) {
		return
	}
	n.Lines = append([]string(nil), n.Lines...)
	cfg.To = append([]string(nil), cfg.To...)
	bg := context.WithoutCancel(ctx)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.log().Error("notify: panic while sending", "event", n.Event, "panic", fmt.Sprint(r))
			}
		}()
		ctx, cancel := context.WithTimeout(bg, asyncTimeout)
		defer cancel()
		if err := s.Send(ctx, cfg, n); err != nil {
			s.log().Warn("notify: send failed", "event", n.Event, "provider", cfg.Provider, "err", err)
			return
		}
		s.log().Info("notify: sent", "event", n.Event, "provider", cfg.Provider, "recipients", len(cfg.To))
	}()
}

// Send delivers n synchronously with cfg, ignoring cfg.Enabled and cfg.Events. Secrets may be plain or
// encrypted (values that secrets.IsEncrypted are decrypted with s.Cipher). Errors never contain secrets.
func (s *Service) Send(ctx context.Context, cfg model.NotifySettings, n model.Notification) error {
	cfg = normalize(cfg)
	cfg, err := decryptSecrets(cfg, s.Cipher)
	if err != nil {
		return err
	}
	if err := validate(cfg, true); err != nil {
		return err
	}
	subject, body := Format(cfg, n, time.Now())
	if hasCRLF(subject) {
		return errors.New("notify: subject must not contain line breaks")
	}
	sc := &scrubber{}
	sc.add(cfg.SMTP.Password, cfg.OAuth.ClientSecret)
	switch cfg.Provider {
	case model.NotifyM365Graph:
		err = s.sendGraph(ctx, cfg, subject, body, sc)
	default:
		err = s.sendSMTP(ctx, cfg, subject, body, sc)
	}
	return sc.err(err)
}

// ---- formatting ------------------------------------------------------------

var defaultSubjects = map[string]string{
	model.EventRunFailed:        "Sync run failed",
	model.EventApprovalsPending: "Changes are waiting for approval",
	model.EventChangesFailed:    "Changes failed to apply",
	model.EventThreshold:        "Safety threshold reached",
	model.EventRunSummary:       "Sync run summary",
	model.EventTest:             "Test message",
}

// Format renders the subject (with SubjectPrefix) and the plain-text body of n.
func Format(cfg model.NotifySettings, n model.Notification, now time.Time) (subject, body string) {
	subj := strings.TrimSpace(n.Subject)
	if subj == "" {
		subj = defaultSubjects[n.Event]
	}
	if subj == "" {
		subj = "Notification"
	}
	if p := strings.TrimSpace(cfg.SubjectPrefix); p != "" {
		subj = p + " " + subj
	}

	var b strings.Builder
	for _, l := range n.Lines {
		b.WriteString(strings.ReplaceAll(strings.ReplaceAll(l, "\r\n", "\n"), "\r", "\n"))
		b.WriteString("\n")
	}
	if base := strings.TrimRight(strings.TrimSpace(cfg.GUIBaseURL), "/"); base != "" {
		link := base + "/"
		switch {
		case n.RunID != "":
			link = base + "/#/runs/" + n.RunID
		case n.Event == model.EventApprovalsPending:
			link = base + "/#/approvals"
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("Open in IDLifecycle: " + link + "\n")
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown host"
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "-- \nSent by IDLifecycle on %s at %s.\n", host, now.UTC().Format("2006-01-02 15:04:05 UTC"))
	return subj, b.String()
}

// ---- secret scrubbing ------------------------------------------------------

// scrubber removes secrets (and their base64 forms) from error strings.
type scrubber struct {
	mu   sync.Mutex
	vals []string
}

func (sc *scrubber) add(vals ...string) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for _, v := range vals {
		if len(v) < 4 {
			continue // too short to scrub meaningfully (and would mangle messages)
		}
		sc.vals = append(sc.vals, v, base64.StdEncoding.EncodeToString([]byte(v)))
	}
}

func (sc *scrubber) clean(s string) string {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	for _, v := range sc.vals {
		s = strings.ReplaceAll(s, v, "[redacted]")
	}
	return s
}

type scrubbedError struct {
	msg string
	err error
}

func (e *scrubbedError) Error() string { return e.msg }

// Unwrap keeps errors.Is for context errors only (the wrapped error text may contain secrets).
func (e *scrubbedError) Unwrap() error {
	if errors.Is(e.err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(e.err, context.Canceled) {
		return context.Canceled
	}
	return nil
}

func (sc *scrubber) err(err error) error {
	if err == nil {
		return nil
	}
	return &scrubbedError{msg: sc.clean(err.Error()), err: err}
}

func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
