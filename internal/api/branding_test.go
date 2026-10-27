package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"idlifecycle/internal/web"
)

func TestBrandingRoutesThroughTheServer(t *testing.T) {
	h := (&Server{Branding: web.NewBranding("", nil), Static: web.Handler()}).Handler()
	get := func(path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	logo := get("/branding/logo")
	if logo.Code != 200 || logo.Header().Get("Content-Security-Policy") != "default-src 'none'; style-src 'unsafe-inline'; sandbox" ||
		logo.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("logo without sign-in, SVG policy over the page policy: %d %v", logo.Code, logo.Header())
	}
	if b := get("/branding/brand.json"); b.Code != 200 || !strings.Contains(b.Body.String(), `"product_name":"IDLifecycle"`) {
		t.Errorf("brand.json: %d %s", b.Code, b.Body.String())
	}
	if ico := get("/branding/favicon.ico"); ico.Code != 200 || !strings.Contains(ico.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Errorf("favicon keeps the server policy: %v", ico.Header())
	}
	for _, p := range []string{"/branding/logo.svg", "/branding/x/../../index.html", "/branding/"} {
		if rec := get(p); rec.Code != http.StatusNotFound && rec.Code/100 != 3 { // 3xx: the mux redirects to the cleaned path, outside /branding/
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
}
