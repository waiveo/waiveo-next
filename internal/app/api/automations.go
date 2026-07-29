package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/rules/clock"
	"github.com/maaxton/waiveo-next/internal/rules/compile"
	"github.com/maaxton/waiveo-next/internal/rules/engine"
	"github.com/maaxton/waiveo-next/internal/rules/model"
	"github.com/maaxton/waiveo-next/internal/rules/registry"
	"github.com/maaxton/waiveo-next/internal/rules/state"
	"github.com/maaxton/waiveo-next/internal/shared/apihttp"
	"github.com/maaxton/waiveo-next/internal/shared/apijob"
	"github.com/maaxton/waiveo-next/internal/shared/apiselector"
)

// automationsConfig is the resource configuration for the rules/1 authored-rule
// kind (the openapi's automations exemplar #2). An automation is the management-API
// resource envelope around a rules/1 Rule; like a scheduling-core row, its OWN
// scope_node is both its placement (what a selector's `scope_node`/`scope_node
// subtree` term evaluates against) and its external_id uniqueness grouping
// (API-101). It sets no per-kind `validate` hook: the compile-gate is enforced in
// the store's Create/Update (compile.Compile), and the store's typed
// *compile.CompileError is surfaced as 422 / VALIDATION_FAILED by writeStoreError
// (api.go) — the compiler is the single validator, never re-run here.
func automationsConfig() resourceConfig {
	return resourceConfig{
		kind:         store.KindAutomation,
		path:         "automations",
		resourceType: "automations",
		displayName:  "automation",
		createSchema: "AutomationCreate",
		updateSchema: "AutomationUpdate",
		selLabels:    func(f resourceFields) map[string]string { return f.Labels },
		placement:    func(f resourceFields) string { return f.ScopeNode },
		extScope:     func(f resourceFields) string { return f.ScopeNode },
		writeScope:   func(f resourceFields) string { return f.ScopeNode },
	}
}

// compileErrorExtra renders a rules/1 compiler error (compile.CompileError) as the
// api/1 `errors` extension member (API-013): a single {field, code, message}
// object naming the offending rule member (RUL-006 addressing) — so a non-compiling
// authored rule is diagnosable exactly like any other api/1 validation failure.
func compileErrorExtra(cerr *compile.CompileError) map[string]any {
	return map[string]any{"errors": []map[string]string{{
		"field":   cerr.Field,
		"code":    cerr.Code,
		"message": cerr.Message,
	}}}
}

// runDeviceClass is the device class the synchronous /run invoke stamps on the
// synthesized observation's entity, so a `device_command` action resolves against
// the fixture registry's media-player command vocabulary. The disposition the run
// returns is a mode-evaluation outcome (RUL-246), independent of whether a command
// dispatch succeeds — but stamping the class keeps a scalar-`to` group match
// (RUL-021) resolvable too.
const runDeviceClass = "media-player"

// nopSink is the /run engine's command sink: a synchronous manual run in this
// increment drives the rule end to end for its mode-evaluation disposition, but
// dispatches to no real device (the relay owns the live device plane). It records
// nothing — the returned RunDisposition is the invoke's whole result.
type nopSink struct{}

func (nopSink) Dispatch(entityID, command string, params map[string]any) error { return nil }

// runAutomation handles POST /api/v1/automations/{id}/run — a synchronous "basic
// invoke" of a stored edge automation. It loads the stored (already compile-gated)
// rule into a throwaway engine, synthesizes the triggering transition of the rule's
// first state trigger, and returns the firing's mode-evaluation disposition
// (RUL-246) as the openapi AutomationRunResult ({run_id, disposition}).
//
// It is a mutating POST tagged mcp:act, so it honors Idempotency-Key (API-050/072,
// openapi runAutomation.IdempotencyKeyParam): a client's retry-on-timeout replays
// the retained response verbatim rather than firing the run — and its device
// dispatch — a second time. The key handling reuses the same idem store + response
// capture the generic create() path uses (srv.idempotent), never a second mechanism.
//
// Deferred (documented): the full trigger-snapshot / AutomationRunRequest.context
// override semantics, and app-side execution of app-classified rules. This increment
// supports a state-triggered edge automation; an app-class rule, a rule without a
// synthesizable state trigger, or a firing whose conditions do not admit a run is
// refused with a precise Problem rather than a fabricated disposition.
func (srv *server) runAutomation(w http.ResponseWriter, r *http.Request) {
	// The body is read up front so its content hash keys Idempotency-Key
	// replay-vs-reuse (API-052) even though AutomationRunRequest.context is
	// deferred; a keyed retry with the same body replays, a different body conflicts.
	raw, ok := readBody(w, r)
	if !ok {
		return
	}
	srv.idempotent(w, r, raw, func(w http.ResponseWriter) { srv.runAutomationExec(w, r) })
}

// runAutomationExec is the run's actual work, executed once per fresh (non-replayed)
// request under the Idempotency-Key guard in runAutomation. It writes into the
// response capture that guard owns, so a firing's exact response bytes are retained
// for a later retry's verbatim replay.
func (srv *server) runAutomationExec(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	res, found, err := srv.store.Get(r.Context(), store.KindAutomation, id)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}
	if !found {
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", "No automation exists with this identifier.")
		return
	}
	// Running an automation begins by reading it, so it is scoped as a read: an
	// automation outside the caller's visible set is answered exactly as an id
	// that names nothing (scopeview.go).
	view, verr := srv.scopeView(r)
	if verr != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}
	node := parseFields(res.Body).ScopeNode
	if !view.canRead(node) {
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", "No automation exists with this identifier.")
		return
	}
	// Reading it is where a run BEGINS, not where it ends: a run dispatches the
	// automation's actions, so it is a write at the automation's own scope node
	// and is authorized as one (SEC-005). 403 rather than 404 here, exactly as
	// on the resource families — the caller has just been shown they may read
	// this row, so the refusal discloses nothing but their own authority.
	if !view.canWrite(node) {
		writeProblem(w, r, http.StatusForbidden, "FORBIDDEN", "Forbidden", unauthorizedWriteDetail)
		return
	}

	// The stored body was compile-gated on write, so Compile/ParseRule are expected
	// to succeed here; a failure is an internal inconsistency, not a client error.
	entry, cerr := compile.Compile(res.Body)
	if cerr != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "The stored automation no longer compiles.")
		return
	}
	rule, perr := model.ParseRule(res.Body)
	if perr != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "The stored automation could not be parsed.")
		return
	}

	if entry.ExecutionClass != "edge" {
		writeProblem(w, r, http.StatusBadRequest, "VALIDATION_FAILED", "Bad Request",
			"This automation is app-class; a synchronous run is limited to edge automations in this increment.")
		return
	}

	dispositions, runErr := synthesizeRun(entry, rule)
	if runErr != nil {
		writeProblem(w, r, http.StatusBadRequest, "VALIDATION_FAILED", "Bad Request", runErr.Error())
		return
	}
	if len(dispositions) == 0 {
		writeProblem(w, r, http.StatusBadRequest, "VALIDATION_FAILED", "Bad Request",
			"The manual run did not start: the automation's conditions were not satisfied for the synthesized trigger context.")
		return
	}

	writeJSONValue(w, http.StatusOK, map[string]string{
		"run_id":      srv.newID(),
		"disposition": string(dispositions[0].Disposition),
	})
}

// synthesizeRun drives a compiled edge rule through a throwaway engine and returns
// the dispositions its first state trigger's firing produces. It seeds the trigger
// subject's prior state, then observes the transition into the trigger's `to` state
// so the rule genuinely fires (the same off→on transition the relay's real
// observation feed would deliver), reusing the built engine/eval stack rather than
// re-implementing rule evaluation. A rule with no synthesizable state trigger is a
// documented limitation of this increment's basic invoke.
func synthesizeRun(entry compile.CompiledRuleEntry, rule model.Rule) ([]engine.RunDisposition, error) {
	var trigger *model.Member
	for i := range rule.Triggers {
		m := rule.Triggers[i]
		if m.Type == "state" && m.EntityRef != nil && m.EntityRef.EntityID != "" {
			trigger = &rule.Triggers[i]
			break
		}
	}
	if trigger == nil {
		return nil, errors.New("a synchronous run in this increment supports a state-triggered automation; this automation has no state trigger with an entity subject")
	}

	var bounds struct {
		To        json.RawMessage `json:"to"`
		From      json.RawMessage `json:"from"`
		Attribute string          `json:"attribute"`
	}
	_ = json.Unmarshal(trigger.Raw, &bounds)
	toState, ok := firstState(bounds.To)
	if !ok || bounds.Attribute != "" {
		return nil, errors.New("a synchronous run in this increment supports a state trigger with a `to` state bound")
	}
	priorState := "waiveo-run-prior"
	if fromState, ok := firstState(bounds.From); ok {
		priorState = fromState
	}
	if priorState == toState {
		// Guarantee a genuine state transition so the observation is classified as a
		// change (RUL-330) and the `to`-bounded trigger can fire.
		priorState += "-prev"
	}

	entityID := trigger.EntityRef.EntityID
	reg := registry.FixtureRegistry{}
	eng := engine.New(reg, clock.NewFakeClock(), nopSink{}, nil)
	if err := eng.Load(entry, rule); err != nil {
		return nil, err
	}
	eng.SeedEntityState(entityID, priorState)
	obs := state.NewObservation(reg,
		state.Entity{ID: entityID, DeviceClass: runDeviceClass, State: priorState},
		state.Entity{ID: entityID, DeviceClass: runDeviceClass, State: toState},
	)
	return eng.Observe(obs), nil
}

// firstState decodes a state trigger's `from`/`to` bound — a scalar string or an
// array of strings — into its first state value, reporting ok=false for an absent,
// null, or empty bound.
func firstState(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && s != "" {
		return s, true
	}
	var arr []string
	if json.Unmarshal(raw, &arr) == nil && len(arr) > 0 {
		return arr[0], true
	}
	return "", false
}

// bulkEnableRequest is the AutomationBulkEnableRequest body: a label-selector
// target predicate and the `enabled` value to apply. BOTH are pointers so an
// ABSENT field is distinguishable from a present zero value — `enabled` absent vs
// an explicit false, and `selector` absent vs an explicit "". The openapi schema
// marks both required precisely so a fleet-mutating request cannot omit its target
// predicate: an empty or whitespace-only selector matches EVERY stored automation
// (apiselector), so the handler rejects an absent/blank selector 422 rather than
// silently touching the whole fleet.
type bulkEnableRequest struct {
	Selector *string `json:"selector"`
	Enabled  *bool   `json:"enabled"`
}

// bulkEnableAutomations handles POST /api/v1/automations/bulk-enable — a
// fleet-mutating operation over a selector-matched set (API-110/111). It resolves
// the selector against every stored automation (reusing apiselector, exactly as the
// list handler does), then returns 202 + an api/1 Job (apijob) whose targets are the
// matched ids, each pending.
//
// It is a mutating POST tagged mcp:act, so it honors Idempotency-Key (API-050/072,
// openapi bulkEnableAutomations.IdempotencyKeyParam): a client's retry-on-timeout
// replays the original 202 + Job verbatim rather than minting a second, distinct
// Job. The key handling reuses the same idem store + response capture the generic
// create() path uses (srv.idempotent), never a second mechanism.
//
// The Job it returns is PERSISTED (store.CreateJob) before the 202 is written,
// so the client can poll it at GET /jobs/{job_id} (API-112) and so it survives a
// restart (API-116) — a Job that lived only in this handler's memory made
// API-112's "read the Job resource again" unsatisfiable.
//
// The work itself runs OFF this request, on the server's JobRunner (jobrun.go):
// API-111 requires the 202 "rather than blocking the request until every target
// finishes", so the handler hands the accepted target set to the runner and
// returns. Each target is then flipped through the ordinary resource update path
// — advancing the store generation like any other authored change — and its
// progress committed through the apijob state machine, which a client observes
// by polling GET /jobs/{job_id} (API-112).
func (srv *server) bulkEnableAutomations(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	srv.idempotent(w, r, body, func(w http.ResponseWriter) { srv.bulkEnableExec(w, r, body) })
}

// bulkEnableExec is the bulk-enable's actual work, executed once per fresh
// (non-replayed) request under the Idempotency-Key guard in bulkEnableAutomations.
// It writes into the response capture that guard owns, so the exact 202 + Job bytes
// are retained for a later retry's verbatim replay (a retry never mints a new Job).
func (srv *server) bulkEnableExec(w http.ResponseWriter, r *http.Request, body []byte) {
	var req bulkEnableRequest
	if err := json.Unmarshal(body, &req); err != nil {
		// API-013a: VALIDATION_FAILED MUST carry 422 when the failure is in the
		// request body, never 400 — an unparseable body is a body failure, not a
		// query-parameter one, and POST /scope-nodes already returns 422 for the
		// identical failure class (parseFields absorbs the parse error into
		// zero-valued fields, which then fail datamodel validation).
		writeProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed", "The request body could not be parsed.")
		return
	}
	// `selector` is required (openapi AutomationBulkEnableRequest.required): an
	// absent OR empty/whitespace selector matches every stored automation
	// (apiselector), so it is rejected 422 rather than silently targeting the whole
	// fleet — the schema requires it precisely to force an explicit target predicate.
	if req.Selector == nil || strings.TrimSpace(*req.Selector) == "" {
		writeProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed", "`selector` is required.")
		return
	}
	if req.Enabled == nil {
		writeProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Validation Failed", "`enabled` is required.")
		return
	}

	sel, serr := apiselector.Parse(*req.Selector)
	if serr != nil {
		writeProblem(w, r, serr.Status, serr.Code, serr.Title, serr.Detail)
		return
	}

	rows, err := srv.store.List(r.Context(), store.KindAutomation, store.ListFilter{})
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}
	view, err := srv.scopeView(r)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}

	// API-110 extends the SAME selector convention a list read uses to a
	// fleet-mutating operation's target predicate — so it inherits the same
	// scoping. The target set is drawn from the rows this caller can see, and
	// the selector narrows that set rather than reaching past it: a Job whose
	// targets[] named automations the submitter may not read would enumerate
	// them by id in its own response body (API-112), which is the list leak in
	// another shape.
	//
	// The membership test is canWrite, not canRead, because this operation
	// MUTATES every row it targets. Substituting the stronger floor is a pure
	// narrowing (auth.CanWrite implies auth.CanRead), so the API-112 reasoning
	// above still holds and a row the caller may read but not mutate is simply
	// not targeted. It is silently omitted rather than refusing the whole
	// request: a selector designates a target SET, and one that reaches past the
	// caller's authority has to narrow — the same rule EVT-121 fixes for the
	// grammar generally — while a 403 keyed to which rows a selector happened to
	// match would report on rows rather than on the caller.
	//
	// Each match's scope-node placement is captured alongside its id: it is what
	// a later poll of this Job scopes the read by (jobs.go), recorded here — at
	// acceptance — rather than re-derived at poll time, so the Job's readability
	// is fixed by the world the job was accepted in and cannot be widened later
	// by moving a target row.
	var targetIDs []string
	targetScopes := map[string]string{}
	for _, res := range rows {
		f := parseFields(res.Body)
		if !view.canWrite(f.ScopeNode) {
			continue
		}
		if sel.Matches(f.Labels, f.ScopeNode, view.inSubtree) {
			targetIDs = append(targetIDs, res.ID)
			targetScopes[res.ID] = f.ScopeNode
		}
	}

	// .UTC() so the Job's created_at serializes as RFC3339 "Z" (api/1 API-112),
	// not whatever the process's local time.Local zone happens to be —
	// time.UnixMilli returns a Time in Local, and time.Time's JSON marshaling
	// preserves whatever Location it carries. The Job's id comes from the same
	// injected srv.newID seam every other server-minted id draws from — never a
	// package-level generator of its own.
	// created_by is the REAL authenticated caller (API-112), resolved by the
	// auth middleware from the presented session or API key — not a fixed
	// constant, and not something this handler can invent, since a request that
	// reaches here always carries a principal (SEC-005).
	job := apijob.New(srv.newID(), auth.PrincipalID(r), time.UnixMilli(srv.nowMs()).UTC(), targetIDs)

	// The Job is PERSISTED before it is returned, and a failure to persist it
	// refuses the whole operation. API-112 makes a poll the only way a client
	// learns this work finished, so a 202 naming a Job no later read could
	// resolve would be a promise the surface cannot keep — better to fail the
	// request the client can retry (a 5xx also Aborts the Idempotency-Key
	// marker, leaving the key retryable, API-054) than to hand back an
	// unpollable id.
	//
	// The accepted OPERATION is persisted with it, in the same transaction as the
	// targets it applies to (API-116): a Job durable enough to poll but not
	// durable enough to say what it was doing leaves a restarted process holding
	// a target list and nothing to re-apply, which is how a target left `running`
	// by a crash stays that way. `enabled` is the whole of this operation's
	// arguments, and re-applying it is safe (jobrun.go), so a bulk-enable
	// interrupted mid-flight is resumable rather than merely reportable.
	wanted := *req.Enabled
	opPayload, err := json.Marshal(bulkEnableOperation{Enabled: wanted})
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}
	if err := srv.store.CreateJob(r.Context(), job, targetScopes, store.JobOperation{
		Kind:    jobOpBulkEnableAutomations,
		Payload: opPayload,
	}); err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}

	// The 202 body is rendered BEFORE the work is handed off, and that ordering
	// is load-bearing rather than incidental: API-111's Job represents "accepted,
	// not-yet-complete work", so the resource this request returns is the job as
	// ACCEPTED — every target pending — not a race against however far the
	// executor happened to get before the bytes were marshaled. A client learns
	// what changed by polling (API-112), which is the only completion signal the
	// contract offers.
	writeJSONValue(w, http.StatusAccepted, job.Resource())

	// The execution runs on the server's runner, under ITS context — never
	// r.Context(), which is canceled the moment this handler returns, i.e.
	// before the accepted work would have started.
	targets := targetIDs
	srv.jobs.submit(func(ctx context.Context) {
		srv.runBulkEnable(ctx, job.ID, targets, wanted)
	})
}

// idempotent runs a mutating server-level operation (runAutomation,
// bulkEnableAutomations — the api/1 mcp:act POSTs outside plain resource creation)
// under the Idempotency-Key convention (API-050/052/072), reusing the SAME idem
// store, responseCapture, and replay the generic create() path uses — never a
// second mechanism. A keyed repeat replays the retained response verbatim, or
// 409-conflicts, BEFORE exec runs; an unkeyed request always executes. A fresh
// keyed request's exact response bytes are captured and retained (Complete) so a
// client's retry-on-timeout replays them rather than re-firing the operation; a
// transient 5xx is Aborted instead, leaving the key retryable. raw is the request
// body the replay-vs-reuse content hash is taken over (API-052).
func (srv *server) idempotent(w http.ResponseWriter, r *http.Request, raw []byte, exec func(http.ResponseWriter)) {
	key := r.Header.Get("Idempotency-Key")
	// API-051's scope tuple: (authenticated principal, method, path).
	scope := apihttp.IdempotencyScope{Principal: auth.PrincipalID(r), Method: r.Method, Path: r.URL.Path}
	hash := apihttp.IdempotencyBodyHash(raw)
	now := srv.nowMs()
	if key != "" {
		switch out := srv.idem.Begin(scope, key, hash, now); out.Kind {
		case apihttp.BeginReplay:
			replay(w, out.Response)
			return
		case apihttp.BeginConflict:
			writeProblem(w, r, http.StatusConflict, apihttp.CodeIdempotencyKeyReused, "Conflict", apihttp.IdempotencyReuseDetail(key))
			return
		case apihttp.BeginInProgress:
			writeProblem(w, r, http.StatusConflict, apihttp.CodeIdempotencyKeyInProgress, "Conflict", "A request with this Idempotency-Key is already in progress.")
			return
		}
	}

	// A fresh keyed request holds an in-flight marker that MUST be resolved on every
	// terminal path (API-052/054): a definitive outcome (any status < 500, success OR
	// a deterministic client Problem) is Completed for verbatim replay; a transient
	// 5xx is Aborted so the key stays retryable.
	rc := &responseCapture{}
	exec(rc)
	status, body, ct := rc.flush(w)

	if key != "" {
		if status < http.StatusInternalServerError {
			srv.idem.Complete(scope, key, hash, apihttp.StoredResponse{Status: status, Body: body, ContentType: ct}, now)
		} else {
			srv.idem.Abort(scope, key, hash)
		}
	}
}

// writeProblem writes an api/1 RFC 9457 Problem (no extension members) from a
// bespoke server-level handler, resolving the request's Trace-Id — the same shape
// the generic resource handlers emit through rs.problem.
func writeProblem(w http.ResponseWriter, r *http.Request, status int, code, title, detail string) {
	apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), status, code, title, detail, nil)
}
