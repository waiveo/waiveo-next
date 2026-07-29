package playerserver

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// signingidentity_test.go asserts the ONE authority rule over this server's
// Lease-signing identity: only SetSigningKey may establish or change it.
//
// It is a check on this package's SOURCE, and its limits are worth stating
// plainly: it says no other function assigns the field, not that a running relay
// signs with the right key. The behavioural half lives next door
// (TestAProgramWriteCannotChangeWhatOtherScreensLeasesAreSignedWith, which pulls
// a real Lease and verifies it against the relay cert's public key). This test
// exists because that behavioural half can only observe the paths that exist
// today, and the property being protected is about a path NOT existing.
//
// The defect it guards is not hypothetical. SetProgram — a PER-SCREEN write —
// used to take an ed25519 key and install any well-formed one it was handed as
// the identity for the WHOLE server. One write for screen B carrying a foreign
// but valid key made screen A's Lease verify under the foreign key and not under
// the relay cert's, so every paired player on the site rejected every Lease
// against its pinned trust anchor (PLY-090) — caused by a write that never named
// them. The guard that shipped before this one blocked only the harmless
// direction (a keyless write clearing the key) and left that one wide open,
// while the doc claimed the parameter was "incidental". A rule enforced by
// nothing is how that returns.
func TestOnlySetSigningKeyEstablishesTheRelayIdentity(t *testing.T) {
	const field = "signingKey"
	const owner = "SetSigningKey"

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	fset := token.NewFileSet()
	assignments := 0
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
				as, ok := n.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for _, lhs := range as.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != field {
						continue
					}
					// s.signingKey on the left of an assignment: an identity write.
					assignments++
					if fn.Name.Name != owner {
						t.Errorf("%s assigns s.%s at %s — only %s may establish this server's Lease-signing identity. A per-screen or per-program write that can move it takes every OTHER screen's Leases with it: they stop verifying against the trust anchor each player pinned (PLY-090), caused by a write that never named them.",
							fn.Name.Name, field, fset.Position(sel.Pos()), owner)
					}
				}
				return true
			})
		}
	}

	// Completeness: if the field were renamed, every check above would match
	// nothing and this test would pass while asserting nothing at all.
	if assignments == 0 {
		t.Errorf("no assignment to s.%s found anywhere in this package, not even in %s — the scan is matching nothing and would pass whatever the package does", field, owner)
	}
}
