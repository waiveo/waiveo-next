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
		{Name: "The Hanger", Type: "AirPlay Remote Video", Address: "192.168.50.31"},
		{Name: "Matt’s MacBook Air", Type: "AirPlay Remote Video", Address: "192.168.51.214"},
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

// THE TRANSACTION IS NOT THE ADDRESS FAMILY. Every line below is copied verbatim
// out of one `avahi-browse -a -t -r -p -k` dump on box .12, and the dump carries
// BOTH mismatches at once — which is the whole reason the old
// `fields[2] != "IPv4"` test could not be repaired by loosening it or by
// deleting it.
//
// The two arms must be read together. Keeping the first line while still
// dropping the third is the only pass: a filter that admits everything passes
// arm 1 and fails arm 2, and the shipped filter fails arm 1 and passes arm 2.
func TestParseAvahiFiltersOnTheResolvedAddressNotTheTransaction(t *testing.T) {
	// Line 1: the Friendly-ranked record that names the onn box, resolved to a
	// good IPv4 address and cached on the IPv6 transaction. Discarded 16 dumps
	// out of 16 by the transaction test, which is why the durable mirror could
	// only ever be offered the Machine-ranked truncation on line 2.
	// Line 2: the same device's `_googlecast` record on the IPv4 transaction —
	// the worse name, which was the ONLY one that got through.
	// Line 3: the same `_googlecast` service on the IPv6 transaction, resolved to
	// a genuine link-local v6 address. This one must STILL be refused: the device
	// plane is IPv4, and ResolveMAC keys the neighbour table by a dotted quad.
	// Line 4: a v6 address on the IPv4 transaction — the mirror-image record the
	// old test LET THROUGH, handing `fe80::…` to ResolveMAC as though it were a
	// v4 host.
	out := `=;eth0;IPv6;onn\.\0324K\032Streaming\032Box;_androidtvremote2._tcp;local;Android_ba8ec90d10bc4ea0b514ad9ddb2b3e86.local;192.168.50.63;6466;"bt=48:5C:2C:31:6E:6F"
=;eth0;IPv4;onn\.-4K-Streaming-Bo-89edfc7ba2211b500945eaeb2c0265c9;_googlecast._tcp;local;89edfc7b-a221-1b50-0945-eaeb2c0265c9.local;192.168.50.63;8009;"fn=onn. 4K Streaming Box"
=;eth0;IPv6;onn\.-4K-Streaming-Bo-89edfc7ba2211b500945eaeb2c0265c9;_googlecast._tcp;local;89edfc7b-a221-1b50-0945-eaeb2c0265c9.local;fe80::225f:9d9b:8178:b8e3;8009;"fn=onn. 4K Streaming Box"
=;eth0;IPv4;4AB0E26A2FDE09DE-000000000001B669;_matter._tcp;local;681DEF40246F.local;fe80::e350:1ce0:2528:f37e;33969;"T=2"
`
	got := parseAvahi(out)
	want := []Service{
		{Name: "onn. 4K Streaming Box", Type: "_androidtvremote2._tcp", Address: "192.168.50.63"},
		{Name: "onn.-4K-Streaming-Bo-89edfc7ba2211b500945eaeb2c0265c9", Type: "_googlecast._tcp", Address: "192.168.50.63"},
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %d services, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("service %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	for _, s := range got {
		if !isIPv4(s.Address) {
			t.Errorf("service %+v carries a non-IPv4 address — ResolveMAC can only look up a dotted quad, and the device plane is IPv4 by contract", s)
		}
	}
}

// The lane's whole job is the NAME, so the filter is proven at the seam an
// operator actually reads: the same four lines through the whole sweep must put
// the Friendly display name on the candidate, not the Cast truncation.
//
// This is the test that fails on a revert of the parse fix with the exact
// hardware symptom in the message, rather than with a parser count.
func TestTheSweepReportsTheFriendlyNameEvenWhenItsRecordIsCachedOnTheIPv6Transaction(t *testing.T) {
	const mac = "48:5c:2c:31:6e:6e"
	store := deviceplane.NewStore("relay-1")
	lane, err := New(Config{
		Store:     store,
		NowMillis: func() int64 { return 1000 },
		ResolveMAC: func(ip string) (string, bool) {
			if ip == "192.168.50.63" {
				return mac, true
			}
			return "", false
		},
		Browse: func() ([]Service, error) {
			// EXACTLY these two lines, and deliberately no third. The friendly
			// record exists ONLY on the v6 transaction here, which is what makes
			// this test fail when the transaction filter comes back — the sweep is
			// then left with the `_googlecast` truncation and nothing else. Adding
			// the same service's v4 twin (avahi often caches both, and
			// TestTheSameServiceCachedOnBothTransactionsMergesOnce uses it) would
			// hand the old filter a friendly record of its own and quietly turn
			// this into a test that passes on a revert.
			return parseAvahi(`=;eth0;IPv6;onn\.\0324K\032Streaming\032Box;_androidtvremote2._tcp;local;Android_ba8ec90d10bc4ea0b514ad9ddb2b3e86.local;192.168.50.63;6466;"bt=48:5C:2C:31:6E:6F"
=;eth0;IPv4;onn\.-4K-Streaming-Bo-89edfc7ba2211b500945eaeb2c0265c9;_googlecast._tcp;local;89edfc7b-a221-1b50-0945-eaeb2c0265c9.local;192.168.50.63;8009;"fn=onn. 4K Streaming Box"
`), nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lane.sweep()

	cands := store.Report().Body.Candidates
	if len(cands) != 1 {
		t.Fatalf("reported %d candidates, want 1: %+v", len(cands), cands)
	}
	if cands[0].Name != "onn. 4K Streaming Box" {
		t.Fatalf("name = %q, want %q — the record carrying the display name resolved to 192.168.50.63 but was cached on the IPv6 transaction, and dropping it leaves the console showing the Cast truncation",
			cands[0].Name, "onn. 4K Streaming Box")
	}
	if cands[0].NameRank != deviceplane.NameRankFriendly {
		t.Fatalf("name_rank = %d, want NameRankFriendly (%d) — the name must arrive with the rank of the record that authored it, or the durable mirror has nothing to refuse the truncation with",
			cands[0].NameRank, deviceplane.NameRankFriendly)
	}
}

// Admitting both transactions means avahi's cache can hand the sweep the SAME
// service twice — same instance name, same type, same resolved address. The
// parse doc claims that is harmless; this is the test that makes the claim
// falsifiable, because nothing else in this file feeds the sweep a real
// duplicate.
//
// All three lines are verbatim from one dump: the onn box's
// `_androidtvremote2._tcp` cached on BOTH transactions, plus its `_googlecast`
// record. The duplicate must not mint a second candidate and must not change
// which name or class wins.
func TestTheSameServiceCachedOnBothTransactionsMergesOnce(t *testing.T) {
	const mac = "48:5c:2c:31:6e:6e"
	dump := `=;eth0;IPv6;onn\.\0324K\032Streaming\032Box;_androidtvremote2._tcp;local;Android_ba8ec90d10bc4ea0b514ad9ddb2b3e86.local;192.168.50.63;6466;"bt=48:5C:2C:31:6E:6F"
=;eth0;IPv4;onn\.-4K-Streaming-Bo-89edfc7ba2211b500945eaeb2c0265c9;_googlecast._tcp;local;89edfc7b-a221-1b50-0945-eaeb2c0265c9.local;192.168.50.63;8009;"fn=onn. 4K Streaming Box"
=;eth0;IPv4;onn\.\0324K\032Streaming\032Box;_androidtvremote2._tcp;local;Android_ba8ec90d10bc4ea0b514ad9ddb2b3e86.local;192.168.50.63;6466;"bt=48:5C:2C:31:6E:6F"
`
	// The duplicate must actually reach the sweep, or the rest proves nothing.
	svcs := parseAvahi(dump)
	if len(svcs) != 3 || svcs[0] != svcs[2] {
		t.Fatalf("fixture no longer carries the same service on both transactions: %+v", svcs)
	}

	store := deviceplane.NewStore("relay-1")
	lane, err := New(Config{
		Store:      store,
		NowMillis:  func() int64 { return 1000 },
		ResolveMAC: func(string) (string, bool) { return mac, true },
		Browse:     func() ([]Service, error) { return svcs, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lane.sweep()

	cands := store.Report().Body.Candidates
	if len(cands) != 1 {
		t.Fatalf("reported %d candidates, want 1 — a service cached on both transactions is one device: %+v", len(cands), cands)
	}
	if cands[0].Name != "onn. 4K Streaming Box" {
		t.Errorf("name = %q, want %q — the duplicate must not change which name wins", cands[0].Name, "onn. 4K Streaming Box")
	}
	if cands[0].DeviceClass != "media-player" {
		t.Errorf("class = %q, want media-player — the duplicate must not change which class wins", cands[0].DeviceClass)
	}
}

// sweepClass runs one sweep over the given verbatim avahi lines, with every
// address resolving to one MAC, and returns the class reported for it.
func sweepClass(t *testing.T, mac, dump string) string {
	t.Helper()
	store := deviceplane.NewStore("relay-1")
	lane, err := New(Config{
		Store:      store,
		NowMillis:  func() int64 { return 1000 },
		ResolveMAC: func(string) (string, bool) { return mac, true },
		Browse:     func() ([]Service, error) { return parseAvahi(dump), nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lane.sweep()
	cands := store.Report().Body.Candidates
	if len(cands) != 1 {
		t.Fatalf("reported %d candidates, want 1: %+v", len(cands), cands)
	}
	return cands[0].DeviceClass
}

// THE CLASS PICK MUST NOT DEPEND ON THE ORDER AVAHI LISTED THE RECORDS IN.
//
// Both lines are verbatim from one `avahi-browse -a -t -r -p -k` on box .12, and
// they are one device: 192.168.50.43, MAC 84:28:59:9f:2f:08 (a Google OUI), a
// speaker that is also a Matter node. `_matter` says smart-home, and
// `_spotify-connect` says media-player.
//
// This is the case #203 created. While the lane filtered on the avahi
// TRANSACTION the `_matter` line was discarded — it is cached on the v6
// transaction — so the only class signal that ever reached the sweep was
// `_spotify-connect`, and the durable mirror on the box holds media-player for
// this device today. Admitting the record on its RESOLVED address is correct and
// necessary for the name, but it hands classFor a competing signal, and the
// pick it fed was first-parsed-wins between two equally-ranked specific classes.
// `_matter` is listed first in every dump measured, so the device silently
// became smart-home — and device_class governs the command vocabulary (REG-052),
// so it is not a cosmetic field.
//
// The reversed arm is the actual invariant: a sweep must be a FUNCTION of the
// cache it read. Run the same two records the other way round and the answer
// must not move.
func TestTheClassPickDoesNotDependOnTheOrderTheRecordsWereListedIn(t *testing.T) {
	const mac = "84:28:59:9f:2f:08"
	const matter = `=;eth0;IPv6;D731507D2F318A3E-0598B3A2BC178981;_matter._tcp;local;0EE8BA5E1B55.local;192.168.50.43;5541;"T=6"`
	const spotify = `=;eth0;IPv4;SpotifyConnect;_spotify-connect._tcp;local;linux.local;192.168.50.43;4070;"CPath=/spotifyConnect"`

	for _, tc := range []struct{ name, dump string }{
		{"as the cache listed them (_matter first)", matter + "\n" + spotify + "\n"},
		{"reversed", spotify + "\n" + matter + "\n"},
	} {
		if got := sweepClass(t, mac, tc.dump); got != "media-player" {
			t.Errorf("%s: class = %q, want media-player — a Matter fabric membership must not take a speaker's class away from its `_spotify-connect` service, and the answer must not depend on which record avahi listed first",
				tc.name, got)
		}
	}
}

// The other half of the ladder, and the reason the tie above is not simply
// broken in favour of media-player: a device's OWN product service outranks a
// media feature bolted onto it.
//
// All four lines are verbatim from the same dump, for one device — the ecobee
// thermostat at 192.168.39.241. It advertises `_airplay` and `_spotify-connect`
// (media-player) and `_ecobee` and `_hap` (smart-home), and in the dump the
// media records are listed FIRST. It is a thermostat; ranking media above
// smart-home outright, or breaking the tie alphabetically, would classify it as
// a media player in dump order.
//
// (This device is on another subnet, so the live lane skips it at ResolveMAC —
// the deferred cross-subnet case in the package doc. The records are real, the
// rule they pin is the one that decides every device, and the test supplies the
// MAC the cross-subnet slice will one day supply.)
func TestADevicesOwnProductServiceOutranksAMediaFeatureBoltedOntoIt(t *testing.T) {
	const mac = "02:34:65:d0:ce:d4"
	dump := `=;eth0;IPv4;Upstairs;_airplay._tcp;local;ecobee-ares.local;192.168.39.241;7000;"model=ECB601"
=;eth0;IPv4;531615707641;_spotify-connect._tcp;local;531615707641.local;192.168.39.241;60597;"CPath=/zc/0"
=;eth0;IPv4;ecobee-ares;_ecobee._tcp;local;ecobee-ares.local;192.168.39.241;1201;"pv=1.1"
=;eth0;IPv4;Upstairs;_hap._tcp;local;ecobee-ares.local;192.168.39.241;46577;"md=ECB601"
`
	if got := sweepClass(t, mac, dump); got != "smart-home" {
		t.Errorf("class = %q, want smart-home — `_ecobee` is the thermostat's own product service and must outrank the `_airplay`/`_spotify-connect` features it also carries, whichever avahi listed first", got)
	}
}

// A class signal that is the ONLY one a device gives still classifies it. The
// authority ladder decides ties; it must not turn a lone feature signal into no
// answer. Both lines are verbatim: one Matter device (192.168.50.48) whose only
// service is `_matter._tcp`, cached on BOTH transactions.
func TestALoneFeatureSignalStillClassifies(t *testing.T) {
	dump := `=;eth0;IPv6;CA580E3802EAF5C2-00000000B0050473;_matter._tcp;local;503DD1E30675.local;192.168.50.48;5540;"T=1"
=;eth0;IPv4;CA580E3802EAF5C2-00000000B0050473;_matter._tcp;local;503DD1E30675.local;192.168.50.48;5540;"T=1"
`
	if got := sweepClass(t, "50:3d:d1:e3:06:75", dump); got != "smart-home" {
		t.Errorf("class = %q, want smart-home", got)
	}
}

// TestUnescapeAvahi pins BOTH escape forms avahi's parseable output uses. The
// `\.`/`\;`/`\X` case is a regression guard: it was dropped once, and reached an
// operator as a stray backslash in a real device name ("onn\. 4K Streaming Box").
func TestUnescapeAvahi(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain", "plain"},
		{`The\032Hanger`, "The Hanger"},                     // \NNN space
		{`Matt\226\128\153s`, "Matt’s"},                     // \NNN UTF-8 (three bytes)
		{`onn\.`, "onn."},                                   // \X — the bug: an escaped dot
		{`onn\. 4K Streaming Box`, "onn. 4K Streaming Box"}, // \X in context
		{`a\\b`, `a\b`},                                     // \\ is just \X with X a backslash
		{`a\;b`, "a;b"},                                     // \X — an escaped field separator
		{`X029009JC6LF`, "X029009JC6LF"},                    // no escapes, untouched
		{`trailing\`, `trailing\`},                          // a lone trailing backslash is kept
		{`\032`, " "},                                       // a bare escape
		{`\.\032\.`, ". ."},                                 // adjacent escapes of both forms
	} {
		if got := unescapeAvahi(tc.in); got != tc.want {
			t.Errorf("unescapeAvahi(%q) = %q, want %q", tc.in, got, tc.want)
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

// A device is classified from the SET of mDNS types it advertises: the Brother
// answers _http and _ipp and _printer, and the specific class (printer) must
// win the generic ones; The Hanger's _airplay makes it a media-player even
// though it is SSDP-silent. Discovery's generic guess, from passive facts.
func TestSweepClassifiesFromServiceTypes(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := func() int64 { return 1000 }
	roku, printer := "c4:8b:66:68:21:25", "ac:50:de:6f:06:a8"
	deviceplaneSeed(t, store, roku, now())
	// Seed the printer host too (neighbour lane would have).
	dp, ni, m, _ := deviceplane.MACIdentity(printer)
	store.Observe(deviceplane.Observation{Match: m, Provenance: deviceplane.ProvenanceDiscovered, Driver: dp, NativeID: ni, DeviceClass: deviceplane.ClassUnclassified, Address: "192.168.50.36"}, now())

	l, _ := New(Config{
		Store: store, NowMillis: now,
		ResolveMAC: func(ip string) (string, bool) {
			switch ip {
			case "192.168.50.31":
				return roku, true
			case "192.168.50.36":
				return printer, true
			}
			return "", false
		},
		Browse: func() ([]Service, error) {
			return []Service{
				{Name: "The Hanger", Type: "_airplay._tcp", Address: "192.168.50.31"},
				{Name: "Brother MFC", Type: "_http._tcp", Address: "192.168.50.36"},
				{Name: "Brother MFC", Type: "_ipp._tcp", Address: "192.168.50.36"},
				{Name: "Brother MFC", Type: "_printer._tcp", Address: "192.168.50.36"},
			}, nil
		},
	})
	l.sweep()

	byMAC := map[string]string{}
	for _, c := range store.Report().Body.Candidates {
		byMAC[c.NativeID] = c.DeviceClass
	}
	if byMAC[ni] != "printer" {
		t.Errorf("printer class = %q, want printer (specific type beats _http)", byMAC[ni])
	}
	rokuNI := "c4:8b:66:68:21:25"
	if byMAC[rokuNI] != "media-player" {
		t.Errorf("Roku class = %q, want media-player from _airplay", byMAC[rokuNI])
	}
}

func TestClassFor(t *testing.T) {
	cases := map[string]string{
		"_airplay._tcp":    "media-player",
		"_googlecast._tcp": "media-player",
		"_printer._tcp":    "printer",
		"_ipp._tcp":        "printer",
		"_smb._tcp":        "storage",
		"_hap._tcp":        "smart-home",
		// Added: definitive signals a device advertises on its own (a Home
		// Assistant hub, an ecobee thermostat, an Android TV) that were falling
		// through to unclassified on the box.
		"_home-assistant._tcp":   "smart-home",
		"_ecobee._tcp":           "smart-home",
		"_androidtvremote2._tcp": "media-player",
		"_http._tcp":             deviceplane.ClassUnclassified,
		"_ssh._tcp":              deviceplane.ClassUnclassified,
		"_nut._tcp":              deviceplane.ClassUnclassified, // a UPS — no generic bucket, stays unclassified
		"":                       deviceplane.ClassUnclassified,
	}
	for in, want := range cases {
		if got := classFor(in); got != want {
			t.Errorf("classFor(%q) = %q, want %q", in, got, want)
		}
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

// THE #198 SIGHTING, end to end through the lane and the store. The lab's onn
// 4K box advertises its display name over `_androidtvremote2._tcp` and a
// truncated hostname form over `_googlecast._tcp`. `avahi-browse -t` dumps
// whatever the cache holds when it is asked — twenty back-to-back runs on a
// static LAN returned between 59 and 64 resolved records — so a sweep can
// genuinely see the Cast record and not the remote one. That sweep must not
// relabel a device the operator already sees named.
func TestASweepMissingTheFriendlyServiceDoesNotDowngradeTheName(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := int64(1000)
	mac := "48:5c:2c:31:6e:6e"
	deviceplaneSeed(t, store, mac, now)

	// The two instance names verbatim, as avahi resolved them on box .12.
	both := []Service{
		{Name: "onn. 4K Streaming Box", Type: "_androidtvremote2._tcp", Address: "192.168.50.63"},
		{Name: "onn.-4K-Streaming-Bo-89edfc7ba2211b500945eaeb2c0265c9", Type: "_googlecast._tcp", Address: "192.168.50.63"},
	}
	castOnly := both[1:]

	dump := both
	l, err := New(Config{
		Store: store, NowMillis: func() int64 { return now },
		ResolveMAC: func(string) (string, bool) { return mac, true },
		Browse:     func() ([]Service, error) { return dump, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	l.sweep()
	if got := hostmdnsName(t, store); got != "onn. 4K Streaming Box" {
		t.Fatalf("after the full sweep name = %q, want %q", got, "onn. 4K Streaming Box")
	}

	// The next sweep's dump is missing the remote service.
	dump = castOnly
	now = 2000
	l.sweep()
	if got := hostmdnsName(t, store); got != "onn. 4K Streaming Box" {
		t.Fatalf("name = %q, want %q — a sweep that did not SEE the better record has not learned the device was renamed, and must not relabel it", got, "onn. 4K Streaming Box")
	}
}

// The pick WITHIN one sweep is ranked by which service said the name, not by
// how the string looks. The lab ecobee falsifies the shape heuristic outright:
// it announces "Upstairs" over HomeKit and "ecobee-ares" over its own service,
// and the len+space score prefers the machine name (11 > 8).
func TestTheServiceTypeRanksTheNameNotItsShape(t *testing.T) {
	store := deviceplane.NewStore("relay-1")
	now := int64(1000)
	mac := "44:61:32:aa:bb:cc"
	deviceplaneSeed(t, store, mac, now)

	l, err := New(Config{
		Store: store, NowMillis: func() int64 { return now },
		ResolveMAC: func(string) (string, bool) { return mac, true },
		Browse: func() ([]Service, error) {
			return []Service{
				{Name: "ecobee-ares", Type: "_ecobee._tcp", Address: "192.168.39.241"},
				{Name: "Upstairs", Type: "_hap._tcp", Address: "192.168.39.241"},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	l.sweep()

	if got := hostmdnsName(t, store); got != "Upstairs" {
		t.Fatalf("name = %q, want %q — HomeKit's instance label is the room the owner typed; the ecobee service's is a machine name that merely scores higher", got, "Upstairs")
	}
}

func TestNameRankFor(t *testing.T) {
	cases := map[string]deviceplane.NameRank{
		// Announce a display name by convention.
		"_androidtvremote2._tcp": deviceplane.NameRankFriendly,
		"_airplay._tcp":          deviceplane.NameRankFriendly,
		"_hap._tcp":              deviceplane.NameRankFriendly,
		"_home-assistant._tcp":   deviceplane.NameRankFriendly,
		// Announce the product, not the unit.
		"_ipp._tcp":     deviceplane.NameRankModel,
		"_printer._tcp": deviceplane.NameRankModel,
		// Announce an id, a hostname form, or a LOSSY REWRITE — the observed
		// offenders. `_display._tcp` is the one that looks friendly and is not;
		// see TestATruncatingServiceTypeIsNotAFriendlyOne.
		"_googlecast._tcp":      deviceplane.NameRankMachine,
		"_display._tcp":         deviceplane.NameRankMachine,
		"_spotify-connect._tcp": deviceplane.NameRankMachine,
		"_ecobee._tcp":          deviceplane.NameRankMachine,
		"_matter._tcp":          deviceplane.NameRankMachine,
		// Promoted on speculation once and demoted on evidence: neither appears
		// on the lab LAN at all, and Roku does not advertise over mDNS.
		"_sonos._tcp": deviceplane.NameRankMachine,
		"_roku._tcp":  deviceplane.NameRankMachine,
		// Unknown types are Machine, not None: still good enough to fill an
		// empty slot and to compete on shape, never good enough to displace a
		// name a known-friendly record authored.
		"_nut._tcp": deviceplane.NameRankMachine,
		"":          deviceplane.NameRankMachine,
	}
	for in, want := range cases {
		if got := nameRankFor(in); got != want {
			t.Errorf("nameRankFor(%q) = %d, want %d", in, got, want)
		}
	}
}

// A SERVICE TYPE THAT TRUNCATES IS A MACHINE SOURCE, however friendly it looks.
//
// `_display._tcp` announces a TV's display name and was ranked friendly for
// exactly that reason — a plausible promotion nobody had checked against the
// LAN. One `avahi-browse -a -t -r -p -k` on box .12 falsifies it: on short names
// the two records agree (`_display._tcp | The Hanger`), but past ~20 characters
// `_display` truncates and appends a `-XXX` disambiguator, which is the same
// string class as `onn.-4K-Streaming-Bo`.
//
//	192.168.39.110  _airplay | 43in office downstairs        _display | 43" office downs-0JX
//	192.168.39.238  _airplay | Office Upstairs small Bedroom  _display | Office Upstairs -6A5
//
// Ranked equal to `_airplay` it ties, and a sweep whose cache held one and not
// the other reproduced #198 on a different pair of records. This is that sweep,
// with the real strings.
func TestATruncatingServiceTypeIsNotAFriendlyOne(t *testing.T) {
	if nameRankFor("_display._tcp") >= nameRankFor("_airplay._tcp") {
		t.Fatalf("_display._tcp ranks at or above _airplay._tcp — it truncates to 20 characters, so a sweep that saw only _display would relabel the device with a mangled name")
	}

	const (
		full      = "Office Upstairs small Bedroom"
		truncated = "Office Upstairs -6A5"
	)
	store := deviceplane.NewStore("relay-1")
	now := int64(1000)
	mac := "ac:63:be:11:22:33"
	deviceplaneSeed(t, store, mac, now)

	dump := []Service{
		{Name: full, Type: "_airplay._tcp", Address: "192.168.39.238"},
		{Name: truncated, Type: "_display._tcp", Address: "192.168.39.238"},
	}
	l, err := New(Config{
		Store: store, NowMillis: func() int64 { return now },
		ResolveMAC: func(string) (string, bool) { return mac, true },
		Browse:     func() ([]Service, error) { return dump, nil },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	l.sweep()
	if got := hostmdnsName(t, store); got != full {
		t.Fatalf("within one sweep name = %q, want %q — the intact name must win the pick", got, full)
	}

	// The next sweep's cache holds `_display` and not `_airplay`. Per-type
	// presence really does vary that way: three consecutive `-t` dumps 12s apart
	// held `_googlecast._tcp` for 192.168.50.63 and no `_androidtvremote2._tcp`.
	dump = dump[1:]
	now = 2000
	l.sweep()
	if got := hostmdnsName(t, store); got != full {
		t.Fatalf("name = %q, want %q — a truncating record must not relabel a device across sweeps, which is #198 with different strings", got, full)
	}
}

// A SWEEP MUST BE A FUNCTION OF THE CACHE, NOT OF THE ORDER IT WAS READ IN.
//
// avahi's browse output has no guaranteed order. Two equally-ranked,
// equally-scoring names would otherwise resolve to whichever record the dump
// listed first — and to the other one 30 seconds later. The store cannot save
// us there: keepName takes an equal-ranked newer name on purpose, because that
// is how a rename lands, so a lane that hands it a coin flip every sweep
// produces a permanent flap that is indistinguishable from a permanent rename.
func TestTheWithinSweepPickDoesNotDependOnBrowseOrder(t *testing.T) {
	// Same rank (both friendly), same nameScore (same length, one space each).
	a := Service{Name: "Den TV", Type: "_airplay._tcp", Address: "192.168.50.77"}
	b := Service{Name: "Bar TV", Type: "_hap._tcp", Address: "192.168.50.77"}

	pick := func(order []Service) string {
		store := deviceplane.NewStore("relay-1")
		mac := "aa:bb:cc:dd:ee:ff"
		deviceplaneSeed(t, store, mac, 1000)
		l, err := New(Config{
			Store: store, NowMillis: func() int64 { return 1000 },
			ResolveMAC: func(string) (string, bool) { return mac, true },
			Browse:     func() ([]Service, error) { return order, nil },
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		l.sweep()
		return hostmdnsName(t, store)
	}

	forward, reverse := pick([]Service{a, b}), pick([]Service{b, a})
	if forward != reverse {
		t.Fatalf("the same cache read in two orders named the device %q and %q — a sweep that depends on browse order flaps forever between two names the store is obliged to accept", forward, reverse)
	}
}

// hostmdnsName reads the single candidate's name out of the store.
func hostmdnsName(t *testing.T, store *deviceplane.Store) string {
	t.Helper()
	cands := store.Report().Body.Candidates
	if len(cands) != 1 {
		t.Fatalf("candidates = %d, want 1 (every service here is one device): %+v", len(cands), cands)
	}
	return cands[0].Name
}
