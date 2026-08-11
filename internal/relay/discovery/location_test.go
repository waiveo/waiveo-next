package discovery

import (
	"context"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
)

// location_test.go pins the one fact an SSDP exchange carries that this lane
// used to throw away: WHERE the responder is.
//
// A USN identifies a device and nothing derives an address from it, so a
// candidate observed without its LOCATION is a device the relay can list, adopt
// and resolve commands for — and can never actually send one to. That was the
// shape of the gap: discovery worked, adoption worked, and control had no
// address to use, so it fell back to an env var nobody sets.

// locationWatch is the Roku watch the relay declares, matching main's own.
func locationWatch() Watch {
	return Watch{
		Match:       deviceplane.Match{SSDP: "roku:ecp"},
		Driver:      "roku-ecp",
		DeviceClass: "media-player",
		Entities:    []deviceplane.CandidateEntity{{Key: "main", DeviceClass: "media-player"}},
	}
}

func newLocationDiscoverer(t *testing.T, store *deviceplane.Store) *Discoverer {
	t.Helper()
	d, err := New(Config{
		Watches:   []Watch{locationWatch()},
		Store:     store,
		NowMillis: func() int64 { return 1000 },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

// TestSearchResponseLocationBecomesTheCandidateAddress: a sweep hit carries the
// responder's LOCATION through to the candidate's relay-local address, which is
// what AddressFor later hands the adoption gate.
func TestSearchResponseLocationBecomesTheCandidateAddress(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	d := newLocationDiscoverer(t, store)
	d.search = func(st string, waitSec int) ([]foundService, error) {
		return []foundService{{
			ST:       st,
			USN:      "uuid:roku:ecp:AA11",
			Location: "http://192.168.50.31:8060/",
		}}, nil
	}
	d.newMonitor = func(onAlive func(aliveNotice)) ssdpMonitor { return noopMonitor{} }

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	d.sweep(ctx)

	addr, ok := store.AddressFor("roku-ecp", "uuid:roku:ecp:AA11")
	if !ok || addr != "192.168.50.31:8060" {
		t.Fatalf("AddressFor after a sweep = (%q, %v), want the response LOCATION normalized to a dialable host:port", addr, ok)
	}
}

// TestAliveNotifyLocationBecomesTheCandidateAddress: the alive-monitor lane
// carries LOCATION too. It matters more than the sweep does for freshness — a
// Roku that reboots onto a new DHCP lease announces itself long before the next
// 60-second sweep would find it.
func TestAliveNotifyLocationBecomesTheCandidateAddress(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	d := newLocationDiscoverer(t, store)

	d.observeAlive(context.Background(), aliveNotice{NT: "roku:ecp", USN: "uuid:roku:ecp:AA11", Location: "http://192.168.50.77:8060/"})

	addr, ok := store.AddressFor("roku-ecp", "uuid:roku:ecp:AA11")
	if !ok || addr != "192.168.50.77:8060" {
		t.Fatalf("AddressFor after an alive NOTIFY = (%q, %v), want the NOTIFY LOCATION normalized to a dialable host:port", addr, ok)
	}
}

// TestResponseWithoutLocationStillObserves: a responder that sends no LOCATION
// is still a real device the operator must be able to see and adopt. It simply
// has no address yet, and the gate refuses to drive it until some lane supplies
// one — never a guess.
func TestResponseWithoutLocationStillObserves(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	d := newLocationDiscoverer(t, store)

	d.observeAlive(context.Background(), aliveNotice{NT: "roku:ecp", USN: "uuid:roku:ecp:AA11", Location: ""})

	if got := len(store.Report().Body.Candidates); got != 1 {
		t.Fatalf("store holds %d candidate(s) after a LOCATION-less NOTIFY, want 1", got)
	}
	if addr, ok := store.AddressFor("roku-ecp", "uuid:roku:ecp:AA11"); ok {
		t.Fatalf("AddressFor = (%q, true) for a LOCATION-less sighting, want not ok", addr)
	}
}

// noopMonitor satisfies ssdpMonitor without touching multicast.
type noopMonitor struct{}

func (noopMonitor) Start() error { return nil }
func (noopMonitor) Close() error { return nil }
