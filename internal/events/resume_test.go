package events

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// corpusBytes reads an events-1 corpus case by file name relative to this
// package (the corpus is the oracle for resume_result / RESUME_FROM_INVALID /
// the gap shape).
func corpusBytes(t *testing.T, name string) []byte {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(thisFile), "..", "..",
		"conformance", "corpora", "events-1", name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read corpus %s: %v", name, err)
	}
	return raw
}

// TestResolve_EmptyResumeFrom_Fresh (EVT-132): omitting resume_from starts
// delivery fresh — resume_result fresh, no backlog, no gap, no error.
func TestResolve_EmptyResumeFrom_Fresh(t *testing.T) {
	var l EventLog
	l.Append(env("01J8Z3K4N5P6Q7R8S9T0V1W2Y7"))
	out, rerr := Resolve(&l, "")
	if rerr != nil {
		t.Fatalf("an empty resume_from must not error; got %v", rerr)
	}
	if out.Result != ResumeResultFresh {
		t.Fatalf("resume_result must be fresh (EVT-132); got %q", out.Result)
	}
	if len(out.Events) != 0 {
		t.Fatalf("a fresh subscribe delivers no backlog (EVT-132); got %d events", len(out.Events))
	}
	if out.Gap != nil {
		t.Fatal("a fresh subscribe carries no gap")
	}
}

// TestResolve_MalformedResumeFrom_Invalid (EVT-131/134) — driven by the
// EVT-134 corpus: a syntactically malformed resume_from is rejected with
// RESUME_FROM_INVALID before any event is delivered, and is NOT treated as an
// omitted (fresh) resume_from.
func TestResolve_MalformedResumeFrom_Invalid(t *testing.T) {
	var c struct {
		Input struct {
			Frame struct {
				Type       string `json:"type"`
				ResumeFrom string `json:"resume_from"`
			} `json:"frame"`
		} `json:"input"`
		Expected struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
			EventsDelivered int  `json:"events_delivered"`
			TreatedAsFresh  bool `json:"treated_as_fresh"`
		} `json:"expected"`
	}
	if err := json.Unmarshal(corpusBytes(t, "EVT-134-invalid-resume-from-malformed.json"), &c); err != nil {
		t.Fatalf("unmarshal EVT-134 corpus: %v", err)
	}

	// The log holds live events; a malformed resume_from must be rejected
	// regardless of what the log contains.
	var l EventLog
	l.Append(env("01J8Z3K4N5P6Q7R8S9T0V1W2Y7"))

	out, rerr := Resolve(&l, c.Input.Frame.ResumeFrom)
	if rerr == nil {
		t.Fatal("a malformed resume_from must return a *ResumeError (EVT-134)")
	}
	if rerr.Code != c.Expected.Error.Code {
		t.Fatalf("error code = %q; want %q (corpus)", rerr.Code, c.Expected.Error.Code)
	}
	if got := len(out.Events); got != c.Expected.EventsDelivered {
		t.Fatalf("events_delivered = %d; want %d (corpus) — no delivery before rejection", got, c.Expected.EventsDelivered)
	}
	treatedAsFresh := rerr == nil && out.Result == ResumeResultFresh
	if treatedAsFresh != c.Expected.TreatedAsFresh {
		t.Fatalf("treated_as_fresh = %v; want %v (corpus) — a malformed resume_from is never silently fresh (EVT-134)", treatedAsFresh, c.Expected.TreatedAsFresh)
	}
}

// TestResolve_NeverRecordedID_Invalid (EVT-134): a well-formed ULID that names
// an id the platform never recorded — and that is not older than the retention
// horizon (so it is not an aged-out gap) — is rejected RESUME_FROM_INVALID,
// never silently treated as fresh.
func TestResolve_NeverRecordedID_Invalid(t *testing.T) {
	var l EventLog
	l.Append(env("01J8Z3K4N5P6Q7R8S9T0V1W2Y7"))
	// A valid ULID greater than the oldest retained id but not in the log.
	out, rerr := Resolve(&l, "01J8Z3K4N5P6Q7R8S9T0V1W2Y9")
	if rerr == nil || rerr.Code != CloseResumeFromInvalid {
		t.Fatalf("a never-recorded id must be RESUME_FROM_INVALID (EVT-134); got out=%+v err=%v", out, rerr)
	}
	if out.Result == ResumeResultFresh {
		t.Fatal("a never-recorded id must never be treated as fresh (EVT-134)")
	}
}

// TestResolve_RetainedID_Resumed (EVT-133): a recorded id still within the
// retention window resumes with exactly the events after it, in id order, with
// neither a gap nor a duplicate.
func TestResolve_RetainedID_Resumed(t *testing.T) {
	var l EventLog
	l.Append(env("01J8Z3K4N5P6Q7R8S9T0V1W2Y6"))
	l.Append(env("01J8Z3K4N5P6Q7R8S9T0V1W2Y7"))
	l.Append(env("01J8Z3K4N5P6Q7R8S9T0V1W2Y8"))

	out, rerr := Resolve(&l, "01J8Z3K4N5P6Q7R8S9T0V1W2Y6")
	if rerr != nil {
		t.Fatalf("a retained resume_from must resume, not error; got %v", rerr)
	}
	if out.Result != ResumeResultResumed {
		t.Fatalf("resume_result must be resumed (EVT-133); got %q", out.Result)
	}
	if out.Gap != nil {
		t.Fatal("a clean resume carries no gap (EVT-133)")
	}
	got := ids(out.Events)
	want := []string{"01J8Z3K4N5P6Q7R8S9T0V1W2Y7", "01J8Z3K4N5P6Q7R8S9T0V1W2Y8"}
	if len(got) != len(want) {
		t.Fatalf("resumed events = %v; want %v (after the resume point, gap-free, no dup)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("resumed events = %v; want %v", got, want)
		}
	}
}

// TestResolve_AgedOutID_Gap (EVT-140/141/143) — driven by the EVT-140 corpus:
// a resume_from older than the retention window acks resume_result gap, emits
// one {from_id,to_id,reason=retention_expired} gap naming the requested point
// and the oldest recoverable id, and resumes delivery at that oldest id with no
// silent loss.
func TestResolve_AgedOutID_Gap(t *testing.T) {
	var c struct {
		Input struct {
			Frame struct {
				Type       string `json:"type"`
				ResumeFrom string `json:"resume_from"`
			} `json:"frame"`
			OldestRetainedID string `json:"oldest_retained_id"`
		} `json:"input"`
		Expected struct {
			Frames []struct {
				Type         string  `json:"type"`
				ResumeResult string  `json:"resume_result"`
				FromID       *string `json:"from_id"`
				ToID         string  `json:"to_id"`
				Reason       string  `json:"reason"`
			} `json:"frames"`
			DeliveryResumesAt string `json:"delivery_resumes_at"`
			SilentLoss        bool   `json:"silent_loss"`
		} `json:"expected"`
	}
	if err := json.Unmarshal(corpusBytes(t, "EVT-140-valid-resume-with-gap.json"), &c); err != nil {
		t.Fatalf("unmarshal EVT-140 corpus: %v", err)
	}

	// Build a log whose oldest retained id is exactly the case's input — the
	// requested resume_from predates it (aged out).
	var l EventLog
	l.Append(env(c.Input.OldestRetainedID))
	out, rerr := Resolve(&l, c.Input.Frame.ResumeFrom)
	if rerr != nil {
		t.Fatalf("an aged-out resume must not error — it gaps, never silently loses (EVT-143); got %v", rerr)
	}

	// hello-ack frame (corpus frame 0).
	ack := c.Expected.Frames[0]
	if out.Result != ack.ResumeResult {
		t.Fatalf("resume_result = %q; want %q (corpus hello-ack)", out.Result, ack.ResumeResult)
	}
	// gap frame (corpus frame 1).
	gf := c.Expected.Frames[1]
	if out.Gap == nil {
		t.Fatal("an aged-out resume must carry a gap frame (EVT-140)")
	}
	if out.Gap.Type != gf.Type {
		t.Fatalf("gap type = %q; want %q", out.Gap.Type, gf.Type)
	}
	if out.Gap.FromID == nil || *out.Gap.FromID != *gf.FromID {
		t.Fatalf("gap from_id = %v; want %q (the requested resume point)", out.Gap.FromID, *gf.FromID)
	}
	if out.Gap.ToID != gf.ToID {
		t.Fatalf("gap to_id = %q; want %q (the oldest recoverable id)", out.Gap.ToID, gf.ToID)
	}
	if out.Gap.Reason != gf.Reason {
		t.Fatalf("gap reason = %q; want %q", out.Gap.Reason, gf.Reason)
	}
	if out.ResumeAtID != c.Expected.DeliveryResumesAt {
		t.Fatalf("delivery_resumes_at = %q; want %q (corpus)", out.ResumeAtID, c.Expected.DeliveryResumesAt)
	}
	// no silent loss: delivery resumes at the oldest retained id inclusive.
	if len(out.Events) == 0 || out.Events[0].ID != c.Expected.DeliveryResumesAt {
		t.Fatalf("delivery must resume AT the oldest retained id inclusive (EVT-143, no silent loss); got %v", ids(out.Events))
	}
}

// TestBufferExceededGap (EVT-142): the slow-consumer mid-stream drop is marked
// with the same {from_id,to_id,reason} shape, reason buffer_exceeded, from_id
// the subscriber's last-delivered point.
func TestBufferExceededGap(t *testing.T) {
	g := BufferExceededGap("01J8Z3K4N5P6Q7R8S9T0V1W2Y6", "01J8Z3K4N5P6Q7R8S9T0V1W2Y9")
	if g.Type != FrameTypeGap {
		t.Fatalf("gap type = %q; want gap", g.Type)
	}
	if g.Reason != ReasonBufferExceeded {
		t.Fatalf("reason = %q; want buffer_exceeded (EVT-142)", g.Reason)
	}
	if g.FromID == nil || *g.FromID != "01J8Z3K4N5P6Q7R8S9T0V1W2Y6" {
		t.Fatalf("from_id must name the subscriber's last-delivered point; got %v", g.FromID)
	}
	if g.ToID != "01J8Z3K4N5P6Q7R8S9T0V1W2Y9" {
		t.Fatalf("to_id = %q; want the id delivery resumes at", g.ToID)
	}
}
