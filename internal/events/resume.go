package events

// resume.go resolves a WS/SSE hello's resume_from against the durable event log
// (eventlog.go) into one of events/1's four resume outcomes (EVT-130–134,
// 140–143): fresh (no resume_from), resumed (a clean backlog after a retained
// id), gap (a resume_from aged past the retention horizon — a retention_expired
// loss marker, never a silent hole), or the RESUME_FROM_INVALID rejection (a
// malformed or never-recorded resume_from, refused before any delivery and
// never silently treated as fresh).
//
// resume_from is a TRANSPARENT event id (EVT-130/131): the exact value of an
// envelope id this principal previously received, matching ^[A-Za-z0-9_-]+$ so
// it round-trips a hello field or an SSE query parameter unescaped. That charset
// is the ONE property it shares with api/1's keyset cursor (API-036); it is
// otherwise a client-persisted, comparable id, not an opaque token — so this
// grammar is defined locally here, not borrowed from the pagination cursor.

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
	if resumeFrom == "" {
		return ResumeOutcome{Result: ResumeResultFresh}, nil // EVT-132
	}
	if !resumeFromGrammar.MatchString(resumeFrom) || !ulid.Valid(resumeFrom) {
		// syntactically malformed — rejected before any delivery (EVT-131/134).
		return ResumeOutcome{}, &ResumeError{Code: ResumeFromInvalidCode}
	}

	if log.AgedOut(resumeFrom) {
		// older than the retention horizon: the requested point is no longer
		// reconstructible, so mark a retention_expired gap and resume at the
		// oldest recoverable id above it — never a silent loss (EVT-140/141/143).
		// With nothing retained above it there is no such id, so the rejection
		// below applies instead (see this function's own doc).
		if to := log.OldestRetainedAfter(resumeFrom); to != "" {
			from := resumeFrom
			return ResumeOutcome{
				Result:     ResumeResultGap,
				Gap:        &GapFrame{Type: FrameTypeGap, FromID: &from, ToID: to, Reason: ReasonRetentionExpired},
				ResumeAtID: to,
				Events:     log.From(to),
			}, nil
		}
	}

	if !log.Has(resumeFrom) {
		// within (or ahead of) the retention window but never recorded — a
		// fabricated or wrong id, rejected, never silently treated as fresh
		// (EVT-134).
		return ResumeOutcome{}, &ResumeError{Code: ResumeFromInvalidCode}
	}

	// a recorded id still within retention — resume cleanly with the backlog
	// strictly after it, in id order, gap-free and duplicate-free (EVT-133).
	return ResumeOutcome{
		Result: ResumeResultResumed,
		Events: log.After(resumeFrom),
	}, nil
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
