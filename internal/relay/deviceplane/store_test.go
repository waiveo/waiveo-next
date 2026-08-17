package deviceplane

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/maaxton/waiveo-next/internal/shared/deviceid"
)

// corpusReportFile decodes only the piece of the REL-110 corpus this task
// (the candidate Store) is the oracle for: input.device_candidates_report.
type corpusReportFile struct {
	Input struct {
		DeviceCandidatesReport json.RawMessage `json:"device_candidates_report"`
	} `json:"input"`
}

const rel110Corpus = "../../../conformance/corpora/relay-1/REL-110-valid-device-candidate-and-command.json"

// The relay_id the REL-110 corpus's device_candidates_report envelope carries.
const rel110RelayID = "01J8Z4K4N5P6Q7R8S9T0V1W3A1"

func loadRel110Report(t *testing.T) json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(rel110Corpus))
	if err != nil {
		t.Fatalf("reading REL-110 corpus: %v", err)
	}
	var f corpusReportFile
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("decoding REL-110 corpus: %v", err)
	}
	if len(f.Input.DeviceCandidatesReport) == 0 {
		t.Fatal("REL-110 corpus has no input.device_candidates_report")
	}
	return f.Input.DeviceCandidatesReport
}

// mediaPlayerEntities is the single addressable handle every candidate in this
// package's fixtures exposes, matching the corpus's own entity fan-out.
var mediaPlayerEntities = []CandidateEntity{{Key: "main", DeviceClass: "media-player"}}

// buildRel110Store builds the exact candidate set the REL-110 corpus reports:
// TWO pending SSDP discoveries answering the SAME declared search target — the
// case that makes identity-keying (REL-111a) observable — and one mDNS
// discovery ignored forever, all last re-observed at 1752537600000, via the
// Store's own mutators.
func buildRel110Store(t *testing.T) *Store {
	t.Helper()
	s := NewStore(rel110RelayID)

	ssdp, err := ParseMatch(json.RawMessage(`{"ssdp":"urn:roku-com:device:player:1"}`))
	if err != nil {
		t.Fatalf("parse ssdp match: %v", err)
	}
	mdns, err := ParseMatch(json.RawMessage(`{"mdns":"_googlecast._tcp"}`))
	if err != nil {
		t.Fatalf("parse mdns match: %v", err)
	}

	roku1 := Observation{Match: ssdp, Provenance: ProvenanceDiscovered, Driver: "roku-ecp",
		NativeID: "uuid:roku:ecp:X10001", DeviceClass: "media-player", Name: "Lobby Roku", Entities: mediaPlayerEntities}
	roku2 := Observation{Match: ssdp, Provenance: ProvenanceDiscovered, Driver: "roku-ecp",
		NativeID: "uuid:roku:ecp:X10002", DeviceClass: "media-player", Name: "Break Room Roku", Entities: mediaPlayerEntities}
	cast := Observation{Match: mdns, Provenance: ProvenanceDiscovered, Driver: "cast",
		NativeID: "Living Room._googlecast._tcp.local", DeviceClass: "media-player", Entities: mediaPlayerEntities}

	// First-observed order is the report order.
	s.Observe(roku1, 1752537000000)
	s.Observe(roku2, 1752537100000)
	s.Observe(cast, 1752530000000)
	// All re-observed later — bumps last_seen, leaves first_seen.
	s.Observe(roku1, 1752537600000)
	s.Observe(roku2, 1752537600000)
	s.Observe(cast, 1752537600000)

	forever := IgnoredForever
	s.Ignore(Key(cast.Driver, cast.NativeID), &forever)
	return s
}

// obs is a terse Observation for the shape tests below: one device of the given
// identity, exposing the standard media-player handle.
func obs(t *testing.T, matchJSON, driver, nativeID string) Observation {
	t.Helper()
	m, err := ParseMatch(json.RawMessage(matchJSON))
	if err != nil {
		t.Fatalf("parse match %s: %v", matchJSON, err)
	}
	return Observation{Match: m, Provenance: ProvenanceDiscovered, Driver: driver,
		NativeID: nativeID, DeviceClass: "media-player", Entities: mediaPlayerEntities}
}

// TestReportMatchesREL110ByteShape asserts the Store's full-set report
// serializes to exactly the device_candidates_report shape the frozen
// REL-110 corpus pins (REL-110/111).
func TestReportMatchesREL110ByteShape(t *testing.T) {
	s := buildRel110Store(t)

	got, err := json.Marshal(s.Report())
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}

	wantRaw := loadRel110Report(t)

	// Semantic equality: decode both to a generic tree and compare — robust
	// to insignificant whitespace while asserting the full shape and values.
	var gotTree, wantTree any
	if err := json.Unmarshal(got, &gotTree); err != nil {
		t.Fatalf("re-decoding our report: %v", err)
	}
	if err := json.Unmarshal(wantRaw, &wantTree); err != nil {
		t.Fatalf("re-decoding corpus report: %v", err)
	}
	if !reflect.DeepEqual(gotTree, wantTree) {
		t.Errorf("report shape mismatch:\n got=%s\nwant=%s", got, wantRaw)
	}
}

// TestReObserveBumpsLastSeenNotFirstSeen asserts a re-observation of an
// already-known match bumps last_seen but leaves first_seen (REL-110).
func TestReObserveBumpsLastSeenNotFirstSeen(t *testing.T) {
	s := NewStore(rel110RelayID)
	o := obs(t, `{"ssdp":"urn:roku-com:device:player:1"}`, "roku-ecp", "X1")

	s.Observe(o, 1000)
	s.Observe(o, 2000)

	rep := s.Report()
	if len(rep.Body.Candidates) != 1 {
		t.Fatalf("expected 1 candidate after re-observe, got %d", len(rep.Body.Candidates))
	}
	c := rep.Body.Candidates[0]
	if c.FirstSeen != 1000 {
		t.Errorf("first_seen = %d, want 1000 (must not move on re-observe)", c.FirstSeen)
	}
	if c.LastSeen != 2000 {
		t.Errorf("last_seen = %d, want 2000 (must bump on re-observe)", c.LastSeen)
	}
}

// TestReObserveOlderTimestampDoesNotRegressLastSeen asserts an out-of-order
// (older) re-observation never moves last_seen backwards.
func TestReObserveOlderTimestampDoesNotRegressLastSeen(t *testing.T) {
	s := NewStore(rel110RelayID)
	o := obs(t, `{"mdns":"_googlecast._tcp"}`, "cast", "Living Room")

	s.Observe(o, 5000)
	s.Observe(o, 3000) // older — must not regress

	c := s.Report().Body.Candidates[0]
	if c.LastSeen != 5000 {
		t.Errorf("last_seen = %d, want 5000 (older re-observe must not regress)", c.LastSeen)
	}
	if c.FirstSeen != 5000 {
		t.Errorf("first_seen = %d, want 5000", c.FirstSeen)
	}
}

// TestIgnoredUntilPresentIffIgnored asserts REL-110's iff invariant across
// every status: ignored_until is non-nil exactly when status is ignored.
func TestIgnoredUntilPresentIffIgnored(t *testing.T) {
	s := NewStore(rel110RelayID)
	pending := obs(t, `{"ssdp":"a"}`, "d", "a")
	adopted := obs(t, `{"ssdp":"b"}`, "d", "b")
	ignored := obs(t, `{"ssdp":"c"}`, "d", "c")

	s.Observe(pending, 1)
	s.Observe(adopted, 1)
	s.Observe(ignored, 1)
	s.Adopt(Key("d", "b"))
	forever := IgnoredForever
	s.Ignore(Key("d", "c"), &forever)

	for _, c := range s.Report().Body.Candidates {
		wantIgnored := c.Status == StatusIgnored
		gotHasUntil := c.IgnoredUntil != nil
		if wantIgnored != gotHasUntil {
			t.Errorf("candidate %s: status=%q ignored_until-present=%v, want present iff ignored",
				Key(c.Driver, c.NativeID), c.Status, gotHasUntil)
		}
	}
}

// TestIgnoredUntilAbsentWhenNotIgnored asserts a non-ignored status marshals
// ignored_until as JSON null (the corpus's absent form) even if a stale
// pointer had lingered — Report enforces the iff invariant on emit.
func TestIgnoredUntilClearedOnAdopt(t *testing.T) {
	s := NewStore(rel110RelayID)
	o := obs(t, `{"ssdp":"x"}`, "d", "x")
	s.Observe(o, 1)
	forever := IgnoredForever
	s.Ignore(Key("d", "x"), &forever)
	s.Adopt(Key("d", "x")) // adopting an ignored candidate must clear ignored_until

	c := s.Report().Body.Candidates[0]
	if c.Status != StatusAdopted {
		t.Fatalf("status = %q, want adopted", c.Status)
	}
	if c.IgnoredUntil != nil {
		t.Errorf("ignored_until = %v, want nil after adopt", *c.IgnoredUntil)
	}
}

// TestReportIsFullSetReplace asserts each Report is the relay's complete
// current candidate view, not a delta — a candidate observed after a prior
// Report appears in the next Report alongside all earlier ones (REL-111).
func TestReportIsFullSetReplace(t *testing.T) {
	s := NewStore(rel110RelayID)
	a := obs(t, `{"ssdp":"a"}`, "d", "a")
	b := obs(t, `{"mdns":"_b._tcp"}`, "d", "b")

	s.Observe(a, 1)
	if got := len(s.Report().Body.Candidates); got != 1 {
		t.Fatalf("first report: %d candidates, want 1", got)
	}

	s.Observe(b, 2)
	rep := s.Report()
	if len(rep.Body.Candidates) != 2 {
		t.Fatalf("second report: %d candidates, want the full set of 2", len(rep.Body.Candidates))
	}
	keys := map[string]bool{}
	for _, c := range rep.Body.Candidates {
		keys[Key(c.Driver, c.NativeID)] = true
	}
	if !keys[Key("d", "a")] || !keys[Key("d", "b")] {
		t.Errorf("full-set report missing a member: keys=%v", keys)
	}
}

// TestReportEnvelope asserts the report carries the device.candidates
// envelope (type + relay_id + body) the corpus and REL-110 pin.
func TestReportEnvelope(t *testing.T) {
	s := NewStore(rel110RelayID)
	rep := s.Report()
	if rep.Type != "device.candidates" {
		t.Errorf("type = %q, want device.candidates", rep.Type)
	}
	if rep.RelayID != rel110RelayID {
		t.Errorf("relay_id = %q, want %q", rep.RelayID, rel110RelayID)
	}
	if rep.Body.Candidates == nil {
		// An empty store still reports an empty (non-null) candidates array.
		t.Error("body.candidates is nil, want an empty (or populated) array")
	}
}

// TestTwoDevicesSharingOneMatchPatternAreTwoCandidates is REL-111a: the store
// is keyed by REL-153 device identity, not by the `match` that found it.
//
// This is the case a match-keyed store gets wrong and no fixture with one device
// per pattern can catch: two Rokus on one LAN both answer the single declared
// search target. Key by `match` and the second sighting is folded into the
// first, so the app peer is told about one device, and the other can never be
// listed, commanded, or adopted.
//
// Change identityKey to return o.Match.Key() and this test fails.
func TestTwoDevicesSharingOneMatchPatternAreTwoCandidates(t *testing.T) {
	s := NewStore(rel110RelayID)
	first := obs(t, `{"ssdp":"urn:roku-com:device:player:1"}`, "roku-ecp", "uuid:roku:X1")
	second := obs(t, `{"ssdp":"urn:roku-com:device:player:1"}`, "roku-ecp", "uuid:roku:X2")

	s.Observe(first, 1000)
	s.Observe(second, 1001)

	cands := s.Report().Body.Candidates
	if len(cands) != 2 {
		t.Fatalf("two devices answering one search target reported as %d candidate(s), want 2 (REL-111a)", len(cands))
	}
	if cands[0].NativeID == cands[1].NativeID {
		t.Fatalf("both candidates carry native_id %q — the two devices collapsed into one", cands[0].NativeID)
	}
}

// TestObservationWithoutIdentityIsDropped proves the store refuses a sighting
// that names no device (REL-110a): without (driver, native_id) the app peer has
// nothing to key, derive an id from, or address it as, so reporting it would
// only produce a candidate the far side must throw away.
func TestObservationWithoutIdentityIsDropped(t *testing.T) {
	s := NewStore(rel110RelayID)
	m, err := ParseMatch(json.RawMessage(`{"ssdp":"urn:roku-com:device:player:1"}`))
	if err != nil {
		t.Fatalf("parse match: %v", err)
	}
	s.Observe(Observation{Match: m, Provenance: ProvenanceDiscovered, NativeID: "X1"}, 1) // no driver
	s.Observe(Observation{Match: m, Provenance: ProvenanceDiscovered, Driver: "roku-ecp"}, 2)
	if n := len(s.Report().Body.Candidates); n != 0 {
		t.Fatalf("report carries %d candidate(s) with no identity, want 0", n)
	}
}

// TestReObserveRefreshesFactsButNotLifecycle asserts the split between what a
// later sighting may correct (the match that found it this time, its name, its
// class, its entities) and what belongs to the store (status, ignored_until) —
// so a device re-appearing on the LAN cannot un-ignore itself.
func TestReObserveRefreshesFactsButNotLifecycle(t *testing.T) {
	s := NewStore(rel110RelayID)
	o := obs(t, `{"ssdp":"a"}`, "roku-ecp", "X1")
	s.Observe(o, 1000)
	forever := IgnoredForever
	s.Ignore(Key("roku-ecp", "X1"), &forever)

	renamed := o
	renamed.Name = "Lobby Roku"
	s.Observe(renamed, 2000)

	c := s.Report().Body.Candidates[0]
	if c.Name != "Lobby Roku" {
		t.Errorf("name = %q, want the re-observed %q — a later sighting may correct a discovered fact", c.Name, "Lobby Roku")
	}
	if c.Status != StatusIgnored || c.IgnoredUntil == nil {
		t.Errorf("status/ignored_until = %q/%v after re-observe, want ignored/forever — re-appearing must not un-ignore a device", c.Status, c.IgnoredUntil)
	}
}

// TestResolveEntityDerivesRatherThanTrusting is REL-110b at the relay end: the
// relay resolves an entity id the app peer addressed by DERIVING every id its
// own candidates could be known by and comparing, so a discovered-but-unadopted
// device is commandable without either peer sending the other an identifier.
//
// It also pins the three refusals: no site adopted yet, an id no candidate
// derives to, and an ignored candidate (suppressing a device must stop commands
// reaching it, not merely hide it from a list).
func TestResolveEntityDerivesRatherThanTrusting(t *testing.T) {
	const site = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"
	s := NewStore(rel110RelayID)
	s.Observe(obs(t, `{"ssdp":"a"}`, "roku-ecp", "X1"), 1000)

	wantEntity := deviceid.Entity(site, "roku-ecp", "X1", "main")
	if _, _, ok := s.ResolveEntity(wantEntity); ok {
		t.Fatal("resolved an entity before any site was adopted — nothing could have been derived against")
	}

	s.SetSite(site)
	deviceID, class, ok := s.ResolveEntity(wantEntity)
	if !ok {
		t.Fatalf("ResolveEntity(%q) did not resolve after the site was adopted", wantEntity)
	}
	if want := deviceid.Device(site, "roku-ecp", "X1"); deviceID != want {
		t.Errorf("resolved device_id = %q, want the derived %q", deviceID, want)
	}
	if class != "media-player" {
		t.Errorf("resolved device_class = %q, want media-player", class)
	}
	if _, _, ok := s.ResolveEntity("01J8Z3K4N5P6Q7R8S9T0V1W2ZZ"); ok {
		t.Error("resolved an entity id no candidate derives to")
	}

	forever := IgnoredForever
	s.Ignore(Key("roku-ecp", "X1"), &forever)
	if _, _, ok := s.ResolveEntity(wantEntity); ok {
		t.Error("an ignored candidate still resolves — suppression would be cosmetic if commands still reached it")
	}
}

// TestObserveDropsHostileSightingsRatherThanPoisoningTheReport pins the fix for
// a LAN kill switch.
//
// A sighting's identity is an SSDP USN or an mDNS PTR record — bytes anything on
// the LAN chooses, with no authentication anywhere in the path. The app applies a
// candidate report ALL-OR-NOTHING, on the sound ground that a partial replace
// would delete the devices whose candidates happened to be malformed. Put those
// together and one spoofed reply carrying an over-long or non-UTF-8 USN makes
// every subsequent report unapplyable — blanking the whole site's device list,
// permanently, because nothing here expires a candidate.
//
// So a hostile sighting is dropped at the boundary and the genuine devices around
// it still report. The store cap is the second half: without it a flood grows a
// report until it no longer fits the relay connection's frame limit, which closes
// the connection rather than refusing the report, taking state sync, command
// dispatch and pairing down with it.
func TestObserveDropsHostileSightingsRatherThanPoisoningTheReport(t *testing.T) {
	s := NewStore("01J8Z2Q1M8H8N4T0V1W2X3Y4Z5")
	const now = int64(1_700_000_000_000)

	good := func(nativeID string) Observation {
		return Observation{
			Driver: "roku-ecp", NativeID: nativeID, DeviceClass: "media-player",
			Match: Match{SSDP: "urn:roku-com:device:player:1"}, Provenance: "ssdp",
		}
	}
	s.Observe(good("uuid:real-one"), now)
	s.Observe(good("uuid:real-two"), now)

	hostile := []struct {
		name string
		obs  Observation
	}{
		{"an over-long USN", good(strings.Repeat("A", 300))},
		{"a non-UTF-8 USN", good(string([]byte{0xff, 0xfe, 0xfd}))},
		{"an over-long name", func() Observation {
			o := good("uuid:named")
			o.Name = strings.Repeat("N", 300)
			return o
		}()},
	}
	for _, h := range hostile {
		s.Observe(h.obs, now)
	}

	got := s.Report().Body.Candidates
	if len(got) != 2 {
		t.Fatalf("store holds %d candidate(s) after hostile sightings, want the 2 genuine ones — a spoofed reply poisons every later report", len(got))
	}
	for _, c := range got {
		if len(c.NativeID) > 256 || !utf8.ValidString(c.NativeID) {
			t.Fatalf("a hostile native_id was stored: %q", c.NativeID)
		}
	}

	// The cap: a flood cannot displace the real devices or grow the report
	// without bound.
	for i := 0; i < 5000; i++ {
		s.Observe(good(fmt.Sprintf("uuid:flood-%d", i)), now)
	}
	if n := len(s.Report().Body.Candidates); n > 1024 {
		t.Fatalf("store grew to %d candidates under a flood; the cap did not hold", n)
	}
	var haveReal int
	for _, c := range s.Report().Body.Candidates {
		if c.NativeID == "uuid:real-one" || c.NativeID == "uuid:real-two" {
			haveReal++
		}
	}
	if haveReal != 2 {
		t.Fatalf("a flood displaced %d of the 2 genuine devices — the cap must refuse newcomers, not evict incumbents", 2-haveReal)
	}
}

// keepClass: a specific class one lane learned must survive another lane's
// re-sighting under the generic default — the class-thrash the neighbour lane
// would otherwise cause on every sweep of a classified device.
func TestObserveDoesNotDowngradeAClassifiedDevice(t *testing.T) {
	store := NewStore("relay-1")
	obs := func(class string) Observation {
		return Observation{
			Match:       Match{MacOui: "c48b66"},
			Provenance:  ProvenanceDiscovered,
			Driver:      HostDriver,
			NativeID:    "c4:8b:66:68:21:25",
			DeviceClass: class,
			Address:     "192.168.50.31",
		}
	}
	store.Observe(obs(ClassUnclassified), 1000) // neighbour lane: unclassified
	store.Observe(obs("media-player"), 1001)    // mDNS lane classifies it
	store.Observe(obs(ClassUnclassified), 1002) // neighbour lane sweeps again

	c := store.Report().Body.Candidates
	if len(c) != 1 {
		t.Fatalf("candidates = %d, want 1", len(c))
	}
	if c[0].DeviceClass != "media-player" {
		t.Fatalf("class = %q after a re-sighting under the generic default, want media-player kept", c[0].DeviceClass)
	}
	// A genuine reclassification (specific → specific) still takes effect.
	store.Observe(obs("printer"), 1003)
	if got := store.Report().Body.Candidates[0].DeviceClass; got != "printer" {
		t.Fatalf("class = %q, want a real reclassification to take", got)
	}
}
