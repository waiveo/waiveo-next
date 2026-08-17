package hostmdns

import (
	"testing"

	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
)

func TestNewValidation(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	resolve := func(string) (string, bool) { return "", false }
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"nil Store", Config{Store: nil, NowMillis: now, ResolveMAC: resolve}},
		{"nil NowMillis", Config{Store: store, NowMillis: nil, ResolveMAC: resolve}},
		{"nil ResolveMAC", Config{Store: store, NowMillis: now, ResolveMAC: nil}},
	} {
		if _, err := New(tc.cfg); err == nil {
			t.Errorf("%s: New() error = nil, want error", tc.name)
		}
	}
	if _, err := New(Config{Store: store, NowMillis: now, ResolveMAC: resolve}); err != nil {
		t.Fatalf("valid config: %v", err)
	}
}

// parseAvahi extracts the human name and IPv4 address from real avahi-browse -p
// resolved lines, decoding escapes and dropping the IPv6, unresolved, and
// hardware-alias (`@`) records.
func TestParseAvahi(t *testing.T) {
	out := `+;eth0;IPv4;The\032Hanger;_airplay._tcp;local
=;eth0;IPv4;The\032Hanger;AirPlay Remote Video;local;TheHanger.local;192.168.50.31;7000;"model=Roku"
=;eth0;IPv6;The\032Hanger;AirPlay Remote Video;local;TheHanger.local;fe80::1;7000;"model=Roku"
=;eth0;IPv4;Matt\226\128\153s\032MacBook\032Air;AirPlay Remote Video;local;Air.local;192.168.51.214;7000;""
=;eth0;IPv4;C48B66682125\064The\032Hanger;AirTunes Remote Audio;local;TheHanger.local;192.168.50.31;7000;""

`
	got := parseAvahi(out)
	want := []Service{
		{Name: "The Hanger", Address: "192.168.50.31"},
		{Name: "Matt’s MacBook Air", Address: "192.168.51.214"},
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d services, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("service %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A sweep NAMES a host the neighbour lane already minted: the avahi service
// merges onto the same MAC candidate (one row) and gives it a real name —
// even for a device that is SSDP-silent. A service whose address is not in the
// neighbour table (cross-subnet) is skipped, not minted under a bad identity.
func TestSweepNamesResolvableHosts(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	mac := "c4:8b:66:68:21:25"

	// The neighbour lane already minted this host, unnamed.
	driver, nativeID, match, _ := deviceplane.MACIdentity(mac)
	store.Observe(deviceplane.Observation{
		Match: match, Provenance: deviceplane.ProvenanceDiscovered,
		Driver: driver, NativeID: nativeID, DeviceClass: "unclassified",
		Address: "192.168.50.31",
	}, now())

	l, err := New(Config{
		Store:     store,
		NowMillis: now,
		ResolveMAC: func(ip string) (string, bool) {
			if ip == "192.168.50.31" {
				return mac, true
			}
			return "", false // 192.168.39.9 is cross-subnet
		},
		Browse: func() ([]Service, error) {
			return []Service{
				{Name: "The Hanger", Address: "192.168.50.31"},
				{Name: "Roaming Laptop", Address: "192.168.39.9"},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.sweep()

	cands := store.Report().Body.Candidates
	if len(cands) != 1 {
		t.Fatalf("candidates = %d, want 1 — the avahi name must MERGE, and the cross-subnet service must be skipped: %+v", len(cands), cands)
	}
	c := cands[0]
	if c.NativeID != nativeID {
		t.Errorf("merged onto identity %q, want the MAC candidate %q", c.NativeID, nativeID)
	}
	if c.Name != "The Hanger" {
		t.Errorf("name = %q, want the avahi instance name merged in even though the host is SSDP-silent", c.Name)
	}
	if c.Match.MacOui == "" {
		t.Errorf("merge lost the OUI Match (would thrash against the neighbour sweep): %+v", c.Match)
	}
}

// The Hanger case: a device advertising a human AirPlay name AND a bare Spotify
// UUID AND a hyphenated Cast name with a hex suffix must show the HUMAN name,
// not the UUID a last-writer merge would have left. This is the exact wart the
// box surfaced (The Hanger labelled 013186e4-…).
func TestBestHumanNameWins(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	mac := "c4:8b:66:68:21:25"
	deviceplaneSeed(t, store, mac, now())

	l, err := New(Config{
		Store: store, NowMillis: now,
		ResolveMAC: func(string) (string, bool) { return mac, true },
		Browse: func() ([]Service, error) {
			return []Service{
				{Name: "013186e4-3622-5568-bbe4-df32fa293b59", Address: "192.168.50.31"}, // Spotify UUID: rejected
				{Name: "The-Hanger-89edfc7ba2211b500945eaeb", Address: "192.168.50.31"},  // Cast: suffix stripped -> "The-Hanger"
				{Name: "The Hanger", Address: "192.168.50.31"},                           // AirPlay: the human name, wins
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.sweep()

	cands := store.Report().Body.Candidates
	if len(cands) != 1 {
		t.Fatalf("candidates = %d, want 1 (all three services are one device)", len(cands))
	}
	if cands[0].Name != "The Hanger" {
		t.Fatalf("name = %q, want %q — the human AirPlay name must beat the UUID and the hyphenated variant", cands[0].Name, "The Hanger")
	}
}

func TestCleanName(t *testing.T) {
	cases := map[string]string{
		"The Hanger":                 "The Hanger",
		"Brother MFC-L2730DW series": "Brother MFC-L2730DW series",
		"onn.-4K-Streaming-Bo-89edfc7ba2211b500945eaeb2c0265c9": "onn.-4K-Streaming-Bo",
		"013186e4-3622-5568-bbe4-df32fa293b59":                  "",
		"CA580E3802EAF5C2-00000000B0050473":                     "",
		"HA-Barn":                                               "HA-Barn",
		"NAS":                                                   "NAS",
	}
	for in, want := range cases {
		if got := cleanName(in); got != want {
			t.Errorf("cleanName(%q) = %q, want %q", in, got, want)
		}
	}
}

func deviceplaneSeed(t *testing.T, store *deviceplane.Store, mac string, atMs int64) {
	t.Helper()
	driver, nativeID, match, ok := deviceplane.MACIdentity(mac)
	if !ok {
		t.Fatalf("MACIdentity(%q) rejected a valid MAC", mac)
	}
	store.Observe(deviceplane.Observation{
		Match: match, Provenance: deviceplane.ProvenanceDiscovered,
		Driver: driver, NativeID: nativeID, DeviceClass: "unclassified",
		Address: "192.168.50.31",
	}, atMs)
}

func TestBrowseErrorIsNonFatal(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	l, err := New(Config{
		Store: store, NowMillis: func() int64 { return 1 },
		ResolveMAC: func(string) (string, bool) { return "x", true },
		Browse:     func() ([]Service, error) { return nil, errBrowse },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.sweep() // must not panic
	if n := len(store.Report().Body.Candidates); n != 0 {
		t.Fatalf("a failed browse minted %d candidates, want 0", n)
	}
}

var errBrowse = errorString("browse failed")

type errorString string

func (e errorString) Error() string { return string(e) }
