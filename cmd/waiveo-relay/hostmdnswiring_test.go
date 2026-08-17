package main

import (
	"go/ast"
	"testing"
)

// main must construct the host-avahi mDNS lane, or every mDNS-advertising host
// stays an anonymous MAC — including a Roku that is SSDP-silent but still
// announcing AirPlay, the exact device the lane exists to name. The lane's own
// tests all pass and the relay builds without it, so pinning the construction
// keeps the naming wired.
func TestMainConstructsTheHostMDNSLane(t *testing.T) {
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
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "hostmdns" && sel.Sel.Name == "New" {
			found++
		}
		return true
	})

	if found != 1 {
		t.Fatalf("func main calls hostmdns.New %d time(s), want exactly 1 — without it discovered hosts never get their mDNS names, the SSDP-silent-Roku case included", found)
	}
}
