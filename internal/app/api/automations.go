package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

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
		selLabels:    func(f resourceFields) map[string]string { return f.Labels },
		placement:    func(f resourceFields) string { return f.ScopeNode },
		extScope:     func(f resourceFields) string { return f.ScopeNode },
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
// Deferred (documented): the per-target toggle-and-regenerate — this increment
// returns the accepted Job and its target set; actually flipping each row's
// `enabled` and bumping the store generation is a fast-follow that advances the
// Job's targets through the apijob state machine.
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
	inSubtree, err := srv.inSubtreeFn(r)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}

	var targetIDs []string
	for _, res := range rows {
		f := parseFields(res.Body)
		if sel.Matches(f.Labels, f.ScopeNode, inSubtree) {
			targetIDs = append(targetIDs, res.ID)
		}
	}

	// .UTC() so the Job's created_at serializes as RFC3339 "Z" (api/1 API-112),
	// not whatever the process's local time.Local zone happens to be —
	// time.UnixMilli returns a Time in Local, and time.Time's JSON marshaling
	// preserves whatever Location it carries. The Job's id comes from the same
	// injected srv.newID seam every other server-minted id draws from — never a
	// package-level generator of its own.
	job := apijob.New(srv.newID(), pocPrincipal, time.UnixMilli(srv.nowMs()).UTC(), targetIDs)
	writeJSONValue(w, http.StatusAccepted, job.Resource())
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
	scope := apihttp.IdempotencyScope{Principal: pocPrincipal, Method: r.Method, Path: r.URL.Path}
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
