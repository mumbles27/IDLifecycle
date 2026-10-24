package notify

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"

	"idlifecycle/internal/model"
)

func (s *Service) tlsConfig(host string) *tls.Config {
	var c *tls.Config
	if s.TLSConfig != nil {
		c = s.TLSConfig.Clone()
	} else {
		c = &tls.Config{}
	}
	if c.ServerName == "" {
		c.ServerName = host
	}
	if c.MinVersion < tls.VersionTLS12 {
		c.MinVersion = tls.VersionTLS12
	}
	return c
}

func (s *Service) dial(ctx context.Context, addr string) (net.Conn, error) {
	if s.Dial != nil {
		return s.Dial(ctx, addr)
	}
	d := net.Dialer{Timeout: defaultTimeout}
	return d.DialContext(ctx, "tcp", addr)
}

func (s *Service) sendSMTP(ctx context.Context, cfg model.NotifySettings, subject, body string, sc *scrubber) error {
	sm := cfg.SMTP
	from, err := parseAddr("from", cfg.From)
	if err != nil {
		return err
	}
	var rcpts []*mail.Address
	for _, t := range cfg.To {
		a, err := parseAddr("to", t)
		if err != nil {
			return err
		}
		rcpts = append(rcpts, a)
	}
	msg, err := buildMessage(from, rcpts, subject, body, time.Now())
	if err != nil {
		return err
	}
	if sm.Security == "none" && sm.Auth != "none" && !isLoopbackHost(sm.Host) {
		return errors.New("smtp: refusing to send credentials over an unencrypted connection (use starttls or tls)")
	}

	// Resolve the OAuth token before connecting so token errors surface without an SMTP session.
	var auth smtp.Auth
	switch sm.Auth {
	case "oauth2":
		tok, err := s.token(ctx, cfg.OAuth, scopeSMTP, sc)
		if err != nil {
			return err
		}
		user := sm.Username
		if user == "" {
			user = from.Address
		}
		auth = &xoauth2Auth{user: user, token: tok}
		sc.add("user=" + user + "\x01auth=Bearer " + tok + "\x01\x01")
	case "login":
		sc.add(sm.Password)
	}

	addr := net.JoinHostPort(sm.Host, strconv.Itoa(sm.Port))
	conn, err := s.dial(ctx, addr)
	if err != nil {
		return fmt.Errorf("smtp: connect %s: %w", addr, err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(asyncTimeout))
	}
	// Abort blocking I/O when ctx is cancelled.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()

	tlsCfg := s.tlsConfig(sm.Host)
	if sm.Security == "tls" {
		tc := tls.Client(conn, tlsCfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			return fmt.Errorf("smtp: TLS handshake with %s: %w", addr, err)
		}
		conn = tc
	}
	c, err := smtp.NewClient(conn, sm.Host)
	if err != nil {
		return fmt.Errorf("smtp: %s: %w", addr, err)
	}
	defer c.Close()
	if err := c.Hello(heloName()); err != nil {
		return fmt.Errorf("smtp: EHLO: %w", err)
	}
	if sm.Security == "starttls" {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return fmt.Errorf("smtp: %s does not offer STARTTLS (security is starttls)", addr)
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("smtp: STARTTLS: %w", err)
		}
	}

	if sm.Auth != "none" {
		ok, mechs := c.Extension("AUTH")
		if !ok {
			return fmt.Errorf("smtp: %s does not offer AUTH", addr)
		}
		has := func(m string) bool {
			for _, x := range strings.Fields(strings.ToUpper(mechs)) {
				if x == m {
					return true
				}
			}
			return false
		}
		loopback := isLoopbackHost(sm.Host)
		switch sm.Auth {
		case "oauth2":
			if !has("XOAUTH2") {
				return fmt.Errorf("smtp: %s does not offer AUTH XOAUTH2 (offered: %s)", addr, mechs)
			}
		case "login":
			switch {
			case has("LOGIN"):
				auth = &loginAuth{user: sm.Username, pass: sm.Password, loopback: loopback}
			case has("PLAIN"):
				auth = &plainAuth{user: sm.Username, pass: sm.Password, loopback: loopback}
			default:
				return fmt.Errorf("smtp: %s offers neither AUTH LOGIN nor PLAIN (offered: %s)", addr, mechs)
			}
		}
		if err := c.Auth(auth); err != nil {
			if sm.Auth == "oauth2" {
				s.dropToken(cfg.OAuth, scopeSMTP)
				return fmt.Errorf("smtp: XOAUTH2 authentication failed: %w (check SMTP.SendAsApp permission, the Exchange service principal and mailbox permission, and that SMTP AUTH is enabled for the mailbox)", err)
			}
			return fmt.Errorf("smtp: authentication failed: %w", err)
		}
	}

	if err := c.Mail(from.Address); err != nil {
		return fmt.Errorf("smtp: MAIL FROM: %w", err)
	}
	for _, r := range rcpts {
		if err := c.Rcpt(r.Address); err != nil {
			return fmt.Errorf("smtp: RCPT TO %s: %w", r.Address, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp: DATA: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("smtp: write message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp: message rejected: %w", err)
	}
	_ = c.Quit()
	return nil
}

func (s *Service) dropToken(o model.OAuthSettings, scope string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.tokens {
		if k.scope == scope && strings.EqualFold(k.client, o.ClientID) && strings.EqualFold(k.tenant, o.TenantID) {
			delete(s.tokens, k)
		}
	}
}

func heloName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "localhost"
	}
	for _, r := range h {
		if !(r == '-' || r == '.' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			return "localhost"
		}
	}
	return h
}

// buildMessage renders an RFC 5322 text/plain message (quoted-printable body, CRLF line endings).
func buildMessage(from *mail.Address, to []*mail.Address, subject, body string, now time.Time) ([]byte, error) {
	if len(to) == 0 {
		return nil, errors.New("no recipients")
	}
	if len(to) > MaxRecipients {
		return nil, fmt.Errorf("at most %d recipients", MaxRecipients)
	}
	if hasCRLF(subject) {
		return nil, errors.New("subject must not contain line breaks")
	}
	rcpt := make([]string, len(to))
	for i, a := range to {
		rcpt[i] = a.String()
	}
	domain := "localhost"
	if i := strings.LastIndexByte(from.Address, '@'); i >= 0 && i < len(from.Address)-1 {
		domain = from.Address[i+1:]
	}
	rnd := make([]byte, 16)
	_, _ = rand.Read(rnd)
	headers := [][2]string{
		{"From", from.String()},
		{"To", strings.Join(rcpt, ", ")},
		{"Subject", mime.QEncoding.Encode("utf-8", subject)},
		{"Date", now.Format(time.RFC1123Z)},
		{"Message-ID", "<" + hex.EncodeToString(rnd) + "@" + domain + ">"},
		{"MIME-Version", "1.0"},
		{"Content-Type", "text/plain; charset=utf-8"},
		{"Content-Transfer-Encoding", "quoted-printable"},
		{"Auto-Submitted", "auto-generated"},
	}
	var buf bytes.Buffer
	for _, h := range headers {
		if hasCRLF(h[1]) {
			return nil, fmt.Errorf("header %s must not contain line breaks", h[0])
		}
		buf.WriteString(h[0] + ": " + h[1] + "\r\n")
	}
	buf.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&buf)
	norm := strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\r", "\n")
	if _, err := qp.Write([]byte(norm)); err != nil {
		return nil, err
	}
	if err := qp.Close(); err != nil {
		return nil, err
	}
	if !bytes.HasSuffix(buf.Bytes(), []byte("\r\n")) {
		buf.WriteString("\r\n")
	}
	return buf.Bytes(), nil
}

// ---- SASL mechanisms -------------------------------------------------------

func requireSecure(info *smtp.ServerInfo, loopback bool) error {
	if !info.TLS && !loopback {
		return errors.New("refusing to authenticate over an unencrypted connection")
	}
	return nil
}

// loginAuth implements AUTH LOGIN (username and password each base64 encoded on a 334 prompt).
type loginAuth struct {
	user, pass string
	loopback   bool
	step       int
}

func (a *loginAuth) Start(info *smtp.ServerInfo) (string, []byte, error) {
	if err := requireSecure(info, a.loopback); err != nil {
		return "", nil, err
	}
	a.step = 0
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	prompt := strings.ToLower(strings.TrimSpace(string(fromServer)))
	a.step++
	switch {
	case strings.HasPrefix(prompt, "user"):
		return []byte(a.user), nil
	case strings.HasPrefix(prompt, "pass"):
		return []byte(a.pass), nil
	case a.step == 1:
		return []byte(a.user), nil
	case a.step == 2:
		return []byte(a.pass), nil
	}
	return nil, errors.New("unexpected AUTH LOGIN challenge")
}

// plainAuth implements AUTH PLAIN (RFC 4616) with the same TLS policy as loginAuth.
type plainAuth struct {
	user, pass string
	loopback   bool
}

func (a *plainAuth) Start(info *smtp.ServerInfo) (string, []byte, error) {
	if err := requireSecure(info, a.loopback); err != nil {
		return "", nil, err
	}
	return "PLAIN", []byte("\x00" + a.user + "\x00" + a.pass), nil
}

func (a *plainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("unexpected AUTH PLAIN challenge")
	}
	return nil, nil
}

// xoauth2Auth implements SASL XOAUTH2 as used by Exchange Online SMTP AUTH.
type xoauth2Auth struct {
	user, token string
}

func (a *xoauth2Auth) Start(info *smtp.ServerInfo) (string, []byte, error) {
	if !info.TLS {
		return "", nil, errors.New("refusing to send an OAuth token over an unencrypted connection")
	}
	return "XOAUTH2", []byte("user=" + a.user + "\x01auth=Bearer " + a.token + "\x01\x01"), nil
}

// Next answers the 334 error challenge with an empty line so the server sends its final error reply.
func (a *xoauth2Auth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return []byte{}, nil
	}
	return nil, nil
}
