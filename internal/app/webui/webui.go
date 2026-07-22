// Package webui serves the built Waiveo console SPA, embedded into the feeder
// binary so the single-binary deployment carries its own web UI — no separate
// asset directory to ship or mount.
//
// The embedded tree is internal/app/webui/dist. Only a committed `.gitkeep`
// sentinel is tracked there (the built index.html + assets are git-ignored, so a
// deterministic Vite build never shows up as a repo diff); `go build ./...` stays
// green without a Node build because the embed always has that sentinel, and when
// no built index.html is present the handler serves the Go-string placeholder
// shell below. `make web-build` populates the tree with the real Vite output
// (built into web/dist, then copied here) before the feeder is compiled for a
// real run, and that build shadows the placeholder.
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

// placeholderShell is the SPA shell served when the embed carries no built
// index.html — a binary compiled before `make web-build` populated
// internal/app/webui/dist. It references no assets (so it degrades to a static
// message rather than 404-ing on a hashed bundle) and carries the same
// `<title>Waiveo</title>` marker the real shell and the web-UI smoke both use, so
// the feeder always answers / with a valid 200 text/html shell. A real build
// shadows it.
const placeholderShell = `<!doctype html>
<html lang="en" data-theme="dark">
  <head>
    <meta charset="UTF-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
    <meta name="color-scheme" content="dark light" />
    <title>Waiveo</title>
  </head>
  <body>
    <div id="root"></div>
    <noscript>The Waiveo console requires JavaScript.</noscript>
    <p style="font-family: system-ui, sans-serif; max-width: 32rem; margin: 4rem auto; text-align: center; color: #888">
      The Waiveo console has not been built into this binary yet. Run
      <code>make web-build</code> and rebuild the feeder to embed the console.
    </p>
  </body>
</html>
`

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
		// No built index.html in the embed (a binary compiled before
		// `make web-build`): serve the Go-string placeholder shell so / always
		// answers with a valid 200 text/html Waiveo shell rather than a 500.
		b = []byte(placeholderShell)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The shell must never be cached stale: hashed asset files it references are
	// immutable and cached by the file server, but the shell itself is revalidated.
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}
