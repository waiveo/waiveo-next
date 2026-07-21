// Package origin is the feeder's content origin: the direct-fetch target
// a screen's signed content references (relay/1 REL-061) point at. A
// relay's `state.snapshot` carries a `url` per content item that resolves
// here — the relay is never in this data path (REL-140); a screen fetches
// bytes from this origin directly, over HTTPS, keyed by the content's own
// sha256 hash.
package origin

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
)

// Store holds content bytes keyed by their own sha256 content hash — the
// hex digest a snapshot's `content[].url` (`<contentBaseURL>/content/<hex>`,
// snapshot.Build) names. It is safe for concurrent use.
//
// A dir-backed Store (Open) is DURABLE: every Add write-throughs the bytes to
// <dir>/<hex> and Open reloads them, so operator-uploaded content survives a
// feeder restart. This matches the persistence of the app store's scheduling
// rows that reference these asset_refs (a SQLite file): without it, a routine
// restart would drop the bytes while the persisted playlists still resolve
// their urls, so every resolved content url 404s and re-authoring a playlist
// for already-uploaded content is spuriously rejected 422. A dir-less Store
// (New) is in-memory only, for tests.
type Store struct {
	mu    sync.RWMutex
	items map[string][]byte // key: hex digest, no "sha256:" prefix
	dir   string            // persistence dir; "" = in-memory only (no write-through)
}

// New returns an empty in-memory Store with no persistence. Use Open to back a
// Store with a directory so uploaded content survives a restart.
func New() *Store {
	return &Store{items: map[string][]byte{}}
}

// Open returns a Store persisted under dir: it creates dir (0700) if absent,
// loads every already-stored asset from it (each file is named by its own hex
// content digest), and write-throughs every subsequent Add to <dir>/<hex>. A
// content asset therefore survives a feeder restart, keeping the content origin
// in lock-step with the persisted scheduling rows that reference it — so a
// resolved content url still resolves, and re-authoring a playlist for an
// already-uploaded asset is not spuriously rejected, across a restart.
//
// A file whose bytes no longer hash to its own filename (a torn write or
// externally corrupted asset) is skipped rather than loaded, so this Store
// never serves content under a hash it does not match (the content-addressing
// integrity invariant). An empty dir yields an in-memory-only Store, matching
// New.
func Open(dir string) (*Store, error) {
	s := &Store{items: map[string][]byte{}, dir: dir}
	if dir == "" {
		return s, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("origin: create dir %s: %w", dir, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("origin: read dir %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("origin: load %s: %w", name, err)
		}
		// Only load a file whose bytes still hash to their own filename. A torn
		// write (or a leftover temp file) fails this check and is dropped, never
		// served under a hash it no longer matches.
		if strings.TrimPrefix(signhash.ContentID(b), "sha256:") != name {
			continue
		}
		s.items[name] = b
	}
	return s, nil
}

// Add stores b, keyed by its own content hash, and returns that hash as a
// `sha256:<hex>` asset_ref (signhash.ContentID's grammar) — the same value
// snapshot.Build computes for the same bytes, so a snapshot's asset_ref and
// this origin's key always agree for identical content.
//
// On a dir-backed Store, the bytes are durably written to <dir>/<hex> BEFORE
// being made visible in memory, so Has/Serve never advertise content that is
// not yet on disk (and would vanish on restart); a persistence failure is
// returned to the caller and nothing is stored. The write is content-addressed
// and idempotent — re-adding already-stored bytes touches no disk (the file is
// immutable once written), so re-seeding the placeholder on every boot is free.
func (s *Store) Add(b []byte) (string, error) {
	assetRef := signhash.ContentID(b)
	hexDigest := strings.TrimPrefix(assetRef, "sha256:")

	if s.dir != "" {
		s.mu.RLock()
		_, present := s.items[hexDigest]
		s.mu.RUnlock()
		if !present {
			if err := writeAtomic(s.dir, hexDigest, b); err != nil {
				return "", fmt.Errorf("origin: persist %s: %w", hexDigest, err)
			}
		}
	}

	s.mu.Lock()
	s.items[hexDigest] = b
	s.mu.Unlock()

	return assetRef, nil
}

// writeAtomic writes b to <dir>/<name> atomically (temp file in the same dir +
// rename), so a concurrent reader — or this Store's own Open on the next boot —
// never observes a partially written file.
func writeAtomic(dir, name string, b []byte) error {
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// Serve returns the bytes stored under hexDigest (no "sha256:" prefix),
// or nil if no content is stored under that hash.
func (s *Store) Serve(hexDigest string) []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.items[hexDigest]
}

// Has reports whether content is stored under hexDigest (no "sha256:" prefix).
// The playlist authoring surface uses it to reject an item whose asset_ref was
// never uploaded to this origin (data-model/1 DAT-041): content that cannot be
// served cannot be scheduled.
func (s *Store) Has(hexDigest string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.items[hexDigest]
	return ok
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
			apihttp.WriteProblem(w, r, apihttp.TraceID(r), http.StatusNotFound, "NOT_FOUND", "Not Found")
			return
		}
		http.ServeContent(w, r, hexDigest, time.Time{}, bytes.NewReader(b))
	})
	return mux
}
