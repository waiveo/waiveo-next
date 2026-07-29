package events

// resume.go resolves a WS/SSE hello's resume_from against the durable event log
// (eventlog.go) into one of events/1's four resume outcomes (EVT-130–134,
// 140–143): fresh (no resume_from), resumed (a clean backlog after a retained
// id), gap (a resume_from aged past the retention horizon — a retention_expired
// loss marker, never a silent hole), or the RESUME_FROM_INVALID rejection (a
// malformed resume_from, or one this principal cannot resolve — refused before
// any delivery and never silently treated as fresh).
//
// resume_from is a TRANSPARENT event id (EVT-130/131): the exact value of an
// envelope id this principal previously received, matching ^[A-Za-z0-9_-]+$ so
// it round-trips a hello field or an SSE query parameter unescaped. That charset
// is the ONE property it shares with api/1's keyset cursor (API-036); it is
// otherwise a client-persisted, comparable id, not an opaque token — so this
// grammar is defined locally here, not borrowed from the pagination cursor.
//
// "This principal previously received" is load-bearing, not descriptive: the
// resolution runs against the subscriber's own visible set (ResolveVisible), so
// an id the platform recorded outside that set is refused identically to one it
// never recorded (EVT-134a). Resolving against the whole log instead would let
// any subscriber probe the existence of any event id.

import (
	"regexp"

	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// resumeFromGrammar is EVT-131's charset: a resume_from MUST match
// ^[A-Za-z0-9_-]+$ (api/1 API-036) to round-trip a WS hello field or an SSE
// query parameter without extra escaping. A well-formed event id is also a ULID
// (EVT-130), so a valid resume_from satisfies both this grammar and ulid.Valid;
// failing either is malformed (EVT-134).
var resumeFromGrammar = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Gap reason values (EVT-141/142) the {from_id,to_id,reason} loss marker
// carries. A resume older than the retention window is retention_expired; a
// slow-consumer mid-stream drop is buffer_exceeded.
const (
	ReasonRetentionExpired = "retention_expired"
	ReasonBufferExceeded   = "buffer_exceeded"
)

// ResumeFromInvalidCode is the Error-taxonomy code a malformed or never-recorded
// resume_from is rejected with (EVT-134), the same taxonomy code a server-
// initiated WS close names when it drops a connection for an invalid cursor
// (EVT-096) — one code, two surfaces.
const ResumeFromInvalidCode = CloseResumeFromInvalid

// ResumeError is the RESUME_FROM_INVALID rejection (EVT-134): a resume_from that
// is syntactically malformed, or names an id the platform never recorded, is
// refused before any event is delivered — it is NOT treated as an omitted
// resume_from (which would start fresh). Code is always ResumeFromInvalidCode.
type ResumeError struct {
	Code string
}

func (e *ResumeError) Error() string { return e.Code }

// ResumeOutcome is the resolution of a hello's resume_from against the event log
// (EVT-132/133/140). Result is fresh | resumed | gap. Gap is non-nil only for a
// gap outcome (the retention_expired loss marker). ResumeAtID names the id
// delivery resumes at (the gap's to_id; empty for fresh/resumed). Events is the
// backlog to deliver in id order — empty for fresh, After(resume_from) for a
// clean resume, and the retained log from the oldest id inclusive for a gap
// (so delivery resumes AT to_id with no silent loss, EVT-143).
type ResumeOutcome struct {
	Result     string
	Gap        *GapFrame
	ResumeAtID string
	Events     []Envelope
}

// Resolve determines a hello's resume outcome against log (EVT-130–134,
// 140–143):
//
//   - resumeFrom == ""            → fresh, no backlog (EVT-132).
//   - malformed resumeFrom        → RESUME_FROM_INVALID, zero events, NOT fresh
//     (fails the grammar or is not a ULID; EVT-131/134).
//   - aged out (Log.AgedOut)      → gap, reason retention_expired, to_id the
//     oldest retained id ABOVE the requested point, delivery resumes AT it
//     inclusive (EVT-140/141/143).
//   - never recorded (not aged, not in log) → RESUME_FROM_INVALID (EVT-134).
//   - retained id                 → resumed, the backlog strictly after it,
//     gap-free and duplicate-free (EVT-133).
//
// Every "retained" above means retained AND resolvable by the caller; see
// ResolveVisible, which this is the whole-log case of.
//
// Resolve answers over the WHOLE log, so it is for a caller that IS the
// platform — webhook delivery progress (EndpointState.PendingAfter), whose
// cursor is the platform's own record of what it has delivered, not a value any
// principal supplied. A subscriber-facing binding MUST use ResolveVisible
// instead: resolving a client-supplied resume_from against the whole log tells
// that client whether an id it cannot read exists (EVT-134a).
//
// The aged-out check precedes the membership check so a resume_from older than
// the retention horizon is a gap, not a silent RESUME_FROM_INVALID — a
// discontinuity is always marked, never silently lost (EVT-143).
//
// to_id is OldestRetainedAfter(resumeFrom), not the oldest retained id. Under a
// single uniform horizon those are the same value (everything below the front
// has aged out). Under per-retention-class windows they are not: a 300-day-old
// audit record can still be retained while last week's telemetry has expired, so
// naming the oldest retained id as to_id would tell the subscriber delivery
// resumes BEHIND the point it already reached, and replay events it has
// already seen as though they were the gap's far side.
//
// An aged-out point with nothing retained above it has no id delivery can
// resume at, so it falls through to EVT-134's rejection rather than emitting a
// gap naming an id that does not exist. That is a refusal, not a silent hole:
// the subscriber is told its cursor is unusable and reconnects fresh, which is
// exactly the outcome EVT-134 defines for a point the platform cannot serve.
func Resolve(log Log, resumeFrom string) (ResumeOutcome, *ResumeError) {
	return ResolveVisible(log, resumeFrom, everyEnvelope)
}

// everyEnvelope is Resolve's whole-log visibility: the platform resolving its
// own cursor sees everything it recorded. It is a named function rather than a
// nil predicate meaning "unrestricted" so that the permissive case is something
// a caller ASKS for and a reader can grep for, never something obtained by
// leaving an argument out (SEC-005: never default-permit).
func everyEnvelope(Envelope) bool { return true }

// ResolveVisible is Resolve restricted to one principal's visible set (EVT-120):
// visible reports whether an envelope is inside it, and every question the
// resolution asks of the log is answered as though the envelopes outside it were
// not there.
//
// It exists because resume_from is a CLIENT-SUPPLIED id resolved against a
// SHARED log. Resolving it against the whole log answers a question the client
// was never entitled to ask: an id recorded but placed outside its visible set
// resolved (a stream opened), while an id that never existed was rejected — so
// the difference between the two responses reported whether an arbitrary event
// id exists. That is exactly the probe EVT-122 forbids a selector from
// performing against scope nodes, and EVT-134a states the same rule for ids.
//
// Both now produce the ONE RESUME_FROM_INVALID rejection, which is what
// reconciles EVT-134 with EVT-122's anti-probing posture rather than trading one
// for the other. EVT-122's remedy for an out-of-reach SCOPE NODE is "match
// nothing, never error", but that remedy cannot be transplanted here: a
// resume_from is a position rather than a predicate, so "matches nothing" has no
// meaning for it, and the two candidate readings — start fresh, or resume from
// nothing — are both forbidden outright by EVT-134 ("MUST NOT be treated as
// equivalent to an omitted resume_from"). What EVT-122 actually protects is that
// a client cannot tell a real-but-unreachable thing from a nonexistent one, and
// that is preserved here by making the unreachable case answer identically to
// the nonexistent one. EVT-130 independently says a resume_from is "the exact
// value of some earlier event.id this same principal received", so an id outside
// the visible set was never a legitimate resume_from to begin with.
//
// The visible set is the scope-node boundary alone (EVT-120), never the client's
// own narrowing selector or schemas restriction (EVT-121/124). Those are the
// client's choices, not a security boundary: a subscriber that resumes from an
// id it really did receive and then narrows its selector must still resume, not
// be told its cursor is invalid.
//
// A nil visible predicate resolves nothing: an unresolvable visible set is the
// empty one, so every resume point is unreachable rather than universally
// reachable (SEC-005, matching the zero Filter's own deny-everything posture).
func ResolveVisible(log Log, resumeFrom string, visible func(Envelope) bool) (ResumeOutcome, *ResumeError) {
	if resumeFrom == "" {
		return ResumeOutcome{Result: ResumeResultFresh}, nil // EVT-132
	}
	if !resumeFromGrammar.MatchString(resumeFrom) || !ulid.Valid(resumeFrom) {
		// syntactically malformed — rejected before any delivery (EVT-131/134).
		return ResumeOutcome{}, &ResumeError{Code: ResumeFromInvalidCode}
	}
	if visible == nil {
		return ResumeOutcome{}, &ResumeError{Code: ResumeFromInvalidCode}
	}

	if log.AgedOut(resumeFrom) {
		// older than the retention horizon: the requested point is no longer
		// reconstructible, so mark a retention_expired gap and resume at the
		// oldest recoverable id above it — never a silent loss (EVT-140/141/143).
		// "Recoverable" is per subscriber: to_id is the oldest id above the
		// requested point THIS subscriber may see, so the gap marker cannot name
		// an event outside its visible set any more than delivery can. With
		// nothing visible retained above it there is no such id, so the rejection
		// below applies instead (see this function's own doc).
		//
		// Which outcome an aged-out point draws therefore depends on the
		// subscriber's own retained visible tail and never on whether the
		// requested id was ever recorded — AgedOut is an ordering comparison
		// against the retention floor, not a membership test.
		if to := oldestVisibleAfter(log, resumeFrom, visible); to != "" {
			from := resumeFrom
			return ResumeOutcome{
				Result:     ResumeResultGap,
				Gap:        &GapFrame{Type: FrameTypeGap, FromID: &from, ToID: to, Reason: ReasonRetentionExpired},
				ResumeAtID: to,
				Events:     log.From(to),
			}, nil
		}
	}

	// Membership, evaluated over the visible set. From — not Has — is the read
	// that answers it, because the question is no longer "is this id retained?"
	// but "is this id retained AND inside the visible set?", and only the
	// envelope itself carries the scope_node that settles the second half. From
	// yields it as the head of the very backlog a clean resume returns, so this
	// is one substrate read rather than a membership probe plus a backlog read.
	batch := log.From(resumeFrom)
	if len(batch) == 0 || batch[0].ID != resumeFrom || !visible(batch[0]) {
		// Never recorded, or recorded outside this subscriber's visible set —
		// one rejection for both, never silently treated as fresh (EVT-134/134a).
		return ResumeOutcome{}, &ResumeError{Code: ResumeFromInvalidCode}
	}

	// a recorded id still within retention — resume cleanly with the backlog
	// strictly after it, in id order, gap-free and duplicate-free (EVT-133).
	// The backlog itself is NOT pre-filtered here: EVT-123 puts scope filtering
	// at delivery time, per event, where the client's selector and schemas
	// restrictions are applied too, and where a transport's own watermark
	// advances over every envelope it CONSIDERED rather than only those it sent.
	return ResumeOutcome{
		Result: ResumeResultResumed,
		Events: batch[1:],
	}, nil
}

// oldestVisibleAfter is OldestRetainedAfter over the visible set: the earliest
// retained id strictly greater than id that visible admits, "" when the retained
// tail holds nothing this subscriber may see. It walks the tail rather than
// asking the substrate, because the substrate's own answer knows nothing about
// which principal is asking.
func oldestVisibleAfter(log Log, id string, visible func(Envelope) bool) string {
	return FirstVisibleID(log.After(id), visible)
}

// FirstVisibleID is the id of the earliest envelope in batch (which is in id
// order) that visible admits, "" when it admits none — the resume point a gap
// marker may name for the subscriber visible describes.
//
// It is exported for the MID-STREAM gap, which is resolved by the live transport
// against a tail it has already read rather than by a resume, and which needs the
// identical answer: a marker naming an id outside the subscriber's visible set
// reports the existence of an event that subscriber may not read, which is the
// probe EVT-122 forbids against scope nodes and EVT-134a forbids against ids.
// One definition serves both so the connect-time and mid-stream gaps cannot come
// to different conclusions about what a subscriber may be told.
//
// A nil visible predicate admits nothing: an unresolvable visible set is the
// empty one, never the universal one (SEC-005, matching ResolveVisible).
func FirstVisibleID(batch []Envelope, visible func(Envelope) bool) string {
	if visible == nil {
		return ""
	}
	for _, env := range batch {
		if visible(env) {
			return env.ID
		}
	}
	return ""
}

// BufferExceededGap builds the mid-stream slow-consumer loss marker (EVT-142):
// the same {from_id,to_id,reason} gap shape a retention_expired resume uses,
// with reason buffer_exceeded. fromID is the subscriber's last-delivered id
// (always present here — a mid-stream drop always has a prior point); toID is
// the id delivery resumes at after catching the subscriber up.
func BufferExceededGap(fromID, toID string) GapFrame {
	from := fromID
	return GapFrame{Type: FrameTypeGap, FromID: &from, ToID: toID, Reason: ReasonBufferExceeded}
}
