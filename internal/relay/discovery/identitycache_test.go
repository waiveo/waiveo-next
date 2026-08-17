package discovery

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
)

// identitycache_test.go bounds the one collection on this lane that had no
// bound. Its key is a USN, which comes off unauthenticated multicast, and a
// miss costs an outbound HTTP probe as well as a map entry — so "unbounded"
// here means both unbounded memory and an unbounded request rate against
// whatever the packets name, driven by anyone on the LAN.

// countingIdentifier is an Identify that answers every probe and counts them.
type countingIdentifier struct {
	mu sync.Mutex
	n  int
}

func (c *countingIdentifier) identify(context.Context, string) (Identity, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return Identity{Name: "Lobby TV"}, true
}

func (c *countingIdentifier) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// newFloodDiscoverer builds a Discoverer whose clock the test drives.
func newFloodDiscoverer(t *testing.T, ids *countingIdentifier, nowMs *int64) *Discoverer {
	t.Helper()
	d, err := New(Config{
		Watches:   []Watch{rokuWatch(t)},
		Store:     deviceplane.NewStore("relay-1"),
		NowMillis: func() int64 { return *nowMs },
		Identify:  ids.identify,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

// TestIdentityCacheIsBoundedAgainstAUSNFlood: one LAN host emitting a fresh USN
// per packet must not be able to grow the cache or the probe rate without
// limit. Both halves are asserted, because gating only the cache write would
// leave every packet still drawing an HTTP request — the half that reaches the
// network.
func TestIdentityCacheIsBoundedAgainstAUSNFlood(t *testing.T) {
	ids := &countingIdentifier{}
	nowMs := int64(1000)
	d := newFloodDiscoverer(t, ids, &nowMs)

	const flood = maxCachedIdentities * 3
	for i := 0; i < flood; i++ {
		d.identityOf(context.Background(), fmt.Sprintf("uuid:spoof:%d", i), "192.168.50.31:8060", true)
	}

	d.idMu.Lock()
	held := len(d.identities)
	d.idMu.Unlock()
	if held > maxCachedIdentities {
		t.Errorf("cache holds %d entries after %d spoofed USNs, want at most %d", held, flood, maxCachedIdentities)
	}
	if probes := ids.count(); probes > maxCachedIdentities {
		t.Errorf("%d outbound probes for %d spoofed USNs, want at most %d — a refused entry must also be a refused request",
			probes, flood, maxCachedIdentities)
	}
}

// TestAFloodDoesNotEvictTheRealDevicesAlreadyFound is why the cap refuses the
// newcomer instead of evicting the oldest, matching deviceplane.Store.Observe:
// evicting would let a flood push the actual TVs out of the cache and then
// re-probe them on every sighting, which is worse than the growth the cap
// exists to stop.
func TestAFloodDoesNotEvictTheRealDevicesAlreadyFound(t *testing.T) {
	ids := &countingIdentifier{}
	nowMs := int64(1000)
	d := newFloodDiscoverer(t, ids, &nowMs)

	const realUSN = "uuid:roku:ecp:X0051LOBBY1"
	d.identityOf(context.Background(), realUSN, "192.168.50.31:8060", true)
	for i := 0; i < maxCachedIdentities*2; i++ {
		d.identityOf(context.Background(), fmt.Sprintf("uuid:spoof:%d", i), "192.168.50.31:8060", true)
	}

	d.idMu.Lock()
	_, stillCached := d.identities[realUSN]
	d.idMu.Unlock()
	if !stillCached {
		t.Fatal("the real device was evicted by a USN flood; the cap must refuse the newcomer, not displace the incumbent")
	}

	// And it is still served from cache rather than re-probed.
	before := ids.count()
	d.identityOf(context.Background(), realUSN, "192.168.50.31:8060", true)
	if after := ids.count(); after != before {
		t.Errorf("probes went %d -> %d for a device that is still cached", before, after)
	}
}

// TestAFullCacheRecoversAsEntriesAgeOut: the cap must not be a one-way door. An
// entry past its own freshness window would be re-probed on its next sighting
// anyway, so dropping it discards nothing — and it is what lets a relay that
// once saw a flood admit a genuinely new device afterwards.
func TestAFullCacheRecoversAsEntriesAgeOut(t *testing.T) {
	ids := &countingIdentifier{}
	nowMs := int64(1000)
	d := newFloodDiscoverer(t, ids, &nowMs)

	for i := 0; i < maxCachedIdentities; i++ {
		d.identityOf(context.Background(), fmt.Sprintf("uuid:spoof:%d", i), "192.168.50.31:8060", true)
	}
	// A new device arriving while the cache is full is refused, not probed.
	before := ids.count()
	if id := d.identityOf(context.Background(), "uuid:roku:ecp:NEWTV", "192.168.50.77:8060", true); id.Name != "" {
		t.Fatalf("a new USN was identified with the cache full: %+v", id)
	}
	if after := ids.count(); after != before {
		t.Fatalf("probes went %d -> %d with the cache full, want no probe", before, after)
	}

	// The flood ages out; the same device is now admitted.
	nowMs += identifyTTL.Milliseconds() + 1
	if id := d.identityOf(context.Background(), "uuid:roku:ecp:NEWTV", "192.168.50.77:8060", true); id.Name != "Lobby TV" {
		t.Fatalf("identity after the flood aged out = %+v, want the probed name", id)
	}
}
