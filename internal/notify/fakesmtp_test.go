package notify

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// testCert returns a self-signed server certificate for 127.0.0.1/localhost and a client config trusting it.
func testCert(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fake smtp"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	server = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
	client = &tls.Config{RootCAs: pool}
	return server, client
}

type captured struct {
	From    string
	To      []string
	Data    string
	TLS     bool
	AuthMec string
	AuthRaw string // decoded SASL payload (PLAIN/XOAUTH2) or "user:pass" for LOGIN
}

// fakeSMTP is a small in-process ESMTP server sufficient for the client in this package.
type fakeSMTP struct {
	t           *testing.T
	ln          net.Listener
	tls         *tls.Config
	Exts        []string // EHLO keywords, e.g. "STARTTLS", "AUTH LOGIN PLAIN XOAUTH2"
	ImplicitTLS bool
	User, Pass  string // accepted LOGIN/PLAIN credentials
	Token       string // accepted XOAUTH2 bearer token
	Echo535     string // text appended to 535 replies (to test scrubbing)

	mu         sync.Mutex
	msgs       []captured
	xoauthSeen []string
	emptyAfter bool // client sent the empty line after the XOAUTH2 334 challenge
	Got        chan captured
}

func newFakeSMTP(t *testing.T, serverTLS *tls.Config, exts ...string) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{t: t, ln: ln, tls: serverTLS, Exts: exts, Got: make(chan captured, 10)}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeSMTP) Port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeSMTP) Messages() []captured {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]captured(nil), f.msgs...)
}

func (f *fakeSMTP) serve(conn net.Conn) {
	defer func() { conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	isTLS := false
	if f.ImplicitTLS {
		tc := tls.Server(conn, f.tls)
		if err := tc.Handshake(); err != nil {
			return
		}
		conn, isTLS = tc, true
	}
	r := bufio.NewReader(conn)
	w := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
	read := func() (string, bool) {
		l, err := r.ReadString('\n')
		if err != nil {
			return "", false
		}
		return strings.TrimRight(l, "\r\n"), true
	}
	w("220 fake ESMTP ready")
	var cur captured
	for {
		line, ok := read()
		if !ok {
			return
		}
		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "EHLO") || strings.HasPrefix(up, "HELO"):
			lines := []string{"fake.test"}
			for _, e := range f.Exts {
				if isTLS && e == "STARTTLS" {
					continue
				}
				lines = append(lines, e)
			}
			for i, l := range lines {
				sep := "-"
				if i == len(lines)-1 {
					sep = " "
				}
				w("250" + sep + l)
			}
		case up == "STARTTLS":
			w("220 2.0.0 go ahead")
			tc := tls.Server(conn, f.tls)
			if err := tc.Handshake(); err != nil {
				return
			}
			conn, isTLS = tc, true
			r = bufio.NewReader(conn)
		case strings.HasPrefix(up, "AUTH LOGIN"):
			w("334 " + base64.StdEncoding.EncodeToString([]byte("Username:")))
			u64, _ := read()
			w("334 " + base64.StdEncoding.EncodeToString([]byte("Password:")))
			p64, _ := read()
			u, _ := base64.StdEncoding.DecodeString(u64)
			p, _ := base64.StdEncoding.DecodeString(p64)
			cur.AuthMec, cur.AuthRaw = "LOGIN", string(u)+":"+string(p)
			if string(u) == f.User && string(p) == f.Pass {
				w("235 2.7.0 ok")
			} else {
				w("535 5.7.8 bad credentials" + f.Echo535)
			}
		case strings.HasPrefix(up, "AUTH PLAIN"):
			arg := strings.TrimSpace(line[len("AUTH PLAIN"):])
			if arg == "" {
				w("334 ")
				arg, _ = read()
			}
			raw, _ := base64.StdEncoding.DecodeString(arg)
			cur.AuthMec, cur.AuthRaw = "PLAIN", string(raw)
			if string(raw) == "\x00"+f.User+"\x00"+f.Pass {
				w("235 2.7.0 ok")
			} else {
				w("535 5.7.8 bad credentials" + f.Echo535)
			}
		case strings.HasPrefix(up, "AUTH XOAUTH2"):
			arg := strings.TrimSpace(line[len("AUTH XOAUTH2"):])
			raw, _ := base64.StdEncoding.DecodeString(arg)
			f.mu.Lock()
			f.xoauthSeen = append(f.xoauthSeen, string(raw))
			f.mu.Unlock()
			cur.AuthMec, cur.AuthRaw = "XOAUTH2", string(raw)
			if strings.HasSuffix(string(raw), "\x01auth=Bearer "+f.Token+"\x01\x01") && f.Token != "" {
				w("235 2.7.0 Authentication successful")
				continue
			}
			w("334 " + base64.StdEncoding.EncodeToString([]byte(`{"status":"401","schemes":"bearer","scope":"https://outlook.office365.com/.default"}`)))
			l, _ := read()
			f.mu.Lock()
			f.emptyAfter = l == ""
			f.mu.Unlock()
			w("535 5.7.3 Authentication unsuccessful" + f.Echo535)
		case line == "*":
			w("501 5.0.0 auth aborted")
		case strings.HasPrefix(up, "MAIL FROM:"):
			cur.From = strings.Trim(strings.TrimSpace(line[len("MAIL FROM:"):]), "<>")
			if i := strings.Index(cur.From, ">"); i >= 0 {
				cur.From = cur.From[:i]
			}
			w("250 2.1.0 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			cur.To = append(cur.To, strings.Trim(strings.TrimSpace(line[len("RCPT TO:"):]), "<>"))
			w("250 2.1.5 ok")
		case up == "DATA":
			w("354 go ahead")
			var b strings.Builder
			for {
				l, ok := read()
				if !ok {
					return
				}
				if l == "." {
					break
				}
				l = strings.TrimPrefix(l, ".")
				b.WriteString(l + "\r\n")
			}
			cur.Data, cur.TLS = b.String(), isTLS
			f.mu.Lock()
			f.msgs = append(f.msgs, cur)
			f.mu.Unlock()
			f.Got <- cur
			cur = captured{}
			w("250 2.0.0 queued")
		case up == "RSET" || up == "NOOP":
			w("250 ok")
		case up == "QUIT":
			w("221 bye")
			return
		default:
			w("502 5.5.2 unknown command")
		}
	}
}
