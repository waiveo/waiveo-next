package neighbor

import (
	"context"
	"testing"

	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
)

func TestNewValidation(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"nil Store", Config{Store: nil, NowMillis: now}},
		{"nil NowMillis", Config{Store: store, NowMillis: nil}},
	} {
		if _, err := New(tc.cfg); err == nil {
			t.Errorf("%s: New() error = nil, want error", tc.name)
		}
	}
	if _, err := New(Config{Store: store, NowMillis: now}); err != nil {
		t.Fatalf("valid config: %v", err)
	}
}

// parseNeighbours pulls the (ip, mac) pairs out of real `ip neigh show` output,
// skipping any line the kernel could not resolve (no lladdr).
func TestParseNeighbours(t *testing.T) {
	out := `192.168.50.113 dev eth0 lladdr bc:24:11:04:7a:60 REACHABLE
192.168.51.147 dev eth0 lladdr 6c:1f:f7:a6:e4:b7 STALE
192.168.50.99 dev eth0  INCOMPLETE
192.168.50.1 dev eth0 lladdr d8:b3:70:11:a2:f0 router REACHABLE
fe80::1 dev eth0 lladdr aa:bb:cc:dd:ee:ff STALE

`
	got := parseNeighbours(out)
	want := []Entry{
		{"192.168.50.113", "bc:24:11:04:7a:60"},
		{"192.168.51.147", "6c:1f:f7:a6:e4:b7"},
		{"192.168.50.1", "d8:b3:70:11:a2:f0"},
		{"fe80::1", "aa:bb:cc:dd:ee:ff"},
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A sweep turns the neighbour table into unclassified MAC-keyed candidates —
// asserted at the far side, the store's report. The link-local neighbour
// (fe80::) is filtered (non-LAN address), so the store never carries noise the
// candidate report would have to blank the address of.
func TestSweepMintsUnclassifiedHosts(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	l, err := New(Config{
		Store:     store,
		NowMillis: func() int64 { return 1000 },
		Read: func() ([]Entry, error) {
			return []Entry{
				{"192.168.50.113", "BC:24:11:04:7A:60"}, // upper-case in; lower-cased out
				{"192.168.50.1", "d8:b3:70:11:a2:f0"},
				{"fe80::1", "aa:bb:cc:dd:ee:ff"}, // link-local: filtered
				{"192.168.50.9", "not-a-mac"},    // no valid OUI: skipped
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.sweep()

	cands := store.Report().Body.Candidates
	if len(cands) != 2 {
		t.Fatalf("minted %d candidates, want 2 (link-local + bad-MAC filtered): %+v", len(cands), cands)
	}
	byID := map[string]deviceplane.Candidate{}
	for _, c := range cands {
		byID[c.NativeID] = c
	}
	c, ok := byID["bc:24:11:04:7a:60"]
	if !ok {
		t.Fatalf("no candidate keyed by the lower-cased MAC: %+v", cands)
	}
	if c.Driver != Driver {
		t.Errorf("driver = %q, want %q (the L2 identity namespace)", c.Driver, Driver)
	}
	if c.DeviceClass != ClassUnclassified {
		t.Errorf("class = %q, want %q", c.DeviceClass, ClassUnclassified)
	}
	if c.Address != "192.168.50.113" {
		t.Errorf("address = %q, want the neighbour's IP", c.Address)
	}
	if c.Match.MacOui != "bc2411" {
		t.Errorf("match OUI = %q, want the vendor prefix so a MAN-071 macOui pattern can claim it", c.Match.MacOui)
	}
	// The candidate must be reportable: a Match that will not marshal is dropped
	// at report time, so a minted candidate that survives Report() proves it.
}

// A read error is non-fatal — the table is transient host state, and the lane
// must keep trying. Run returns only on ctx cancel, never on a read failure.
func TestReadErrorIsNonFatal(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	l, err := New(Config{
		Store:     store,
		NowMillis: func() int64 { return 1000 },
		Read:      func() ([]Entry, error) { return nil, context.DeadlineExceeded },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.sweep() // must not panic
	if n := len(store.Report().Body.Candidates); n != 0 {
		t.Fatalf("a failed read minted %d candidates, want 0", n)
	}
}
