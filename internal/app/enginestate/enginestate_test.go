package enginestate

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// fixedClock returns a stamping clock a test can assert an exact instant
// against, rather than a window.
func fixedClock(ms int64) func() int64 { return func() int64 { return ms } }

const fixedNow = int64(1_755_000_000_000)

func TestNeverReportedIsAbsentNotZeroed(t *testing.T) {
	// The distinction the whole frame exists for. Zeroes mean "this relay is
	// watching for nothing", which is a real alarm worth waking someone for;
	// a relay that has simply not spoken must not raise it.
	r := New(fixedClock(fixedNow))
	if got := r.States(); len(got) != 0 {
		t.Fatalf("a registry nobody has reported to must be empty, got %+v", got)
	}

	r.ApplyDiscoveryEngineState("relay-a", wire.DiscoveryEngineStateBody{
		SSDPLane: true, MDNSLane: true,
	})
	got := r.States()
	if len(got) != 1 {
		t.Fatalf("want 1 state, got %d", len(got))
	}
	if !got[0].WatchingNothing {
		t.Error("a relay reporting zero watches in both lanes IS watching nothing — the alarm must fire once it has actually said so")
	}
}

func TestWatchingNothingIsFalseWhenEitherLaneHoldsAWatch(t *testing.T) {
	// Either lane alone is enough to be discovering; the judgement is an AND of
	// emptiness, not of one lane.
	for _, tc := range []struct {
		name       string
		ssdp, mdns int
		want       bool
	}{
		{"neither lane", 0, 0, true},
		{"ssdp only", 3, 0, false},
		{"mdns only", 0, 2, false},
		{"both", 3, 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := New(fixedClock(fixedNow))
			r.ApplyDiscoveryEngineState("relay-a", wire.DiscoveryEngineStateBody{
				SSDPWatches: tc.ssdp, MDNSWatches: tc.mdns,
			})
			if got := r.States()[0].WatchingNothing; got != tc.want {
				t.Errorf("WatchingNothing = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReportIsFiledUnderTheRelayAndReplacesWholesale(t *testing.T) {
	r := New(fixedClock(fixedNow))
	r.ApplyDiscoveryEngineState("relay-a", wire.DiscoveryEngineStateBody{
		SSDPWatches: 7, PackPatterns: 4, Malformed: 1,
	})
	// A second report REPLACES rather than merges: the relay is the authority on
	// its own current watch set, and a merge would let a count from a retired
	// generation survive into the view of a current one.
	r.ApplyDiscoveryEngineState("relay-a", wire.DiscoveryEngineStateBody{
		SSDPWatches: 2, PackPatterns: 1,
	})
	got := r.States()
	if len(got) != 1 {
		t.Fatalf("a second report from one relay must replace, not append: got %d", len(got))
	}
	if got[0].SSDPWatches != 2 || got[0].PackPatterns != 1 {
		t.Errorf("latest report must win, got %+v", got[0])
	}
	if got[0].Malformed != 0 {
		t.Error("a member absent from the newer report must not survive from the older one")
	}
}

func TestEmptyRelayIDIsIgnored(t *testing.T) {
	// The caller passes the AUTHENTICATED connection identity, so an empty one is
	// a wiring defect. Filing it under "" would put an engine state on the
	// console attributable to no relay.
	r := New(fixedClock(fixedNow))
	r.ApplyDiscoveryEngineState("", wire.DiscoveryEngineStateBody{SSDPWatches: 9})
	if got := r.States(); len(got) != 0 {
		t.Fatalf("an empty relay id must be ignored, got %+v", got)
	}
}

func TestReceiptIsStampedFromTheAppClock(t *testing.T) {
	// Stamped by the receiver, never carried on the wire: the relay's clock is
	// not trusted for app-side ordering, and "how long ago did this box hear
	// this" is a property of this box.
	r := New(fixedClock(fixedNow))
	r.ApplyDiscoveryEngineState("relay-a", wire.DiscoveryEngineStateBody{})
	if got := r.States()[0].ReportedAtMs; got != fixedNow {
		t.Errorf("ReportedAtMs = %d, want the app clock's %d", got, fixedNow)
	}
}

func TestStatesAreOrderedByRelayID(t *testing.T) {
	// A stable order, so a console does not reshuffle its rows between polls.
	r := New(fixedClock(fixedNow))
	for _, id := range []string{"relay-c", "relay-a", "relay-b"} {
		r.ApplyDiscoveryEngineState(id, wire.DiscoveryEngineStateBody{})
	}
	got := r.States()
	want := []string{"relay-a", "relay-b", "relay-c"}
	for i := range want {
		if got[i].RelayID != want[i] {
			t.Fatalf("order = %v..., want %v", got[i].RelayID, want[i])
		}
	}
}

func TestNilRegistryIsInert(t *testing.T) {
	// A deployment that wires no clock gets no registry, and every caller must
	// survive that rather than panicking on a nil map.
	var r *Registry
	r.ApplyDiscoveryEngineState("relay-a", wire.DiscoveryEngineStateBody{})
	if got := r.States(); got != nil {
		t.Errorf("a nil registry must report nothing, got %+v", got)
	}
	if New(nil) != nil {
		t.Error("New(nil) must not hand back a registry with no clock to stamp with")
	}
}
