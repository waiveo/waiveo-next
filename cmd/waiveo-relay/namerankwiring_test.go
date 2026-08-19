package main

import (
	"context"
	"errors"
	"go/ast"
	"testing"

	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
	"github.com/maaxton/waiveo-next/internal/relay/ecp"
	"github.com/maaxton/waiveo-next/internal/relay/hostmdns"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
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

// THE PROJECTION ONTO THE WIRE (REL-110c). The ladder is an ordered int in relay
// memory and a TOKEN on the wire, and this function is the only translation
// between them — so a rank the relay ranks correctly and spells as nothing is a
// name the app peer's durable mirror cannot defend.
//
// Every declared rank must reach a DISTINCT token, and the order must survive
// the trip. The distinctness half is the one that catches the likely mistake: a
// rank added to the ladder without a case here falls to the default arm and is
// silently reported as the weakest source, which looks like nothing at all going
// wrong.
func TestEveryDeclaredNameRankReachesItsOwnWireToken(t *testing.T) {
	ranks := []deviceplane.NameRank{
		deviceplane.NameRankNone, deviceplane.NameRankMachine,
		deviceplane.NameRankModel, deviceplane.NameRankFriendly,
	}
	seen := map[string]deviceplane.NameRank{}
	for _, r := range ranks {
		tok := nameRankToken(r)
		if tok == "" {
			t.Fatalf("nameRankToken(%d) is empty — an ABSENT name_rank means 'this relay does not rank names' (REL-110c), so a relay that DOES rank them must never report absence", r)
		}
		if prev, dup := seen[tok]; dup {
			t.Fatalf("nameRankToken maps both %d and %d to %q — one of them is silently reported as a different source than it is", prev, r, tok)
		}
		seen[tok] = r
	}

	// A rank inserted into the MIDDLE of the ladder would keep every case above
	// compiling and would fall through to the default arm. deviceplane's own AST
	// test guards the ladder's TOP; this guards its length, which is what this
	// switch has to be total over.
	if got := int(deviceplane.NameRankFriendly); got != len(ranks)-1 {
		t.Fatalf("the NameRank ladder now has %d entries and this projection knows %d — a rank was added without a REL-110c token, and it is being reported as the weakest source on the wire",
			got+1, len(ranks))
	}
}

// ...and the report must actually CARRY it. The projection can be perfect while
// toWireCandidates never calls it, which is the same class of gap
// TestMainCarriesTheNameRankOutOfTheIdentifyProbe exists for one hop earlier.
func TestToWireCandidatesCarriesTheNameRank(t *testing.T) {
	driver, nativeID, match, ok := deviceplane.MACIdentity("48:5c:2c:31:6e:6e")
	if !ok {
		t.Fatal("MACIdentity refused the onn box's MAC")
	}
	store := deviceplane.NewStore("relay-1")
	store.Observe(deviceplane.Observation{
		Match:       match,
		Provenance:  deviceplane.ProvenanceDiscovered,
		Driver:      driver,
		NativeID:    nativeID,
		DeviceClass: "media-player",
		Name:        "onn. 4K Streaming Box",
		NameRank:    deviceplane.NameRankFriendly,
	}, 1000)

	got := toWireCandidates(store.Report().Body.Candidates)
	if len(got) != 1 {
		t.Fatalf("projected %d candidates, want 1", len(got))
	}
	if got[0].NameRank != wire.CandidateNameRankFriendly {
		t.Fatalf("name_rank = %q, want %q — the rank never reaches the app peer, so its mirror is back to ranking names by whether they are non-empty",
			got[0].NameRank, wire.CandidateNameRankFriendly)
	}
}

// THE TWO FIXES MEET AT THIS SEAM (#203 x #202), and neither one's own tests can
// see it.
//
// #203 decides WHICH avahi record the host-mDNS lane is allowed to read, and the
// record it was wrongly discarding is precisely the Friendly-ranked one. #202
// carries whatever rank that lane produced across the wire so a durable mirror
// can act on it. Fix either alone and the box stays wrong: without #203 the lane
// never sees the Friendly record, so the best rank it can honestly report is
// `machine` and the mirror correctly refuses nothing; without #202 the lane's
// Friendly rank dies with the relay process.
//
// The chain is proven in three links, and this is the middle one — the real lane
// into the real candidate store into the real wire projection:
//
//	the box .12 avahi dump -> parseAvahi -> the lane's ranked pick
//	  (internal/relay/hostmdns: the sweep test built on the IPv6-transaction
//	   record, which is #203's own proof)
//	    -> HERE: Lane -> deviceplane.Store -> toWireCandidates
//	      -> the app peer's intake, durable merge and restart
//	        (cmd/waiveo-feeder, TestTheReportedNameSourceOutlivesTheRelayThatReportedIt)
//
// The lane is driven through its own public seam — Config.Browse plus a Run
// whose context is already cancelled, which is exactly one sweep — rather than
// through a test-only entry point, so what runs here is what the binary runs.
func TestTheLanesRankedNameReachesTheWireAsAFriendlyToken(t *testing.T) {
	const mac = "48:5c:2c:31:6e:6e"
	store := deviceplane.NewStore("relay-1")
	lane, err := hostmdns.New(hostmdns.Config{
		Store:     store,
		NowMillis: func() int64 { return 1000 },
		ResolveMAC: func(ip string) (string, bool) {
			if ip == "192.168.50.63" {
				return mac, true
			}
			// A v6 address must never even be offered to the neighbour table:
			// the device plane is IPv4 and the lookup could only miss. #203's
			// parse test is what keeps one from getting this far.
			t.Errorf("the lane asked the neighbour table to resolve %q", ip)
			return "", false
		},
		// What parseAvahi yields from the box .12 dump: the same device seen
		// through both of its records, the Friendly one (which the transaction
		// filter used to throw away) and the Machine-ranked Cast truncation.
		Browse: func() ([]hostmdns.Service, error) {
			return []hostmdns.Service{
				{Name: "onn. 4K Streaming Box", Type: "_androidtvremote2._tcp", Address: "192.168.50.63"},
				{Name: "onn.-4K-Streaming-Bo-89edfc7ba2211b500945eaeb2c0265c9", Type: "_googlecast._tcp", Address: "192.168.50.63"},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("hostmdns.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := lane.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled — this test relies on Run sweeping once before it observes the cancellation", err)
	}

	got := toWireCandidates(store.Report().Body.Candidates)
	if len(got) != 1 {
		t.Fatalf("the report carries %d candidates, want 1 — one host's several services must merge onto one candidate", len(got))
	}
	if got[0].Name != "onn. 4K Streaming Box" {
		t.Fatalf("name on the wire = %q, want %q — the lane picked the wrong record of the two", got[0].Name, "onn. 4K Streaming Box")
	}
	if got[0].NameRank != wire.CandidateNameRankFriendly {
		t.Fatalf("name_rank on the wire = %q, want %q — without it the app peer cannot keep this name across the relay restart that is coming",
			got[0].NameRank, wire.CandidateNameRankFriendly)
	}
}
