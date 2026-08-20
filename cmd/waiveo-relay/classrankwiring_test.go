package main

import (
	"context"
	"errors"
	"testing"

	"github.com/maaxton/waiveo-next/internal/relay/deviceplane"
	"github.com/maaxton/waiveo-next/internal/relay/hostmdns"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// classrankwiring_test.go pins REL-110d's projection and the hop that carries it
// — the same two gaps namerankwiring_test.go exists for, on the class.
//
// The gap this file is really about is the one #204 was: a rank the relay
// computes CORRECTLY and then throws away is enforcement with nothing authoring
// it, and it goes green in every unit test of the merge that consumes it.

// THE PROJECTION ONTO THE WIRE (REL-110d). The ladder is an ordered int in relay
// memory and a TOKEN on the wire, and this function is the only translation
// between them.
//
// The mapping is asserted EXACTLY, rank by rank, and that is the whole point of
// the table. "Every rank reaches a distinct non-empty token" sounds like the
// property that matters and is satisfied by a projection that reports Product as
// `feature` and Feature as `product` — which is not a cosmetic error: it inverts
// the ecobee at 192.168.39.241, whose entire defence is that its own `_ecobee`
// service outranks the `_airplay` bolted onto it. A swapped pair would send the
// app peer a rank that refuses the right class and accepts the wrong one, and no
// merge test on either side could see it.
func TestEveryDeclaredClassRankReachesItsOwnWireToken(t *testing.T) {
	ladder := []struct {
		rank deviceplane.ClassRank
		want string
	}{
		{deviceplane.ClassRankNone, wire.CandidateClassRankNone},
		{deviceplane.ClassRankFeature, wire.CandidateClassRankFeature},
		{deviceplane.ClassRankProduct, wire.CandidateClassRankProduct},
	}
	seen := map[string]deviceplane.ClassRank{}
	for _, tc := range ladder {
		tok := classRankToken(tc.rank)
		if tok != tc.want {
			t.Fatalf("classRankToken(%d) = %q, want %q — the app peer reasons about AUTHORITY off this token, so a mis-projected rank refuses the wrong class on disk",
				tc.rank, tok, tc.want)
		}
		if prev, dup := seen[tok]; dup {
			t.Fatalf("classRankToken maps both %d and %d to %q — one of them is silently reported as a different authority than it is", prev, tc.rank, tok)
		}
		seen[tok] = tc.rank
	}

	// A rank inserted into the MIDDLE of the ladder would keep every case above
	// compiling and fall through to the default arm.
	if got := int(deviceplane.ClassRankProduct); got != len(ladder)-1 {
		t.Fatalf("the ClassRank ladder now has %d entries and this projection knows %d — a rank was added without a REL-110d token, and it is being reported as the weakest authority on the wire",
			got+1, len(ladder))
	}
}

// THE ABSENCE PATH IS UNREACHABLE FROM THIS RELAY, AND THAT IS THE CONTRACT —
// not a coverage gap to engineer around.
//
// `class_rank` is `omitempty` on wire.DeviceCandidate, and this asserts the tag
// is never exercised by this PRODUCER. REL-110a makes `device_class` required, so
// a peer that ranks classes always has one to rank; an absent `class_rank`
// therefore means "a peer older than REL-110d" and can mean nothing else. A relay
// that emitted absence would be claiming to be older than it is and would hand
// its own next report a licence to overwrite a class it had just refused.
//
// So this is a CROSS-VERSION statement, deliberately labelled as one: the tag's
// absence path belongs to an old peer's report and to the feeder rebuilding a
// mirror row whose rank was never recorded, never to this binary.
//
// It covers the DEFAULT ARM specifically — the declared ranks are pinned to
// their exact tokens above, so what is left to say about this function is what
// it does with a value no ladder entry names. Two things, and both are
// decisions: never absence, and never anything but the FLOOR. A future rank
// falling through and being reported as `product` would grant an authority
// nothing stated, which is the one direction a rank must never be wrong in.
func TestAnUnnamedClassRankIsReportedAsTheFloorAndNeverAsAbsence(t *testing.T) {
	for r := deviceplane.ClassRank(0); r < 8; r++ {
		tok := classRankToken(r)
		if tok == "" {
			t.Fatalf("classRankToken(%d) returned the empty string — the member would be omitted and the app peer would read this relay as one that does not rank classes at all", r)
		}
		if r > deviceplane.ClassRankProduct && tok != wire.CandidateClassRankNone {
			t.Fatalf("classRankToken(%d) = %q, want %q — a rank this projection does not name must be reported as the honest floor, never as an authority nothing stated",
				r, tok, wire.CandidateClassRankNone)
		}
	}
}

// ...and the report must actually CARRY it. The projection can be perfect while
// toWireCandidates never calls it — the same class of gap as the rank dying
// inside a sweep, one hop later.
func TestToWireCandidatesCarriesTheClassRank(t *testing.T) {
	driver, nativeID, match, ok := deviceplane.MACIdentity("84:28:59:9f:2f:08")
	if !ok {
		t.Fatal("MACIdentity refused the speaker's MAC")
	}
	store := deviceplane.NewStore("relay-1")
	store.Observe(deviceplane.Observation{
		Match:       match,
		Provenance:  deviceplane.ProvenanceDiscovered,
		Driver:      driver,
		NativeID:    nativeID,
		DeviceClass: "media-player",
		ClassRank:   deviceplane.ClassRankFeature,
	}, 1000)

	got := toWireCandidates(store.Report().Body.Candidates)
	if len(got) != 1 {
		t.Fatalf("projected %d candidates, want 1", len(got))
	}
	if got[0].ClassRank != wire.CandidateClassRankFeature {
		t.Fatalf("class_rank = %q, want %q — the rank never reaches the app peer, so its mirror is back to comparing class tokens and taking whichever arrived last",
			got[0].ClassRank, wire.CandidateClassRankFeature)
	}
}

// THE MEASURED INSTANCE, END TO END THROUGH THE REAL LANE (#204).
//
// This is the link the unit tests cannot make: the rank has to survive the SWEEP
// BOUNDARY. Before this change hostmdns held its class authority in a `host`
// struct declared inside sweep(), so betterClass's verdict was discarded the
// moment the sweep returned and the candidate store compared bare class tokens.
//
// Two sweeps of 192.168.50.43, exactly as box .12's avahi cache produced them:
// the first holds both records, the second is missing `_spotify-connect`. The
// device has not changed. The lane is driven through its own public seam —
// Config.Browse plus a Run whose context is already cancelled, which is exactly
// one sweep — so what runs here is what the binary runs.
func TestASweepMissingTheSpotifyRecordDoesNotReclassifyTheSpeakerOnTheWire(t *testing.T) {
	const (
		mac = "84:28:59:9f:2f:08"
		ip  = "192.168.50.43"
	)
	// Sweep 1 sees both of the speaker's services; sweep 2's browse is missing
	// the Spotify record, which is the nondeterminism this whole guard is about.
	dumps := [][]hostmdns.Service{
		{
			{Name: "d3b79a8f0c4e", Type: "_spotify-connect._tcp", Address: ip},
			{Name: "D731507D2F318A3E", Type: "_matter._tcp", Address: ip},
		},
		{
			{Name: "D731507D2F318A3E", Type: "_matter._tcp", Address: ip},
		},
	}
	sweep := 0

	store := deviceplane.NewStore("relay-1")
	lane, err := hostmdns.New(hostmdns.Config{
		Store:      store,
		NowMillis:  func() int64 { return int64(1000 * (sweep + 1)) },
		ResolveMAC: func(addr string) (string, bool) { return mac, addr == ip },
		Browse: func() ([]hostmdns.Service, error) {
			d := dumps[sweep]
			return d, nil
		},
	})
	if err != nil {
		t.Fatalf("hostmdns.New: %v", err)
	}
	runOnce := func() {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := lane.Run(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want context.Canceled — this test relies on Run sweeping once before it observes the cancellation", err)
		}
	}

	runOnce()
	first := toWireCandidates(store.Report().Body.Candidates)
	if len(first) != 1 {
		t.Fatalf("the report carries %d candidates, want 1 — one host's several services must merge onto one candidate", len(first))
	}
	if first[0].DeviceClass != "media-player" {
		t.Fatalf("device_class = %q, want media-player — `_spotify-connect` and `_matter` are both FEATURE authority, so the sweep's own pick is settled by concreteness", first[0].DeviceClass)
	}
	if first[0].ClassRank != wire.CandidateClassRankFeature {
		t.Fatalf("class_rank = %q, want %q", first[0].ClassRank, wire.CandidateClassRankFeature)
	}

	// THE SECOND SWEEP. Same lane, same store, one record fewer.
	sweep = 1
	runOnce()
	second := toWireCandidates(store.Report().Body.Candidates)
	if len(second) != 1 {
		t.Fatalf("the report carries %d candidates, want 1", len(second))
	}
	if second[0].DeviceClass != "media-player" {
		t.Fatalf("device_class = %q, want media-player — the sweep's own verdict must OUTLIVE the sweep, or one missing mDNS record reclassifies a device that has not changed and the app peer writes it to disk",
			second[0].DeviceClass)
	}
}

// THE CASE THAT ACTUALLY NEEDS THE WIRE, end to end through the real lane.
//
// The speaker above is defended by concreteness alone, which is re-derivable at
// every layer. The ecobee at 192.168.39.241 is not: it advertises `_ecobee._tcp`
// (smart-home, PRODUCT) beside `_airplay._tcp` and `_spotify-connect._tcp`
// (media-player, FEATURE), so the MORE CONCRETE class is the WRONG one and only
// authority saves it. Authority is a fact about the mDNS service type, which no
// layer above this lane can see.
//
// One missing `_ecobee` record is all that stands between this thermostat and
// being durably classified a media player. It is latent rather than live today
// only because it is cross-subnet and absent from the box's neighbour table (the
// lane skips a host it cannot resolve to a MAC), so it goes live the moment such
// a device lands on a MAC-resolvable segment. ResolveMAC is supplied here for
// exactly that reason: the defect is one network topology away, not one code
// change away.
func TestASweepMissingTheEcobeeRecordDoesNotMakeTheThermostatAMediaPlayer(t *testing.T) {
	const (
		mac = "44:61:32:1b:9e:c4"
		ip  = "192.168.39.241"
	)
	dumps := [][]hostmdns.Service{
		// The whole advertisement, as the box's avahi cache holds it.
		{
			{Name: "ecobee-ares", Type: "_ecobee._tcp", Address: ip},
			{Name: "Upstairs", Type: "_hap._tcp", Address: ip},
			{Name: "Upstairs", Type: "_airplay._tcp", Address: ip},
			{Name: "d3b79a8f0c4e", Type: "_spotify-connect._tcp", Address: ip},
		},
		// The same device, one record short. Everything left says media-player,
		// and every one of those records is a bolted-on capability.
		{
			{Name: "Upstairs", Type: "_hap._tcp", Address: ip},
			{Name: "Upstairs", Type: "_airplay._tcp", Address: ip},
			{Name: "d3b79a8f0c4e", Type: "_spotify-connect._tcp", Address: ip},
		},
	}
	sweep := 0

	store := deviceplane.NewStore("relay-1")
	lane, err := hostmdns.New(hostmdns.Config{
		Store:      store,
		NowMillis:  func() int64 { return int64(1000 * (sweep + 1)) },
		ResolveMAC: func(addr string) (string, bool) { return mac, addr == ip },
		Browse:     func() ([]hostmdns.Service, error) { return dumps[sweep], nil },
	})
	if err != nil {
		t.Fatalf("hostmdns.New: %v", err)
	}
	runOnce := func() {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := lane.Run(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	}

	runOnce()
	first := toWireCandidates(store.Report().Body.Candidates)
	if len(first) != 1 || first[0].DeviceClass != "smart-home" {
		t.Fatalf("first sweep reported %+v, want one candidate at smart-home — a device's own product service must outrank the media features bolted onto it", first)
	}
	if first[0].ClassRank != wire.CandidateClassRankProduct {
		t.Fatalf("class_rank = %q, want %q — without the authority on the wire the app peer's mirror has only concreteness, which gets this device backwards",
			first[0].ClassRank, wire.CandidateClassRankProduct)
	}

	sweep = 1
	runOnce()
	second := toWireCandidates(store.Report().Body.Candidates)
	if len(second) != 1 || second[0].DeviceClass != "smart-home" {
		t.Fatalf("second sweep reported %+v, want one candidate still at smart-home — one missing `_ecobee` record must not turn a thermostat into a media player, and this class governs the command vocabulary (REG-052)", second)
	}
	if second[0].ClassRank != wire.CandidateClassRankProduct {
		t.Fatalf("class_rank = %q, want %q — the held class and the authority that authored it must move as a pair across sweeps",
			second[0].ClassRank, wire.CandidateClassRankProduct)
	}
}
