package devices

import (
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/deviceid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// discoveredfacts_test.go covers the two things a candidate report gained
// alongside its original members: the reachability/identification facts an
// operator and a command both need, and the ADOPTION flag no relay may touch.
//
// It sits beside intake_test.go rather than inside it because the subject is
// different in kind: intake_test pins what a hostile relay cannot do to
// IDENTITY, and these pin what a well-formed report contributes, plus the one
// row member whose authority is entirely on this side.

// TestReportedAddressAndIdentityReachTheRow: the discovered facts must survive
// intake rather than being silently dropped — a listed device with no address
// is one nothing can be dispatched to.
func TestReportedAddressAndIdentityReachTheRow(t *testing.T) {
	r := New(testSite, func() int64 { return 0 })
	c := candidate("roku-ecp", "X1")
	c.Address = "192.168.50.31:8060"
	c.Model = "Roku Ultra"
	c.Serial = "X00500ABC123"
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{c}); err != nil {
		t.Fatalf("ApplyCandidates: %v", err)
	}

	d := r.Devices()[0]
	if d.Address != "192.168.50.31:8060" || d.Model != "Roku Ultra" || d.Serial != "X00500ABC123" {
		t.Errorf("row = {address %q, model %q, serial %q}, want the reported values", d.Address, d.Model, d.Serial)
	}
	if d.Adopted {
		t.Error("a reported device arrived adopted — discovery is not adoption, and nothing a relay sends may set it")
	}
}

// TestOversizedLearnedFieldsRefuseTheReport keeps the new members under the same
// bound every other string off this wire is: they are rendered into a response
// an operator reads, and "merely descriptive" is not a reason to accept an
// unbounded or non-UTF-8 value from an untrusted relay.
func TestOversizedLearnedFieldsRefuseTheReport(t *testing.T) {
	for _, tc := range []struct {
		field string
		set   func(*wire.DeviceCandidate)
	}{
		{"address", func(c *wire.DeviceCandidate) { c.Address = strings.Repeat("a", maxIdentityFieldBytes+1) }},
		{"model", func(c *wire.DeviceCandidate) { c.Model = strings.Repeat("m", maxIdentityFieldBytes+1) }},
		{"serial", func(c *wire.DeviceCandidate) { c.Serial = strings.Repeat("s", maxIdentityFieldBytes+1) }},
		{"non-utf8 address", func(c *wire.DeviceCandidate) { c.Address = "\xff\xfe" }},
	} {
		t.Run(tc.field, func(t *testing.T) {
			r := New(testSite, func() int64 { return 0 })
			c := candidate("roku-ecp", "X1")
			tc.set(&c)
			if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{c}); err == nil {
				t.Fatalf("ApplyCandidates accepted an unacceptable %s, want a refusal", tc.field)
			}
			if got := len(r.Devices()); got != 0 {
				t.Errorf("a refused report left %d row(s), want 0", got)
			}
		})
	}
}

// TestMarkAdoptedOutlivesAReport is the reason adoption is held beside the relay
// views rather than on their rows: a report is a full-set REPLACE, and an
// adoption stored inside one would be erased by the next report — including a
// report that simply no longer mentions a device that is powered off.
func TestMarkAdoptedOutlivesAReport(t *testing.T) {
	r := New(testSite, func() int64 { return 0 })
	id := deviceid.Device(testSite, "roku-ecp", "X1")

	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{candidate("roku-ecp", "X1")}); err != nil {
		t.Fatalf("first report: %v", err)
	}
	r.MarkAdopted(id)
	if d, _ := r.Device(id); !d.Adopted {
		t.Fatal("MarkAdopted did not take effect")
	}

	// The device re-reports, then drops off the LAN entirely, then comes back.
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{candidate("roku-ecp", "X1")}); err != nil {
		t.Fatalf("second report: %v", err)
	}
	if d, _ := r.Device(id); !d.Adopted {
		t.Error("adoption was erased by a re-report")
	}
	if err := r.ApplyCandidates(relayA, nil); err != nil {
		t.Fatalf("empty report: %v", err)
	}
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{candidate("roku-ecp", "X1")}); err != nil {
		t.Fatalf("return report: %v", err)
	}
	if d, _ := r.Device(id); !d.Adopted {
		t.Error("adoption was erased while the device was off the network — a powered-off device is not an un-adopted one")
	}
}

// TestMarkIgnoredOutlivesAReport is the same requirement for the ignore flag,
// and it matters MORE here than for adoption: an ignored device keeps being
// reported (that is the point — it is not adopted, so the relay has no reason to
// stop listing it), so a re-report that erased the ignore is the COMMON path,
// not the rare powered-off one. If ignore did not outlive a report, a device an
// operator set aside would reappear on the next sweep, seconds later.
func TestMarkIgnoredOutlivesAReport(t *testing.T) {
	r := New(testSite, func() int64 { return 0 })
	id := deviceid.Device(testSite, "roku-ecp", "X1")

	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{candidate("roku-ecp", "X1")}); err != nil {
		t.Fatalf("first report: %v", err)
	}
	r.MarkIgnored(id)
	if d, _ := r.Device(id); !d.Ignored {
		t.Fatal("MarkIgnored did not take effect")
	}

	// The device re-reports — the ordinary case for an ignored device — and the
	// ignore must still be there.
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{candidate("roku-ecp", "X1")}); err != nil {
		t.Fatalf("second report: %v", err)
	}
	if d, _ := r.Device(id); !d.Ignored {
		t.Error("the ignore decision was erased by a re-report — a re-sighting must not un-ignore")
	}
}

// TestUnmarkIgnoredIsReversible pins spec §7's "reversible": un-ignoring returns
// the device to plain discovered, and the flag clears on the next read without a
// report having to arrive.
func TestUnmarkIgnoredIsReversible(t *testing.T) {
	r := New(testSite, func() int64 { return 0 })
	id := deviceid.Device(testSite, "roku-ecp", "X1")

	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{candidate("roku-ecp", "X1")}); err != nil {
		t.Fatalf("report: %v", err)
	}
	r.MarkIgnored(id)
	if d, _ := r.Device(id); !d.Ignored {
		t.Fatal("MarkIgnored did not take effect")
	}
	r.UnmarkIgnored(id)
	if d, _ := r.Device(id); d.Ignored {
		t.Error("UnmarkIgnored left the device ignored — the decision must be reversible")
	}
	// And ignore is independent of adoption: a device can be adopted while never
	// having been ignored, and clearing ignore does not touch adoption.
	r.MarkAdopted(id)
	r.MarkIgnored(id)
	r.UnmarkIgnored(id)
	if d, _ := r.Device(id); !d.Adopted || d.Ignored {
		t.Errorf("after adopt+ignore+unignore: adopted=%v ignored=%v, want adopted=true ignored=false", d.Adopted, d.Ignored)
	}
}

// TestTheRelaysOwnFirstSeenNeverReachesTheRow is the app-side half of defect
// #196, pinned at the layer where it was silently dropped and then silently
// wrong.
//
// The intake carried neither seen instant onto the row for as long as the row
// existed — so the field a discovery console needs most was written to disk,
// destroyed on every relay restart, and never served to anybody. Restoring it
// naively would be worse than the silence: a relay does not persist candidates,
// so ITS first_seen is its own process uptime, and copying that through would
// report a device the site has watched for months as new.
//
// So the row's age has exactly one source — the durable ledger, projected in by
// MarkSeen — and this asserts both halves: the reported value does not reach the
// row, and the projected one does and outlives the next report.
func TestTheRelaysOwnFirstSeenNeverReachesTheRow(t *testing.T) {
	r := New(testSite, func() int64 { return 0 })
	c := candidate("roku-ecp", "X1")
	// What a freshly restarted relay sends: "I first saw this a moment ago."
	c.FirstSeen, c.LastSeen = 900_000, 900_100
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{c}); err != nil {
		t.Fatalf("ApplyCandidates: %v", err)
	}

	d := r.Devices()[0]
	if d.FirstSeen != 0 || d.LastSeen != 0 {
		t.Fatalf("row age = %d/%d after a report alone, want 0/0 — a relay's own timestamps are its process uptime, not this site's history",
			d.FirstSeen, d.LastSeen)
	}

	// The store commits, and projects what stands.
	id := deviceid.Device(testSite, "roku-ecp", "X1")
	r.MarkSeen(map[string]Seen{id: {FirstMs: 1_700_000_000_000, LastMs: 1_799_999_000_000}})
	d = r.Devices()[0]
	if d.FirstSeen != 1_700_000_000_000 || d.LastSeen != 1_799_999_000_000 {
		t.Fatalf("row age = %d/%d after MarkSeen, want the ledger's values", d.FirstSeen, d.LastSeen)
	}

	// And the next report — which replaces the whole view — does not take it
	// away again, for the reason `adopted` and `ignored` are held beside the
	// views rather than on them.
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{c}); err != nil {
		t.Fatalf("second ApplyCandidates: %v", err)
	}
	if d = r.Devices()[0]; d.FirstSeen != 1_700_000_000_000 {
		t.Errorf("first_seen = %d after a re-report, want it to outlive the report exactly as adoption does", d.FirstSeen)
	}
}

// TestForgetFirstSeenClearsTheAgeAndKeepsTheLastSighting is the read-model half
// of retiring a stored first_seen, and it pins the asymmetry that the obvious
// implementation gets wrong.
//
// The durable retire clears the ledger row and its mirror projection. This map is
// the THIRD copy — the one the running process serves every list from — so
// without a clearing path the console goes on showing the retired instant until
// the next restart, which is the "the database says one thing and the process
// says another" split MarkSeen exists to prevent in the other direction.
//
// And `delete(r.seen, id)` is the obvious implementation and the wrong one: it
// would take last_seen with it, reporting a device that reported a minute ago as
// never heard from. first_seen and last_seen are different facts answered by
// different rules, and only the first is being retired.
func TestForgetFirstSeenClearsTheAgeAndKeepsTheLastSighting(t *testing.T) {
	r := New(testSite, func() int64 { return 0 })
	c := candidate("roku-ecp", "X1")
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{c}); err != nil {
		t.Fatalf("ApplyCandidates: %v", err)
	}
	id := deviceid.Device(testSite, "roku-ecp", "X1")
	r.MarkSeen(map[string]Seen{id: {FirstMs: 1_700_000_000_000, LastMs: 1_799_999_000_000}})

	r.ForgetFirstSeen(id)
	d, ok := r.Device(id)
	if !ok {
		t.Fatalf("the device left the read model; retiring an age must not retire the device")
	}
	if d.FirstSeen != 0 {
		t.Errorf("first_seen = %d after ForgetFirstSeen, want 0 — the API omits the member and the console shows "+
			"an em dash, which is the honest answer until a report plants a new one", d.FirstSeen)
	}
	if d.LastSeen != 1_799_999_000_000 {
		t.Errorf("last_seen = %d after ForgetFirstSeen, want it untouched at 1799999000000 — retiring an age must "+
			"not report a device that reported a minute ago as never heard from", d.LastSeen)
	}

	// Idempotent, and harmless against an id this registry has no age for.
	r.ForgetFirstSeen(id)
	r.ForgetFirstSeen("01J8Z8D1SC0NEVERHEARD0FTH1S")
	r.ForgetFirstSeen("")

	// And the ordinary next projection repairs it: retiring makes a value
	// non-permanent, it does not make the device permanently ageless.
	r.MarkSeen(map[string]Seen{id: {FirstMs: 1_800_000_000_000, LastMs: 1_800_000_000_000, FirstOrigin: "planted"}})
	if d, _ := r.Device(id); d.FirstSeen != 1_800_000_000_000 {
		t.Errorf("first_seen = %d after a fresh plant was projected, want 1800000000000", d.FirstSeen)
	}
	if d, _ := r.Device(id); d.FirstSeenOrigin != "planted" {
		t.Errorf("first_seen_origin = %q after a fresh plant, want \"planted\"", d.FirstSeenOrigin)
	}
}

// TestForgetFirstSeenDropsTheProvenanceWithTheValue: `first_seen_origin` describes
// `first_seen`, so a device that no longer has an age must not go on reporting
// where the age it no longer has came from. Left behind, an `adopted` marker on a
// retired device tells the console to caveat a value that is not there.
func TestForgetFirstSeenDropsTheProvenanceWithTheValue(t *testing.T) {
	r := New(testSite, func() int64 { return 0 })
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{candidate("roku-ecp", "X1")}); err != nil {
		t.Fatalf("ApplyCandidates: %v", err)
	}
	id := deviceid.Device(testSite, "roku-ecp", "X1")
	r.MarkSeen(map[string]Seen{id: {FirstMs: 1_700_000_000_000, LastMs: 1_799_999_000_000, FirstOrigin: "adopted"}})
	if d, _ := r.Device(id); d.FirstSeenOrigin != "adopted" {
		t.Fatalf("fixture: origin = %q, want adopted", d.FirstSeenOrigin)
	}

	r.ForgetFirstSeen(id)
	if d, _ := r.Device(id); d.FirstSeenOrigin != "" {
		t.Errorf("first_seen_origin = %q after the value was retired, want empty", d.FirstSeenOrigin)
	}
}

// TestMarkSeenTeachesWithAZeroRatherThanErasing pins the merge rule the projection
// depends on, and it is not a nicety: it is what lets the caller carry BOTH
// instants of a partially-known device instead of dropping the row.
//
// Zero is the ABSENT answer everywhere along this fact's chain — the store returns
// it, the api/1 member is omitted, the console draws an em dash — so "I have no
// answer" must never overwrite "I have one". The caller used to enforce that by
// discarding any row whose first_seen was zero (cmd/waiveo-feeder seenFrom), which
// protected the age and silently destroyed the last-seen half of a RETIRED
// device's record at every restart. Erasing has its own deliberate method
// (ForgetFirstSeen) and this is not it.
func TestMarkSeenTeachesWithAZeroRatherThanErasing(t *testing.T) {
	r := New(testSite, func() int64 { return 0 })
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{candidate("roku-ecp", "X1")}); err != nil {
		t.Fatalf("ApplyCandidates: %v", err)
	}
	id := deviceid.Device(testSite, "roku-ecp", "X1")
	r.MarkSeen(map[string]Seen{id: {FirstMs: 1_700_000_000_000, LastMs: 1_799_999_000_000, FirstOrigin: "planted"}})

	// A projection that knows only the sighting advances the sighting and leaves
	// the age — and its provenance — exactly where they were.
	r.MarkSeen(map[string]Seen{id: {LastMs: 1_800_000_000_000}})
	d, _ := r.Device(id)
	if d.FirstSeen != 1_700_000_000_000 {
		t.Errorf("first_seen = %d after a projection carrying only a sighting, want it untouched at 1700000000000 "+
			"— an absent answer must not erase a known one", d.FirstSeen)
	}
	if d.FirstSeenOrigin != "planted" {
		t.Errorf("first_seen_origin = %q, want it untouched at \"planted\"", d.FirstSeenOrigin)
	}
	if d.LastSeen != 1_800_000_000_000 {
		t.Errorf("last_seen = %d, want the advanced 1800000000000", d.LastSeen)
	}

	// And the mirror image: an age with no sighting keeps the sighting.
	r.MarkSeen(map[string]Seen{id: {FirstMs: 1_650_000_000_000, FirstOrigin: "adopted"}})
	d, _ = r.Device(id)
	if d.LastSeen != 1_800_000_000_000 {
		t.Errorf("last_seen = %d after a projection carrying only an age, want it untouched at 1800000000000",
			d.LastSeen)
	}
	if d.FirstSeen != 1_650_000_000_000 || d.FirstSeenOrigin != "adopted" {
		t.Errorf("the age and its origin move together; got %d/%q", d.FirstSeen, d.FirstSeenOrigin)
	}
}
