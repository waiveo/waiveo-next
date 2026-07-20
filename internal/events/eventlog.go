package events

// eventlog.go is the durable-event log substrate resume/gap resolution runs
// against (EVT-130–144): an in-memory log ordered by envelope id (the natural
// bound for a subscriber stream, EVT-140/144) with a retention horizon. A
// resume_from is checked for membership and compared against the oldest still
// retained id to decide fresh | resumed | gap | invalid (resume.go). The live
// socket transport that feeds this log is a deferred thin wrapper; this is the
// pure ordering/retention logic the corpus resume cases resolve against.
//
// The retention horizon is what makes EVT-141's retention_expired reachable: an
// id that has aged past the horizon is no longer retained, so a resume from it
// resolves to a gap, never a silent hole (EVT-143). The horizon is a bounded
// entry count here; the specific number is a platform-configuration concern the
// contract leaves open (EVT-153/154 draft-notes), so it is a constructor input,
// not a baked constant.

import "sort"

// EventLog is an in-memory durable-event log ordered by envelope id with an
// optional retention horizon. Its zero value is a valid, unbounded log; use
// NewEventLog to bound it. It is not safe for concurrent use — the live
// transport (deferred) owns the synchronization boundary; this type is the
// ordering/retention logic exercised by the corpus.
type EventLog struct {
	// retention is the maximum number of entries retained; 0 means unbounded.
	// Once len(events) would exceed retention, the oldest entries are dropped
	// (aged out), which advances OldestRetainedID.
	retention int
	// events holds the retained envelopes in ascending id order.
	events []Envelope
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
		// reallocate so aged-out envelopes are not pinned by the backing array.
		l.events = append([]Envelope(nil), l.events[drop:]...)
	}
}

// OldestRetainedID is the earliest id still recoverable — the point a
// retention_expired resume resumes delivery at (EVT-141's to_id). It is "" for
// an empty log.
func (l *EventLog) OldestRetainedID() string {
	if len(l.events) == 0 {
		return ""
	}
	return l.events[0].ID
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

// from returns the retained envelopes whose id is greater than OR EQUAL to id,
// in id order — the inclusive backlog a retention_expired resume delivers,
// starting AT the oldest recoverable id (EVT-141/143, no silent loss). It is
// unexported: the public log surface is After (strictly-after); Resolve uses
// from for the gap case, which resumes AT the oldest id rather than after it.
func (l *EventLog) from(id string) []Envelope {
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
