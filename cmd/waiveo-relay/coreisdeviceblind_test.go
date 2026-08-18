package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// coreisdeviceblind_test.go is a wiring guard for the property the Roku
// extraction exists to create: this binary must not know what any device KIND
// is. Discovery enumerates; extensions recognise (owner, 2026-08-17 — "the Roku
// should supply that pattern").
//
// It is written against the SOURCE rather than against behaviour because the
// failure it guards is silent and additive: someone adds one convenient builtin
// watch "just for Roku", every test still passes, and the pattern seam quietly
// becomes decorative again — a deployment without the extension keeps working,
// so nothing ever reveals that extensions are not really supplying anything.

// TestCoreSuppliesNoSSDPDiscoveryPattern: the builtin SSDP watch set must be
// constructed empty. A vendor search target compiled in here is knowledge that
// belongs in an extension's manifest.
func TestCoreSuppliesNoSSDPDiscoveryPattern(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		id, ok := as.Lhs[0].(*ast.Ident)
		if !ok || id.Name != "builtinSSDP" {
			return true
		}
		found = true
		lit, ok := as.Rhs[0].(*ast.CompositeLit)
		if !ok {
			t.Errorf("builtinSSDP is not a composite literal; this guard cannot read it — keep it a literal so its emptiness stays checkable")
			return false
		}
		if len(lit.Elts) != 0 {
			t.Errorf("builtinSSDP declares %d watch(es). Core must supply NO discovery pattern: recognising a device kind is an extension's contribution, and a builtin makes the pattern seam decorative — a deployment without the extension would still classify the device, so nothing would ever reveal the seam was unused.", len(lit.Elts))
		}
		return false
	})
	if !found {
		t.Fatal("no builtinSSDP assignment found in main.go — if the seam was renamed, re-point this guard rather than deleting it")
	}
}

// TestNoVendorSearchTargetIsCompiledIn: the same property stated as an absence,
// so reintroducing the constant is caught even if it is wired somewhere new.
func TestNoVendorSearchTargetIsCompiledIn(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		// "roku:ecp" is the search target a Roku answers. Its presence as a
		// literal in the relay is the exact regression this guards.
		if strings.Contains(strings.ToLower(lit.Value), "roku:ecp") {
			t.Errorf("main.go compiles in the search target %s — a vendor's discovery pattern belongs in that vendor's extension manifest (MAN-071), not in the relay", lit.Value)
		}
		return true
	})
}
