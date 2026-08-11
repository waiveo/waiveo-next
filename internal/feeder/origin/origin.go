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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/maaxton/waiveo-next/internal/feeder/contenturl"
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

	// signingKey, when non-empty, makes Handler require a valid signed content
	// URL (WithSigningKey). Empty means serve by address, unsigned.
	signingKey []byte
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

// WithSigningKey makes Handler REQUIRE a valid signed content URL
// (internal/feeder/contenturl): every fetch must present an `exp`/`sig` pair
// this key verifies, and a request that does not is refused before any byte of
// content is written.
//
// # Why this is an option rather than always-on
//
// Because the key has to reach every party that constructs a content URL, and
// one of them is not this process: REL-066 has the RELAY build a
// schedule-resolved item's url itself, from the `content_origin` base it was
// handed. That party is now supplied — the key rides the signed snapshot as
// `content_url_key` (REL-066a) and the relay mints with it
// (internal/relay/schedulehost) — and so are the IN-PROCESS parties, which take
// their signer from this very store (Store.Signer).
//
// The gap between those two sentences is the whole of this option's history and
// worth keeping written down. Enforcement shipped here first, on the argument
// above, while the app-side producers went on concatenating bare URLs; because
// cmd/waiveo-feeder loads-or-creates the key unconditionally, enforcement was on
// in every real deployment and no `image` or `video` layer could display on any
// screen. Setting a key must never again be a decision one half of the system
// makes alone — which is why Store.Signer exists and why in-process producers
// are expected to go through it rather than be handed a key of their own.
//
// A caller that sets no key gets the previous behaviour: content served by
// address, unsigned — and Store.Signer's minters match that posture, so the two
// remain consistent in either configuration.
//
// An empty key is ignored rather than stored, so a caller that computes a key
// and gets nothing cannot silently end up with verification "enabled" against
// a key that authenticates every forgery (contenturl.ErrNoKey exists for the
// same reason).
func WithSigningKey(key []byte) Option {
	return func(s *Store) {
		if len(key) > 0 {
			s.signingKey = append([]byte(nil), key...)
		}
	}
}

// Signer returns the content-URL minter for THIS origin under base: a
// contenturl.Signer carrying this store's own signing key and its own clock,
// with the stated ttl (contenturl.ServeTTL for a URL handed straight to its
// consumer, contenturl.SnapshotTTL for one minted into a signed generation).
//
// # This method is the point, not a convenience
//
// Minting and verifying are two halves of one agreement, and the whole class of
// defect this exists to close is the two halves disagreeing. The origin refuses
// an unsigned fetch exactly when it holds a key (Handler); a producer that
// signs only when IT was separately handed a key can therefore be wired to a
// key-holding origin while minting bare URLs — which is what shipped, and it
// made every uploaded image unfetchable through the very process that stored
// it, `201` on the way in and `403` on the way out.
//
// Taking the key from the verifier makes that unconstructible instead of
// merely discouraged: there is no argument to pass wrong. The clock comes from
// here for the same reason — the deadline is judged against this store's clock
// (Handler passes s.nowMs() to contenturl.Verify), so measuring it from any
// other one measures a skew rather than a lifetime.
//
// Every in-process producer of a content URL is expected to obtain its Signer
// here. A caller with no origin to ask — a fixture builder, a test — constructs
// a contenturl.Signer literally and thereby states its signing posture where a
// reader can see it.
func (s *Store) Signer(base string, ttl time.Duration) contenturl.Signer {
	s.mu.RLock()
	key := append([]byte(nil), s.signingKey...)
	s.mu.RUnlock()
	return contenturl.Signer{Base: base, Key: key, TTL: ttl, NowMs: s.nowMs()}
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
	// An Add in flight is the same hazard here as it is in Remove, and for the
	// same reason: Add writes its file OUTSIDE this lock and publishes after, so
	// emptying the map underneath one leaves the re-publish pointing at a file
	// this call already unlinked — an asset advertised in memory with no bytes on
	// disk, served until the next restart and then silently gone. Remove refuses
	// in that window; this must too, rather than being the one path that ignores
	// the counter introduced to close it.
	//
	// Refusing is right for an erasure: the caller is destroying a workspace and
	// needs to know it did not complete, not to be told it did while an upload
	// raced past it.
	if len(s.adding) > 0 {
		return fmt.Errorf("origin: purge: %d add(s) in flight; retry once they complete", len(s.adding))
	}
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
//
// Taken from the minting package rather than restated, so the path this origin
// SERVES and the path every producer MINTS are one string by construction
// (contenturl.PathPrefix) — a route and a producer that drifted apart would
// answer 404 for a URL that verified perfectly.
const contentPathPrefix = contenturl.PathPrefix

// Handler returns an http.Handler serving GET /content/<hex> — the exact
// bytes Serve(<hex>) returns, via http.ServeContent, or 404 for an unknown
// hash. Mount it on the feeder's HTTPS listener (crypto/tls, using the
// feeder's own signing.Identity TLS cert/key) so screens can fetch content
// directly, never through the relay (REL-140).
//
// # Cache validators, and why they are unconditional here
//
// Every 200 carries a STRONG ETag and a year-long `immutable` freshness
// lifetime, so a client that already holds an asset can revalidate it for the
// price of a 304 with no body, or skip the request entirely.
//
// Both are trivially TRUE rather than optimistic, and that is the whole
// argument: the request path IS the sha256 of the response body. Bytes cannot
// change under their own hash, so there is no version of this resource that
// differs from the one a client cached, ever — the usual worry with `immutable`
// (a deploy quietly replacing a URL's content) is not merely unlikely here, it
// is unrepresentable. The ETag is the digest itself for the same reason: it is
// already the strongest validator such a resource can have, and computing a
// second one would be inventing a weaker name for the same fact.
//
// This matters most for the fleet's largest assets. A screen re-polls its
// program every ~10s (player-v3's wvProgramPollIntervalMs); before these
// headers, a signage box's own LAN carried a full re-download of every
// scheduled item on every poll, which for a video is the difference between an
// idle link and a saturated one. The player's own content cache
// (player-v3/source/Program.brs) is the primary fix — it skips the request
// altogether — and these headers are what make the SAME saving available to
// every other client of this origin (a browser preview, a proxy, curl) without
// each having to reimplement content-addressed caching.
//
// `public` versus `private` follows the signing posture, and is the one part
// that is not purely about immutability. Unsigned, the bytes are served to
// anyone who knows the digest, so a shared cache holding them grants nothing
// the origin does not. Signed (WithSigningKey), the URL carries an `exp` a
// shared cache does NOT enforce — it would keep serving a stored response for
// an expired signed URL — so the response is marked `private` and only the
// requesting client may store it. The freshness lifetime is identical either
// way: what expires is permission to fetch, never the validity of the bytes.
func (s *Store) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(contentPathPrefix, func(w http.ResponseWriter, r *http.Request) {
		hexDigest := strings.TrimPrefix(r.URL.Path, contentPathPrefix)
		// The signature is checked BEFORE the store is consulted, so a
		// caller without one learns nothing about which digests this origin
		// holds — an unsigned request gets the same refusal whether the
		// asset exists or not. Checking after would make the pair of
		// responses a presence oracle over the whole store.
		if len(s.signingKey) > 0 {
			if err := contenturl.Verify(s.signingKey, hexDigest, r.URL.Query(), s.nowMs()); err != nil {
				apihttp.WriteProblem(w, r, apihttp.TraceID(r), http.StatusForbidden, "FORBIDDEN", "Forbidden")
				return
			}
		}
		b := s.Serve(hexDigest)
		if b == nil {
			apihttp.WriteProblem(w, r, apihttp.TraceID(r), http.StatusNotFound, "NOT_FOUND", "Not Found")
			return
		}
		// Set BEFORE ServeContent: it reads the ETag header we set here to
		// answer a conditional request (If-None-Match) with a bodyless 304, and
		// a header written afterwards would be too late for that and for the
		// 304's own header set. See this method's doc for why both values are
		// unconditionally true of a content-addressed asset.
		w.Header().Set("ETag", `"`+hexDigest+`"`)
		w.Header().Set("Cache-Control", s.cacheControl())
		http.ServeContent(w, r, hexDigest, time.Time{}, bytes.NewReader(b))
	})
	return mux
}

// contentMaxAgeSeconds is the freshness lifetime every content response
// declares: one year, HTTP's conventional "effectively forever" (RFC 9111 caps
// a meaningful value there). It is not a guess about how long an asset stays
// interesting — a content-addressed body is valid for as long as the URL exists
// — and it rides alongside `immutable`, which tells a client not to revalidate
// even on a user-initiated reload.
const contentMaxAgeSeconds = 31536000

// cacheControl is the Cache-Control value for a content response: shareable
// when this origin serves by address alone, per-client when it requires a
// signed URL whose `exp` a shared cache would not enforce. Handler's doc has
// the full argument.
func (s *Store) cacheControl() string {
	shareability := "public"
	if len(s.signingKey) > 0 {
		shareability = "private"
	}
	return shareability + ", max-age=" + strconv.Itoa(contentMaxAgeSeconds) + ", immutable"
}
