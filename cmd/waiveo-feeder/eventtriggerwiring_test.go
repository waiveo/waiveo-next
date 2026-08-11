package main

import (
	"go/ast"
	"testing"
)

// eventtriggerwiring_test.go guards the two lines that make rules/1's `event`
// trigger (RUL-080/081) fire on a real deployment, and the fact that both name
// the SAME dispatcher.
//
// It exists because both were UNGUARDED, in this repo's recurring shape.
// Deleting `api.WithEventTriggers(eventTriggers)` from func main, and separately
// deleting the dispatcher from the ingest's construction, each left `go test
// ./...` at exit 0 across every package. Every api-layer test builds its own
// dispatcher and calls Deliver on it directly, so the EVALUATOR is covered from
// every angle while "does a shipped feeder connect the durable event log to
// it?" was answered by nobody. Without those lines an `event` trigger is exactly
// what it was before this track: a member of the closed vocabulary an author can
// write, the compiler accepts and the store persists, that fires nothing — with
// every gate green.
//
// The wiring is a FOUR-part fact and this asserts all of it, because any one
// part alone is inert:
//
//  1. a dispatcher exists (`eventTriggers := &api.EventTriggerDispatcher{}`);
//  2. the telemetry ingest DELIVERS to it (eventingest.New's EventDeliverer
//     argument) — without this nothing ever calls it;
//  3. api.New BINDS the evaluator into it (api.WithEventTriggers) — without
//     this it is delivered to and does nothing, which is its documented inert
//     state and is indistinguishable from working.
//  4. api.New is given the event log to REPORT each fired run into
//     (api.WithRunEvents) — without this a run that fires and refuses every
//     target is indistinguishable, from outside, from a run that never
//     happened.
//
// And it asserts (2) and (3) name the same object. Two dispatchers is the
// nastiest version of this bug: both lines are present, review reads them as
// correct, and every press still vanishes.
//
// It is a check on main's SOURCE, with the limits clockwiring_test.go states for
// its own: it catches a call being removed or an argument being changed, not a
// call being made unreachable. The behavioural halves live in
// internal/app/api (a real telemetry push through the real ingest into the real
// evaluator, changing a real screen) and internal/app/eventingest (delivery
// happens, off the lock, before the ack).
func TestMainWiresTheEventTriggerDispatcher(t *testing.T) {
	mainFn := parseMainFunc(t)

	// (1) The dispatcher itself. Without pinning the construction, every check
	// below could be satisfied by some other local that happens to be named
	// eventTriggers.
	const dispatcher = "eventTriggers"
	if !assignsEventTriggerDispatcher(mainFn, dispatcher) {
		t.Fatalf("func main does not construct the event-trigger dispatcher (`%s := &api.EventTriggerDispatcher{}`); "+
			"the checks below would then be agreeing about the wrong object", dispatcher)
	}

	// (2) The ingest delivers to it. eventingest.New takes the EventDeliverer as
	// its LAST argument, and a nil there is a legal, silent "no app-side
	// consumer of durable events" — which is what a deployment gets if this is
	// dropped.
	const (
		ingestCallee   = "eventingest.New"
		wantDeliverArg = dispatcher + ".Deliver"
		ingestArgs     = 6
	)
	ingestCalls := 0
	ast.Inspect(mainFn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || calleeName(call) != ingestCallee {
			return true
		}
		ingestCalls++
		if len(call.Args) != ingestArgs {
			t.Errorf("%s is called with %d argument(s), want %d — its EventDeliverer is the last one",
				ingestCallee, len(call.Args), ingestArgs)
			return true
		}
		if got := render(call.Args[ingestArgs-1]); got != wantDeliverArg {
			t.Errorf("%s receives %q as its EventDeliverer, want %s. A nil (or any other) deliverer means every durable event this deployment ingests — a viewer's press included — is recorded and acted on by nothing: no `event` trigger anywhere fires.",
				ingestCallee, got, wantDeliverArg)
		}
		return true
	})
	if ingestCalls == 0 {
		t.Errorf("func main never calls %s; the telemetry ingest is the ONLY producer of durable events on this peer, so nothing can fire an `event` trigger", ingestCallee)
	}
	if ingestCalls > 1 {
		t.Errorf("func main calls %s %d times; a second ingest would push events into a dispatcher the first one does not share", ingestCallee, ingestCalls)
	}

	// (3) api.New binds the evaluator into that same dispatcher.
	const bindCallee = "api.WithEventTriggers"
	bindCalls := 0
	ast.Inspect(mainFn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || calleeName(call) != bindCallee {
			return true
		}
		bindCalls++
		if len(call.Args) != 1 {
			t.Errorf("%s is called with %d argument(s), want exactly 1", bindCallee, len(call.Args))
			return true
		}
		if got := render(call.Args[0]); got != dispatcher {
			t.Errorf("%s binds %s, but the ingest delivers to %s. Two dispatchers is the worst form of this bug: both lines are present and read as correct, the ingest delivers into an object nothing bound, and every press still fires nothing.",
				bindCallee, got, dispatcher)
		}
		return true
	})
	if bindCalls == 0 {
		t.Errorf("func main never calls %s. The dispatcher is then delivered to and INERT — its documented no-op state — so every `event` trigger an operator can author, the compiler accepts and the store persists fires nothing, and a viewer pressing a button on a screen makes nothing happen anywhere.",
			bindCallee)
	}
	if bindCalls > 1 {
		t.Errorf("func main calls %s %d times; the evaluator is bound once, at construction", bindCallee, bindCalls)
	}

	// (4) The RUN REPORT reaches the durable event log. An event-fired run has
	// no caller to answer, so this option is the only route by which its
	// per-target refusals become readable — a rule whose target has moved out of
	// its subtree otherwise produces an unchanged screen and nothing else. It is
	// exactly as deletable-with-the-suite-green as (2) and (3) were: every
	// api-layer test that needs the record wires its own sink.
	//
	// The sink must be the SAME hub the ingest appends to, or the press and the
	// run it caused land in two different feeds and no subscriber sees both.
	const (
		runEventsCallee = "api.WithRunEvents"
		wantRunSink     = "eventHub"
	)
	runEventCalls := 0
	ast.Inspect(mainFn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || calleeName(call) != runEventsCallee {
			return true
		}
		runEventCalls++
		if len(call.Args) != 1 {
			t.Errorf("%s is called with %d argument(s), want exactly 1", runEventsCallee, len(call.Args))
			return true
		}
		if got := render(call.Args[0]); got != wantRunSink {
			t.Errorf("%s publishes to %s, but the ingest appends to %s. Two logs means the press and the run it caused are in different feeds, and the correlation the trace id exists for reaches nobody.",
				runEventsCallee, got, wantRunSink)
		}
		return true
	})
	if runEventCalls == 0 {
		t.Errorf("func main never calls %s. An event-fired run then reports to nothing but this process's log: a target the rule may no longer write is refused, the screen does not change, and there is no record anywhere an operator can read.",
			runEventsCallee)
	}
	if runEventCalls > 1 {
		t.Errorf("func main calls %s %d times; the sink is set once, at construction", runEventsCallee, runEventCalls)
	}
}

// assignsEventTriggerDispatcher reports whether main binds name to a fresh
// &api.EventTriggerDispatcher{}.
func assignsEventTriggerDispatcher(mainFn *ast.FuncDecl, name string) bool {
	found := false
	ast.Inspect(mainFn, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		if lhs, ok := as.Lhs[0].(*ast.Ident); !ok || lhs.Name != name {
			return true
		}
		unary, ok := as.Rhs[0].(*ast.UnaryExpr)
		if !ok {
			return true
		}
		lit, ok := unary.X.(*ast.CompositeLit)
		if !ok {
			return true
		}
		if render(lit.Type) == "api.EventTriggerDispatcher" {
			found = true
		}
		return true
	})
	return found
}
