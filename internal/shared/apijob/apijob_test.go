package apijob

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	apiv1 "github.com/maaxton/waiveo-next/api/gen/go"
)

// jobCorpusCase is the slice of an api-1 Job corpus case's pinned response this
// test drives against: the 202 Job body's own members (API-112). The frozen
// corpus JSON is the oracle — the shape and every state value are read from the
// file, never re-typed here.
type jobCorpusCase struct {
	Expected struct {
		Status int `json:"status"`
		Body   struct {
			ID        string `json:"id"`
			CreatedBy string `json:"created_by"`
			State     string `json:"state"`
			CreatedAt string `json:"created_at"`
			Targets   []struct {
				TargetID string `json:"target_id"`
				State    string `json:"state"`
			} `json:"targets"`
		} `json:"body"`
	} `json:"expected"`
}

func loadJobCase(t *testing.T, name string) jobCorpusCase {
	t.Helper()
	path := filepath.Join("..", "..", "..", "conformance", "corpora", "api-1", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read corpus case %s: %v", name, err)
	}
	var c jobCorpusCase
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatalf("decode corpus case %s: %v", name, err)
	}
	return c
}

// TestNewJobMatchesAPI111Corpus drives API-111/112: a freshly-accepted Job over
// a two-target set renders to the pinned 202 body verbatim — id, created_by,
// created_at, a job-level state of pending, and one {target_id, state:pending}
// entry per target, under the exact API-112 member names.
func TestNewJobMatchesAPI111Corpus(t *testing.T) {
	c := loadJobCase(t, "API-111-valid-bulk-enable-202-job.json")

	createdAt, err := time.Parse(time.RFC3339, c.Expected.Body.CreatedAt)
	if err != nil {
		t.Fatalf("parse corpus created_at: %v", err)
	}
	targetIDs := make([]string, 0, len(c.Expected.Body.Targets))
	for _, tgt := range c.Expected.Body.Targets {
		targetIDs = append(targetIDs, tgt.TargetID)
	}

	job := New(c.Expected.Body.ID, c.Expected.Body.CreatedBy, createdAt, targetIDs)

	if got := job.State(); got != apiv1.JobStatePending {
		t.Errorf("new job state = %q, want pending (all targets pending, API-113)", got)
	}

	// Render to the wire model and compare the emitted JSON, member for member,
	// against the corpus's pinned body — the corpus is the oracle for the
	// API-112 shape (field names) and every state value.
	raw, err := json.Marshal(job.Resource())
	if err != nil {
		t.Fatalf("marshal job resource: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode job resource: %v", err)
	}

	wantBody, err := os.ReadFile(filepath.Join("..", "..", "..", "conformance", "corpora", "api-1", "API-111-valid-bulk-enable-202-job.json"))
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var full map[string]any
	if err := json.Unmarshal(wantBody, &full); err != nil {
		t.Fatalf("decode corpus: %v", err)
	}
	want := full["expected"].(map[string]any)["body"].(map[string]any)

	for k, wv := range want {
		gv, present := got[k]
		if !present {
			t.Errorf("job body missing member %q (API-112), want %v", k, wv)
			continue
		}
		if !reflect.DeepEqual(gv, wv) {
			t.Errorf("job body[%q] = %v, want %v", k, gv, wv)
		}
	}
}

// TestJobStateProgression drives API-113: a job progresses pending -> running
// -> terminal, and the job-level terminal value is succeeded / failed / partial
// by whether the targets' outcomes are uniform or mixed — partial is job-level
// only and never appears as a target state.
func TestJobStateProgression(t *testing.T) {
	newTwo := func() *Job {
		return New("01J8Z3K4N5P6Q7R8S9T0V1W2Y3", "usr_A", time.Unix(0, 0).UTC(), []string{"t1", "t2"})
	}

	t.Run("all succeeded -> succeeded", func(t *testing.T) {
		j := newTwo()
		mustStart(t, j, "t1", "t2")
		if s := j.State(); s != apiv1.JobStateRunning {
			t.Errorf("after starting both, state = %q, want running", s)
		}
		mustSucceed(t, j, "t1", "t2")
		if s := j.State(); s != apiv1.JobStateSucceeded {
			t.Errorf("all-succeeded state = %q, want succeeded", s)
		}
	})

	t.Run("mixed -> partial", func(t *testing.T) {
		j := newTwo()
		mustStart(t, j, "t1", "t2")
		mustSucceed(t, j, "t1")
		if err := j.FailTarget("t2", "INTERNAL", "boom"); err != nil {
			t.Fatalf("FailTarget: %v", err)
		}
		if s := j.State(); s != apiv1.JobStatePartial {
			t.Errorf("mixed-outcome state = %q, want partial (API-113)", s)
		}
		// partial is job-level only — assert no target carries it.
		for _, tgt := range j.Targets() {
			if string(tgt.State) == string(apiv1.JobStatePartial) {
				t.Errorf("target %q has state partial — partial is job-level only (API-113)", tgt.ID)
			}
		}
	})

	t.Run("all failed -> failed", func(t *testing.T) {
		j := newTwo()
		mustStart(t, j, "t1", "t2")
		if err := j.FailTarget("t1", "INTERNAL", "a"); err != nil {
			t.Fatalf("FailTarget t1: %v", err)
		}
		if err := j.FailTarget("t2", "UNAVAILABLE", "b"); err != nil {
			t.Fatalf("FailTarget t2: %v", err)
		}
		if s := j.State(); s != apiv1.JobStateFailed {
			t.Errorf("all-failed state = %q, want failed", s)
		}
	})
}

// TestJobInvalidTransitionsRejected drives API-113's closed sequence: the only
// legal entry into execution is pending -> running, and a terminal value is only
// reachable from running. Every off-path transition is an error, not a silent
// no-op.
func TestJobInvalidTransitionsRejected(t *testing.T) {
	j := New("j", "usr_A", time.Unix(0, 0).UTC(), []string{"t1"})

	if err := j.SucceedTarget("t1"); err == nil {
		t.Error("succeeding a pending target should be rejected (pending -> running -> terminal, API-113)")
	}
	if err := j.FailTarget("t1", "INTERNAL", "x"); err == nil {
		t.Error("failing a pending target should be rejected (must be running first, API-113)")
	}
	if err := j.StartTarget("nope"); err == nil {
		t.Error("starting an unknown target should be rejected")
	}

	mustStart(t, j, "t1")
	if err := j.StartTarget("t1"); err == nil {
		t.Error("starting an already-running target should be rejected (API-113)")
	}
	mustSucceed(t, j, "t1")
	if err := j.SucceedTarget("t1"); err == nil {
		t.Error("re-succeeding a terminal target should be rejected (API-113)")
	}
}

// TestPerTargetFailureUsesRegistry drives API-115: a per-target failure MUST be
// typed with a code from api/1's own error-code registry — a code outside it is
// rejected, and the recorded failure carries the registry code.
func TestPerTargetFailureUsesRegistry(t *testing.T) {
	j := New("j", "usr_A", time.Unix(0, 0).UTC(), []string{"t1"})
	mustStart(t, j, "t1")

	if err := j.FailTarget("t1", "KABOOM", "not a registry code"); err == nil {
		t.Error("a per-target failure code outside the api/1 registry must be rejected (API-115)")
	}
	if j.Targets()[0].State != apiv1.JobTargetStatePending && j.Targets()[0].State != apiv1.JobTargetStateRunning {
		t.Errorf("a rejected failure must not move the target to failed, got %q", j.Targets()[0].State)
	}
	if err := j.FailTarget("t1", "INTERNAL", "the real failure"); err != nil {
		t.Fatalf("FailTarget with a registry code: %v", err)
	}
	got := j.Targets()[0]
	if got.State != apiv1.JobTargetStateFailed {
		t.Errorf("target state = %q, want failed", got.State)
	}
	if !CodeInRegistry(got.ErrCode) {
		t.Errorf("failed target code %q is not in the api/1 registry (API-115)", got.ErrCode)
	}
}

// TestCancelSemantics drives API-114: cancel marks not-yet-started (pending)
// targets failed with a cancellation-attributed registry error, leaves an
// already-running target running and an already-terminal target untouched, and
// is a no-op that returns the current state on an already-terminal job.
func TestCancelSemantics(t *testing.T) {
	j := New("j", "usr_A", time.Unix(0, 0).UTC(), []string{"done", "running", "waiting"})
	// done: reach a terminal state before cancel.
	mustStart(t, j, "done")
	mustSucceed(t, j, "done")
	// running: started but not terminal.
	mustStart(t, j, "running")
	// waiting: still pending.

	state := j.Cancel()

	byID := map[string]Target{}
	for _, tgt := range j.Targets() {
		byID[tgt.ID] = tgt
	}
	if byID["done"].State != apiv1.JobTargetStateSucceeded {
		t.Errorf("cancel rolled back an already-terminal target: done = %q, want succeeded (API-114)", byID["done"].State)
	}
	if byID["running"].State != apiv1.JobTargetStateRunning {
		t.Errorf("cancel stopped an already-running target: running = %q, want running (API-114)", byID["running"].State)
	}
	if byID["waiting"].State != apiv1.JobTargetStateFailed {
		t.Errorf("cancel left a pending target unstarted-but-not-failed: waiting = %q, want failed (API-114)", byID["waiting"].State)
	}
	if byID["waiting"].ErrCode != cancelCode {
		t.Errorf("canceled target error code = %q, want %q (cancellation-attributed registry error, API-114/115)", byID["waiting"].ErrCode, cancelCode)
	}
	if !CodeInRegistry(byID["waiting"].ErrCode) {
		t.Errorf("cancellation-attributed code %q is not in the api/1 registry (API-115)", byID["waiting"].ErrCode)
	}
	// One succeeded, one failed, one running -> not yet terminal -> running.
	if state != apiv1.JobStateRunning {
		t.Errorf("post-cancel state = %q, want running (a running target remains, API-114)", state)
	}

	// Drive the running target to terminal, then confirm cancel-on-terminal is a
	// no-op returning the current (unchanged) state.
	mustSucceed(t, j, "running")
	terminal := j.State() // succeeded + failed + succeeded -> partial
	if terminal != apiv1.JobStatePartial {
		t.Fatalf("precondition: state = %q, want partial", terminal)
	}
	if got := j.Cancel(); got != apiv1.JobStatePartial {
		t.Errorf("cancel on a terminal job = %q, want the unchanged partial (no-op, API-114)", got)
	}
}

// TestNewJobDedupesTargets confirms a duplicated target id collapses to a single
// entry, so the target index is unambiguous and the job does not double-count a
// resource named twice.
func TestNewJobDedupesTargets(t *testing.T) {
	j := New("j", "usr_A", time.Unix(0, 0).UTC(), []string{"t1", "t1", "t2"})
	if n := len(j.Targets()); n != 2 {
		t.Errorf("targets = %d, want 2 (duplicate id collapsed)", n)
	}
}

func mustStart(t *testing.T, j *Job, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := j.StartTarget(id); err != nil {
			t.Fatalf("StartTarget(%q): %v", id, err)
		}
	}
}

func mustSucceed(t *testing.T, j *Job, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := j.SucceedTarget(id); err != nil {
			t.Fatalf("SucceedTarget(%q): %v", id, err)
		}
	}
}
