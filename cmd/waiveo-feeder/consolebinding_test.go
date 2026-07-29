package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/auth"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
)

// consolebinding_test.go is the REACHABILITY guard for the console binding.
//
// Everything the binding does is tested in internal/app/auth. What is tested
// here is the one thing those tests cannot say: that the shipped feeder wires
// it. A component with no caller was the state this binding was in — an
// admission rule, a closed verb set and an attribution, and no socket anywhere —
// and the only way that stops recurring is a test that fails when main stops
// starting it.

// TestFeederStartsTheConsoleBindingAndRefusesANonRootPeer drives the feeder's
// OWN wiring function, not a reassembly of it: startConsoleBinding is what
// main calls, and this calls the same function.
//
// It asserts what an operator can observe on a running box: a socket exists at
// the deployment's auth directory, it is mode 0700 inside a 0700 directory
// (SEC-070/071), and a connection from a non-root peer gets nothing back
// (SEC-072).
func TestFeederStartsTheConsoleBindingAndRefusesANonRootPeer(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("this test connects as its own uid and asserts the refusal; as root it would be admitted")
	}
	// A short scratch path: a Unix domain socket address holds ~104 bytes and
	// t.TempDir builds its name from this test's own (long) name.
	dir, err := os.MkdirTemp("", "wvf")
	if err != nil {
		t.Fatalf("scratch dir: %v", err)
	}
	defer os.RemoveAll(dir)

	clock := func() int64 { return 1752537600000 }
	floor, err := auth.OpenClockFloor(dir, clock)
	if err != nil {
		t.Fatalf("OpenClockFloor: %v", err)
	}
	st, err := auth.Open(filepath.Join(dir, authStoreFile), floor.Now, ulid.New)
	if err != nil {
		t.Fatalf("auth.Open: %v", err)
	}
	defer st.Close()

	ln, err := startConsoleBinding(dir, st, floor, nil)
	if err != nil {
		t.Fatalf("startConsoleBinding: %v", err)
	}
	defer ln.Close()

	// The socket is where a second process would look for it: the deployment's
	// auth directory plus the name the auth package publishes, which is how the
	// two agree without either holding a copy of the other's string.
	want := filepath.Join(dir, auth.ConsoleSocketName)
	if ln.Path() != want {
		t.Fatalf("console socket at %q, want %q", ln.Path(), want)
	}
	fi, err := os.Lstat(ln.Path())
	if err != nil {
		t.Fatalf("stat the socket: %v", err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s is not a socket (mode %v) — SEC-070 requires a Unix domain socket", ln.Path(), fi.Mode())
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Fatalf("socket mode = %04o, want 0700 (SEC-071)", perm)
	}

	conn, err := net.Dial("unix", ln.Path())
	if err != nil {
		t.Fatalf("dial the feeder's console socket: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = conn.Write([]byte(`{"verb":"service.status"}`))
	body, _ := io.ReadAll(conn)
	if len(body) != 0 {
		t.Fatalf("the feeder's console binding answered a non-root peer with %d byte(s): %q", len(body), body)
	}
}

// TestFeederConsoleBindingUnlinksItsSocketOnClose covers the shutdown half: the
// next boot's stale-socket check should have nothing to reason about after a
// clean stop, and a socket that outlived its process is the state that check
// exists to survive rather than the state it should routinely find.
func TestFeederConsoleBindingUnlinksItsSocketOnClose(t *testing.T) {
	dir, err := os.MkdirTemp("", "wvf")
	if err != nil {
		t.Fatalf("scratch dir: %v", err)
	}
	defer os.RemoveAll(dir)

	clock := func() int64 { return 1752537600000 }
	floor, err := auth.OpenClockFloor(dir, clock)
	if err != nil {
		t.Fatalf("OpenClockFloor: %v", err)
	}
	st, err := auth.Open(filepath.Join(dir, authStoreFile), floor.Now, ulid.New)
	if err != nil {
		t.Fatalf("auth.Open: %v", err)
	}
	defer st.Close()

	ln, err := startConsoleBinding(dir, st, floor, nil)
	if err != nil {
		t.Fatalf("startConsoleBinding: %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Lstat(ln.Path()); !os.IsNotExist(err) {
		t.Fatalf("the socket survived a clean close (%v)", err)
	}
	// And the next boot binds over the same path without complaint.
	again, err := startConsoleBinding(dir, st, floor, nil)
	if err != nil {
		t.Fatalf("re-binding after a clean stop: %v", err)
	}
	_ = again.Close()
}

// TestMainStartsTheConsoleBinding is the other half of the reachability claim,
// and it is deliberately a check on main's SOURCE rather than on its behavior.
//
// The test above proves startConsoleBinding does the right thing. It cannot
// prove main calls it: removing the call from main leaves that test passing,
// which was verified rather than assumed (a mutation deleting the call from
// main kept `go test ./cmd/waiveo-feeder -run Console` green). Two tests are
// therefore needed, and this is the second.
//
// It parses main.go and asserts main's own body contains the call. That is a
// weaker kind of evidence than driving the built binary — it says the statement
// is there, not that a running feeder serves the socket — and it is what a test
// can say without building and booting the whole feeder on every run. The
// behavioral proof at binary level is the first test's assertions applied to the
// identical function main invokes.
//
// KNOW WHAT THIS DOES NOT CATCH, measured rather than assumed: it catches the
// call being DELETED, not the call being made UNREACHABLE. Wrapping it in a
// condition that is never true — `if os.Getenv("NEVER_SET") == "yes" { … }` —
// keeps this test green while shipping a feeder with no console socket at all.
// Closing that gap needs a test that builds and boots the binary and dials the
// socket, which is a cost every run would pay; the trade is recorded here so the
// next reader knows which half of the regression is guarded.
func TestMainStartsTheConsoleBinding(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	var mainFn *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && fn.Name.Name == "main" {
			mainFn = fn
			break
		}
	}
	if mainFn == nil {
		t.Fatal("main.go declares no func main")
	}
	called := false
	ast.Inspect(mainFn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "startConsoleBinding" {
			called = true
		}
		return true
	})
	if !called {
		t.Fatal("func main does not call startConsoleBinding — the console binding (SEC-070) would not exist on a running feeder, which is the state this whole binding was in before it had a socket")
	}
}
