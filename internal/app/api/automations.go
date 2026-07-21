package api

import (
	"encoding/json"
	"errors"
	"net/http"
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
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
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
// Deferred (documented): the full trigger-snapshot / AutomationRunRequest.context
// override semantics, and app-side execution of app-classified rules. This increment
// supports a state-triggered edge automation; an app-class rule, a rule without a
// synthesizable state trigger, or a firing whose conditions do not admit a run is
// refused with a precise Problem rather than a fabricated disposition.
func (srv *server) runAutomation(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	res, found, err := srv.store.Get(r.Context(), store.KindAutomation, id)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "INTERNAL", "Internal Server Error", "An unexpected server error occurred.")
		return
	}
	if !found {
		writeProblem(w, r, http.StatusNotFound, "NOT_FOUND", "Not Found", "No resource exists at this identifier.")
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
		"run_id":      ulid.New(),
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
// target predicate and the `enabled` value to apply. Enabled is a pointer so an
// absent field (a body that names no boolean) is distinguishable from an explicit
// false.
type bulkEnableRequest struct {
	Selector string `json:"selector"`
	Enabled  *bool  `json:"enabled"`
}

// bulkEnableAutomations handles POST /api/v1/automations/bulk-enable — a
// fleet-mutating operation over a selector-matched set (API-110/111). It resolves
// the selector against every stored automation (reusing apiselector, exactly as the
// list handler does), then returns 202 + an api/1 Job (apijob) whose targets are the
// matched ids, each pending.
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
	var req bulkEnableRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "VALIDATION_FAILED", "Bad Request", "The request body could not be parsed.")
		return
	}
	if req.Enabled == nil {
		writeProblem(w, r, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "Unprocessable Entity", "`enabled` is required.")
		return
	}

	sel, serr := apiselector.Parse(req.Selector)
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

	job := apijob.New(ulid.New(), pocPrincipal, time.UnixMilli(srv.nowMs()), targetIDs)
	writeJSONValue(w, http.StatusAccepted, job.Resource())
}

// writeProblem writes an api/1 RFC 9457 Problem (no extension members) from a
// bespoke server-level handler, resolving the request's Trace-Id — the same shape
// the generic resource handlers emit through rs.problem.
func writeProblem(w http.ResponseWriter, r *http.Request, status int, code, title, detail string) {
	apihttp.WriteProblemExt(w, r, apihttp.TraceID(r), status, code, title, detail, nil)
}
