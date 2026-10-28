package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// MaxResponseBytes caps one HTTP response body (one page of an HR API).
const MaxResponseBytes = 50 << 20

// HTTPClient is the shared plumbing for HTTP-based connectors: per-request timeout, retries with backoff on
// 429/502/503/504 (honouring Retry-After), a response size cap, no redirects to another scheme/host (so credentials are
// never forwarded), and error messages that never contain secrets or query strings.
type HTTPClient struct {
	Client     *http.Client
	MaxRetries int           // retries after the first attempt (default 4)
	BaseDelay  time.Duration // first backoff (default 1 s); doubles per retry; Retry-After wins when given
	MaxDelay   time.Duration // cap for one wait (default 60 s)
	secrets    []string
}

// NewHTTPClient returns a client with the given per-request timeout. Secrets passed here are scrubbed from every error
// message the client produces (and should be passed to Redact by callers that build their own messages).
func NewHTTPClient(timeout time.Duration, secretValues ...string) *HTTPClient {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	c := &HTTPClient{MaxRetries: 4, BaseDelay: time.Second, MaxDelay: 60 * time.Second}
	c.Client = &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			first := via[0].URL
			if !strings.EqualFold(req.URL.Scheme, first.Scheme) || !strings.EqualFold(req.URL.Host, first.Host) {
				return fmt.Errorf("refusing a redirect to another host (%s://%s)", req.URL.Scheme, req.URL.Host)
			}
			return nil
		},
	}
	for _, s := range secretValues {
		c.AddSecret(s)
	}
	return c
}

// AddSecret registers a value that must never appear in an error (e.g. a token fetched at run time).
func (c *HTTPClient) AddSecret(s string) {
	if len(s) >= 4 {
		c.secrets = append(c.secrets, s)
	}
}

// Redact replaces every registered secret in s with ***.
func (c *HTTPClient) Redact(s string) string {
	for _, sec := range c.secrets {
		s = strings.ReplaceAll(s, sec, "***")
	}
	return s
}

// SafeURL is u without query string and user info, for messages.
func SafeURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	c := *u
	c.RawQuery, c.User, c.Fragment = "", nil, ""
	return c.String()
}

// Response is a successful (2xx) HTTP response, already read.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// HTTPError is a non-2xx answer (after retries).
type HTTPError struct {
	Status int
	URL    string // without query string
	Body   string // first 200 bytes, secrets redacted
}

func (e *HTTPError) Error() string {
	msg := fmt.Sprintf("HTTP %d from %s", e.Status, e.URL)
	if e.Body != "" {
		msg += ": " + e.Body
	}
	return msg
}

// Do sends the request built by build (called again for every attempt, so bodies can be re-read) and returns the
// response of the first 2xx answer. 429/502/503/504 and network errors are retried; any other status is an *HTTPError.
func (c *HTTPClient) Do(ctx context.Context, build func() (*http.Request, error)) (*Response, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		req, err := build()
		if err != nil {
			return nil, err
		}
		req = req.WithContext(ctx)
		safe := SafeURL(req.URL)
		resp, err := c.Client.Do(req)
		var wait time.Duration
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = fmt.Errorf("request to %s failed: %s", safe, c.Redact(networkError(err)))
		} else {
			body, rerr := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
			resp.Body.Close()
			switch {
			case rerr != nil:
				lastErr = fmt.Errorf("reading the response from %s: %s", safe, c.Redact(rerr.Error()))
			case len(body) > MaxResponseBytes:
				return nil, fmt.Errorf("the response from %s is larger than %d MiB; use a smaller page size", safe, MaxResponseBytes>>20)
			case resp.StatusCode >= 200 && resp.StatusCode < 300:
				return &Response{Status: resp.StatusCode, Header: resp.Header, Body: body}, nil
			default:
				herr := &HTTPError{Status: resp.StatusCode, URL: safe, Body: c.Redact(Snippet(body))}
				if !retryable(resp.StatusCode) {
					return nil, herr
				}
				lastErr = herr
				wait = retryAfter(resp.Header.Get("Retry-After"))
			}
		}
		if attempt >= c.MaxRetries {
			return nil, lastErr
		}
		if wait <= 0 {
			wait = c.BaseDelay << attempt
		}
		if c.MaxDelay > 0 && wait > c.MaxDelay {
			wait = c.MaxDelay
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func retryable(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// retryAfter parses seconds or an HTTP date; 0 when absent or invalid.
func retryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// networkError strips the URL (which may carry secret query parameters) from net/http's error text.
func networkError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timed out"
	}
	return err.Error()
}

// Snippet is the first 200 bytes of a body, trimmed, for error messages (run it through Redact).
func Snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

// DecodeJSON parses a JSON body; a non-JSON body gives a clear error with the content type and a redacted snippet.
func (c *HTTPClient) DecodeJSON(r *Response, safeURL string) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(r.Body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		ct := r.Header.Get("Content-Type")
		if ct == "" {
			ct = "no content type"
		}
		return nil, fmt.Errorf("the response from %s is not JSON (%s): %s", safeURL, ct, c.Redact(Snippet(r.Body)))
	}
	return v, nil
}

// IsLoopbackHost reports whether h is localhost or a loopback IP (plain http is allowed only there).
func IsLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
