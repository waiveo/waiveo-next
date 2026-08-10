package discovery

import (
	"context"
	"net"
	"sync"
	"testing"

	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
)

// identify_test.go drives the two properties that make a discovered candidate
// actionable rather than merely counted: it carries an ADDRESS the relay can
// dial, and it carries what the device said about itself when probed there.
//
// These are behavioural tests against the real Store, not assertions about
// intermediate values: the question is what a device.candidates report ends up
// containing, because that is what the app peer lists and an operator adopts.

const identifyST = "roku:ecp"

// rokuWatch is the deployment's real Roku watch shape, including the declared
// ECP control port the NOTIFY fallback needs.
func rokuWatch(t *testing.T) Watch {
	t.Helper()
	w := watchFor(mustMatch(t, `{"ssdp":"`+identifyST+`"}`))
	w.DefaultPort = 8060
	return w
}

// TestSweptCandidateCarriesTheLocationAddress is the regression this lane's
// whole rework exists for: before it, the LOCATION header was read off the wire
// and discarded, so every candidate reached the app peer with no address and
// nothing could ever be commanded.
func TestSweptCandidateCarriesTheLocationAddress(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	d, err := New(Config{
		Watches:   []Watch{rokuWatch(t)},
		Store:     store,
		NowMillis: func() int64 { return 1000 },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.search = func(st string, _ int) ([]foundService, error) {
		return []foundService{{ST: st, USN: "uuid:roku:ecp:X0051LOBBY1", Location: "http://192.168.50.31:8060/"}}, nil
	}

	d.sweep(context.Background())

	cands := store.Report().Body.Candidates
	if len(cands) != 1 {
		t.Fatalf("candidates = %d, want 1", len(cands))
	}
	if got, want := cands[0].Address, "192.168.50.31:8060"; got != want {
		t.Errorf("candidate address = %q, want %q — a candidate with no address can be adopted and never commanded", got, want)
	}
}

// TestAliveNotifyFallsBackToThePacketSource proves the NOTIFY lane still yields
// a dialable address when the device's own LOCATION is unusable — the case a
// LOCATION-only implementation drops silently.
func TestAliveNotifyFallsBackToThePacketSource(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	d, err := New(Config{
		Watches:   []Watch{rokuWatch(t)},
		Store:     store,
		NowMillis: func() int64 { return 1000 },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	d.observeAlive(context.Background(), aliveNotice{
		NT:  identifyST,
		USN: "uuid:roku:ecp:X0051BREAK1",
		// Garbage the device published: parseable as a URL, dialable as nothing.
		Location: "http:///no-host-here",
		From:     &net.UDPAddr{IP: net.ParseIP("192.168.50.44"), Port: 51515},
	})

	cands := store.Report().Body.Candidates
	if len(cands) != 1 {
		t.Fatalf("candidates = %d, want 1", len(cands))
	}
	if got, want := cands[0].Address, "192.168.50.44:8060"; got != want {
		t.Errorf("candidate address = %q, want %q (the sender's IP plus the watch's declared control port)", got, want)
	}
}

// TestIdentifyEnrichesTheCandidate drives the classification half: the probe
// answers, and its name/model/serial reach the reported candidate.
func TestIdentifyEnrichesTheCandidate(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	var probed []string
	var mu sync.Mutex

	d, err := New(Config{
		Watches:   []Watch{rokuWatch(t)},
		Store:     store,
		NowMillis: func() int64 { return 1000 },
		Identify: func(_ context.Context, address string) (Identity, bool) {
			mu.Lock()
			probed = append(probed, address)
			mu.Unlock()
			return Identity{Name: "Lobby TV", Model: "Roku Ultra", Serial: "X0051LOBBY1"}, true
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.search = func(st string, _ int) ([]foundService, error) {
		return []foundService{{ST: st, USN: "uuid:roku:ecp:X0051LOBBY1", Location: "http://192.168.50.31:8060/"}}, nil
	}

	d.sweep(context.Background())

	mu.Lock()
	gotProbes := append([]string(nil), probed...)
	mu.Unlock()
	if len(gotProbes) != 1 || gotProbes[0] != "192.168.50.31:8060" {
		t.Fatalf("probed = %v, want exactly [192.168.50.31:8060] — the probe must go to the discovered address", gotProbes)
	}

	c := store.Report().Body.Candidates[0]
	if c.Name != "Lobby TV" || c.Model != "Roku Ultra" || c.Serial != "X0051LOBBY1" {
		t.Errorf("candidate = {name %q, model %q, serial %q}, want the probed identity — without it an operator sees only a USN",
			c.Name, c.Model, c.Serial)
	}
}

// TestIdentifyIsCachedPerDevice pins the cadence rule: a device is probed on
// first sight and NOT again on the next sweep. Without it, every sweep would
// put an HTTP request on every device on the LAN forever to re-learn three
// fields that do not move.
func TestIdentifyIsCachedPerDevice(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	var probes int
	var mu sync.Mutex

	// A clock that advances a second per read — enough to prove the cache is
	// not merely a same-instant coincidence, far short of identifyTTL.
	tick := int64(0)
	d, err := New(Config{
		Watches: []Watch{rokuWatch(t)},
		Store:   store,
		NowMillis: func() int64 {
			tick += 1000
			return tick
		},
		Identify: func(context.Context, string) (Identity, bool) {
			mu.Lock()
			probes++
			mu.Unlock()
			return Identity{Name: "Lobby TV"}, true
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.search = func(st string, _ int) ([]foundService, error) {
		return []foundService{{ST: st, USN: "uuid:roku:ecp:X0051LOBBY1", Location: "http://192.168.50.31:8060/"}}, nil
	}

	d.sweep(context.Background())
	d.sweep(context.Background())
	d.sweep(context.Background())

	mu.Lock()
	got := probes
	mu.Unlock()
	if got != 1 {
		t.Errorf("probes = %d over three sweeps, want 1 (cached for identifyTTL)", got)
	}
}

// TestIdentifyReprobesWhenTheAddressChanges is the other half of the cache
// rule: a device that moved (DHCP) must be re-probed, because serving the old
// address's facts under a new one would misidentify the device an operator is
// about to adopt.
func TestIdentifyReprobesWhenTheAddressChanges(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	var probed []string
	var mu sync.Mutex

	d, err := New(Config{
		Watches:   []Watch{rokuWatch(t)},
		Store:     store,
		NowMillis: func() int64 { return 1000 },
		Identify: func(_ context.Context, address string) (Identity, bool) {
			mu.Lock()
			probed = append(probed, address)
			mu.Unlock()
			return Identity{Name: "Lobby TV", Serial: "X0051LOBBY1"}, true
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	location := "http://192.168.50.31:8060/"
	d.search = func(st string, _ int) ([]foundService, error) {
		return []foundService{{ST: st, USN: "uuid:roku:ecp:X0051LOBBY1", Location: location}}, nil
	}
	d.sweep(context.Background())
	location = "http://192.168.50.99:8060/"
	d.sweep(context.Background())

	mu.Lock()
	got := append([]string(nil), probed...)
	mu.Unlock()
	want := []string{"192.168.50.31:8060", "192.168.50.99:8060"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("probed = %v, want %v — a moved device must be re-probed, not served from the old address's cache", got, want)
	}
	if addr := store.Report().Body.Candidates[0].Address; addr != "192.168.50.99:8060" {
		t.Errorf("candidate address = %q, want the new %q", addr, "192.168.50.99:8060")
	}
}

// TestFailedIdentifyKeepsWhatWasAlreadyKnown is the durability rule behind
// deviceplane.Store.Observe's orKeep: a probe that stops answering — a device
// mid-reboot — must not blank the address and identity a working sighting
// already established, because that address is what an adopted device's
// commands are dispatched to.
func TestFailedIdentifyKeepsWhatWasAlreadyKnown(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	answering := true

	// identifyRetryAfter is 2 minutes, so the clock has to move past it for the
	// second sweep to re-probe at all — otherwise this would test the cache
	// rather than the failure.
	nowMs := int64(0)
	d, err := New(Config{
		Watches:   []Watch{rokuWatch(t)},
		Store:     store,
		NowMillis: func() int64 { return nowMs },
		Identify: func(context.Context, string) (Identity, bool) {
			if !answering {
				return Identity{}, false
			}
			return Identity{Name: "Lobby TV", Model: "Roku Ultra", Serial: "X0051LOBBY1"}, true
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	location := "http://192.168.50.31:8060/"
	d.search = func(st string, _ int) ([]foundService, error) {
		return []foundService{{ST: st, USN: "uuid:roku:ecp:X0051LOBBY1", Location: location}}, nil
	}
	d.sweep(context.Background())

	// The device reboots: it still answers SSDP (its LOCATION is still
	// published) but its ECP surface is down, and a moment later it stops
	// publishing a usable LOCATION at all.
	answering = false
	nowMs = identifyTTL.Milliseconds() * 2
	location = "http:///gone"
	d.sweep(context.Background())

	c := store.Report().Body.Candidates[0]
	if c.Address != "192.168.50.31:8060" {
		t.Errorf("address = %q, want the previously learned %q retained — one thin sighting must not delete a working address", c.Address, "192.168.50.31:8060")
	}
	if c.Name != "Lobby TV" || c.Model != "Roku Ultra" || c.Serial != "X0051LOBBY1" {
		t.Errorf("identity = {%q, %q, %q}, want the previously learned values retained", c.Name, c.Model, c.Serial)
	}
}

// TestNoIdentifierStillYieldsAnAddressableCandidate: the probe is optional, and
// a deployment that wires none must still discover reachable devices — just
// less informative ones.
func TestNoIdentifierStillYieldsAnAddressableCandidate(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	d, err := New(Config{
		Watches:   []Watch{rokuWatch(t)},
		Store:     store,
		NowMillis: func() int64 { return 1000 },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.search = func(st string, _ int) ([]foundService, error) {
		return []foundService{{ST: st, USN: "uuid:roku:ecp:X0051LOBBY1", Location: "http://192.168.50.31:8060/"}}, nil
	}

	d.sweep(context.Background())

	c := store.Report().Body.Candidates[0]
	if c.Address != "192.168.50.31:8060" {
		t.Errorf("address = %q, want it present with no Identify wired", c.Address)
	}
	if c.Model != "" || c.Serial != "" {
		t.Errorf("model/serial = %q/%q, want both empty — nothing probed, so nothing may be claimed", c.Model, c.Serial)
	}
}
