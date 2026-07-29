// Package origin is the feeder's content origin: the direct-fetch target
// a screen's signed content references (relay/1 REL-061) point at. A
// relay's `state.snapshot` carries a `url` per content item that resolves
// here — the relay is never in this data path (REL-140); a screen fetches
// bytes from this origin directly, over HTTPS, keyed by the content's own
// sha256 hash.
package origin

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
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
	items map[string]item // key: hex digest, no "sha256:" prefix
	dir   string          // persistence dir; "" = in-memory only (no write-through)

	// adding counts the in-flight Adds per hex digest. Add writes the file
	// OUTSIDE the write lock deliberately — a multi-megabyte upload must not
	// stall every concurrent Serve — which leaves a window in which a
	// just-written file has not yet been published into items. Remove refuses
	// a digest with a non-zero count so it cannot unlink the file an Add is
	// about to publish, which would leave the asset advertised in memory with
	// no bytes on disk: served until the next restart, then silently gone,
	// with every playlist that referenced it dangling.
	adding map[string]int

	// nowMs stamps a newly added asset's StoredAtMs. Injectable so a retention
	// sweep's age arithmetic (contentgc) is drivable from a test without
	// waiting out a real window; production leaves it at the wall clock.
	nowMs func() int64
}

// item is one stored asset: its bytes, and when this deployment first obtained
// them. Content is immutable once stored (the key IS the hash), so StoredAt is
// written once and never refreshed by a re-Add of the same bytes.
type item struct {
	bytes     []byte
	storedAt  int64 // ms since epoch; the file's mtime for a reloaded asset
	fromDisk  bool  // loaded by Open rather than added in this process
	sizeBytes int
}

// Option configures a Store at construction.
type Option func(*Store)

// WithClock overrides the clock a newly added asset's StoredAtMs is stamped
// from (default: the wall clock). Test-only by intent — the retention sweep
// compares its own clock against these stamps, so a test that pins one must be
// able to pin the other.
func WithClock(nowMs func() int64) Option {
	return func(s *Store) {
		if nowMs != nil {
			s.nowMs = nowMs
		}
	}
}

// New returns an empty in-memory Store with no persistence. Use Open to back a
// Store with a directory so uploaded content survives a restart.
func New(opts ...Option) *Store {
	s := &Store{items: map[string]item{}, adding: map[string]int{}, nowMs: wallClockMs}
	for _, o := range opts {
		o(s)
	}
	return s
}

func wallClockMs() int64 { return time.Now().UnixMilli() }

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
func Open(dir string, opts ...Option) (*Store, error) {
	s := &Store{items: map[string]item{}, adding: map[string]int{}, dir: dir, nowMs: wallClockMs}
	for _, o := range opts {
		o(s)
	}
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
		// The file's mtime is when this deployment obtained the bytes, and it is
		// the ONLY record of that which survives a restart — the retention sweep's
		// "do not reclaim an asset nobody has had time to schedule yet" guard is
		// measured from it. A file whose mtime cannot be read is treated as
		// arriving NOW (the newest possible, therefore the most protected), never
		// as the epoch, which would make it instantly reclaimable.
		storedAt := s.nowMs()
		if info, err := e.Info(); err == nil {
			storedAt = info.ModTime().UnixMilli()
		}
		s.items[name] = item{bytes: b, storedAt: storedAt, fromDisk: true, sizeBytes: len(b)}
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
		// Declare the add in-flight BEFORE dropping the lock to write the file,
		// so a concurrent retention sweep cannot unlink what this is about to
		// publish (see Store.adding).
		s.mu.Lock()
		_, present := s.items[hexDigest]
		s.adding[hexDigest]++
		s.mu.Unlock()
		defer s.addDone(hexDigest)

		if !present {
			if err := writeAtomic(s.dir, hexDigest, b); err != nil {
				return "", fmt.Errorf("origin: persist %s: %w", hexDigest, err)
			}
		}
	}

	s.mu.Lock()
	if prior, ok := s.items[hexDigest]; ok {
		// Already stored: the bytes are identical (the key is their hash), so the
		// only thing a re-Add could change is the stored-at stamp — and it must
		// NOT. Re-seeding the placeholder image on every boot would otherwise keep
		// resetting its age, and an asset re-uploaded by a retrying client would
		// look freshly arrived forever. The stamp records when this deployment
		// first obtained the bytes; that fact does not change.
		prior.bytes = b
		s.items[hexDigest] = prior
	} else {
		s.items[hexDigest] = item{bytes: b, storedAt: s.nowMs(), sizeBytes: len(b)}
	}
	s.mu.Unlock()

	return assetRef, nil
}

// addDone clears one in-flight Add for hexDigest.
func (s *Store) addDone(hexDigest string) {
	s.mu.Lock()
	if n := s.adding[hexDigest] - 1; n > 0 {
		s.adding[hexDigest] = n
	} else {
		delete(s.adding, hexDigest)
	}
	s.mu.Unlock()
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
	it, ok := s.items[hexDigest]
	if !ok {
		return nil
	}
	return it.bytes
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

// Purge destroys every stored asset — the content-addressed half of the
// workspace a data-subject delete erases (`api/1` API-120/122, routed to
// `security-model.md` SEC-121's destruction path).
//
// It is here rather than in the api layer because only this type knows the
// asset store has two representations that must be destroyed together: the
// in-memory map every Serve/Has answers from, and the dir-backed files Open
// reloads from on the next boot. Clearing one and not the other would leave a
// deployment that reports the content gone until it restarts and finds it all
// again — which for a data-subject erasure is the failure mode that matters.
//
// The on-disk half is removed FIRST, so a failure part-way leaves files whose
// bytes are still advertised, rather than advertised content whose files are
// gone. An asset the filesystem refuses to remove aborts with that error rather
// than being reported as destroyed: an erasure that could not complete must not
// answer as though it had. A dir-less (in-memory) Store simply clears the map.
func (s *Store) Purge() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dir != "" {
		for hexDigest := range s.items {
			if err := os.Remove(filepath.Join(s.dir, hexDigest)); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("origin: purge %s: %w", hexDigest, err)
			}
		}
	}
	s.items = map[string]item{}
	return nil
}

// Entry is one stored asset as a retention sweep sees it: its key, its size, and
// when this deployment first obtained the bytes.
//
// StoredAtMs is the file's mtime for an asset Open reloaded and the add clock's
// reading for one added in this process. It is deliberately NOT "when this asset
// was last referenced" — nothing records that — so the only thing a sweep may
// conclude from it is a lower bound on how long the asset has been sitting here,
// which is exactly what the "do not reclaim an upload nobody has had time to
// schedule yet" guard needs and nothing more.
type Entry struct {
	HexDigest  string
	SizeBytes  int
	StoredAtMs int64
}

// Entries lists every stored asset, in hex-digest order, as a point-in-time
// snapshot — the enumeration a retention sweep decides over.
//
// The snapshot may go stale in exactly one direction: an Add concurrent with the
// caller's decision-making appears in no snapshot taken before it, so a sweep can
// never consider — and therefore never reclaim — an asset that arrived after it
// looked. Removal is this package's own, and Remove re-checks under the same lock.
func (s *Store) Entries() []Entry {
	s.mu.RLock()
	out := make([]Entry, 0, len(s.items))
	for hexDigest, it := range s.items {
		out = append(out, Entry{HexDigest: hexDigest, SizeBytes: it.sizeBytes, StoredAtMs: it.storedAt})
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].HexDigest < out[j].HexDigest })
	return out
}

// ErrAddInFlight is returned by Remove for a digest an Add is concurrently
// publishing. It is not a failure to act on: the asset is present and wanted, and
// a retention sweep that meets it must simply keep the asset.
var ErrAddInFlight = errors.New("origin: an add of this content is in flight")

// Remove reclaims ONE asset: it unlinks the file and stops advertising the bytes.
// Unlike Purge (which destroys the whole store for a data-subject erasure), this
// is the single-asset reclamation a retention sweep drives, and it exists ONLY to
// serve that sweep — nothing in the api/1 surface deletes content.
//
// Everything here is arranged so a failed removal keeps the asset rather than
// half-removes it:
//
//   - The file is unlinked FIRST, and the in-memory entry is dropped only if that
//     succeeded. The opposite order would, on an unlink failure, leave bytes on
//     disk that this process no longer advertises — an asset that resurrects at
//     the next boot and has to be reclaimed all over again — whereas this order's
//     failure leaves the asset fully intact, still served, still schedulable.
//   - A file that is already gone is not an error: the in-memory entry is dropped
//     and the reclamation is reported as done, because it is.
//   - A digest with an Add in flight is refused with ErrAddInFlight rather than
//     removed. See Store.adding: unlinking there would leave the asset advertised
//     with no bytes behind it.
//
// Removing an asset a playlist still references would strand every screen the
// playlist plays on. This type cannot see playlists, so it does not try to judge
// that — the caller holds the app store's write lock across its reference read and
// this call, which is what actually makes the decision safe (contentgc).
func (s *Store) Remove(hexDigest string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.items[hexDigest]; !ok {
		return nil
	}
	if s.adding[hexDigest] > 0 {
		return ErrAddInFlight
	}
	if s.dir != "" {
		if err := os.Remove(filepath.Join(s.dir, hexDigest)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("origin: remove %s: %w", hexDigest, err)
		}
	}
	delete(s.items, hexDigest)
	return nil
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
