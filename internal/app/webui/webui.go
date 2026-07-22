// Package webui serves the built Waiveo console SPA, embedded into the feeder
// binary so the single-binary deployment carries its own web UI — no separate
// asset directory to ship or mount.
//
// The embedded tree is internal/app/webui/dist. A committed placeholder
// index.html lives there so `go build ./...` is always green without a Node
// build; `make web-build` populates it with the real Vite output (built into
// web/dist, then copied here) before the feeder is compiled for a real run.
package webui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var embedded embed.FS

// Handler returns an http.Handler that serves the embedded SPA with
// index.html fallback: a request that maps to a real embedded asset is served
// as that file (with the right content type and caching), and anything else —
// a client-side route the SPA router owns — is answered with the index shell so
// a deep link reload lands on the app rather than a 404.
//
// It is mounted at "/" on the feeder mux; the API, event-stream, content-origin
// and telemetry prefixes are registered as more specific patterns, so
// http.ServeMux routes them ahead of this catch-all.
func Handler() (http.Handler, error) {
	dist, err := fs.Sub(embedded, "dist")
	if err != nil {
		return nil, err
	}
	return &spaHandler{fsys: dist, files: http.FileServerFS(dist)}, nil
}

type spaHandler struct {
	fsys  fs.FS
	files http.Handler
}

func (h *spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Resolve the request path to an embedded FS path (fs paths are slash-rooted
	// and never start with "/"). A cleaned traversal can never escape the tree.
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" {
		h.serveIndex(w, r)
		return
	}
	if f, err := h.fsys.Open(name); err == nil {
		info, statErr := f.Stat()
		_ = f.Close()
		if statErr == nil && !info.IsDir() {
			h.files.ServeHTTP(w, r)
			return
		}
	}
	// Unknown path -> the SPA shell so client-side routing can take over.
	h.serveIndex(w, r)
}

func (h *spaHandler) serveIndex(w http.ResponseWriter, _ *http.Request) {
	b, err := fs.ReadFile(h.fsys, "index.html")
	if err != nil {
		http.Error(w, "web UI not built", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The shell must never be cached stale: hashed asset files it references are
	// immutable and cached by the file server, but the shell itself is revalidated.
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}
