package eventsse

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/maaxton/waiveo-next/internal/app/auth/authtest"
	"github.com/maaxton/waiveo-next/internal/events"
)

// resume_visibility_test.go drives EVT-134a: a resume_from naming an event the
// platform DID record, but placed outside the subscriber's visible set, must be
// refused exactly as one naming an event that was never recorded — same status,
// same Problem, same close reason.
//
// Resolving a client-supplied cursor against the whole durable log rather than
// against the client's own visible set turns resume_from into an existence
// oracle for arbitrary event ids: "rejected" means the id never existed,
// "stream opened" means it exists somewhere you cannot read. That is the probe
// EVT-122 forbids a selector from performing against scope nodes, and the
// fixture here is the same two-sided one scope_filter_test.go uses, for the same
// reason: the id alice is refused is, at the same instant, proven to exist and
// to be a perfectly good cursor by bob resuming from it.

// neverRecordedEventID is a syntactically valid ULID the fixture never appends.
// It sorts ABOVE the oldest retained id on purpose: an id BELOW the retention
// floor is an aged-out cursor (EVT-141's gap) whatever its provenance, and this
// case is about the membership branch, not the retention branch.
const neverRecordedEventID = idPrefix + "Y3"

// resumeProbe is one connection attempt's whole observable answer.
type resumeProbe struct {
	status      int
	contentType string
	problem     map[string]any
}

// probeResume opens an SSE subscribe carrying resumeFrom and returns everything
// a caller could observe about the answer. trace_id is dropped from the Problem
// for the same reason api/1's own anti-probing test drops it: it is per-request
// by construction (API-010), so it differs between ANY two requests and says
// nothing about the resource. Every other member has to agree.
func probeResume(t *testing.T, e *scopeEnv, cred authtest.Credential, resumeFrom string) resumeProbe {
	t.Helper()
	resp, err := http.DefaultClient.Do(e.request(t, cred, "resume_from="+resumeFrom))
	if err != nil {
		t.Fatalf("dialing SSE: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}
	probe := resumeProbe{status: resp.StatusCode, contentType: resp.Header.Get("Content-Type")}
	if resp.StatusCode != http.StatusOK {
		if err := json.Unmarshal(body, &probe.problem); err != nil {
			t.Fatalf("a refusal must carry an api/1 Problem; got %q (%v)", body, err)
		}
		delete(probe.problem, "trace_id")
	}
	return probe
}

// TestSSE_OutOfScopeResumeFromIsIndistinguishableFromNeverRecorded is EVT-134a's
// oracle on the SSE binding.
func TestSSE_OutOfScopeResumeFromIsIndistinguishableFromNeverRecorded(t *testing.T) {
	e := newScopeEnv(t)
	e.appendWorld()
	// The whole world is recorded before anything connects, and no live delivery
	// is involved: every probe below is answered by the resume resolution alone.
	e.hub.Close()

	// idB is real: recorded, retained, and placed at screen B — inside bob's
	// subtree, outside alice's.
	outOfScope := probeResume(t, e, e.alice, idB)
	neverRecorded := probeResume(t, e, e.alice, neverRecordedEventID)

	if neverRecorded.status != http.StatusBadRequest {
		t.Fatalf("a never-recorded resume_from must be refused 400/RESUME_FROM_INVALID (EVT-134); got %d %v",
			neverRecorded.status, neverRecorded.problem)
	}
	if neverRecorded.problem["code"] != events.ResumeFromInvalidCode {
		t.Fatalf("a never-recorded resume_from's code = %v, want %s (EVT-134)",
			neverRecorded.problem["code"], events.ResumeFromInvalidCode)
	}
	if outOfScope.status != neverRecorded.status {
		t.Fatalf("a resume_from naming an event outside the subscriber's visible set answered %d where a "+
			"never-recorded one answered %d — the difference reports that the id EXISTS to a caller not "+
			"entitled to know it does (EVT-134a)", outOfScope.status, neverRecorded.status)
	}
	if outOfScope.contentType != neverRecorded.contentType {
		t.Fatalf("the two refusals must not differ in Content-Type: out-of-scope %q vs never-recorded %q",
			outOfScope.contentType, neverRecorded.contentType)
	}
	if !reflect.DeepEqual(outOfScope.problem, neverRecorded.problem) {
		t.Fatalf("an out-of-scope resume_from's refusal must be INDISTINGUISHABLE from a never-recorded one's, "+
			"member for member (EVT-134a)\nout-of-scope    %v\nnever-recorded  %v",
			outOfScope.problem, neverRecorded.problem)
	}

	// Both controls matter. Without the first, a handler that refused EVERY
	// resume_from would pass; without the second, a handler that had simply lost
	// the ability to resolve idB at all would pass, and the property under test
	// (the id is real and resumable — just not for alice) would be untested.
	if own := probeResume(t, e, e.alice, idA); own.status != http.StatusOK {
		t.Fatalf("a subscriber must still resume from an id in its OWN visible set (EVT-133); got %d %v",
			own.status, own.problem)
	}
	if theirs := probeResume(t, e, e.bob, idB); theirs.status != http.StatusOK {
		t.Fatalf("the very id alice was refused must resume cleanly for the principal that can see it — "+
			"otherwise the refusal above proves nothing about VISIBILITY; got %d %v", theirs.status, theirs.problem)
	}
}

// TestWS_OutOfScopeResumeFromClosesLikeANeverRecordedOne is the same property on
// the binding where the refusal cannot be an HTTP Problem: the upgrade has
// already happened, so both refusals are a close naming RESUME_FROM_INVALID
// (EVT-096), delivered before any hello-ack or event. A WS binding that closed
// differently — or that answered a hello-ack at all — would reopen on one
// transport the oracle the other closed.
func TestWS_OutOfScopeResumeFromClosesLikeANeverRecordedOne(t *testing.T) {
	e := newScopeEnv(t)
	e.appendWorld()

	closeReasonFor := func(resumeFrom string) string {
		t.Helper()
		conn := dialWS(t, e.srv, e.alice)
		conn.send(t, events.HelloFrame{Type: events.FrameTypeHello, ResumeFrom: resumeFrom})

		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		_, data, err := conn.conn.Read(ctx)
		if err == nil {
			t.Fatalf("a refused resume_from must deliver NOTHING — not a hello-ack, not an event (EVT-134); got %s", data)
		}
		var ce websocket.CloseError
		if !errors.As(err, &ce) {
			t.Fatalf("expected a close naming %s; got %v", events.CloseResumeFromInvalid, err)
		}
		return ce.Reason
	}

	outOfScope := closeReasonFor(idB)
	neverRecorded := closeReasonFor(neverRecordedEventID)
	if neverRecorded != events.CloseResumeFromInvalid {
		t.Fatalf("a never-recorded resume_from must close %s (EVT-096/134); got %q",
			events.CloseResumeFromInvalid, neverRecorded)
	}
	if outOfScope != neverRecorded {
		t.Fatalf("an out-of-scope resume_from closed %q where a never-recorded one closed %q; the two must be "+
			"indistinguishable on this binding too (EVT-134a)", outOfScope, neverRecorded)
	}

	// The positive control, on this binding: the id alice was refused opens a
	// stream for the principal that can see it.
	conn, result := openWS(t, e.srv, e.bob, events.HelloFrame{ResumeFrom: idB})
	defer conn.conn.CloseNow()
	if result != events.ResumeResultResumed {
		t.Fatalf("the id alice was refused must resume cleanly for the principal that can see it; got %q", result)
	}
}
