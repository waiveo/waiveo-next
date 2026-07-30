package events

// eventlog.go is the durable-event log substrate resume/gap resolution runs
// against (EVT-130–144): a log ordered by envelope id (the natural bound for a
// subscriber stream, EVT-140/144) behind a retention horizon. A resume_from is
// checked for membership and compared against the oldest still retained id to
// decide fresh | resumed | gap | invalid (resume.go).
//
// The substrate is an INTERFACE (Log) with two implementations, because
// "durable event" is a storage property this package deliberately does not
// pick: EVT-010 calls an event "retained for a bounded window regardless of
// whether any subscriber is connected", which a process-lifetime slice cannot
// satisfy for anything that has to outlive a restart. EventLog below is the
// in-memory, count-bounded implementation — the pure ordering/retention logic
// the corpus resume cases resolve against, and the substrate a fixture or a
// driver wants. The persistent implementation lives in internal/app/store
// (SQLite, per-retention-class windows keyed by the classes class.go pins);
// that is the one a real deployment wires, and the one an audit trail
// (SEC-150 makes audit.event the platform's SOLE audit record) depends on.
//
// The retention horizon is what makes EVT-141's retention_expired reachable: an
// id that has aged past the horizon is no longer retained, so a resume from it
// resolves to a gap, never a silent hole (EVT-143). The horizon is a bounded
// entry count here; the durations behind the contract's retention CLASSES are
// a platform-configuration concern events/1 leaves open (EVT-010's own
// "concrete vocabulary is maintained outside this contract"), and live in
// retention.go.

import "sort"

// Log is the durable-event log substrate resume resolution (Resolve), the live
// SSE transport (internal/app/eventsse) and webhook delivery progress
// (EndpointState.PendingAfter) run against. Every method is id-ordered:
// envelope ids are ULIDs, so lexicographic order IS recording order (EVT-011),
// which is what lets every question below be answered by an id comparison.
//
// Two invariants an implementation MUST hold, because Resolve's four-outcome
// resolution is only correct under them:
//
//   - Append is idempotent on id (EVT-135: at-least-once delivery makes id the
//     subscriber's dedup key, so a redelivered id must not duplicate a row).
//   - AgedOut(id) is false for any id Has(id) reports retained. A retained id
//     resumes cleanly; only a point the log can no longer reconstruct is a gap.
//
// The methods do not return errors. A persistent implementation reports its own
// I/O failures through a sink of its own and answers conservatively (see
// internal/app/store's event log): the alternative — threading an error through
// Append — would put an error return on every producer in the tree, including
// the audit sink (internal/app/auth.EventSink), for a condition none of them can
// act on. What they CAN do is be told, loudly, which is what that sink is for.
type Log interface {
	// Append records env, keeping the log ordered by id and idempotent on id.
	Append(env Envelope)
	// Has reports whether id is a currently-retained envelope id — the
	// membership test distinguishing a never-recorded resume_from
	// (RESUME_FROM_INVALID, EVT-134) from a retained one (resumed, EVT-133).
	Has(id string) bool
	// After returns the retained envelopes whose id is strictly greater than
	// id, in id order — the gap-free, duplicate-free backlog a clean resume
	// delivers (EVT-133). An empty id ("") returns the whole retained log.
	After(id string) []Envelope
	// From returns the retained envelopes whose id is greater than OR EQUAL to
	// id — the inclusive backlog a retention_expired resume delivers, starting
	// AT the oldest recoverable id (EVT-141/143, no silent loss).
	From(id string) []Envelope
	// HeadID is the newest retained id ("" for an empty log) — the fresh-
	// subscribe watermark (EVT-132) and the floor a restarted process seeds its
	// recording-order id generator from (EVT-011).
	HeadID() string
	// AgedOut reports whether id names a point the log can no longer resume
	// from because it (and everything at or below it) has been evicted — the
	// retention_expired condition (EVT-141). It is false for a retained id.
	AgedOut(id string) bool
	// EvictedAfter reports whether any event with an id strictly greater than
	// id has been evicted — a point past the caller's last-delivered position
	// that was dropped before it could be delivered (EVT-142's mid-stream
	// buffer_exceeded condition).
	EvictedAfter(id string) bool
}

// EventLog satisfies Log in memory.
var _ Log = (*EventLog)(nil)

// EventLog is an in-memory durable-event log ordered by envelope id with an
// optional retention horizon. Its zero value is a valid, unbounded log; use
// NewEventLog to bound it. It is not safe for concurrent use — the live
// transport owns the synchronization boundary (internal/app/eventsse.Hub); this
// type is the ordering/retention logic exercised by the corpus.
//
// It does NOT survive a process restart, and is not the implementation a
// deployment wires: see the file header and internal/app/store.
type EventLog struct {
	// retention is the maximum number of entries retained; 0 means unbounded.
	// Once len(events) would exceed retention, the oldest entries are dropped
	// (aged out), which advances OldestRetainedID.
	retention int
	// events holds the retained envelopes in ascending id order.
	events []Envelope
	// evictedThrough is the highest id ever aged out of retention ("" if none).
	// It is the substrate a live subscriber's mid-stream loss check reads: an
	// event with an id at or below it is no longer reconstructible. It only ever
	// increases (entries age out oldest-first, ids ascending), so it records the
	// high-water mark of everything the log has silently dropped (EVT-142).
	evictedThrough string
}

// NewEventLog returns a log bounded to at most retention entries. A retention of
// 0 (or negative) is unbounded — the same as the zero-value EventLog.
func NewEventLog(retention int) *EventLog {
	if retention < 0 {
		retention = 0
	}
	return &EventLog{retention: retention}
}

// search returns the index where id is or would be inserted to keep events in
// ascending id order (envelope ids are ULIDs — lexicographic order is
// chronological order).
func (l *EventLog) search(id string) int {
	return sort.Search(len(l.events), func(i int) bool { return l.events[i].ID >= id })
}

// Append records env, keeping the log ordered by id and idempotent on id
// (EVT-135: at-least-once delivery uses id as the dedup key, so a redelivered
// id must not create a duplicate entry). Appending past the retention horizon
// drops the oldest entries, advancing OldestRetainedID — an explicit aging-out,
// the substrate a retention_expired gap resolves against (EVT-141).
func (l *EventLog) Append(env Envelope) {
	pos := l.search(env.ID)
	if pos < len(l.events) && l.events[pos].ID == env.ID {
		return // already recorded — idempotent (EVT-135)
	}
	l.events = append(l.events, Envelope{})
	copy(l.events[pos+1:], l.events[pos:])
	l.events[pos] = env

	if l.retention > 0 && len(l.events) > l.retention {
		drop := len(l.events) - l.retention
		// The highest of the dropped prefix is the newest id being aged out; keep
		// evictedThrough as the max ever dropped so it never regresses if an
		// out-of-order older id is appended then immediately evicted (EVT-142).
		if h := l.events[drop-1].ID; h > l.evictedThrough {
			l.evictedThrough = h
		}
		// reallocate so aged-out envelopes are not pinned by the backing array.
		l.events = append([]Envelope(nil), l.events[drop:]...)
	}
}

// OldestRetainedID is the earliest id still recoverable. It is "" for an empty
// log, and it is the retention floor AgedOut compares against.
//
// It is NOT the point an aged-out resume resumes at, and no resolution path may
// use it as one. EVT-141's to_id has to be the oldest id above the requested
// point that THIS subscriber may see, which is a question about the subscriber's
// visible set rather than about the substrate — so it is answered by
// oldestVisibleAfter over the retained tail (resume.go), never by a substrate
// read. A substrate-level "oldest retained after id" reader is therefore
// deliberately absent from Log: its answer looks right and is wrong, because it
// can name an event outside the visible set (EVT-134a) and resume delivery
// behind a subscriber that had already read past it.
func (l *EventLog) OldestRetainedID() string {
	if len(l.events) == 0 {
		return ""
	}
	return l.events[0].ID
}

// HeadID is the newest retained id, "" for an empty log (Log.HeadID).
func (l *EventLog) HeadID() string {
	if len(l.events) == 0 {
		return ""
	}
	return l.events[len(l.events)-1].ID
}

// AgedOut reports whether id has aged past this log's retention horizon
// (Log.AgedOut). Under one uniform horizon that is exactly "older than the
// oldest retained id": entries age out oldest-first, so nothing below the front
// of the log is still reconstructible. A retained id is never aged out, and an
// id ahead of the whole log (never yet recorded) is not aged out either — that
// is EVT-134's never-recorded case, which Resolve rejects rather than gaps.
func (l *EventLog) AgedOut(id string) bool {
	if id == "" {
		return false
	}
	oldest := l.OldestRetainedID()
	return oldest != "" && id < oldest
}

// EvictedAfter reports whether any event with an id strictly greater than id has
// aged out of retention — an event past the caller's last-delivered point that
// was dropped before it could be delivered. It is the precise mid-stream analogue
// of Resolve's connect-time retention_expired check (EVT-142/143): a live
// subscriber whose last-delivered id predates the highest aged-out id has a real
// buffer_exceeded gap and must be told; one caught up to or past that id does not,
// so no spurious gap is marked. It is always false on a log that has never
// evicted (evictedThrough == ""), so an unbounded log never reports a gap.
func (l *EventLog) EvictedAfter(id string) bool {
	return l.evictedThrough != "" && id < l.evictedThrough
}

// After returns the retained envelopes whose id is strictly greater than id, in
// id order — the gap-free, duplicate-free backlog a clean resume delivers
// (EVT-133). An empty id ("") returns the whole retained log.
func (l *EventLog) After(id string) []Envelope {
	pos := l.search(id)
	if pos < len(l.events) && l.events[pos].ID == id {
		pos++ // exclude the resume point itself
	}
	out := make([]Envelope, len(l.events)-pos)
	copy(out, l.events[pos:])
	return out
}

// From returns the retained envelopes whose id is greater than OR EQUAL to id,
// in id order — the inclusive backlog a retention_expired resume delivers,
// starting AT the oldest recoverable id (EVT-141/143, no silent loss).
// Resolve uses From for the gap case, which resumes AT that id rather than
// after it; After is the strictly-after form a clean resume uses.
func (l *EventLog) From(id string) []Envelope {
	pos := l.search(id)
	out := make([]Envelope, len(l.events)-pos)
	copy(out, l.events[pos:])
	return out
}

// Has reports whether id is a currently-retained envelope id — the membership
// test that distinguishes a never-recorded resume_from (RESUME_FROM_INVALID,
// EVT-134) from a retained one (resumed, EVT-133).
func (l *EventLog) Has(id string) bool {
	pos := l.search(id)
	return pos < len(l.events) && l.events[pos].ID == id
}
