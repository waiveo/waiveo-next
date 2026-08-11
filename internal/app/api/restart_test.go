package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/api"
	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/shared/apijob"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// restart_test.go drives POST /api/v1/system/restart (api/1 API-150-157).
//
// The property every case here is arranged around is the one this operation can
// most easily get wrong: a REFUSED restart must not have armed anything. It is
// not enough that the response says 403 or 409 — the process must still be
// running afterwards, and the only way to assert that from a test is to observe
// the seam the process would have been stopped through. So every case that
// expects a refusal asserts `arms == 0`, and the accepted cases assert exactly
// how many times it armed. A refusal that returned the right status while
// stopping the box anyway would be the worst possible defect in this file, and
// it is the one the status assertion alone cannot see.

// restartEnv is a testEnv plus the recording restart seam wired into it.
type restartEnv struct {
	*testEnv
	mu     sync.Mutex
	orders []api.RestartOrder
	// allow is what arm reports. Set false to simulate a process that has
	// already armed a restart, which is the state RESTART_IN_PROGRESS describes.
	allow bool
}

// newRestartEnv builds an env whose restart seam DECLARES a supervisor, so the
// operation reaches its accepted path. The seam records rather than stops, for
// the obvious reason: a test binary that actually honoured the order would end
// at the first case.
func newRestartEnv(t *testing.T) *restartEnv {
	t.Helper()
	re := &restartEnv{allow: true}
	re.testEnv = newEnvWithOptions(t, api.WithRestart(api.RestartConfig{
		Supervisor:    "test-supervisor",
		DrainBudgetMs: 5_000,
		Arm: func(o api.RestartOrder) bool {
			re.mu.Lock()
			defer re.mu.Unlock()
			if !re.allow {
				return false
			}
			re.orders = append(re.orders, o)
			return true
		},
	}), api.WithSystemHealth(api.SystemHealthConfig{
		StartedAtMs: restartFixtureStartedAtMs,
		Version:     "test-build",
		DataDir:     t.TempDir(),
	}))
	return re
}

// restartFixtureStartedAtMs is the process-start instant the fixture publishes.
// Deliberately NOT fixedNowMs: the acceptance must echo the START instant, and a
// fixture where the two coincide would let a handler that echoed `nowMs()` pass.
const restartFixtureStartedAtMs = fixedNowMs - 3_600_000

func (re *restartEnv) arms() int {
	re.mu.Lock()
	defer re.mu.Unlock()
	return len(re.orders)
}

func (re *restartEnv) lastOrder(t *testing.T) api.RestartOrder {
	t.Helper()
	re.mu.Lock()
	defer re.mu.Unlock()
	if len(re.orders) == 0 {
		t.Fatal("nothing was armed")
	}
	return re.orders[len(re.orders)-1]
}

type restartAcceptanceBody struct {
	AcceptedAtMs  int64  `json:"accepted_at_ms"`
	StoppingInMs  int64  `json:"stopping_in_ms"`
	DrainBudgetMs int64  `json:"drain_budget_ms"`
	StartedAtMs   int64  `json:"started_at_ms"`
	Supervisor    string `json:"supervisor"`
}

type problemBody struct {
	Status int    `json:"status"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

func decodeProblem(t *testing.T, raw []byte) problemBody {
	t.Helper()
	var p problemBody
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("decode Problem: %v (body %s)", err, raw)
	}
	return p
}

// postRestart drives the operation as the env's root-bound owner.
func (re *restartEnv) postRestart(t *testing.T, body any, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	re.orgRoot(t)
	return re.do(t, http.MethodPost, "/api/v1/system/restart", mustJSON(t, body), headers)
}

// TestRestartIsAcceptedNeverClaimedDone is API-152/153: the answer is 202, and
// its body publishes what a client can actually observe.
func TestRestartIsAcceptedNeverClaimedDone(t *testing.T) {
	re := newRestartEnv(t)

	resp, raw := re.postRestart(t, map[string]any{"confirm": true}, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /system/restart = %d, want 202 — a 200 would claim a restart the responding process cannot have observed (body %s)", resp.StatusCode, raw)
	}
	var got restartAcceptanceBody
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode acceptance: %v (body %s)", err, raw)
	}
	if got.Supervisor != "test-supervisor" {
		t.Errorf("supervisor = %q, want the DECLARED one — the acceptance has to say who is responsible for the process coming back", got.Supervisor)
	}
	if got.StoppingInMs <= 0 {
		t.Errorf("stopping_in_ms = %d; a client that is not told the window has to guess it", got.StoppingInMs)
	}
	if got.DrainBudgetMs != 5_000 {
		t.Errorf("drain_budget_ms = %d, want the budget the PROCESS enforces (5000) — a published window nothing honours is worse than none", got.DrainBudgetMs)
	}
	// The load-bearing one: the echoed instant must be this process instance's
	// START, not "now". A client compares it against a later /system-health, and
	// an echo of `now` would differ on every read and report a restart that never
	// happened.
	if got.StartedAtMs != restartFixtureStartedAtMs {
		t.Errorf("started_at_ms = %d, want the process START instant %d — this is the value a client compares a later health read against",
			got.StartedAtMs, restartFixtureStartedAtMs)
	}
	if got.AcceptedAtMs != fixedNowMs {
		t.Errorf("accepted_at_ms = %d, want %d", got.AcceptedAtMs, fixedNowMs)
	}

	if re.arms() != 1 {
		t.Fatalf("armed %d time(s), want exactly 1", re.arms())
	}
	if actor := re.lastOrder(t).Actor; actor != re.auth.Credential().PrincipalID {
		t.Errorf("the armed order's actor = %q, want the REAL authenticated caller %q", actor, re.auth.Credential().PrincipalID)
	}
}

// TestRestartEchoesTheSameInstantHealthReports is the other half of API-153: the
// completion signal only works if the two surfaces agree on the value.
//
// They are asserted against each other rather than each against a constant,
// because the defect this guards is DIVERGENCE — two separately-sourced "start
// instants" would make a client's before/after comparison meaningless the first
// time they disagreed, and each would still match its own constant.
func TestRestartEchoesTheSameInstantHealthReports(t *testing.T) {
	re := newRestartEnv(t)
	re.orgRoot(t)

	_, healthRaw := re.do(t, http.MethodGet, "/api/v1/system-health", nil, nil)
	var health struct {
		StartedAtMs int64 `json:"started_at_ms"`
	}
	if err := json.Unmarshal(healthRaw, &health); err != nil {
		t.Fatalf("decode health: %v", err)
	}

	_, raw := re.postRestart(t, map[string]any{"confirm": true}, nil)
	var acc restartAcceptanceBody
	if err := json.Unmarshal(raw, &acc); err != nil {
		t.Fatalf("decode acceptance: %v", err)
	}
	if acc.StartedAtMs != health.StartedAtMs {
		t.Fatalf("the acceptance echoes started_at_ms=%d and /system-health reports %d — a client comparing the two learns nothing",
			acc.StartedAtMs, health.StartedAtMs)
	}
}

// TestRestartRequiresAnExplicitConfirmation: `confirm: false` satisfies the
// declared schema (it is a boolean) and must not satisfy the operation, and an
// empty body must not either.
func TestRestartRequiresAnExplicitConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name string
		body any
	}{
		{"confirm false", map[string]any{"confirm": false}},
		{"no confirm at all", map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			re := newRestartEnv(t)
			resp, raw := re.postRestart(t, tc.body, nil)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422 (body %s)", resp.StatusCode, raw)
			}
			if code := decodeProblem(t, raw).Code; code != "VALIDATION_FAILED" {
				t.Errorf("code = %q, want VALIDATION_FAILED", code)
			}
			if re.arms() != 0 {
				t.Fatal("an unconfirmed request armed a restart — the confirmation is the whole guard")
			}
		})
	}
}

// TestRestartRefusesAnUndeclaredMember: the declared schema is
// additionalProperties:false, so a member the document does not define is
// refused rather than silently ignored. A caller who believes they passed
// `force` and had it dropped is worse off than one who was told.
func TestRestartRefusesAnUndeclaredMember(t *testing.T) {
	re := newRestartEnv(t)
	resp, raw := re.postRestart(t, map[string]any{"confirm": true, "force": true}, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", resp.StatusCode, raw)
	}
	if re.arms() != 0 {
		t.Fatal("a body with an undeclared member armed a restart")
	}
}

// TestRestartRefusesWithoutADeclaredSupervisor is API-154, and it is the case
// this whole design exists for: stopping a process nothing will restart is not a
// restart, it is taking the box down from a button in the console.
//
// Both shapes of "no supervisor" are driven — the option unwired entirely, and
// the option wired with an empty declaration — because they are two ways a
// deployment reaches the same state and an implementation that handled one and
// not the other would be a live trap on the other.
func TestRestartRefusesWithoutADeclaredSupervisor(t *testing.T) {
	armed := 0
	for _, tc := range []struct {
		name string
		opts []api.Option
	}{
		{"no restart option at all", nil},
		{"wired, but declaring no supervisor", []api.Option{api.WithRestart(api.RestartConfig{
			DrainBudgetMs: 5_000,
			Arm:           func(api.RestartOrder) bool { armed++; return true },
		})}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			armed = 0
			e := newEnvWithOptions(t, tc.opts...)
			e.orgRoot(t)
			resp, raw := e.do(t, http.MethodPost, "/api/v1/system/restart",
				mustJSON(t, map[string]any{"confirm": true}), nil)
			if resp.StatusCode != http.StatusNotImplemented {
				t.Fatalf("status = %d, want 501 (body %s)", resp.StatusCode, raw)
			}
			p := decodeProblem(t, raw)
			if p.Code != "RESTART_UNSUPPORTED" {
				t.Fatalf("code = %q, want RESTART_UNSUPPORTED — a generic UNAVAILABLE would invite a retry that can never succeed", p.Code)
			}
			// The refusal has to be actionable. Naming the variable is the whole
			// difference between "this box cannot restart" and "here is how to make
			// it able to".
			if !strings.Contains(p.Detail, "WAIVEO_FEEDER_SUPERVISOR") {
				t.Errorf("detail does not name what to declare, so an operator cannot act on it: %q", p.Detail)
			}
			if armed != 0 {
				t.Fatal("an unsupervised deployment armed a restart — the box would have gone down and stayed down")
			}
		})
	}
}

// TestRestartRefusesASecondConcurrentRequest is API-155. The first stop is the
// only one that can happen, so a second 202 would report an act that will not
// occur.
func TestRestartRefusesASecondConcurrentRequest(t *testing.T) {
	re := newRestartEnv(t)

	if resp, raw := re.postRestart(t, map[string]any{"confirm": true}, nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first restart = %d, want 202 (body %s)", resp.StatusCode, raw)
	}
	// The process has armed; its seam now reports "already".
	re.mu.Lock()
	re.allow = false
	re.mu.Unlock()

	resp, raw := re.postRestart(t, map[string]any{"confirm": true}, nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("second restart = %d, want 409 (body %s)", resp.StatusCode, raw)
	}
	if code := decodeProblem(t, raw).Code; code != "RESTART_IN_PROGRESS" {
		t.Fatalf("code = %q, want RESTART_IN_PROGRESS", code)
	}
	if re.arms() != 1 {
		t.Fatalf("armed %d time(s) across two requests, want 1", re.arms())
	}
}

// TestRestartReplaysAKeyedRetryRatherThanRefusingIt is the Idempotency-Key
// convention doing the job it exists for, on the one operation where a client's
// retry-on-timeout is the EXPECTED case rather than an edge: the connection dies
// as part of the operation succeeding.
//
// The retry must replay the original 202 verbatim, and must not arm a second
// time. Getting RESTART_IN_PROGRESS here would be technically true and
// practically useless — the client that never saw its acceptance would conclude
// somebody else restarted the box.
func TestRestartReplaysAKeyedRetryRatherThanRefusingIt(t *testing.T) {
	re := newRestartEnv(t)
	key := map[string]string{"Idempotency-Key": "01J8Z3K4N5P6Q7R8S9T0V1W2X3"}

	resp1, raw1 := re.postRestart(t, map[string]any{"confirm": true}, key)
	if resp1.StatusCode != http.StatusAccepted {
		t.Fatalf("first = %d, want 202 (body %s)", resp1.StatusCode, raw1)
	}
	// A retry arriving while the process is still winding down: the seam would
	// now report "already armed", so a replay that did NOT happen would surface
	// as a 409.
	re.mu.Lock()
	re.allow = false
	re.mu.Unlock()

	resp2, raw2 := re.postRestart(t, map[string]any{"confirm": true}, key)
	if resp2.StatusCode != http.StatusAccepted {
		t.Fatalf("keyed retry = %d, want the original 202 replayed (body %s)", resp2.StatusCode, raw2)
	}
	if string(raw1) != string(raw2) {
		t.Errorf("keyed retry did not replay the original bytes\n first: %s\nsecond: %s", raw1, raw2)
	}
	if re.arms() != 1 {
		t.Fatalf("armed %d time(s), want 1 — a replay must not re-execute", re.arms())
	}
}

// startNonResumableJob commits a Job holding one `running` target and carrying
// NO re-appliable operation — the shape a workspace export or delete has while
// it is executing, and the shape API-116's resume reconciles to `failed` rather
// than resuming.
func startNonResumableJob(t *testing.T, e *testEnv, node string) string {
	t.Helper()
	return startJobWithOp(t, e, node, store.JobOperation{})
}

func startJobWithOp(t *testing.T, e *testEnv, node string, op store.JobOperation) string {
	t.Helper()
	ctx := context.Background()
	job := apijob.New(ulid.New(), e.auth.Credential().PrincipalID, time.UnixMilli(fixedNowMs).UTC(), []string{node})
	if err := e.store.CreateJob(ctx, job, map[string]string{node: node}, op); err != nil {
		t.Fatalf("CreateJob: %v", err)
	}
	if _, err := e.store.AdvanceJob(ctx, job.ID, func(j *apijob.Job) error { return j.StartTarget(node) }); err != nil {
		t.Fatalf("StartTarget: %v", err)
	}
	return job.ID
}

// TestRestartIsBlockedByWorkItCannotResume is API-156.
//
// The two halves are asserted together on purpose. A guard that blocked on ANY
// in-flight job would be safe and useless — a bulk-enable is re-applied on the
// next boot, so blocking on one stops a restart for no reason. The distinction
// under test is exactly the one jobrun.go's resume already makes, read from the
// same field, so the set that blocks and the set a restart would strand cannot
// drift apart.
func TestRestartIsBlockedByWorkItCannotResume(t *testing.T) {
	t.Run("a non-resumable run blocks, and is named", func(t *testing.T) {
		re := newRestartEnv(t)
		node := re.orgRoot(t)
		jobID := startNonResumableJob(t, re.testEnv, node)

		resp, raw := re.postRestart(t, map[string]any{"confirm": true}, nil)
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("status = %d, want 409 (body %s)", resp.StatusCode, raw)
		}
		p := decodeProblem(t, raw)
		if p.Code != "RESTART_BLOCKED" {
			t.Fatalf("code = %q, want RESTART_BLOCKED", p.Code)
		}
		// Naming the job is what makes the refusal a wait an operator can watch
		// end rather than a retry in the dark.
		if !strings.Contains(p.Detail, jobID) {
			t.Errorf("detail does not name the blocking job %s, so the operator has nothing to wait for: %q", jobID, p.Detail)
		}
		if re.arms() != 0 {
			t.Fatal("a blocked restart armed anyway — the irreversible sequence it was protecting would have been cut in half")
		}
	})

	t.Run("a re-appliable run does not block", func(t *testing.T) {
		re := newRestartEnv(t)
		node := re.orgRoot(t)
		startJobWithOp(t, re.testEnv, node, store.JobOperation{
			Kind: "automations.bulk-enable", Payload: []byte(`{"enabled":true}`),
		})

		resp, raw := re.postRestart(t, map[string]any{"confirm": true}, nil)
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("status = %d, want 202 — a bulk-enable is resumed on the next boot (API-116) and must not stop a restart (body %s)",
				resp.StatusCode, raw)
		}
	})
}

// TestRestartRefusesANonOwner is API-151, and it also pins the ORDER of the
// checks.
//
// The admin here is bound at a site, so they are authenticated and hold real
// authority — just not over the whole deployment. They must get 403 on a box
// that could restart, AND 403 (never 501) on a box that could not: every refusal
// past authorization discloses something about the deployment's state, and a
// caller who may not restart may not learn it either. A 501 to a non-owner would
// make the operation an oracle for "is this box supervised".
func TestRestartRefusesANonOwner(t *testing.T) {
	t.Run("on a restartable box", func(t *testing.T) {
		re := newRestartEnv(t)
		org := re.orgRoot(t)
		site := re.createNode(t, siteUnder(org))
		admin := re.principalWith(t, roleAt{node: site, role: auth.RoleAdmin})

		resp, raw := re.as(t, admin, http.MethodPost, "/api/v1/system/restart",
			mustJSON(t, map[string]any{"confirm": true}), nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (body %s)", resp.StatusCode, raw)
		}
		if re.arms() != 0 {
			t.Fatal("a non-owner restarted the box")
		}
	})

	t.Run("on an unsupervised box, the answer is still 403 and not 501", func(t *testing.T) {
		e := newEnvWithOptions(t)
		org := e.orgRoot(t)
		site := e.createNode(t, siteUnder(org))
		admin := e.principalWith(t, roleAt{node: site, role: auth.RoleAdmin})

		resp, raw := e.as(t, admin, http.MethodPost, "/api/v1/system/restart",
			mustJSON(t, map[string]any{"confirm": true}), nil)
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 — a 501 here tells a caller who may not restart whether this box could (body %s)",
				resp.StatusCode, raw)
		}
	})
}

// TestRestartIsAudited is API-157 / EVT-081, both halves: the accepted case and
// the refused one (EVT-083 — "a failed action is exactly as auditable as a
// successful one").
//
// This is the last record written before the api/1 surface goes away, so it is
// the one that explains the gap in the trail that follows it. The oracle is the
// live /events/v1 stream, not "Emit was called": what matters is that the record
// reaches a subscriber.
func TestRestartIsAudited(t *testing.T) {
	armed := 0
	e := newAuditEnv(t, api.WithRestart(api.RestartConfig{
		Supervisor:    "test-supervisor",
		DrainBudgetMs: 5_000,
		Arm:           func(api.RestartOrder) bool { armed++; return true },
	}))
	e.orgRoot(t)

	br, closeConn := e.subscribe(t, e.auth.Credential())
	defer closeConn()

	// Accepted.
	if resp, raw := e.do(t, http.MethodPost, "/api/v1/system/restart",
		mustJSON(t, map[string]any{"confirm": true}), nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("restart = %d, want 202 (body %s)", resp.StatusCode, raw)
	}
	rec := readAudit(t, br)
	if rec.Action != "system.restart" {
		t.Errorf("action = %q, want system.restart", rec.Action)
	}
	if rec.Target != "system" {
		t.Errorf("target = %q, want system", rec.Target)
	}
	if rec.Result != "success" {
		t.Errorf("result = %q, want success", rec.Result)
	}
	if rec.Actor != e.auth.Credential().PrincipalID {
		t.Errorf("actor = %q, want the invoking principal %q", rec.Actor, e.auth.Credential().PrincipalID)
	}

	// Refused — and recorded just as fully. An operator investigating who tried
	// to take the box down needs the attempts, not only the successes.
	if resp, _ := e.do(t, http.MethodPost, "/api/v1/system/restart",
		mustJSON(t, map[string]any{"confirm": false}), nil); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unconfirmed restart = %d, want 422", resp.StatusCode)
	}
	failed := readAudit(t, br)
	if failed.Action != "system.restart" || failed.Result != "failure" {
		t.Errorf("a refused restart recorded action=%q result=%q, want system.restart/failure", failed.Action, failed.Result)
	}
	if armed != 1 {
		t.Fatalf("armed %d time(s) across one accepted and one refused request, want 1", armed)
	}
}
