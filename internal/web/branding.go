package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"idlifecycle/internal/datadir"
)

// Default product name and the limits for replacement branding files.
const (
	DefaultProductName = "IDLifecycle"
	MaxBrandingFile    = 1 << 20 // 1 MiB per file
	brandingTTL        = 60 * time.Second
	svgPolicy          = "default-src 'none'; style-src 'unsafe-inline'; sandbox"
)

var accentRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// Brand is what /branding/brand.json returns.
type Brand struct {
	ProductName string `json:"product_name"`
	Accent      string `json:"accent"` // "" = the UI's own colour
	Custom      bool   `json:"custom"` // something came from the override folder
}

type brandState struct {
	brand    Brand
	logo     []byte
	logoType string
	favicon  []byte
	at       time.Time
}

// Branding serves /branding/brand.json, /branding/logo and /branding/favicon.ico: the embedded defaults, or the files
// in Dir while that folder is trusted (datadir.CheckTrustedFolder: only administrators can change it). Every file is
// optional; an invalid or oversized one is ignored with a warning. The folder is re-read at most every 60 seconds.
type Branding struct {
	Dir string
	Log *slog.Logger
	Now func() time.Time

	mu       sync.Mutex
	state    *brandState
	lastWarn string
}

// NewBranding returns the branding handler for dir ("" = defaults only).
func NewBranding(dir string, log *slog.Logger) *Branding {
	if log == nil {
		log = slog.Default()
	}
	return &Branding{Dir: dir, Log: log, Now: time.Now}
}

func defaultFile(name string) []byte {
	b, err := fs.ReadFile(content, "static/branding/"+name)
	if err != nil {
		panic("web: embedded branding file missing: " + name) // compile-time content
	}
	return b
}

// Current returns the brand in effect (for other components, e.g. page titles in notifications).
func (b *Branding) Current() Brand { return b.load().brand }

func (b *Branding) load() *brandState {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.Now()
	if b.state != nil && now.Sub(b.state.at) < brandingTTL {
		return b.state
	}
	st := &brandState{
		brand:    Brand{ProductName: DefaultProductName},
		logo:     defaultFile("logo.svg"),
		logoType: "image/svg+xml",
		favicon:  defaultFile("favicon.ico"),
		at:       now,
	}
	var warns []string
	if b.Dir != "" {
		warns = b.override(st)
	}
	if w := strings.Join(warns, "; "); w != b.lastWarn {
		b.lastWarn = w
		if w != "" {
			b.Log.Warn("branding: "+w, "dir", b.Dir)
		}
	}
	b.state = st
	return st
}

// override applies the files of the override folder to st and returns what was ignored and why.
func (b *Branding) override(st *brandState) []string {
	if _, err := os.Stat(b.Dir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := datadir.CheckTrustedFolder(b.Dir); err != nil {
		return []string{"the override folder is not used (" + err.Error() + "); the default branding is shown"}
	}
	var warns []string
	read := func(name string) []byte {
		p := filepath.Join(b.Dir, name)
		fi, err := os.Lstat(p)
		switch {
		case errors.Is(err, os.ErrNotExist):
			return nil
		case err != nil:
			warns = append(warns, name+": "+err.Error())
			return nil
		case !fi.Mode().IsRegular():
			warns = append(warns, name+" is not a regular file; ignored")
			return nil
		case fi.Size() > MaxBrandingFile:
			warns = append(warns, name+" is larger than 1 MiB; ignored")
			return nil
		}
		data, err := os.ReadFile(p) // #nosec G304 -- a fixed file name in the trusted branding folder
		if err != nil {
			warns = append(warns, name+": "+err.Error())
			return nil
		}
		return data
	}

	if raw := read("brand.json"); raw != nil {
		var in struct {
			ProductName *string `json:"product_name"`
			Accent      *string `json:"accent"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			warns = append(warns, "brand.json is not valid JSON; ignored")
		} else {
			if in.ProductName != nil {
				if n := strings.TrimSpace(*in.ProductName); validProductName(n) {
					st.brand.ProductName, st.brand.Custom = n, true
				} else {
					warns = append(warns, "brand.json product_name must be 1-40 printable characters; ignored")
				}
			}
			if in.Accent != nil && *in.Accent != "" {
				if accentRe.MatchString(*in.Accent) {
					st.brand.Accent, st.brand.Custom = strings.ToLower(*in.Accent), true
				} else {
					warns = append(warns, "brand.json accent must look like #1558b0; ignored")
				}
			}
		}
	}
	if svg := read("logo.svg"); svg != nil {
		if looksLikeSVG(svg) {
			st.logo, st.logoType, st.brand.Custom = svg, "image/svg+xml", true
		} else {
			warns = append(warns, "logo.svg is not an SVG image; ignored")
		}
	} else if png := read("logo.png"); png != nil {
		if bytes.HasPrefix(png, []byte("\x89PNG\r\n\x1a\n")) {
			st.logo, st.logoType, st.brand.Custom = png, "image/png", true
		} else {
			warns = append(warns, "logo.png is not a PNG image; ignored")
		}
	}
	if ico := read("favicon.ico"); ico != nil {
		if bytes.HasPrefix(ico, []byte{0, 0, 1, 0}) {
			st.favicon, st.brand.Custom = ico, true
		} else {
			warns = append(warns, "favicon.ico is not an icon file; ignored")
		}
	}
	return warns
}

func validProductName(s string) bool {
	n := utf8.RuneCountInString(s)
	if n < 1 || n > 40 {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func looksLikeSVG(b []byte) bool {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	head := bytes.ToLower(b[:min(len(b), 1024)])
	return bytes.Contains(head, []byte("<svg"))
}

// ServeHTTP answers the three branding paths and 404s everything else under /branding/.
func (b *Branding) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	st := b.load()
	var data []byte
	var ctype string
	switch r.URL.Path {
	case "/branding/brand.json":
		data, _ = json.Marshal(st.brand)
		ctype = "application/json; charset=utf-8"
	case "/branding/logo":
		data, ctype = st.logo, st.logoType
	case "/branding/favicon.ico":
		data, ctype = st.favicon, "image/x-icon"
	default:
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Content-Type-Options", "nosniff")
	if ctype == "image/svg+xml" {
		h.Set("Content-Security-Policy", svgPolicy) // opened directly, an SVG could otherwise run script on this origin
	}
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
}
