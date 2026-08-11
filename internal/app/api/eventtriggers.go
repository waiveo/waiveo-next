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
// Every durable event appended to this deployment's event log is offered here —
// by the telemetry ingest's post-append deliverer (internal/app/eventingest's
// EventDeliverer, wired in cmd/waiveo-feeder), which hands it over with the
// ingest's own lock released and the batch's ack not yet written. An automation
// fires when ALL of:
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
// A firing has no human principal: nobody asked for it, an event did. It runs
// under the authority of THE RULE ITSELF — a view bounded by the automation's
// own `scope_node` subtree (automationScopeView) — and each of the rule's
// action targets is authorized against that view, per target, inside the run,
// exactly as a hand-initiated `POST /automations/{id}/run` authorizes each
// target against the caller's view.
//
// Two rejected alternatives, and why each is wrong:
//
//   - THE CALLER'S BINDINGS. There is no caller. Substituting whoever happened
//     to be logged in would make whether a viewer's button press worked depend
//     on who had a session open at the time, which is not a security property,
//     just an unpredictable one.
//   - AN ALL-PERMISSIVE SYSTEM VIEW, which is what this file did first. It is a
//     privilege escalation, and a total one. `POST /automations` authorizes
//     only the automation's OWN scope node at write time — automations.go says
//     so in as many words, and says that per-target authorization inside the run
//     is what covers the rest — so an operator who may write only at node A
//     could author a rule whose `play_cast` names a screen at node B, or a
//     `selector: "*"` that resolves against every screen in the deployment, and
//     the first `screen.interaction` would execute it fleet-wide. The same rule
//     run BY HAND is refused per target. The precedent offered for the system
//     view (the relay's edge engine, which performs no per-target check) does
//     not transfer: a relay only ever holds its own site's rows, so its scope is
//     bounded by what it can see; the app peer spans the whole deployment and
//     has no such bound unless one is imposed.
//
// The bound is the automation's placement subtree, INCLUSIVE of the node
// itself, because that is precisely the authority the author had to hold to
// create the row: canWrite at the automation's scope node, which SEC-010
// inherits down to every descendant. A rule can therefore reach exactly what a
// human authorized to write it could have reached by hand, and nothing more.

// EventTriggerDispatcher is the seam the feeder wires between the durable event
// log and this package's rule execution.
//
// It exists because the api package's server is unexported (api.New returns an
// http.Handler) while the ingest that must reach it is constructed separately in
// cmd/waiveo-feeder — and the ingest is constructed FIRST, since the api handler
// is mounted onto the same mux later. A caller therefore creates an empty
// dispatcher, passes its Deliver method to eventingest.New as the ingest's
// EventDeliverer, and passes the same pointer to api.New via WithEventTriggers;
// New binds the live implementation into it.
//
// Deliver is deliberately NOT part of the ingest's EventSink. A sink runs under
// the ingest's own mutex, and what this dispatcher starts is a full rule run
// that reaches devices over the network — holding the one durable telemetry
// channel every relay shares while that happens stalled heartbeats and playback
// records deployment-wide behind a single button press.
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
// It is synchronous with respect to its caller on purpose: the ingest handler
// acks a telemetry batch only after every envelope in it has been delivered, and
// firing in a detached goroutine would let the ack outrun the work, so a crash
// between the two would lose a press with the ack already given. Synchronous
// with respect to ONE push, and concurrent with respect to every other — the
// ingest releases its lock first (eventingest.EventDeliverer).
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

	// The scope tree is read AT MOST ONCE per delivered event, lazily, and only
	// if some rule actually matched — an event no rule watches must not cost a
	// tree read. Each matching rule then gets its OWN view over that one tree,
	// bounded by its own placement: the tree is shared, the authority is not.
	var tree datamodel.ScopeTree
	treeRead := false
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
		if !treeRead {
			t, terr := srv.scopeTree(ctx)
			if terr != nil {
				log.Printf("event-trigger: reading the scope tree: %v", terr)
				return
			}
			tree, treeRead = t, true
		}
		// The rule's OWN placement is the authority its run executes under. It
		// is read from the stored row rather than from the parsed rule because
		// scope_node is a resource-row member (DAT-006), not part of rules/1's
		// rule document — the same read every other placement check on this
		// surface makes.
		node := parseFields(row.Body).ScopeNode
		view := automationScopeView(tree, node)
		rep := srv.runAutomationNow(ctx, env.TraceID, rule, view, node, false)
		// A fired run has no caller to answer, so its effect report — above all
		// the targets it REFUSED — is published as an events/1 automation.run
		// (eventtriggers_report.go). Without it a refusal is a screen that did not
		// change and nothing an operator can read.
		srv.publishRunReport(row.ID, node, row.Revision, env, rep)
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

// scopeTree reads the deployment's scope-node tree from the same
// DesiredStateRows snapshot every request-scoped view is built from, so an
// event-fired run and an operator's request answer containment questions the
// same way.
func (srv *server) scopeTree(ctx context.Context) (datamodel.ScopeTree, error) {
	nodes, _, _, _, err := srv.store.DesiredStateRows(ctx)
	if err != nil {
		return datamodel.ScopeTree{}, err
	}
	tree, _ := datamodel.BuildScopeTree(nodes)
	return tree, nil
}

// automationScopeView is the scope view ONE event-fired automation run executes
// under: the real scope tree, with read and write permitted exactly within the
// automation's own placement subtree — the node itself and everything below it —
// and refused everywhere else.
//
// It is a real authorization boundary, not a formality, and it is what makes an
// event-fired run no more powerful than the operator who authored the rule (see
// this file's "# Authority" section for the two alternatives this replaced and
// why the all-permissive one was a privilege escalation). Both halves matter:
//
//   - canRead bounds what a `selector` RESOLVES against. screenOverrideSink's
//     target resolution and resolveEntityRef both filter candidates by canRead
//     before matching, so `selector: "*"` on a rule placed at a site expands to
//     that site's screens rather than to every screen in the deployment.
//   - canWrite bounds what is ACTED on. A target named explicitly by id —
//     `screen_id`, an `entity_id` — bypasses selector resolution entirely, and
//     is refused here, per target, and REPORTED as a failed target rather than
//     silently skipped (relayCommandSink.dispatch, screenOverrideSink.write).
//
// A node the tree does not contain is refused, which is the fail-closed
// direction (SEC-005) — and that now includes the automation's OWN node. A node
// below the automation's already failed closed for free, because a node the tree
// does not contain has an empty ancestor chain; the node ITSELF did not, because
// the `node == scopeNode` arm compared two strings and never asked the tree
// anything. So a rule whose placement had since been deleted still authorized
// itself at that placement — "unknown fails closed" with one hole in it, at
// exactly the node the rule reaches most often. Both ends are now checked
// against the tree. An automation row carrying no scope_node at all —
// impossible through this surface, which requires the placement (DAT-006) —
// authorizes nothing rather than everything, for the same reason.
//
// roleAt answers auth.RoleOperator inside the subtree and "no binding" outside
// it. Operator, not owner: this view MIRRORS the authority the author had to
// hold, and what POST /automations requires is canWrite, which auth.CanWrite
// grants from `operator` upward (auth/role.go). Answering `owner` handed the
// run a role its author may never have held — three levels above the floor
// that admitted the write — so any future operation configured above the
// coarse write floor (the data-subject ones are the two that exist today, and
// no rule action reaches them) would have been answered yes on authority
// nobody granted. Grant no more than the authority being mirrored.
func automationScopeView(tree datamodel.ScopeTree, scopeNode string) scopeView {
	inSubtree := subtreePredicate(tree)
	within := automationSubtreeBound(tree, scopeNode)
	return scopeView{
		inSubtree: inSubtree,
		canRead:   within,
		canWrite:  within,
		roleAt: func(node string) (auth.Role, bool) {
			if !within(node) {
				return "", false
			}
			return auth.RoleOperator, true
		},
	}
}

// automationSubtreeBound is the containment predicate automationScopeView is
// built from, and the SAME one the authoring-time target check applies
// (automationTargetsInScope). One definition, because a rule whose targets
// authoring accepts and the run then refuses is precisely the
// accepts-work-it-never-performs shape this file's own doc condemns — and two
// transcriptions of "is this node within the rule's reach" is how that gap
// reopens.
//
// # The both-ends test is ONE statement on purpose
//
// It reads as two conditions and used to be written as two `if`s, and that form
// was untestable in a way worth naming rather than living with: over the tree
// this package builds, EITHER half alone produces the whole predicate's
// behaviour, so deleting either one on its own left every test in the repository
// green. Not because the rule is untested — deleting BOTH is caught — but
// because the two are exactly redundant:
//
//   - node == scopeNode makes the two tests the same test.
//   - inSubtree can only be true when scopeNode appears in AncestorChain(node),
//     and AncestorChain yields only ids the tree holds (it returns nil for an
//     absent node and STOPS at a dangling parent_id), so it already implies
//     both ends are present.
//
// Two mutually-dead statements are worse than one live one: no test can pin
// either, and the pair looks like coverage. Written as one loop over both ends
// the guard is a single deletable unit, and
// TestAutomationSubtreeBoundRefusesEitherEndTheTreeDoesNotHold kills that
// deletion. The redundancy is kept deliberately — the predicate must not depend
// on AncestorChain's dangling-parent behaviour staying what it is today — it is
// just no longer spelled as two things that can rot apart.
func automationSubtreeBound(tree datamodel.ScopeTree, scopeNode string) func(string) bool {
	inSubtree := subtreePredicate(tree)
	return func(node string) bool {
		if scopeNode == "" || node == "" {
			return false
		}
		// Both ends must be nodes the tree actually holds. A deleted node is not
		// a node this rule may reach, whichever end of the comparison it is on —
		// and the automation's OWN placement is an end like any other, which is
		// the end the `node == scopeNode` arm below would otherwise wave through
		// on a string comparison that asked the tree nothing.
		for _, end := range [...]string{scopeNode, node} {
			if _, ok := tree.KindOf(end); !ok {
				return false
			}
		}
		return node == scopeNode || inSubtree(scopeNode, node)
	}
}
