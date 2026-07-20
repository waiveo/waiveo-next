package apihttp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
)

// CodeIdempotencyKeyReused is the api/1 error-code registry value (API-011,
// Error taxonomy) a repeat request that reuses an Idempotency-Key scope with a
// DIFFERENT body is rejected under: 409 Conflict (API-053).
const CodeIdempotencyKeyReused = "IDEMPOTENCY_KEY_REUSED"

// CodeIdempotencyKeyInProgress is the api/1 error-code registry value a repeat
// request whose original is still executing (no stored response yet) is
// rejected under: 409 Conflict (API-054).
const CodeIdempotencyKeyInProgress = "IDEMPOTENCY_KEY_IN_PROGRESS"

// defaultRetentionMs is the API-052 minimum retention window — 24 hours — the
// store applies when NewIdempotencyStore is given a non-positive retention.
const defaultRetentionMs = int64(24 * 60 * 60 * 1000)

// IdempotencyScope is the tuple api/1 API-051 keys an Idempotency-Key by: the
// authenticated principal, the HTTP method, and the request path. The same key
// value under a different principal, method, or path is an unrelated, fresh
// request — never a replay. All fields are plain strings, so IdempotencyScope
// is comparable and usable directly as (part of) a map key.
type IdempotencyScope struct {
	Principal string
	Method    string
	Path      string
}

// IdempotencyBodyHash is the content hash API-052 keys replay-vs-reuse on: the
// SHA-256 of the raw request body, hex-encoded. A matching hash under the same
// scope+key is a replay (API-052); a mismatching hash is a reuse conflict
// (API-053). The caller passes the exact bytes it will hand the handler, so
// the hash reflects the body as received.
func IdempotencyBodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// IdempotencyReuseDetail is the human-readable `detail` an IDEMPOTENCY_KEY_REUSED
// Problem carries (API-053): it names the offending key so a client can tell
// its own retry apart from a genuinely new request. It is the verbatim string
// the api-1 corpus pins for API-053.
func IdempotencyReuseDetail(key string) string {
	return fmt.Sprintf("Idempotency-Key %q was already used with a different request body.", key)
}

// BeginKind is the classification IdempotencyStore.Begin returns for a request
// presenting an Idempotency-Key: whether it is a fresh execution, a replay of a
// retained response, a reuse conflict, or a still-in-progress repeat.
type BeginKind int

const (
	// BeginFresh: no live entry for this scope+key (or the prior one's
	// retention window elapsed, API-055) — the caller MUST execute the
	// operation and then record its response with Complete. Begin has already
	// recorded an in-flight marker so a concurrent repeat sees BeginInProgress
	// (API-054) rather than executing a second time.
	BeginFresh BeginKind = iota
	// BeginReplay: a completed entry with a matching body hash exists — the
	// caller MUST return Response verbatim WITHOUT re-executing the operation
	// (API-052).
	BeginReplay
	// BeginConflict: an entry exists whose body hash does not match — the
	// caller MUST reject 409 IDEMPOTENCY_KEY_REUSED and MUST NOT execute
	// (API-053).
	BeginConflict
	// BeginInProgress: a matching-body entry exists but has not completed —
	// the caller MUST reject 409 IDEMPOTENCY_KEY_IN_PROGRESS rather than
	// executing concurrently or blocking (API-054).
	BeginInProgress
)

// String renders a BeginKind for diagnostics and test failure messages.
func (k BeginKind) String() string {
	switch k {
	case BeginFresh:
		return "Fresh"
	case BeginReplay:
		return "Replay"
	case BeginConflict:
		return "Conflict"
	case BeginInProgress:
		return "InProgress"
	default:
		return fmt.Sprintf("BeginKind(%d)", int(k))
	}
}

// StoredResponse is the retained original response API-052 replays verbatim:
// the status, the raw body bytes, and the Content-Type. A replay returns this
// exactly as recorded, never a re-serialized approximation.
type StoredResponse struct {
	Status      int
	Body        []byte
	ContentType string
}

// BeginOutcome is what IdempotencyStore.Begin reports: the Kind, and — only for
// BeginReplay — the retained Response to return verbatim. Response is the zero
// StoredResponse for every non-replay Kind.
type BeginOutcome struct {
	Kind     BeginKind
	Response StoredResponse
}

// idempotencyKey is the composite map key an entry is stored under: the API-051
// scope tuple together with the client-supplied key value. IdempotencyScope is
// comparable, so this whole struct is a valid map key.
type idempotencyKey struct {
	scope IdempotencyScope
	key   string
}

// idempotencyEntry is one retained Idempotency-Key record. hash is the body
// content hash of the original request; completed distinguishes a still-
// executing original (API-054) from one whose response is recorded (API-052);
// response is the retained original response (valid only once completed);
// createdMs is the injected timestamp of first use, which the retention window
// (API-052/055) is measured from.
type idempotencyEntry struct {
	hash      string
	completed bool
	response  StoredResponse
	createdMs int64
}

// IdempotencyStore is the in-memory realization of api/1's Idempotency-Key
// semantics (API-050-056): at-most-once execution per (scope, key), verbatim
// replay of a retained response, reuse and in-progress conflict detection, and
// a retention window measured from an INJECTED clock — the store never reads
// the wall clock itself (API-052/055), so its behavior is fully deterministic
// and testable. It is safe for concurrent use.
type IdempotencyStore struct {
	mu          sync.Mutex
	entries     map[idempotencyKey]idempotencyEntry
	clock       func() int64
	retentionMs int64
}

// NewIdempotencyStore builds a store whose sense of "now" comes solely from
// clock (a func returning epoch milliseconds) — no time.Now() read lives in
// this package, keeping retention deterministic (API-052/055). retentionMs is
// the window an entry is retained for; a non-positive value applies the API-052
// minimum of 24 hours. Callers source the nowMs they pass Begin/Complete from
// Now (which reads clock); tests pass controlled values directly so a single
// fixed clock can still exercise time passing.
func NewIdempotencyStore(clock func() int64, retentionMs int64) *IdempotencyStore {
	if retentionMs <= 0 {
		retentionMs = defaultRetentionMs
	}
	return &IdempotencyStore{
		entries:     make(map[idempotencyKey]idempotencyEntry),
		clock:       clock,
		retentionMs: retentionMs,
	}
}

// Now reports the store's injected wall clock in epoch milliseconds — the value
// a production caller passes as the nowMs argument to Begin and Complete, so
// the store never itself reads time.Now() (API-052/055).
func (s *IdempotencyStore) Now() int64 { return s.clock() }

// expired reports whether an entry created at createdMs is past its retention
// window as of nowMs. The window is inclusive of its far boundary (API-052
// "at least 24 hours from first use"): an entry exactly retentionMs old is
// still retained; one strictly older is treated as absent.
func (s *IdempotencyStore) expired(createdMs, nowMs int64) bool {
	return nowMs-createdMs > s.retentionMs
}

// Begin classifies a request presenting scope+key with body content hash, as of
// nowMs, and — when the request is fresh — atomically records an in-flight
// marker so a concurrent repeat is BeginInProgress rather than a second
// execution (API-054).
//
//   - An empty key means the optional header was absent (API-050): always
//     BeginFresh, and nothing is stored (an unkeyed request can never replay,
//     conflict, or block a later one).
//   - No live entry (none, or the prior one's window elapsed, API-055):
//     BeginFresh; an in-flight marker is recorded.
//   - A live entry whose hash differs: BeginConflict (API-053) — the body-hash
//     mismatch wins whether or not the original has completed.
//   - A live, completed entry whose hash matches: BeginReplay with the retained
//     response (API-052).
//   - A live, not-yet-completed entry whose hash matches: BeginInProgress
//     (API-054).
func (s *IdempotencyStore) Begin(scope IdempotencyScope, key, hash string, nowMs int64) BeginOutcome {
	if key == "" {
		return BeginOutcome{Kind: BeginFresh}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	k := idempotencyKey{scope: scope, key: key}
	if e, ok := s.entries[k]; ok && !s.expired(e.createdMs, nowMs) {
		if e.hash != hash {
			return BeginOutcome{Kind: BeginConflict}
		}
		if e.completed {
			return BeginOutcome{Kind: BeginReplay, Response: e.response}
		}
		return BeginOutcome{Kind: BeginInProgress}
	}

	// Fresh: record (or overwrite an expired) in-flight marker so the very next
	// repeat, even before this one completes, is InProgress (API-054) and never
	// a concurrent second execution.
	s.entries[k] = idempotencyEntry{hash: hash, createdMs: nowMs}
	return BeginOutcome{Kind: BeginFresh}
}

// Complete records the original response for a scope+key whose Begin was
// BeginFresh, so a later matching repeat replays it (API-052). It is a no-op
// for an empty key (nothing was stored), for a scope+key with no in-flight
// entry, or when hash does not match the recorded body — Complete never
// finalizes an entry other than the one its own Begin opened. The entry's
// first-use timestamp is preserved, so the retention window still runs from
// first use.
func (s *IdempotencyStore) Complete(scope IdempotencyScope, key, hash string, resp StoredResponse, nowMs int64) {
	if key == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	k := idempotencyKey{scope: scope, key: key}
	e, ok := s.entries[k]
	if !ok || e.hash != hash {
		return
	}
	e.completed = true
	e.response = resp
	s.entries[k] = e
}

// len reports the number of retained entries. It exists for the package's own
// tests to assert that unkeyed requests store nothing (API-050).
func (s *IdempotencyStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}
