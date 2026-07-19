package deviceplane

import "sync"

// This file (Task 2) carries the candidate Store: the relay's view of the
// devices its own discovery has observed but that are not (or no longer)
// adopted, and the full-set device.candidates report the app peer replaces
// its prior view with (contracts/relay-1.md REL-110/111).

// Provenance records how a candidate came to the relay's attention (REL-110):
// discovered by the relay's own listeners, or manually asserted by an operator.
type Provenance string

const (
	// ProvenanceDiscovered marks a candidate the relay's own discovery observed.
	ProvenanceDiscovered Provenance = "discovered"
	// ProvenanceManual marks a candidate an operator asserted by hand.
	ProvenanceManual Provenance = "manual"
)

// Status is a candidate's lifecycle position (REL-110): pending (observed,
// not acted on), adopted (promoted to an entity), or ignored (suppressed
// until ignored_until).
type Status string

const (
	// StatusPending is an observed, not-yet-acted-on candidate.
	StatusPending Status = "pending"
	// StatusAdopted is a candidate promoted to an adopted entity.
	StatusAdopted Status = "adopted"
	// StatusIgnored is a candidate suppressed until IgnoredUntil.
	StatusIgnored Status = "ignored"
)

// IgnoredForever is REL-110's literal ignored_until value meaning a candidate
// is suppressed with no expiry (as opposed to a Timestamp-ms expiry).
const IgnoredForever = "forever"

// candidatesMessageType is the device.candidates envelope type (REL-110).
const candidatesMessageType = "device.candidates"

// Candidate is one entry in a device.candidates report (REL-110): a discovery
// Match, how it was learned (Provenance), its lifecycle Status, an
// IgnoredUntil that is present if and only if Status is ignored (a
// Timestamp-ms string or the literal "forever"), and first/last-seen
// Timestamp-ms marks. Serialized to the corpus's field order/shape.
type Candidate struct {
	Match        Match      `json:"match"`
	Provenance   Provenance `json:"provenance"`
	Status       Status     `json:"status"`
	IgnoredUntil *string    `json:"ignored_until"`
	FirstSeen    int64      `json:"first_seen"`
	LastSeen     int64      `json:"last_seen"`
}

// CandidatesBody is the device.candidates message body (REL-110): the full
// current set of candidates.
type CandidatesBody struct {
	Candidates []Candidate `json:"candidates"`
}

// CandidatesReport is the full device.candidates message the relay sends the
// app peer (REL-110/111): a type + relay_id envelope wrapping the full-set
// candidate body the peer replaces its prior view of this relay with.
type CandidatesReport struct {
	Type    string         `json:"type"`
	RelayID string         `json:"relay_id"`
	Body    CandidatesBody `json:"body"`
}

// Store is the relay's candidate set, keyed by Match.Key() so re-observations
// of the same discovery Match dedup rather than accumulate (REL-111). It
// tracks first-observed order so a full-set Report is deterministic, and is
// safe for concurrent Observe/mutate/Report.
type Store struct {
	mu      sync.Mutex
	relayID string
	order   []string              // Match.Key()s in first-observed order
	byKey   map[string]*Candidate // Match.Key() -> candidate
}

// NewStore returns an empty candidate Store that stamps its device.candidates
// reports with relayID (REL-110).
func NewStore(relayID string) *Store {
	return &Store{relayID: relayID, byKey: make(map[string]*Candidate)}
}

// Observe records that discovery saw m at atMs (a Timestamp-ms). On first
// sight it inserts a pending candidate with first_seen == last_seen == atMs;
// a re-observation of the same Match (by Key) bumps last_seen forward (never
// backward) and never moves first_seen (REL-110/111 dedup-by-match).
func (s *Store) Observe(m Match, provenance Provenance, atMs int64) {
	key := m.Key()
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.byKey[key]; ok {
		if atMs > c.LastSeen {
			c.LastSeen = atMs
		}
		return
	}
	s.byKey[key] = &Candidate{
		Match:      m,
		Provenance: provenance,
		Status:     StatusPending,
		FirstSeen:  atMs,
		LastSeen:   atMs,
	}
	s.order = append(s.order, key)
}

// Adopt marks the candidate with the given Match.Key() adopted and clears any
// ignored_until (REL-110: ignored_until is present only while ignored). A
// key naming no known candidate is a no-op.
func (s *Store) Adopt(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.byKey[key]; ok {
		c.Status = StatusAdopted
		c.IgnoredUntil = nil
	}
}

// Ignore marks the candidate with the given Match.Key() ignored until `until`
// — a Timestamp-ms string or IgnoredForever. Passing nil is treated as
// IgnoredForever so the REL-110 iff invariant (ignored_until present while
// ignored) always holds. A key naming no known candidate is a no-op.
func (s *Store) Ignore(key string, until *string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.byKey[key]; ok {
		c.Status = StatusIgnored
		if until == nil {
			forever := IgnoredForever
			until = &forever
		}
		c.IgnoredUntil = until
	}
}

// Report returns the relay's full current candidate set as a device.candidates
// message (REL-110/111): every known candidate in first-observed order, with
// REL-110's iff invariant enforced on emit (ignored_until is carried only for
// ignored candidates, cleared to null/absent otherwise). The body's
// candidates array is always non-nil (an empty relay reports an empty set,
// not a null). Each call reflects the complete current view — a full replace,
// not a delta.
func (s *Store) Report() CandidatesReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	cands := make([]Candidate, 0, len(s.order))
	for _, key := range s.order {
		c := *s.byKey[key] // copy — never expose internal pointers
		if c.Status != StatusIgnored {
			c.IgnoredUntil = nil
		}
		cands = append(cands, c)
	}
	return CandidatesReport{
		Type:    candidatesMessageType,
		RelayID: s.relayID,
		Body:    CandidatesBody{Candidates: cands},
	}
}
