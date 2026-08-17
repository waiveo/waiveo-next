package main

import (
	"go/ast"
	"testing"
)

// main must actually CONSTRUCT the neighbour lane, or the enumerate-all
// foundation ships dead: the lane's own package tests all pass, the relay
// builds, and the box still lists only the one pattern-named device — exactly
// the wired-but-never-started shape this repo has shipped before. The guard is
// the construction (neighbor.New); its Run is started in the same block, and a
// constructed-but-never-run lane would fail the package's behaviour tests the
// moment anything asserted a candidate, so pinning the construction is enough
// to keep the wiring honest without pinning the goroutine's exact spelling.
func TestMainConstructsTheNeighbourLane(t *testing.T) {
	mainFn := parseRelayMainFunc(t)

	found := 0
	ast.Inspect(mainFn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if ok && pkg.Name == "neighbor" && sel.Sel.Name == "New" {
			found++
		}
		return true
	})

	if found != 1 {
		t.Fatalf("func main calls neighbor.New %d time(s), want exactly 1 — without it every host the kernel already knows stays invisible and discovery lists only pattern-named devices, the defect enumerate-all exists to fix", found)
	}
}
