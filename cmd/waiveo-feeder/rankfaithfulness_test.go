package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// rankfaithfulness_test.go pins the ONE hop in the rank chain that nothing
// downstream can observe today — and pins it for exactly that reason.
//
// restoreDeviceRegistry rebuilds wire.DeviceCandidate values out of mirrored
// rows and pushes them through registry.ApplyCandidates. It never calls
// ReplaceDiscoveredDevices, so mergeDiscovered does not run on the restore path
// and no assertion about a restored device's name, class or age can tell whether
// the ranks were carried. Both `NameRank` and `ClassRank` are carried there
// anyway, deliberately: a reconstructed candidate that silently reported the
// bottom of a ladder would be a value nothing vouches for wearing the shape of
// one that does — the identical argument that file already makes for NOT
// inventing a `first_seen`.
//
// A hop with no observable consequence is precisely the hop a later refactor
// deletes without a test going red, and "the enforcement half shipped and the
// AUTHORING half did not" is this subsystem's recurring way of failing quietly:
// the class rank itself lived inside one sweep for two releases (#204), and the
// relay's own ECP name rank had the same shape before it. So this is a
// STRUCTURAL assertion by design — it is checking that the assignment exists,
// because behaviour cannot.
func TestTheRestoreRebuildsCandidatesCarryingBothRanks(t *testing.T) {
	fn := parseFeederFunc(t, "candidatemirror.go", "restoreDeviceRegistry")

	carried := map[string]bool{}
	ast.Inspect(fn, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "DeviceCandidate" {
			return true
		}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			// The value must come off the ROW, not be a literal: a hardcoded
			// token would be the exact defect this test exists to catch, wearing
			// the shape of the fix.
			val, ok := kv.Value.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			if key.Name == "NameRank" && val.Sel.Name == "NameRank" {
				carried["NameRank"] = true
			}
			if key.Name == "ClassRank" && val.Sel.Name == "ClassRank" {
				carried["ClassRank"] = true
			}
		}
		return true
	})

	for _, member := range []string{"NameRank", "ClassRank"} {
		if !carried[member] {
			t.Errorf("restoreDeviceRegistry rebuilds a wire.DeviceCandidate without carrying %s off the mirrored row — "+
				"nothing downstream reads it today, which is why nothing else would go red, and a restored candidate that "+
				"reports a rank the mirror never held is a claim no relay made", member)
		}
	}
}

// ...and the LIVE projection carries both too. rowsFor is the hop with a
// behavioural test (TestTheReportedClassSourceOutlivesTheRelayThatReportedIt
// goes red without it); this asserts the same shape in the same place so the
// pair is read together rather than one being noticed and the other not.
func TestRowsForCarriesBothRanksOntoTheMirrorRow(t *testing.T) {
	fn := parseFeederFunc(t, "candidatemirror.go", "rowsFor")

	carried := map[string]bool{}
	ast.Inspect(fn, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "DiscoveredDevice" {
			return true
		}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			if val, ok := kv.Value.(*ast.SelectorExpr); ok && key.Name == val.Sel.Name {
				carried[key.Name] = true
			}
		}
		return true
	})

	for _, member := range []string{"NameRank", "ClassRank"} {
		if !carried[member] {
			t.Errorf("rowsFor builds a store.DiscoveredDevice without carrying %s off the reported candidate — "+
				"the durable merge then has nothing to rank with and is back to taking whichever report arrived last", member)
		}
	}
}

// parseFeederFunc returns the named top-level func decl from one of this
// binary's own source files, so a wiring assertion reads the code the binary is
// built from rather than a copy of it.
func parseFeederFunc(t *testing.T, file, name string) *ast.FuncDecl {
	t.Helper()
	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, decl := range parsed.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name {
			return fn
		}
	}
	t.Fatalf("%s declares no func %s — this test can no longer see the hop it exists to guard", file, name)
	return nil
}
