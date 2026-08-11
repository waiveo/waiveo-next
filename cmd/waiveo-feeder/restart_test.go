package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/api"
)

// restart_test.go covers the PROCESS half of the restart operation: the
// fail-closed supervisor declaration, the arm-once guarantee, and — the part
// nothing else can say — that the shipped binary reaches all of it.
//
// The api layer's own refusals are tested in internal/app/api/restart_test.go
// against the live mux. What is tested here is the wiring, because "the
// component exists and nothing a deployment runs reaches it" is the exact shape
// of defect this repo keeps producing: enforcement built, authoring never.

// TestAnUndeclaredSupervisorFailsClosed. The refusal an unsupervised deployment
// gives comes from the api layer, and it can only give it if the config it is
// handed says so. Anything that filled in a default here would arm a restart on
// a box where nothing starts a replacement.
func TestAnUndeclaredSupervisorFailsClosed(t *testing.T) {
	cfg := newRestarter("").config()
	if cfg.Supervisor != "" {
		t.Fatalf("an undeclared supervisor produced %q — the api layer would accept a restart on a box that would never come back", cfg.Supervisor)
	}
	// And the wiring is still SUPPLIED rather than withheld, so the refusal has
	// exactly one source. Two ways to reach RESTART_UNSUPPORTED (an unwired
	// option and an empty declaration) is two things that can disagree.
	if cfg.Arm == nil {
		t.Fatal("config() withheld Arm — the refusal must come from the empty declaration, not from a missing seam")
	}
	if cfg.DrainBudgetMs != restartDrainBudget.Milliseconds() {
		t.Errorf("DrainBudgetMs = %d, want the budget main's drain actually enforces (%d)",
			cfg.DrainBudgetMs, restartDrainBudget.Milliseconds())
	}
}

// TestConfigDeclaresWhatWasDeclared.
func TestConfigDeclaresWhatWasDeclared(t *testing.T) {
	if got := newRestarter("systemd").config().Supervisor; got != "systemd" {
		t.Fatalf("Supervisor = %q, want systemd", got)
	}
}

// TestArmingIsExactlyOnce is API-155's guarantee at the only place that can make
// it: the process. The first stop is the only one that can happen, so a second
// arm must report false rather than schedule a second one.
func TestArmingIsExactlyOnce(t *testing.T) {
	rs := newRestarter("systemd")
	rs.hold = 0

	if !rs.arm(api.RestartOrder{Actor: "principal-a", TraceID: "trace-a"}) {
		t.Fatal("the first arm reported false")
	}
	if rs.arm(api.RestartOrder{Actor: "principal-b"}) {
		t.Fatal("a second arm reported true — two accepted restarts, one of which cannot happen")
	}

	select {
	case order := <-rs.requests:
		if order.Actor != "principal-a" {
			t.Errorf("the delivered order names %q, want the FIRST caller principal-a", order.Actor)
		}
		if order.TraceID != "trace-a" {
			t.Errorf("the delivered order's trace = %q, want trace-a — it is what ties the shutdown log lines to the request", order.TraceID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing reached main's shutdown select — the process would answer 202 and never stop")
	}

	// Exactly one order, not two.
	select {
	case extra := <-rs.requests:
		t.Fatalf("a second order was delivered: %+v", extra)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestArmingDoesNotBlockWhenNobodyIsListening. A SIGTERM that arrives first
// takes main out of the select, and the delivery goroutine must not be left
// parked on a receiver that has gone.
func TestArmingDoesNotBlockWhenNobodyIsListening(t *testing.T) {
	rs := newRestarter("systemd")
	rs.hold = 0
	// Fill the buffer so the send has nowhere to go.
	rs.requests <- api.RestartOrder{Actor: "already-there"}

	done := make(chan struct{})
	go func() {
		rs.arm(api.RestartOrder{Actor: "late"})
		// The delivery is on its own goroutine inside arm; give it a moment to
		// have either completed or parked, then confirm the buffer is untouched.
		time.Sleep(50 * time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("arm blocked")
	}
	if got := <-rs.requests; got.Actor != "already-there" {
		t.Fatalf("the buffered order was displaced by %q", got.Actor)
	}
}

// TestSupervisorIsReadFromTheEnvironmentAndDefaultsToNothing.
func TestSupervisorIsReadFromTheEnvironmentAndDefaultsToNothing(t *testing.T) {
	if got := loadConfig(func(string) string { return "" }).supervisor; got != "" {
		t.Fatalf("the default supervisor is %q; it must be empty, because the safe answer for a deployment that has said nothing is that nothing will restart it", got)
	}
	env := map[string]string{"WAIVEO_FEEDER_SUPERVISOR": "  systemd  "}
	if got := loadConfig(func(k string) string { return env[k] }).supervisor; got != "systemd" {
		t.Fatalf("supervisor = %q, want systemd (trimmed)", got)
	}
}

// mainFunc parses main.go and returns func main's declaration.
func mainFunc(t *testing.T) *ast.FuncDecl {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && fn.Name.Name == "main" {
			return fn
		}
	}
	t.Fatal("main.go declares no func main")
	return nil
}

// TestMainWiresTheRestartOperation. Without this option the route still mounts
// and refuses RESTART_UNSUPPORTED forever — a complete, tested operation that no
// shipped binary can perform, which is precisely the state the marketplace
// resolver and the required-pack floor were each found in.
func TestMainWiresTheRestartOperation(t *testing.T) {
	wired := false
	ast.Inspect(mainFunc(t), func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "WithRestart" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "api" {
			wired = true
		}
		return true
	})
	if !wired {
		t.Fatal("func main does not pass api.WithRestart — POST /api/v1/system/restart would refuse RESTART_UNSUPPORTED on every shipped box, whatever the unit file declares")
	}
}

// TestARestartTakesTheSAMEDrainASignalDoes is API-157, asserted structurally
// because it cannot be asserted behaviourally: a case that observed only the
// response cannot tell a graceful drain from an abrupt exit.
//
// What it checks is that the drain is NOT inside the select. Both a signal and a
// restart select and then fall through to one shared drain, so there is exactly
// one shutdown sequence and a restart cannot become a second, quietly less
// careful copy of it. The failure this prevents is concrete and has an obvious
// shape: someone adds a `case order := <-restarter.requests:` arm and writes its
// own abbreviated teardown inside it.
func TestARestartTakesTheSAMEDrainASignalDoes(t *testing.T) {
	fn := mainFunc(t)

	var shutdownSelect *ast.SelectStmt
	ast.Inspect(fn, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectStmt)
		if !ok {
			return true
		}
		// The shutdown select is the one with an arm receiving the restart order.
		if selectReceivesFrom(sel, "restarter", "requests") {
			shutdownSelect = sel
		}
		return true
	})
	if shutdownSelect == nil {
		t.Fatal("func main's shutdown select has no arm receiving from restarter.requests — an accepted restart would arm and the process would never stop")
	}
	if callsMethod(shutdownSelect, "server", "Shutdown") {
		t.Fatal("the graceful drain runs INSIDE the shutdown select — it must run after it, shared by every arm, or a restart is a second shutdown sequence that will drift from the signal one")
	}
	if !callsMethod(fn, "server", "Shutdown") {
		t.Fatal("func main never calls server.Shutdown — there is no graceful drain at all")
	}
}

// selectReceivesFrom reports whether any arm of sel receives from `x.field`.
func selectReceivesFrom(sel *ast.SelectStmt, x, field string) bool {
	found := false
	ast.Inspect(sel, func(n ast.Node) bool {
		unary, ok := n.(*ast.UnaryExpr)
		if !ok || unary.Op != token.ARROW {
			return true
		}
		s, ok := unary.X.(*ast.SelectorExpr)
		if !ok || s.Sel.Name != field {
			return true
		}
		if ident, ok := s.X.(*ast.Ident); ok && ident.Name == x {
			found = true
		}
		return true
	})
	return found
}

// callsMethod reports whether node contains a call to `recv.method(...)`.
func callsMethod(node ast.Node, recv, method string) bool {
	found := false
	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != method {
			return true
		}
		if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == recv {
			found = true
		}
		return true
	})
	return found
}
