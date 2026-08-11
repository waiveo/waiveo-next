package api

import (
	"context"
	"encoding/json"
	"log"
	"sync"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/events"
	"github.com/maaxton/waiveo-next/internal/rules/model"
)

// eventtriggers.go is the app peer's execution of rules/1's `event` TRIGGER
// (RUL-080/081): the hop that turns a durable event into a rule actually
// running.
//
// # Why this exists at all
//
// rules/1 splits triggers into two execution classes. The EDGE ones (`state`,
// `numeric`, `time`, …) are watched by the relay's engine, which observes device
// entities directly. The APP ones (`template`, `event`, `webhook`) are the
// platform's, because their subject is something only the app peer sees. Until
// now the app peer watched none of them: `event` was a member of the closed
// vocabulary (internal/rules/vocab), an author could write one, the compiler
// accepted it, the store persisted it — and nothing anywhere ever fired it. That
// is a surface that accepts work it never performs, and it is the exact reason a
// viewer pressing a button on a screen could not make anything happen.
//
// # What fires, and on what
//
// Every durable event appended to this deployment's event log is offered here
// (internal/app/eventingest's sink chain, wired in cmd/waiveo-feeder). An
// automation fires when ALL of:
//
//   - it is enabled. A disabled automation firing is the single worst bug this
//     file could have: an operator disables a rule precisely to stop it acting,
//     and an event path that ignored the flag would act anyway, invisibly.
//   - it carries an `event` trigger whose `event` equals the envelope's schema
//     (RUL-080's "durable event name": for a platform-registered schema that IS
//     the schema name — `screen.interaction` — and for a pack event it is the
//     pack-namespaced name, which is likewise the schema).
//   - every `match` constraint that trigger declares holds against a TOP-LEVEL
//     field of the event's payload, compared by exact JSON value (RUL-081).
//
// A rule with several `event` triggers fires ONCE per matching event, not once
// per matching trigger: RUL-081 fires "once per matching durable event
// delivered", and a rule whose two triggers both match one event has still seen
// one event.
//
// # Then what runs
//
// The rule's conditions and actions run through runAutomationNow — the SAME real
// execution `POST /automations/{id}/run` performs, dispatching device commands
// over the relay connection and writing signage overrides through the store. Not
// a simulation, and not a second copy of the action evaluator.
//
// # Authority
//
// A firing has no human principal: nobody asked for it, an event did. It
// therefore runs under a SYSTEM view of the scope tree (systemScopeView) rather
// than under any user's bindings — the same authority the edge engine already
// runs edge rules with on the relay, where there is no principal in the picture
// at all and no per-target authorization is performed. Making it a user's
// authority instead would mean an automation's behaviour depended on who
// happened to be logged in when a viewer pressed a button, which is not a
// security property, just an unpredictable one.

// EventTriggerDispatcher is the seam the feeder wires between the durable event
// log and this package's rule execution.
//
// It exists because the api package's server is unexported (api.New returns an
// http.Handler) while the ingest sink that must reach it is constructed
// separately in cmd/waiveo-feeder — and the ingest is constructed FIRST, since
// the api handler is mounted onto the same mux later. A caller therefore creates
// an empty dispatcher, hands it to eventingest as part of the sink chain, and
// passes the same pointer to api.New via WithEventTriggers; New binds the live
// implementation into it.
//
// A dispatcher nothing has bound is INERT — Deliver returns immediately. That is
// the honest degrade for a deployment that never wired the option (every test
// that builds a bare handler), and it is a state the wiring makes transient
// rather than permanent: the feeder binds it during startup, before it serves.
type EventTriggerDispatcher struct {
	mu   sync.RWMutex
	fire func(context.Context, events.Envelope)
}

// Deliver offers one durable event to the event-trigger evaluator. It is called
// on the ingest path, after the envelope has been validated and appended, so
// what fires a rule is exactly what the event log durably holds — never a record
// that failed EVT-013 and was dropped.
//
// It is synchronous with respect to the caller's context on purpose: the ingest
// handler acks a telemetry batch only after processing it, and firing in a
// detached goroutine would let the ack outrun the work, so a crash between the
// two would lose a press with the ack already given.
func (d *EventTriggerDispatcher) Deliver(ctx context.Context, env events.Envelope) {
	d.mu.RLock()
	fire := d.fire
	d.mu.RUnlock()
	if fire == nil {
		return
	}
	fire(ctx, env)
}

// bind installs the live evaluator. Called once, by New, under the option.
func (d *EventTriggerDispatcher) bind(fire func(context.Context, events.Envelope)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.fire = fire
}

// WithEventTriggers binds d to this handler's rule store and executor, so every
// durable event delivered to d.Deliver evaluates the deployment's `event`
// triggers and runs the automations that match (RUL-080/081).
//
// Omitting the option leaves `event` triggers unfired. That is not a silent
// difference: it is the difference between a deployment that has an app-side
// rules path and one that does not, and every production wiring passes it.
func WithEventTriggers(d *EventTriggerDispatcher) Option {
	return func(srv *server) {
		if d == nil {
			return
		}
		d.bind(srv.fireEventTriggers)
	}
}

// eventTrigger is the `event` trigger's own wire shape (RUL-080): the durable
// event name to match, and an optional set of exact-value constraints against
// top-level payload fields.
//
// Match values are json.RawMessage rather than `any` so comparison is by the
// event's own JSON value, not by whatever Go type an interface decode happened
// to choose: `{"count": 2}` in a rule and `2` in a payload must compare equal
// without this file having an opinion about float64.
type eventTrigger struct {
	Type  string                     `json:"type"`
	Event string                     `json:"event"`
	Match map[string]json.RawMessage `json:"match"`
}

// fireEventTriggers evaluates every enabled automation's `event` triggers
// against env and runs the ones that match.
//
// Failures are logged and skipped per automation, never propagated: one
// unparseable rule row must not stop a matching sibling from running, and there
// is no caller to return an error to — the event has already been delivered.
func (srv *server) fireEventTriggers(ctx context.Context, env events.Envelope) {
	if env.Schema == "" {
		return
	}
	rows, err := srv.store.List(ctx, store.KindAutomation, store.ListFilter{})
	if err != nil {
		log.Printf("event-trigger: listing automations for %s: %v", env.Schema, err)
		return
	}

	var view scopeView
	built := false
	for _, row := range rows {
		if !automationEnabled(row.Body) {
			continue
		}
		rule, perr := model.ParseRule(row.Body)
		if perr != nil {
			// The row was compile-gated on write, so this is an internal
			// inconsistency rather than an authoring error; skip this rule and
			// keep evaluating the rest.
			log.Printf("event-trigger: parsing automation %s: %v", row.ID, perr)
			continue
		}
		if !ruleMatchesEvent(rule, env) {
			continue
		}
		if !built {
			v, verr := srv.systemScopeView(ctx)
			if verr != nil {
				log.Printf("event-trigger: building system scope view: %v", verr)
				return
			}
			view, built = v, true
		}
		rep := srv.runAutomationNow(ctx, env.TraceID, rule, view, false)
		log.Printf("event-trigger: %s fired automation %s (%s): %d command(s), %d signage action(s)",
			env.Schema, row.ID, rep.Disposition, len(rep.Commands), len(rep.Signage))
	}
}

// automationEnabled reads the automation row's own `enabled` flag.
//
// An ABSENT flag reads as DISABLED, and that direction is deliberate. The
// alternative — treating absence as enabled — would make any row this decoder
// cannot understand fire, which is the wrong way round for a mechanism whose
// whole failure mode is acting when it should not. Every row this API writes
// carries the flag explicitly.
func automationEnabled(body []byte) bool {
	var row struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.Unmarshal(body, &row); err != nil {
		return false
	}
	return row.Enabled != nil && *row.Enabled
}

// ruleMatchesEvent reports whether ANY of rule's `event` triggers matches env
// (RUL-080/081). It stops at the first match: a rule fires once per event, not
// once per trigger that happens to match it.
func ruleMatchesEvent(rule model.Rule, env events.Envelope) bool {
	for _, t := range rule.Triggers {
		if t.Type != triggerTypeEvent {
			continue
		}
		var et eventTrigger
		if err := json.Unmarshal(t.Raw, &et); err != nil {
			continue
		}
		if et.Event == "" || et.Event != env.Schema {
			continue
		}
		if matchConstraintsHold(et.Match, env.Payload) {
			return true
		}
	}
	return false
}

// triggerTypeEvent is rules/1's `event` trigger discriminant (RUL-080). It is
// the same string internal/rules/vocab classes as App; spelled here rather than
// exported from there because vocab's table is keyed by it and adding an
// accessor for one lookup would be more indirection than the literal.
const triggerTypeEvent = "event"

// matchConstraintsHold evaluates a trigger's `match` against a payload
// (RUL-081): every constraint must equal a TOP-LEVEL field of the payload, by
// exact JSON value. No constraints means the trigger fires on any event of that
// name.
//
// A constraint naming a field the payload does not carry FAILS the match rather
// than being ignored — an absent field is not a satisfied constraint, and
// ignoring it would make a typo'd field name silently widen a rule to every
// event of that schema, which is the opposite of what the author wrote.
//
// Comparison is by canonicalized JSON value (json.Marshal of the decoded value)
// rather than by raw bytes, so `"front_desk"` matches `"front_desk"` regardless
// of insignificant whitespace or key order a producer emitted.
func matchConstraintsHold(match map[string]json.RawMessage, payload json.RawMessage) bool {
	if len(match) == 0 {
		return true
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return false
	}
	for name, want := range match {
		got, present := fields[name]
		if !present {
			return false
		}
		if !jsonValuesEqual(want, got) {
			return false
		}
	}
	return true
}

// jsonValuesEqual compares two JSON values structurally.
func jsonValuesEqual(a, b json.RawMessage) bool {
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	ab, err := json.Marshal(av)
	if err != nil {
		return false
	}
	bb, err := json.Marshal(bv)
	if err != nil {
		return false
	}
	return string(ab) == string(bb)
}

// systemScopeView is the scope view an EVENT-INITIATED run executes under: the
// real scope tree (so subtree containment answers correctly for any selector a
// rule's actions resolve) with read and write permitted everywhere.
//
// It is NOT a bypass of scopeView's authorization; it is the absence of a
// principal to authorize. A request-scoped view answers "may THIS CALLER do
// this?", and an event has no caller — the platform itself is acting on a rule
// an operator already authored and enabled, which is precisely the authority the
// edge engine exercises on the relay for every edge rule, with no per-target
// check at all. Substituting some user's bindings would make whether a viewer's
// button press worked depend on who was logged in at the time.
//
// roleAt answers auth.RoleOwner: the two operations that consult it are the
// data-subject ones, which no rule action reaches, and answering "no binding"
// there would be a different lie than the one this view is telling.
func (srv *server) systemScopeView(ctx context.Context) (scopeView, error) {
	nodes, _, _, _, err := srv.store.DesiredStateRows(ctx)
	if err != nil {
		return scopeView{}, err
	}
	tree, _ := datamodel.BuildScopeTree(nodes)
	return scopeView{
		inSubtree: func(ancestor, node string) bool {
			if ancestor == node {
				return false
			}
			for _, id := range tree.AncestorChain(node) {
				if id == ancestor {
					return true
				}
			}
			return false
		},
		canRead:  func(string) bool { return true },
		canWrite: func(string) bool { return true },
		roleAt:   func(string) (auth.Role, bool) { return auth.RoleOwner, true },
	}, nil
}
