package main

import (
	"go/ast"
	"testing"

	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
	"github.com/maaxton/waiveo-next/internal/relay/ecp"
)

// namerankwiring_test.go pins the ONE mapping only this binary can make: the ECP
// probe knows which element of a Roku's device-info document named the device,
// the candidate store knows how to rank a name's source, and neither imports the
// other on purpose. If this mapping is wrong or missing, the store's merge still
// runs and simply ranks every ECP name identically — enforcement with nothing
// authoring it, which is this subsystem's recurring way of failing quietly.

func TestECPNameRankMapsTheDocumentElementToTheMergeRank(t *testing.T) {
	cases := map[ecp.NameSource]deviceplane.NameRank{
		// A name out of the device's own configuration, by either element. The
		// two are one fact — 192.168.50.31 answers BOTH with "The Hanger" — and
		// pickField has already preferred `user-device-name` between them, so
		// the merge never sees them compete.
		ecp.NameSourceUser:     deviceplane.NameRankFriendly,
		ecp.NameSourceFriendly: deviceplane.NameRankFriendly,
		// A factory string ("onn•Roku TV - X029009JC6LF"). It ranks BELOW a
		// friendly mDNS name deliberately: a box nobody has renamed should show
		// what its AirPlay record announces, not its product label.
		ecp.NameSourceDefault: deviceplane.NameRankModel,
		// A probe that found no name contributes no opinion.
		ecp.NameSourceNone: deviceplane.NameRankNone,
	}
	for src, want := range cases {
		if got := ecpNameRank(src); got != want {
			t.Errorf("ecpNameRank(%d) = %d, want %d", src, got, want)
		}
	}
	// The factory default must lose to a friendly mDNS instance name, which is
	// the whole reason the ECP lane cannot be ranked as one lane.
	if ecpNameRank(ecp.NameSourceDefault) >= deviceplane.NameRankFriendly {
		t.Errorf("a factory default-device-name ranks at or above a friendly mDNS name — an un-renamed Roku would show its product label forever")
	}
}

// THE REFRESHABILITY CONSTRAINT, at the seam where it was broken.
//
// This relay PROBES ECP only inside an operator's `discovery.scan`:
// discovery.Discoverer.Run is passive-only by owner decision, and identityOf's
// passive branch replays a cached answer with no TTL check. Host-mDNS, by
// contrast, re-reads the whole avahi cache every 30 seconds. keepName refuses a
// worse rank and remembers the held one until the process exits, so any ECP rank
// ABOVE what host-mDNS can mint is a rank nothing can restate: the first scan
// pins the name and a later rename can never land.
//
// That is exactly what the first version of this mapping did — `user-device-name`
// mapped to a top rank above every mDNS source, and 192.168.50.31 answers
// `<user-device-name>The Hanger</user-device-name>`, so The Hanger's name would
// have frozen at the first scan. The fix is this inequality, not a comment.
func TestNoECPNameOutranksWhatTheSweepingLaneCanRestate(t *testing.T) {
	// The best rank host-mDNS mints for a friendly service type. Named here
	// rather than reached into hostmdns, which keeps its table unexported.
	const sweepable = deviceplane.NameRankFriendly

	for _, src := range []ecp.NameSource{
		ecp.NameSourceNone, ecp.NameSourceDefault, ecp.NameSourceFriendly, ecp.NameSourceUser,
	} {
		if got := ecpNameRank(src); got > sweepable {
			t.Fatalf("ecpNameRank(%d) = %d, above the best rank a 30-second sweep can produce (%d) — "+
				"only an operator-initiated scan mints this rank, so nothing would ever restate it and "+
				"a renamed device could never be corrected", src, got, sweepable)
		}
	}
}

// main must actually PUT the rank on the Identity it builds. The probe, the
// mapping and the merge can all be individually correct while the value never
// leaves the composition root.
func TestMainCarriesTheNameRankOutOfTheIdentifyProbe(t *testing.T) {
	mainFn := parseRelayMainFunc(t)

	found := false
	ast.Inspect(mainFn, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Identity" {
			return true
		}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != "NameRank" {
				continue
			}
			call, ok := kv.Value.(*ast.CallExpr)
			if !ok {
				continue
			}
			if fn, ok := call.Fun.(*ast.Ident); ok && fn.Name == "ecpNameRank" {
				found = true
			}
		}
		return true
	})

	if !found {
		t.Fatalf("func main builds no discovery.Identity whose NameRank comes from ecpNameRank — the probe's knowledge of WHICH element named the device is being thrown away again, and the candidate store cannot refuse a factory label that arrives after an owner-set name")
	}
}
