// Package origin is the feeder's content origin: the direct-fetch target
// a screen's signed content references (relay/1 REL-061) point at. A
// relay's `state.snapshot` carries a `url` per content item that resolves
// here — the relay is never in this data path (REL-140); a screen fetches
// bytes from this origin directly, over HTTPS, keyed by the content's own
// sha256 hash.
package origin

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/maaxton/waiveo-next/internal/shared/signhash"
)

// Store holds content bytes keyed by their own sha256 content hash — the
// hex digest a snapshot's `content[].url` (`<contentBaseURL>/content/<hex>`,
// snapshot.Build) names. It is safe for concurrent use.
type Store struct {
	mu    sync.RWMutex
	items map[string][]byte // key: hex digest, no "sha256:" prefix
}

// New returns an empty Store.
func New() *Store {
	return &Store{items: map[string][]byte{}}
}

// Add stores b, keyed by its own content hash, and returns that hash as a
// `sha256:<hex>` asset_ref (signhash.ContentID's grammar) — the same
// value snapshot.Build computes for the same bytes, so a snapshot's
// asset_ref and this origin's key always agree for identical content.
func (s *Store) Add(b []byte) string {
	assetRef := signhash.ContentID(b)
	hexDigest := strings.TrimPrefix(assetRef, "sha256:")

	s.mu.Lock()
	s.items[hexDigest] = b
	s.mu.Unlock()

	return assetRef
}

// Serve returns the bytes stored under hexDigest (no "sha256:" prefix),
// or nil if no content is stored under that hash.
func (s *Store) Serve(hexDigest string) []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.items[hexDigest]
}

// contentPathPrefix is the route content is served under: /content/<hex>.
const contentPathPrefix = "/content/"

// Handler returns an http.Handler serving GET /content/<hex> — the exact
// bytes Serve(<hex>) returns, via http.ServeContent, or 404 for an unknown
// hash. Mount it on the feeder's HTTPS listener (crypto/tls, using the
// feeder's own signing.Identity TLS cert/key) so screens can fetch content
// directly, never through the relay (REL-140).
func (s *Store) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(contentPathPrefix, func(w http.ResponseWriter, r *http.Request) {
		hexDigest := strings.TrimPrefix(r.URL.Path, contentPathPrefix)
		b := s.Serve(hexDigest)
		if b == nil {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, hexDigest, time.Time{}, bytes.NewReader(b))
	})
	return mux
}
