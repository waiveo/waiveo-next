package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// signingkeywiring_test.go asserts that a shipped relay installs its own
// Lease-signing identity onto the player/1 server (PLY-090).
//
// It exists because nothing else can say this, and the gap had already cost us
// once. Every other test builds its own fixture server and calls SetSigningKey
// itself, so the mechanism is covered from every angle while the question
// "does main do it?" was answered by nobody. Deleting main's own call and
// running the entire suite gave 81 packages ok, exit 0 — the same shape of hole
// that let the original defect ship, where the key was installed only as a side
// effect of installing a program and a relay holding none answered every pull
// 500 INTERNAL.
//
// It is a check on main's SOURCE, in the same spirit and with the same limits as
// clockwiring_test.go in cmd/waiveo-feeder: it catches the call being REMOVED or
// its argument being changed, not the call being made unreachable, and it says
// the statement is there rather than that a running relay signs correctly. That
// is what a test can say without booting the whole binary on every run. The
// behavioural half — a terminal-default Lease verifying under the relay cert's
// own key — is asserted in internal/relay/playerserver
// (TestTerminalDefaultIsServableWithNoProgramEverInstalled).
func TestMainInstallsTheRelaysLeaseSigningIdentity(t *testing.T) {
	mainFn := parseRelayMainFunc(t)

	const (
		wantCallee = "pairingSrv.SetSigningKey"
		wantArg    = "relayID.PrivateKey"
	)

	found := 0
	ast.Inspect(mainFn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || renderExpr(call.Fun) != wantCallee {
			return true
		}
		found++
		if len(call.Args) != 1 {
			t.Errorf("%s is called with %d argument(s), want exactly 1", wantCallee, len(call.Args))
			return true
		}
		if got := renderExpr(call.Args[0]); got != wantArg {
			t.Errorf("%s receives %s, want %s — the Lease-signing key MUST be the private half of the keypair the certificate this listener presents certifies, or a player's PLY-090 signature check against its pinned trust anchor cannot line up with what it connected to",
				wantCallee, got, wantArg)
		}
		return true
	})

	if found == 0 {
		t.Errorf("func main never calls %s. Without it this relay has no Lease-signing identity: every paired screen's /player/v1/program pull answers 500 INTERNAL — including the pull that should return data-model/1's terminal default (DAT-118), which is the state a site whose screen_programs is REL-060's empty placeholder is already in. No program write can supply the key; SetProgram takes none.",
			wantCallee)
	}
	if found > 1 {
		t.Errorf("func main calls %s %d times; the identity is installed once, at construction, before any route is mounted", wantCallee, found)
	}

	assertKeyReachesNothingElse(t, mainFn, wantCallee)
}

// assertKeyReachesNothingElse is the completeness half: no OTHER call in main
// names the selector `relayID.PrivateKey`.
//
// It matters because the defect being guarded was not a missing call — it was
// the key arriving as a side effect of something else (a program write), which
// made its presence incidental and its absence invisible. A second call passing
// the same selector would restore exactly that: the explicit call could be
// deleted and the relay would still sign, until the day that other path stopped
// running.
//
// READ THE LIMIT. This is a check on one SELECTOR EXPRESSION, not on key flow.
// main hands the whole identity struct — private key included — to
// relayTLSCertificate and to bootAutomationStack, and this says nothing about
// either. Nor would it notice `k := relayID.PrivateKey` bound to a local and
// passed on from there. What it does catch is the shape the defect actually
// took: a second direct hand-off of the key at a call site, sitting beside the
// explicit one and quietly making it redundant.
func assertKeyReachesNothingElse(t *testing.T, mainFn *ast.FuncDecl, allowedCallee string) {
	t.Helper()

	uses := 0
	ast.Inspect(mainFn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		callee := renderExpr(call.Fun)
		for _, arg := range call.Args {
			if renderExpr(arg) != "relayID.PrivateKey" {
				continue
			}
			uses++
			if callee != allowedCallee {
				t.Errorf("func main also hands relayID.PrivateKey to %s at %s. The relay's Lease-signing identity must reach the player server through %s alone; a second path makes that call deletable without anything failing, which is how the key became an incidental side effect the first time.",
					callee, callee, allowedCallee)
			}
			return false
		}
		return true
	})

	if uses == 0 {
		t.Error("func main never references relayID.PrivateKey at all; this scan is matching nothing and would pass whatever main does")
	}
}

// parseRelayMainFunc returns main.go's func main, failing the test if absent.
func parseRelayMainFunc(t *testing.T) *ast.FuncDecl {
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

// renderExpr prints the identifier forms this test compares against, and "" for
// every expression shape it does not need to understand.
func renderExpr(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		if x, ok := v.X.(*ast.Ident); ok {
			return x.Name + "." + v.Sel.Name
		}
		return "." + v.Sel.Name
	case *ast.CallExpr:
		return renderExpr(v.Fun) + "(…)"
	}
	return ""
}
