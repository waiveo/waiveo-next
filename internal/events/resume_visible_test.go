package events

import (
	"reflect"
	"testing"
)

// resume_visible_test.go covers the visible-set half of resume resolution
// (EVT-134a): the questions ResolveVisible asks of the log are asked as though
// the envelopes outside the subscriber's visible set were not there.
//
// The transport-level oracle — two real connections over one real handler,
// compared response member by response member — lives in
// internal/app/eventsse/resume_visibility_test.go. What is pinned here is the
// resolution itself, including the case that never reaches an HTTP status: the
// gap marker's own to_id.

// scopedEnv is an envelope placed at a scope node; scope_node is the sole input
// to visibility (EVT-010/120).
func scopedEnv(id, scopeNode string) Envelope {
	return Envelope{ID: id, ScopeNode: scopeNode}
}

// mine/theirs are the two placements every case here uses, and visibleToMe is
// the subscriber's visible set over them.
const (
	mine   = "01J8Z7A0B1C2D3E4F5G6H7Z5A0"
	theirs = "01J8Z7A0B1C2D3E4F5G6H7Z5B0"
)

func visibleToMe(env Envelope) bool { return env.ScopeNode == mine }

// TestResolveVisible_OutOfScopeIdRejectsExactlyLikeANeverRecordedOne is
// EVT-134a: the two refusals must be the same value, not merely both refusals —
// a difference in code, or a partially-populated outcome carried alongside one
// of them, is a difference a caller could learn from.
func TestResolveVisible_OutOfScopeIdRejectsExactlyLikeANeverRecordedOne(t *testing.T) {
	var l EventLog
	l.Append(scopedEnv("01J8Z3K4N5P6Q7R8S9T0V1W2Y1", mine))
	l.Append(scopedEnv("01J8Z3K4N5P6Q7R8S9T0V1W2Y5", theirs))
	l.Append(scopedEnv("01J8Z3K4N5P6Q7R8S9T0V1W2Y7", mine))

	// Both ids sort inside the retained range, so both take the membership
	// branch rather than the retention branch.
	outOfScope, outOfScopeErr := ResolveVisible(&l, "01J8Z3K4N5P6Q7R8S9T0V1W2Y5", visibleToMe)
	neverRecorded, neverRecordedErr := ResolveVisible(&l, "01J8Z3K4N5P6Q7R8S9T0V1W2Y6", visibleToMe)

	if neverRecordedErr == nil || neverRecordedErr.Code != ResumeFromInvalidCode {
		t.Fatalf("a never-recorded resume_from must be RESUME_FROM_INVALID (EVT-134); got %v", neverRecordedErr)
	}
	if outOfScopeErr == nil {
		t.Fatalf("a resume_from naming an event outside the visible set must be refused, not resolved: %+v", outOfScope)
	}
	if !reflect.DeepEqual(outOfScopeErr, neverRecordedErr) {
		t.Fatalf("the two refusals must be identical (EVT-134a); out-of-scope %+v vs never-recorded %+v",
			outOfScopeErr, neverRecordedErr)
	}
	if !reflect.DeepEqual(outOfScope, neverRecorded) {
		t.Fatalf("a refusal carries no outcome to differ in (EVT-134a); out-of-scope %+v vs never-recorded %+v",
			outOfScope, neverRecorded)
	}

	// The control: the same log, the same call, an id this subscriber CAN see.
	// Without it a resolution that refused everything would satisfy the above.
	out, rerr := ResolveVisible(&l, "01J8Z3K4N5P6Q7R8S9T0V1W2Y1", visibleToMe)
	if rerr != nil {
		t.Fatalf("an id inside the visible set must resume (EVT-133); got %v", rerr)
	}
	if out.Result != ResumeResultResumed {
		t.Fatalf("resume_result must be resumed (EVT-133); got %q", out.Result)
	}
	// The backlog is NOT pre-filtered: EVT-123 applies the delivery boundary per
	// event at delivery time, where the client's selector and schemas
	// restrictions apply too, and where the transport's watermark advances over
	// every envelope it considered rather than only those it sent.
	if want := []string{"01J8Z3K4N5P6Q7R8S9T0V1W2Y5", "01J8Z3K4N5P6Q7R8S9T0V1W2Y7"}; !reflect.DeepEqual(ids(out.Events), want) {
		t.Fatalf("a clean resume returns the whole backlog strictly after the cursor for the transport to filter\n got %v\nwant %v",
			ids(out.Events), want)
	}
}

// TestResolveVisible_GapToIDNamesOnlyAVisibleEvent pins the marker EVT-140/141
// puts on the wire. to_id is an event id the server hands the subscriber, so
// naming the oldest RETAINED id there would disclose an id outside the visible
// set just as surely as resolving a cursor against the whole log would — and it
// would name a point delivery then never actually resumes at, since the event is
// not deliverable to this subscriber.
func TestResolveVisible_GapToIDNamesOnlyAVisibleEvent(t *testing.T) {
	l := NewEventLog(3) // bounded, so the oldest entry really ages out
	l.Append(scopedEnv("01J8Z3K4N5P6Q7R8S9T0V1W2Y1", mine))
	l.Append(scopedEnv("01J8Z3K4N5P6Q7R8S9T0V1W2Y5", theirs))
	l.Append(scopedEnv("01J8Z3K4N5P6Q7R8S9T0V1W2Y6", theirs))
	l.Append(scopedEnv("01J8Z3K4N5P6Q7R8S9T0V1W2Y7", mine))
	// retained: Y5, Y6, Y7 — so Y1 has aged out and the retained tail begins
	// with two events this subscriber may not see.

	out, rerr := ResolveVisible(l, "01J8Z3K4N5P6Q7R8S9T0V1W2Y1", visibleToMe)
	if rerr != nil {
		t.Fatalf("an aged-out resume point is a gap, not a rejection (EVT-141/143); got %v", rerr)
	}
	if out.Result != ResumeResultGap || out.Gap == nil {
		t.Fatalf("resume_result must be gap with a marker (EVT-140/141); got %q %+v", out.Result, out.Gap)
	}
	if out.Gap.Reason != ReasonRetentionExpired {
		t.Fatalf("an aged-out cursor's reason must be retention_expired (EVT-141); got %q", out.Gap.Reason)
	}
	const wantToID = "01J8Z3K4N5P6Q7R8S9T0V1W2Y7"
	if out.Gap.ToID != wantToID {
		t.Fatalf("to_id must be the oldest id above the cursor that this subscriber may see (EVT-134a); got %q, want %q",
			out.Gap.ToID, wantToID)
	}
	if out.ResumeAtID != out.Gap.ToID {
		t.Fatalf("delivery must resume AT to_id (EVT-143); resume-at %q vs to_id %q", out.ResumeAtID, out.Gap.ToID)
	}
	if want := []string{wantToID}; !reflect.DeepEqual(ids(out.Events), want) {
		t.Fatalf("the gap backlog starts AT to_id inclusive\n got %v\nwant %v", ids(out.Events), want)
	}
}

// TestResolveVisible_AgedOutWithNothingVisibleAboveItRejects: an aged-out cursor
// whose retained tail holds nothing this subscriber may see has no id delivery
// could resume at, so it draws EVT-134's rejection rather than a gap naming an
// id the subscriber cannot receive. Which outcome an aged-out cursor draws
// therefore depends on the subscriber's own visible tail — never on whether the
// requested id was ever recorded, since AgedOut is an ordering comparison
// against the retention floor rather than a membership test.
func TestResolveVisible_AgedOutWithNothingVisibleAboveItRejects(t *testing.T) {
	l := NewEventLog(2)
	l.Append(scopedEnv("01J8Z3K4N5P6Q7R8S9T0V1W2Y1", mine))
	l.Append(scopedEnv("01J8Z3K4N5P6Q7R8S9T0V1W2Y5", theirs))
	l.Append(scopedEnv("01J8Z3K4N5P6Q7R8S9T0V1W2Y6", theirs))
	// retained: Y5, Y6 — both invisible to this subscriber.

	for _, resumeFrom := range []string{
		"01J8Z3K4N5P6Q7R8S9T0V1W2Y1", // aged out, and it really was recorded
		"01J8Z3K4N5P6Q7R8S9T0V1W2Y0", // aged out, and never recorded at all
	} {
		out, rerr := ResolveVisible(l, resumeFrom, visibleToMe)
		if rerr == nil {
			t.Fatalf("resume_from %s: an aged-out cursor with nothing visible above it must be refused, not "+
				"gapped to an id the subscriber cannot receive; got %+v", resumeFrom, out)
		}
		if rerr.Code != ResumeFromInvalidCode {
			t.Fatalf("resume_from %s: refusal code = %q, want %s", resumeFrom, rerr.Code, ResumeFromInvalidCode)
		}
	}
}

// TestResolveVisible_NilPredicateResolvesNothing: an unresolvable visible set is
// the EMPTY one, never the whole log (SEC-005 — refuse rather than
// default-permit, the same posture the zero Filter takes).
func TestResolveVisible_NilPredicateResolvesNothing(t *testing.T) {
	var l EventLog
	l.Append(scopedEnv("01J8Z3K4N5P6Q7R8S9T0V1W2Y1", mine))

	if _, rerr := ResolveVisible(&l, "01J8Z3K4N5P6Q7R8S9T0V1W2Y1", nil); rerr == nil {
		t.Fatal("a nil visible predicate must resolve nothing, not everything (SEC-005)")
	}
	// An omitted resume_from still starts fresh: there is no cursor to resolve,
	// so there is nothing to be permissive about (EVT-132).
	out, rerr := ResolveVisible(&l, "", nil)
	if rerr != nil || out.Result != ResumeResultFresh {
		t.Fatalf("an omitted resume_from is fresh whatever the visible set; got %q %v", out.Result, rerr)
	}
}

// TestResolve_IsTheWholeLogCase pins what Resolve now means: the same resolution
// with everything visible — the platform resolving its own cursor (webhook
// delivery progress), never a principal's.
func TestResolve_IsTheWholeLogCase(t *testing.T) {
	var l EventLog
	l.Append(scopedEnv("01J8Z3K4N5P6Q7R8S9T0V1W2Y1", mine))
	l.Append(scopedEnv("01J8Z3K4N5P6Q7R8S9T0V1W2Y5", theirs))

	out, rerr := Resolve(&l, "01J8Z3K4N5P6Q7R8S9T0V1W2Y5")
	if rerr != nil {
		t.Fatalf("the whole-log resolution sees every placement; got %v", rerr)
	}
	if out.Result != ResumeResultResumed {
		t.Fatalf("resume_result must be resumed; got %q", out.Result)
	}
}
