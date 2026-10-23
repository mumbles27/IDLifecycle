// Package web embeds the single-page admin UI (internal/web/static) and serves it.
//
// The UI is plain HTML, CSS and ES modules - no build step, no CDN. Everything under /api/ belongs to
// the API handler; this package never answers those paths (it returns 404 so a mis-routed mux is obvious).
package web

import (
	"bytes"
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

//go:embed static
var content embed.FS

// ContentSecurityPolicy is the policy the UI is written for (no inline script/style, no eval, same-origin
// only). Handler() adds it to every response it serves; the API layer should send the same value.
const ContentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:"

const (
	cacheHTML  = "no-cache"
	cacheOther = "public, max-age=3600"
	indexName  = "index.html"
)

// Explicit types: mime.TypeByExtension consults the Windows registry, which is not reliable for .js.
var contentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".json":  "application/json; charset=utf-8",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".ico":   "image/x-icon",
	".txt":   "text/plain; charset=utf-8",
	".map":   "application/json; charset=utf-8",
	".woff2": "font/woff2",
}

type handler struct {
	fsys fs.FS
}

// Handler returns the http.Handler for the embedded UI.
//
//   - /api and /api/... are never handled (404).
//   - An existing static file is served with the right MIME type. HTML is "no-cache", everything else is
//     cached for an hour.
//   - Any other GET/HEAD path falls back to index.html so client-side (hash or path) routes survive a
//     reload. Missing paths that look like assets (".js", ".css", ...) get a real 404 instead.
func Handler() http.Handler {
	sub, err := fs.Sub(content, "static")
	if err != nil {
		// Cannot happen: "static" is embedded at compile time.
		panic("web: embedded static directory missing: " + err.Error())
	}
	return &handler{fsys: sub}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if p == "/api" || strings.HasPrefix(p, "/api/") {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	name := strings.TrimPrefix(path.Clean("/"+p), "/")
	if name == "" {
		name = indexName
	}

	data, err := h.readFile(name)
	if err != nil {
		if isAssetPath(name) {
			http.NotFound(w, r)
			return
		}
		name = indexName
		data, err = h.readFile(name)
		if err != nil {
			http.Error(w, "UI is not available", http.StatusInternalServerError)
			return
		}
	}

	ext := strings.ToLower(path.Ext(name))
	ctype, ok := contentTypes[ext]
	if !ok {
		ctype = mime.TypeByExtension(ext)
		if ctype == "" {
			ctype = "application/octet-stream"
		}
	}
	hdr := w.Header()
	hdr.Set("Content-Type", ctype)
	if ext == ".html" {
		hdr.Set("Cache-Control", cacheHTML)
	} else {
		hdr.Set("Cache-Control", cacheOther)
	}
	hdr.Set("X-Content-Type-Options", "nosniff")
	if hdr.Get("Content-Security-Policy") == "" { // the API layer sends a stricter superset when it wraps us
		hdr.Set("Content-Security-Policy", ContentSecurityPolicy)
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}

// readFile returns the bytes of a regular file inside the embedded tree.
func (h *handler) readFile(name string) ([]byte, error) {
	if !fs.ValidPath(name) {
		return nil, fs.ErrInvalid
	}
	info, err := fs.Stat(h.fsys, name)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fs.ErrNotExist
	}
	return fs.ReadFile(h.fsys, name)
}

// isAssetPath reports whether a missing path should 404 rather than fall back to the SPA shell.
func isAssetPath(name string) bool {
	switch strings.ToLower(path.Ext(name)) {
	case ".js", ".mjs", ".css", ".map", ".png", ".ico", ".svg", ".json", ".woff2", ".txt":
		return true
	}
	return false
}
