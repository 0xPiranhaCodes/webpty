package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/0xPiranhaCodes/webpty/internal/auth"
	"github.com/0xPiranhaCodes/webpty/internal/httpapi"
	"github.com/0xPiranhaCodes/webpty/internal/store"
)

const testIndex = `<!doctype html><html><head><script type="module" src="/assets/index-abc123.js"></script></head><body><div id="root"></div></body></html>`

func spaFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":              {Data: []byte(testIndex)},
		"favicon.svg":             {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
		"assets/index-abc123.js":  {Data: []byte(`console.log("app")`)},
		"assets/fonts/plex.woff2": {Data: []byte("wOF2")},
	}
}

func serveSPA(t *testing.T, handler http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(method, "http://localhost:8000"+path, nil))
	return recorder
}

func assertDocumentSecurityHeaders(t *testing.T, path string, header http.Header) {
	t.Helper()
	csp := header.Get("Content-Security-Policy")
	for _, directive := range []string{"default-src 'self'", "script-src 'self'", "object-src 'none'", "base-uri 'none'",
		"frame-ancestors 'none'", "connect-src 'self'", "font-src 'self'", "form-action 'self'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("%s: CSP %q lacks %q", path, csp, directive)
		}
	}
	if strings.Contains(csp, "unsafe-eval") || strings.Contains(strings.SplitN(csp, "style-src", 2)[0], "unsafe-inline") {
		t.Errorf("%s: CSP %q allows unsafe script", path, csp)
	}
	want := map[string]string{
		"X-Content-Type-Options":     "nosniff",
		"Referrer-Policy":            "no-referrer",
		"X-Frame-Options":            "DENY",
		"Cross-Origin-Opener-Policy": "same-origin",
	}
	for key, value := range want {
		if got := header.Get(key); got != value {
			t.Errorf("%s: %s = %q, want %q", path, key, got, value)
		}
	}
	if header.Get("Permissions-Policy") == "" {
		t.Errorf("%s: no Permissions-Policy", path)
	}
}

func TestSPAServesIndexForAppRoutesWithoutCaching(t *testing.T) {
	handler := httpapi.NewRouter(httpapi.WithSPA(spaFS()))
	for _, path := range []string{"/", "/admin", "/admin/", "/admin/sessions/abc", "/admin/recordings/r1", "/join"} {
		response := serveSPA(t, handler, http.MethodGet, path)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d", path, response.Code)
		}
		if response.Body.String() != testIndex {
			t.Errorf("%s body = %q", path, response.Body.String())
		}
		if got := response.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
			t.Errorf("%s Content-Type = %q", path, got)
		}
		if got := response.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s Cache-Control = %q, want no-cache", path, got)
		}
		assertDocumentSecurityHeaders(t, path, response.Header())
	}
}

func TestSPAServesHashedAssetsImmutably(t *testing.T) {
	handler := httpapi.NewRouter(httpapi.WithSPA(spaFS()))
	for path, body := range map[string]string{"/assets/index-abc123.js": `console.log("app")`, "/assets/fonts/plex.woff2": "wOF2"} {
		response := serveSPA(t, handler, http.MethodGet, path)
		if response.Code != http.StatusOK || response.Body.String() != body {
			t.Fatalf("%s = %d %q", path, response.Code, response.Body.String())
		}
		if got := response.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
			t.Errorf("%s Cache-Control = %q", path, got)
		}
		if response.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s lacks nosniff", path)
		}
	}
	if got := serveSPA(t, handler, http.MethodGet, "/assets/index-abc123.js").Header().Get("Content-Type"); !strings.HasPrefix(got, "text/javascript") {
		t.Errorf("js Content-Type = %q", got)
	}
	favicon := serveSPA(t, handler, http.MethodGet, "/favicon.svg")
	if favicon.Code != http.StatusOK || favicon.Header().Get("Cache-Control") == "public, max-age=31536000, immutable" {
		t.Errorf("favicon = %d %q", favicon.Code, favicon.Header().Get("Cache-Control"))
	}
}

func TestSPARejectsMissingAssetsAndDirectoryListings(t *testing.T) {
	handler := httpapi.NewRouter(httpapi.WithSPA(spaFS()))
	for _, path := range []string{"/assets/missing.js", "/assets/", "/assets/fonts/", "/assets/../index.html", "/unknown", "/admin.html"} {
		response := serveSPA(t, handler, http.MethodGet, path)
		if response.Code == http.StatusOK {
			t.Errorf("%s status = 200, want an error", path)
		}
		if strings.Contains(response.Body.String(), "index-abc123.js") && path != "/assets/../index.html" {
			t.Errorf("%s leaked a listing or index: %q", path, response.Body.String())
		}
		if response.Header().Get("Cache-Control") == "public, max-age=31536000, immutable" {
			t.Errorf("%s: error response is cached immutably", path)
		}
	}
	if response := serveSPA(t, handler, http.MethodPost, "/admin"); response.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /admin = %d, want 405", response.Code)
	}
}

func TestSPADoesNotShadowAPIRoutes(t *testing.T) {
	h := newAPIWith(t, httpapi.SecuritySettings{}, func(*auth.Service, *store.Store) []httpapi.Option {
		return []httpapi.Option{httpapi.WithSPA(spaFS())}
	})
	if response := h.do(request{method: http.MethodGet, path: "/healthz"}); response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"ok"`) {
		t.Errorf("healthz = %d %q", response.Code, response.Body.String())
	}
	for _, path := range []string{"/api/v1/unknown", "/api/v1/admin/unknown", "/api/"} {
		if response := h.do(request{method: http.MethodGet, path: path}); response.Code != http.StatusNotFound || response.Body.String() == testIndex {
			t.Errorf("%s = %d %q", path, response.Code, response.Body.String())
		}
	}
	response := h.do(request{method: http.MethodGet, path: "/api/v1/admin/session"})
	if response.Code != http.StatusUnauthorized || response.Header().Get("Content-Type") != "application/json" {
		t.Errorf("session = %d %q", response.Code, response.Header().Get("Content-Type"))
	}
}

func TestSPAWithoutBuiltIndexExplainsHowToBuild(t *testing.T) {
	handler := httpapi.NewRouter(httpapi.WithSPA(fstest.MapFS{".gitkeep": {}}))
	response := serveSPA(t, handler, http.MethodGet, "/admin")
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "go generate ./internal/webassets") {
		t.Fatalf("status = %d body = %q", response.Code, response.Body.String())
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", response.Header().Get("Cache-Control"))
	}
	assertDocumentSecurityHeaders(t, "/admin", response.Header())
}
