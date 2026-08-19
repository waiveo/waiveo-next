package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// clockwiring_test.go asserts that every component this feeder constructs with a
// clock is constructed with THE SAME clock: nowMs, the app's persisted monotonic
// clock floor reading (internal/app/auth.ClockFloor.Now, security-model/1
// SEC-066).
//
// It exists because no other test can say this. Each package's own tests prove
// it HONOURS the clock it is given — internal/app/store stamps a row's baseline
// from it, internal/app/eventingest stamps an envelope's ts from it — and every
// one of those tests passes an arbitrary fixture clock, as it should. What none
// of them can observe is which clock a shipped feeder actually hands over. That
// choice lives in exactly one place, func main, and it is one edit away from
// being wrong in a way nothing else notices: a `store.WallClockMs` in place of
// `nowMs` builds, vets, and passes the entire suite, and is invisible on any
// deployment whose floor is still 0 — which today is every deployment.
//
// The window in which that is invisible closes the day an authenticated time
// source advances the floor, and it closes badly: whichever component kept the
// host clock starts stamping BEHIND the ones that did not, so an audit record
// and the row it describes disagree about when they happened.
//
// It is a check on main's SOURCE, in the same spirit and with the same limits as
// TestMainStartsTheConsoleBinding: it catches the argument being CHANGED, not a
// call being made unreachable, and it says the statement is there rather than
// that a running feeder behaves. That is what a test can say without booting the
// whole binary on every run.

// clockArg names one constructor main calls and the position its clock is passed
// in. The set is deliberately explicit rather than derived: a new component that
// takes a clock has to be ADDED here, and having to add it is the point — the
// question "which clock does this get?" should be answered once, in review,
// rather than defaulted.
type clockArg struct {
	// callee is the call expression's rendered function, e.g. "store.Open" or
	// "st.EventLog".
	callee string
	// index is the zero-based argument position the clock occupies.
	index int
	// why says what reads this clock, so a failure names the damage rather than
	// the mechanics.
	why string
}

var mainClockArgs = []clockArg{
	{"origin.WithClock", 0, "an asset's stored-at stamp, which the retention sweep measures a minimum age against"},
	{"store.Open", 1, "every resource row's created_at/updated_at baseline"},
	{"apihttp.NewIdempotencyStore", 0, "an idempotency record's replay window (API-052/055)"},
	{"st.EventLog", 1, "the durable event log's retention decisions"},
	{"eventingest.New", 3, "an ingested telemetry envelope's ts"},
	{"auth.NewAuditor", 2, "every audit.event's timestamp (SEC-150)"},
	{"auth.Open", 1, "grant ttl expiry (SEC-032/035) and every TOTP step (SEC-004)"},
	{"api.New", 2, "the api layer's own stamps"},
	{"startWebhookDelivery", 4, "outbound webhook delivery timing and signature rotation overlap"},
	{"newContentSweeper", 3, "the content retention sweep's age measurement"},
	{"overrideExpiryLoop", 2, "when a screen override's TTL is judged to have lapsed (DAT-004d)"},
}

// TestMainWiresOneClockIntoEveryComponentThatTakesOne parses main.go and checks
// each call above passes the bare identifier nowMs.
func TestMainWiresOneClockIntoEveryComponentThatTakesOne(t *testing.T) {
	mainFn := parseMainFunc(t)

	// nowMs must itself BE the floor's reading, not some other local named nowMs.
	// Without this the whole test could be satisfied by `nowMs := time.Now...`.
	if !assignsFloorReading(mainFn) {
		t.Fatal("func main does not assign nowMs from the clock floor (`nowMs := clockFloor.Now`); every check below would then be asserting agreement on the wrong clock")
	}

	seen := map[string]bool{}
	ast.Inspect(mainFn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := calleeName(call)
		for _, want := range mainClockArgs {
			if name != want.callee {
				continue
			}
			seen[want.callee] = true
			if want.index >= len(call.Args) {
				t.Errorf("%s is called with %d argument(s); its clock was expected at index %d",
					want.callee, len(call.Args), want.index)
				continue
			}
			if id, ok := call.Args[want.index].(*ast.Ident); !ok || id.Name != "nowMs" {
				t.Errorf("%s receives %s as its clock, want nowMs — %s would then be stamped from a clock the rest of the process does not share",
					want.callee, render(call.Args[want.index]), want.why)
			}
		}
		return true
	})

	for _, want := range mainClockArgs {
		if !seen[want.callee] {
			t.Errorf("func main makes no call to %s; either the wiring was removed (and %s is now unstamped or stamped elsewhere) or this table is stale",
				want.callee, want.why)
		}
	}

	checkNoOtherClockReaches(t)
}

// parseMainFunc returns main.go's func main, failing the test if it is absent.
func parseMainFunc(t *testing.T) *ast.FuncDecl {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == "main" {
			return fn
		}
	}
	t.Fatal("main.go declares no func main")
	return nil
}

// assignsFloorReading reports whether main binds nowMs to clockFloor.Now.
func assignsFloorReading(mainFn *ast.FuncDecl) bool {
	found := false
	ast.Inspect(mainFn, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		lhs, ok := as.Lhs[0].(*ast.Ident)
		if !ok || lhs.Name != "nowMs" {
			return true
		}
		if render(as.Rhs[0]) == "clockFloor.Now" {
			found = true
		}
		return true
	})
	return found
}

// calleeName renders a call's function as "pkg.Fn", "recv.Method" or "Fn".
// Anything more complex renders empty, which simply matches nothing in the table.
func calleeName(call *ast.CallExpr) string { return render(call.Fun) }

// render prints the identifier forms this test compares against, and "" for
// every expression shape it does not need to understand.
func render(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		if x, ok := v.X.(*ast.Ident); ok {
			return x.Name + "." + v.Sel.Name
		}
	}
	return ""
}

// hostClockException names one place this package is ALLOWED to read the host
// clock, and why. Anything not on this list is a finding.
type hostClockException struct {
	fn   string // enclosing function name
	expr string // the rendered expression, e.g. "store.WallClockMs"
	why  string
}

var hostClockExceptions = []hostClockException{
	// runStoreCheckSections is reportStoreIDs' body — the open lives there since
	// the report grew a verdict its caller prints last.
	{"runStoreCheckSections", "store.WallClockMs",
		"a read-only diagnostic subcommand that stamps no row, so there is no stamp for the app's clock floor to keep " +
			"consistent with — and reaching for the floor would create the auth-state directory it lives in as a side " +
			"effect of a check that must not write. NOT 'it runs outside a serving process': the first-photon runbook " +
			"says to run it against a box whose old build is still up"},
	{"main", "time.Now",
		"the host reading the clock floor itself is built ON — the floor's whole job is to clamp it, so it has to be able to see it"},
}

// checkNoOtherClockReaches is the COMPLETENESS half of this file, and it is the
// half that actually holds.
//
// The table above asserts what main hands each component at a call site. Two
// escapes from that shape were demonstrated rather than imagined:
//
//   - desiredStateSource takes its clock by STRUCT LITERAL, which is not a call
//     expression and therefore cannot be expressed as a {callee, index} row at
//     all. That is the component deciding which program each screen is serving
//     at this instant, and pointing it at the host clock passed the whole suite.
//   - startWebhookDelivery and newContentSweeper are local WRAPPERS. The table
//     proves main hands them nowMs and says nothing about what they do with it;
//     replacing the clock INSIDE either one passed the whole suite too. The
//     content sweeper is the worse case, because it deletes content bytes based
//     on an age measured from that clock.
//
// So instead of enumerating the ways a clock can arrive — which is unbounded —
// this asserts the host clock is not READ anywhere in this package except the
// two places it legitimately is. That covers call arguments, struct literals,
// wrappers, and whatever shape the next component is wired in, without anyone
// having to remember to add a row.
func checkNoOtherClockReaches(t *testing.T) {
	t.Helper()
	allowed := map[string]bool{}
	for _, e := range hostClockExceptions {
		allowed[e.fn+" "+e.expr] = true
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	found := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				expr := render(sel)
				if expr != "store.WallClockMs" && expr != "time.Now" {
					return true
				}
				found++
				if !allowed[fn.Name.Name+" "+expr] {
					t.Errorf("%s reads the host clock (%s) at %s — every component in this process must stamp from nowMs, the clock floor's reading. If this one genuinely must not, add it to hostClockExceptions with the reason.",
						fn.Name.Name, expr, fset.Position(sel.Pos()))
				}
				return true
			})
		}
	}
	if found == 0 {
		t.Error("no host-clock read found anywhere in this package, not even the allowed ones; the scan is matching nothing and would pass whatever the package does")
	}
}
