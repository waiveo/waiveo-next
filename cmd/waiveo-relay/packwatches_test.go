package main

import (
	"encoding/json"
	"go/ast"
	"testing"

	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
	"github.com/maaxton/waiveo-next/internal/relay/discovery"
	"github.com/maaxton/waiveo-next/internal/relay/mdns"
	"github.com/maaxton/waiveo-next/internal/shared/deviceid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// testSSDPTarget is a stand-in search target for exercising the watch
// machinery. Deliberately NOT a real vendor's: core supplies no discovery
// pattern, so a real one appearing in this binary — even in a fixture — would
// blur the property coreisdeviceblind_test.go guards.
const testSSDPTarget = "urn:example-org:device:thing:1"

// packwatches_test.go covers the REL-064 seam — pack-declared patterns
// becoming live lane watches. The lanes' own swap behavior is covered in
// their packages; here the subject is the DERIVATION (manifest shape → lane
// watches), the PRECEDENCE (builtin facts never lost to a colliding pack
// declaration), and the WIRING (main must actually drive it, which an AST
// guard pins the same way the device-plane sync's does).

func rawPatterns(t *testing.T, patterns ...string) []json.RawMessage {
	t.Helper()
	out := make([]json.RawMessage, len(patterns))
	for i, p := range patterns {
		out[i] = json.RawMessage(p)
	}
	return out
}

func TestPatternWatchSetsRoutesEachFormToItsLane(t *testing.T) {
	ssdpW, mdnsW, macOui, malformed := patternWatchSets(rawPatterns(t,
		`{"deviceClass":"media-player","match":[{"ssdp":"urn:acme:device:tv:1"},{"mdns":"_acme._tcp"}]}`,
		`{"deviceClass":"printer","match":[{"macOui":"AABBCC"}]}`,
	))

	if len(ssdpW) != 1 || ssdpW[0].Match.SSDP != "urn:acme:device:tv:1" {
		t.Fatalf("ssdp watches = %+v, want the one declared search target", ssdpW)
	}
	if ssdpW[0].DeviceClass != "media-player" || ssdpW[0].Driver != patternSSDPDriver {
		t.Errorf("ssdp watch carries class %q driver %q, want the pattern's class under the lane driver", ssdpW[0].DeviceClass, ssdpW[0].Driver)
	}
	if len(ssdpW[0].Entities) != 1 || ssdpW[0].Entities[0].Key != mainEntityKey || ssdpW[0].Entities[0].DeviceClass != "media-player" {
		t.Errorf("ssdp watch entities = %+v, want one main entity of the pattern's class", ssdpW[0].Entities)
	}
	if len(mdnsW) != 1 || mdnsW[0].Match.MDNS != "_acme._tcp" || mdnsW[0].Driver != mdnsDriver {
		t.Fatalf("mdns watches = %+v, want the one declared service type under the mdns driver", mdnsW)
	}
	// macOui is a VALID declaration no lane can deliver yet (gap G2): it must
	// be counted, never silently absorbed and never turned into a watch.
	if macOui != 1 {
		t.Errorf("macOui = %d, want the declared-but-undeliverable pattern counted", macOui)
	}
	if malformed != 0 {
		t.Errorf("malformed = %d, want 0", malformed)
	}
}

func TestPatternWatchSetsCountsMalformedInsteadOfDying(t *testing.T) {
	ssdpW, mdnsW, _, malformed := patternWatchSets(rawPatterns(t,
		`not json at all`,
		`{"match":[{"ssdp":"urn:no-class:1"}]}`,
		`{"deviceClass":"tv","match":[{"ssdp":"urn:ok:1"},{"ssdp":"","mdns":"_two._tcp"}]}`,
	))
	// The section is signed and hash-checked, so malformed entries are
	// producer bugs — but ONE bad pattern must never cost the good ones.
	if len(ssdpW) != 1 || ssdpW[0].Match.SSDP != "urn:ok:1" {
		t.Fatalf("ssdp watches = %+v, want exactly the well-formed one", ssdpW)
	}
	if len(mdnsW) != 0 {
		t.Fatalf("mdns watches = %+v, want none", mdnsW)
	}
	if malformed != 3 {
		t.Errorf("malformed = %d, want 3 (bad json, missing class, two-form match)", malformed)
	}
}

func TestBuiltinWatchesWinCollisions(t *testing.T) {
	builtin := []discovery.Watch{{
		Match:       deviceplane.Match{SSDP: testSSDPTarget},
		Driver:      rokuDriver,
		DeviceClass: mediaPlayerClass,
		DefaultPort: rokuECPPort,
	}}
	derived, _, _, _ := patternWatchSets(rawPatterns(t,
		`{"deviceClass":"media-player","match":[{"ssdp":"`+testSSDPTarget+`"},{"ssdp":"urn:other:1"}]}`,
	))

	merged := mergeSSDPWatches(builtin, derived)
	if len(merged) != 2 {
		t.Fatalf("merged = %d watches, want builtin + the non-colliding derived one", len(merged))
	}
	// The colliding target keeps the BUILTIN's declaration-side facts: losing
	// the control driver and default port to a pack declaration would make
	// installing a pack DEGRADE discovery of the very devices it declares.
	for _, w := range merged {
		if w.Match.SSDP == testSSDPTarget && (w.Driver != rokuDriver || w.DefaultPort != rokuECPPort) {
			t.Fatalf("collision lost the builtin facts: %+v", w)
		}
	}
}

func TestMDNSMergeFoldsCase(t *testing.T) {
	builtin := []mdns.Watch{{Match: deviceplane.Match{MDNS: "_Waiveo._tcp"}, Driver: mdnsDriver, DeviceClass: mediaPlayerClass}}
	_, derived, _, _ := patternWatchSets(rawPatterns(t,
		`{"deviceClass":"tv","match":[{"mdns":"_waiveo._TCP"}]}`,
	))
	if merged := mergeMDNSWatches(builtin, derived); len(merged) != 1 {
		t.Fatalf("merged = %d watches; two case-spellings of one service type are ONE watch (RFC 1035 §2.3.3)", len(merged))
	}
}

// The applier drives REAL lanes, asserted at the far side (each lane's live
// watch count), including the property that makes pack REMOVAL work: a later
// apply carrying no patterns returns each lane to exactly its builtin set.
func TestDiscoveryWatchApplierInstallsAndForgets(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	disc, err := discovery.New(discovery.Config{Store: store, NowMillis: now})
	if err != nil {
		t.Fatalf("discovery.New: %v", err)
	}
	mdnsL, err := mdns.New(mdns.Config{Store: store, NowMillis: now})
	if err != nil {
		t.Fatalf("mdns.New: %v", err)
	}
	builtinSSDP := []discovery.Watch{{Match: deviceplane.Match{SSDP: testSSDPTarget}, Driver: rokuDriver, DeviceClass: mediaPlayerClass}}

	apply := discoveryWatchApplier(disc, mdnsL, store, builtinSSDP, nil, nil)

	apply(wire.DeviceInventory{PackMatchPatterns: rawPatterns(t,
		`{"deviceClass":"tv","match":[{"ssdp":"urn:acme:tv:1"},{"mdns":"_acme._tcp"}]}`,
	)})
	if got := disc.WatchCount(); got != 2 {
		t.Fatalf("ssdp lane has %d watch(es) after apply, want builtin + pack", got)
	}
	if got := mdnsL.WatchCount(); got != 1 {
		t.Fatalf("mdns lane has %d watch(es) after apply, want the pack's", got)
	}

	// The pack is uninstalled: the next generation carries no patterns, and
	// the lanes must return to the builtin set — a leaked watch would keep a
	// removed pack's discovery running forever.
	apply(wire.DeviceInventory{})
	if got := disc.WatchCount(); got != 1 {
		t.Fatalf("ssdp lane has %d watch(es) after the pack left, want only the builtin", got)
	}
	if got := mdnsL.WatchCount(); got != 0 {
		t.Fatalf("mdns lane has %d watch(es) after the pack left, want none", got)
	}

	// Nil lanes (discovery off) must not panic: the applier still runs for
	// its log line.
	discoveryWatchApplier(nil, nil, nil, nil, nil, nil)(wire.DeviceInventory{})
}

// main must WIRE the applier, or all of the above is a tested join nothing
// drives — the exact shape that shipped pack patterns as a counted-but-inert
// section in the first place. Three things are load-bearing: the constructor
// (with the lanes, both builtin sets, and the engine-state reporter), the boot
// apply, and the connect-time RESEND.
//
// The resend is asserted here and nowhere else, for the reason this file already
// exists: REL-070 suppresses re-applying an unchanged generation, so a relay
// whose packs have not changed reports its engine state exactly once per process
// and never again. Drop `engineState.resend` from the connect hook and every
// other test still passes — the frame is still sent, the registry still fills,
// the console still renders — while a reconnecting relay silently leaves its app
// peer with no engine state at all. That is a join nothing else drives.
func TestMainWiresTheDiscoveryWatchApplier(t *testing.T) {
	mainFn := parseRelayMainFunc(t)

	wantArgs := []string{"disc", "mdnsListener", "candStore", "builtinSSDP", "builtinMDNS", "engineState"}
	constructed := 0
	bootApplies := 0
	resends := 0
	ast.Inspect(mainFn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "discoveryWatchApplier" {
			constructed++
			if len(call.Args) != len(wantArgs) {
				t.Errorf("discoveryWatchApplier called with %d args, want %d", len(call.Args), len(wantArgs))
				return true
			}
			for i, want := range wantArgs {
				if got := renderExpr(call.Args[i]); got != want {
					t.Errorf("discoveryWatchApplier arg %d = %s, want %s", i, got, want)
				}
			}
		}
		if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "applyDiscoveryWatches" {
			bootApplies++
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "resend" {
			if recv, ok := sel.X.(*ast.Ident); ok && recv.Name == "engineState" {
				resends++
			}
		}
		return true
	})

	if constructed != 1 {
		t.Fatalf("func main constructs discoveryWatchApplier %d time(s), want exactly 1 — unconstructed, every pack-declared pattern stays a counted-but-inert section (REL-064), which is exactly how this shipped before the join existed", constructed)
	}
	if resends != 1 {
		t.Errorf("func main calls engineState.resend %d time(s), want exactly 1 — on connect (REL-117). Without it a relay whose generation has not changed never re-states its watch set, and a reconnecting app peer holds no engine state for a relay that has been watching correctly the whole time", resends)
	}
	if bootApplies < 2 {
		t.Fatalf("func main calls applyDiscoveryWatches %d time(s), want >=2: once at boot (the boot generation's log line) and once inside the rePuller applyInventory hook (live applies) — missing either leaves half the lifecycle unwired", bootApplies)
	}
}

// AN APPLY MUST RETIRE WHAT THE PREVIOUS GENERATION DECLARED, not only install
// what this one does.
//
// Installing the new watch set is the visible half. The other half has no
// sighting behind it: a removed pack's watch stops observing, which is
// byte-for-byte what a device going quiet looks like, so no rule reading
// sightings can distinguish them — while the neighbour and host-mDNS lanes keep
// re-observing the same host with nothing to say about its fan-out. Nothing
// expires a candidate and the relay's store lives as long as the process, so
// without this the removed pack's entities stay REPORTED and stay
// COMMAND-RESOLVABLE until the next restart.
func TestAnApplyRetiresTheFanOutOfAWatchItNoLongerDeclares(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	store.SetSite(dcSite)
	now := func() int64 { return 1000 }
	disc, err := discovery.New(discovery.Config{Store: store, NowMillis: now})
	if err != nil {
		t.Fatalf("discovery.New: %v", err)
	}
	apply := discoveryWatchApplier(disc, nil, store, nil, nil, nil)

	// A pack declares an SSDP watch with a fan-out, and the lane observes a
	// device through it.
	apply(wire.DeviceInventory{PackMatchPatterns: rawPatterns(t,
		`{"deviceClass":"media-player","match":[{"ssdp":"urn:acme:tv:1"}]}`,
	)})
	w := discovery.Watch{
		Match:       deviceplane.Match{SSDP: "urn:acme:tv:1"},
		Driver:      rokuDriver,
		DeviceClass: mediaPlayerClass,
		Entities:    []deviceplane.CandidateEntity{{Key: "main", DeviceClass: mediaPlayerClass}},
	}
	store.Observe(deviceplane.Observation{
		Match: w.Match, Provenance: deviceplane.ProvenanceDiscovered,
		Driver: w.Driver, NativeID: "uuid:acme:tv:1", DeviceClass: w.DeviceClass,
		Address: "192.168.50.31:8060", Entities: w.Entities, EntitySource: w.Match.Key(),
	}, 1000)

	entityID := deviceid.Entity(dcSite, rokuDriver, "uuid:acme:tv:1", "main")
	if _, _, ok := store.ResolveEntity(entityID); !ok {
		t.Fatalf("ResolveEntity(%q) failed while the pack is installed — the fixture is wrong before the apply is exercised", entityID)
	}

	// Re-applying the SAME generation must not disturb anything: applies are
	// idempotent and happen on every desired-state refresh.
	apply(wire.DeviceInventory{PackMatchPatterns: rawPatterns(t,
		`{"deviceClass":"media-player","match":[{"ssdp":"urn:acme:tv:1"}]}`,
	)})
	if _, _, ok := store.ResolveEntity(entityID); !ok {
		t.Fatalf("ResolveEntity(%q) failed after re-applying the SAME generation — an idempotent apply has retired a live declaration", entityID)
	}

	// The pack is uninstalled.
	apply(wire.DeviceInventory{})
	if _, _, ok := store.ResolveEntity(entityID); ok {
		t.Fatalf("ResolveEntity(%q) still resolves after the declaring pack was removed — the relay would execute an inbound command against a device no installed pack claims", entityID)
	}
	if ents := store.Report().Body.Candidates[0].Entities; len(ents) != 0 {
		t.Fatalf("candidate still reports %+v — a removed pack's entity fan-out must stop being reported, not linger until the relay restarts", ents)
	}
}
