package httpapi

import (
	"bytes"
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"time"
)

// documentCSP allows only same-origin scripts. Inline styles are allowed
// because xterm.js injects its renderer styles at runtime.
const documentCSP = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; " +
	"font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

const immutableCache = "public, max-age=31536000, immutable"

var assetTypes = map[string]string{
	".woff2": "font/woff2",
	".woff":  "font/woff",
	".svg":   "image/svg+xml",
}

type spa struct {
	files fs.FS
}

// WithSPA serves the embedded single-page application for /, /admin, and
// /join. Vite's content-hashed /assets are cached immutably; the HTML shell
// is revalidated on every load. API routes are more specific and win.
func WithSPA(files fs.FS) Option {
	app := &spa{files: files}
	return func(mux *http.ServeMux) {
		for _, pattern := range []string{"GET /{$}", "GET /admin", "GET /admin/{path...}", "GET /join"} {
			mux.HandleFunc(pattern, app.document)
		}
		mux.HandleFunc("GET /assets/{file...}", app.asset)
		mux.HandleFunc("GET /favicon.svg", app.favicon)
	}
}

func setDocumentHeaders(header http.Header) {
	header.Set("Content-Security-Policy", documentCSP)
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Cross-Origin-Opener-Policy", "same-origin")
	header.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
}

func (s *spa) document(w http.ResponseWriter, r *http.Request) {
	setDocumentHeaders(w.Header())
	index, err := fs.ReadFile(s.files, "index.html")
	if errors.Is(err, fs.ErrNotExist) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("The webpty web UI is not built. Run: go generate ./internal/webassets\n"))
		return
	}
	if err != nil {
		internalError(w, "read index", err)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(index))
}

func (s *spa) asset(w http.ResponseWriter, r *http.Request) {
	s.serveFile(w, r, "assets/"+r.PathValue("file"), immutableCache)
}

func (s *spa) favicon(w http.ResponseWriter, r *http.Request) {
	s.serveFile(w, r, "favicon.svg", "public, max-age=86400")
}

// serveFile serves one regular file; directories and missing files are 404.
func (s *spa) serveFile(w http.ResponseWriter, r *http.Request, name, cacheControl string) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}
	info, err := fs.Stat(s.files, name)
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(s.files, name)
	if err != nil {
		internalError(w, "read asset", err)
		return
	}
	contentType := assetTypes[path.Ext(name)]
	if contentType == "" {
		contentType = mime.TypeByExtension(path.Ext(name))
	}
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("Cache-Control", cacheControl)
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}
