package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func get(t *testing.T, h http.Handler, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, target, nil))
	return rec
}

func TestIndex(t *testing.T) {
	rec := get(t, Handler(), http.MethodGet, "/")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("cache-control = %q, want no-cache", cc)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="app"`) || !strings.Contains(body, "/app.js") {
		t.Errorf("index.html does not look like the SPA shell:\n%s", body)
	}
}

func TestJavaScriptContentType(t *testing.T) {
	rec := get(t, Handler(), http.MethodGet, "/app.js")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("content-type = %q, want text/javascript", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=3600" {
		t.Errorf("cache-control = %q, want public, max-age=3600", cc)
	}
	if !strings.Contains(rec.Body.String(), "export async function boot") {
		t.Error("app.js body does not contain the boot function")
	}

	// Nested modules too.
	rec = get(t, Handler(), http.MethodGet, "/views/approvals.js")
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/javascript") {
		t.Errorf("/views/approvals.js: status %d, content-type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func TestCSSContentType(t *testing.T) {
	rec := get(t, Handler(), http.MethodGet, "/app.css")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("content-type = %q, want text/css", ct)
	}
}

func TestSPAFallback(t *testing.T) {
	h := Handler()
	index := get(t, h, http.MethodGet, "/").Body.String()
	for _, p := range []string{"/approvals", "/runs/1234", "/some/deep/client/route", "/views"} {
		rec := get(t, h, http.MethodGet, p)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200", p, rec.Code)
			continue
		}
		if rec.Body.String() != index {
			t.Errorf("%s: body is not index.html", p)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s: cache-control = %q, want no-cache", p, cc)
		}
	}
}

func TestMissingAssetIs404(t *testing.T) {
	rec := get(t, Handler(), http.MethodGet, "/nope.js")
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

func TestAPIPathsAreNotHandled(t *testing.T) {
	h := Handler()
	for _, p := range []string{"/api/x", "/api/status", "/api/", "/api"} {
		for _, m := range []string{http.MethodGet, http.MethodPost} {
			rec := get(t, h, m, p)
			if rec.Code != http.StatusNotFound {
				t.Errorf("%s %s: status = %d, want 404", m, p, rec.Code)
			}
			if strings.Contains(rec.Body.String(), `id="app"`) {
				t.Errorf("%s %s: served the SPA shell for an API path", m, p)
			}
		}
	}
}

func TestOnlyReadMethods(t *testing.T) {
	rec := get(t, Handler(), http.MethodPost, "/")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /: status = %d, want 405", rec.Code)
	}
	rec = get(t, Handler(), http.MethodHead, "/app.css")
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("HEAD /app.css: status %d, body %d bytes", rec.Code, rec.Body.Len())
	}
}

func TestPathTraversalStaysInsideEmbed(t *testing.T) {
	rec := get(t, Handler(), http.MethodGet, "/../web.go")
	if strings.Contains(rec.Body.String(), "package web") {
		t.Error("served a file outside the embedded static directory")
	}
}

func TestCSPHeader(t *testing.T) {
	rec := get(t, Handler(), http.MethodGet, "/")
	if got := rec.Header().Get("Content-Security-Policy"); got != ContentSecurityPolicy {
		t.Errorf("CSP = %q", got)
	}
}

// The UI must stay CSP-clean: no inline handlers/styles/scripts, no HTML-string sinks, no eval.
func TestStaticFilesAreCSPClean(t *testing.T) {
	forbiddenJS := regexp.MustCompile(`\.innerHTML|\.outerHTML|insertAdjacentHTML|document\.write|\beval\s*\(|new\s+Function\s*\(|setAttribute\(\s*['"]style['"]`)
	forbiddenHTML := regexp.MustCompile(`(?i)<script(?:\s[^>]*)?>\s*[^<\s]|\son[a-z]+\s*=|\sstyle\s*=|<style`)

	sub, err := fs.Sub(content, "static")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	err = fs.WalkDir(sub, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		count++
		data, err := fs.ReadFile(sub, name)
		if err != nil {
			return err
		}
		switch {
		case strings.HasSuffix(name, ".js"):
			if m := forbiddenJS.Find(data); m != nil {
				t.Errorf("%s contains forbidden construct %q", name, m)
			}
		case strings.HasSuffix(name, ".html"):
			if m := forbiddenHTML.Find(data); m != nil {
				t.Errorf("%s contains inline script/style/handler: %q", name, m)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatal("no embedded files found")
	}
}
