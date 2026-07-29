package devices

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/shared/deviceid"
	"github.com/maaxton/waiveo-next/internal/shared/ulid"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// intake_test.go covers the boundary rules in isolation. The pipeline itself —
// a real relay's report over a real connection producing a listable device and a
// commandable entity — is driven end to end in
// internal/feeder/relayconn/devicediscovery_e2e_test.go; nothing here stands in
// for that, and these cases exist to pin the refusals that whole-stack test
// cannot enumerate one by one.

const (
	testSite  = "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5"
	relayA    = "relay-0f1e2d3c4b5a69788796a5b4c3d2e1f0"
	relayB    = "relay-1a2b3c4d5e6f78899a8b7c6d5e4f3021"
	classMP   = "media-player"
	entityKey = "main"
)

// candidate is a well-formed candidate for the given identity, which each case
// then perturbs in exactly one way.
func candidate(driver, nativeID string) wire.DeviceCandidate {
	return wire.DeviceCandidate{
		Match:       json.RawMessage(`{"ssdp":"urn:roku-com:device:player:1"}`),
		Provenance:  wire.CandidateProvenanceDiscovered,
		Status:      wire.CandidateStatusPending,
		FirstSeen:   1752537000000,
		LastSeen:    1752537600000,
		Driver:      driver,
		NativeID:    nativeID,
		DeviceClass: classMP,
		Entities:    []wire.CandidateEntity{{Key: entityKey, DeviceClass: classMP}},
	}
}

// TestReportedDeviceGetsDerivedIdsAndAppSidePlacement pins what the app authors
// about a reported device: its ids (derived), its placement (the site), its
// labels (empty — a relay has no field to set them), and its external_id
// (unset). None of these is anything a relay said.
func TestReportedDeviceGetsDerivedIdsAndAppSidePlacement(t *testing.T) {
	r := New(testSite)
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{candidate("roku-ecp", "X1")}); err != nil {
		t.Fatalf("ApplyCandidates: %v", err)
	}

	devs := r.Devices()
	if len(devs) != 1 {
		t.Fatalf("got %d device(s), want 1", len(devs))
	}
	d := devs[0]
	if want := deviceid.Device(testSite, "roku-ecp", "X1"); d.ID != want {
		t.Errorf("device id = %q, want the derived %q", d.ID, want)
	}
	if !ulid.Valid(d.ID) {
		t.Errorf("device id %q is not a canonical ULID (DAT-005a)", d.ID)
	}
	if d.ScopeNode != testSite {
		t.Errorf("scope_node = %q, want the site %q — placement is an app-side decision", d.ScopeNode, testSite)
	}
	if d.Labels == nil || len(d.Labels) != 0 {
		t.Errorf("labels = %v, want an empty (non-nil) map — labels are api/1 authored data", d.Labels)
	}
	if d.ExternalID != nil {
		t.Errorf("external_id = %v, want unset", *d.ExternalID)
	}
	if d.RelayID != relayA {
		t.Errorf("relay_id = %q, want the authenticated reporter %q", d.RelayID, relayA)
	}
	if ulid.Valid(d.RelayID) {
		t.Errorf("relay_id %q is a valid ULID — the fixture no longer proves a relay_id is not typed as one (REL-012/014)", d.RelayID)
	}

	ents := r.Entities()
	if len(ents) != 1 {
		t.Fatalf("got %d entit(ies), want 1", len(ents))
	}
	if want := deviceid.Entity(testSite, "roku-ecp", "X1", entityKey); ents[0].ID != want {
		t.Errorf("entity id = %q, want the derived %q", ents[0].ID, want)
	}
	if ents[0].DeviceID != d.ID {
		t.Errorf("entity device_id = %q, want %q", ents[0].DeviceID, d.ID)
	}
}

// TestMalformedCandidateRefusesTheWholeReport is the all-or-nothing rule. A
// report is a full-set replace (REL-111), so applying the candidates that
// happened to parse would install a view that is NOT the relay's — silently
// deleting the devices whose candidates were the malformed ones.
//
// Change ApplyCandidates to `continue` past an invalid candidate instead of
// returning, and this test fails on the "still holds" assertion: the prior
// device is gone, replaced by the partial view.
func TestMalformedCandidateRefusesTheWholeReport(t *testing.T) {
	r := New(testSite)
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{candidate("roku-ecp", "X1")}); err != nil {
		t.Fatalf("seed report: %v", err)
	}
	before := r.Devices()

	bad := candidate("roku-ecp", "")
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{candidate("roku-ecp", "X2"), bad}); err == nil {
		t.Fatal("a report containing a candidate with no native_id was accepted")
	}

	after := r.Devices()
	if len(after) != len(before) || after[0].ID != before[0].ID {
		t.Fatalf("a refused report changed the view: %v, want the prior %v", after, before)
	}
}

// TestBoundaryRefusals enumerates the shape rules a relay's report must satisfy
// (REL-110/110a). Each case is one field away from valid, and every one must be
// refused rather than repaired: a repaired identity names a different device
// than the relay reported.
func TestBoundaryRefusals(t *testing.T) {
	forever := wire.CandidateIgnoredForever
	oversize := strings.Repeat("x", maxIdentityFieldBytes+1)

	mutate := func(f func(*wire.DeviceCandidate)) wire.DeviceCandidate {
		c := candidate("roku-ecp", "X1")
		f(&c)
		return c
	}

	cases := map[string]wire.DeviceCandidate{
		"no driver":                            mutate(func(c *wire.DeviceCandidate) { c.Driver = "" }),
		"no native_id":                         mutate(func(c *wire.DeviceCandidate) { c.NativeID = "" }),
		"no device_class":                      mutate(func(c *wire.DeviceCandidate) { c.DeviceClass = "" }),
		"unknown provenance":                   mutate(func(c *wire.DeviceCandidate) { c.Provenance = "invented" }),
		"unknown status":                       mutate(func(c *wire.DeviceCandidate) { c.Status = "half-adopted" }),
		"ignored_until without ignored status": mutate(func(c *wire.DeviceCandidate) { c.IgnoredUntil = &forever }),
		"ignored status without ignored_until": mutate(func(c *wire.DeviceCandidate) { c.Status = wire.CandidateStatusIgnored }),
		"oversize driver":                      mutate(func(c *wire.DeviceCandidate) { c.Driver = oversize }),
		"oversize native_id":                   mutate(func(c *wire.DeviceCandidate) { c.NativeID = oversize }),
		"oversize name":                        mutate(func(c *wire.DeviceCandidate) { c.Name = strings.Repeat("n", maxNameBytes+1) }),
		"invalid utf-8 native_id":              mutate(func(c *wire.DeviceCandidate) { c.NativeID = "\xc3\x28" }),
		"invalid utf-8 name":                   mutate(func(c *wire.DeviceCandidate) { c.Name = "\xff\xfe" }),
		"entity with no key":                   mutate(func(c *wire.DeviceCandidate) { c.Entities = []wire.CandidateEntity{{DeviceClass: classMP}} }),
		"entity with no class":                 mutate(func(c *wire.DeviceCandidate) { c.Entities = []wire.CandidateEntity{{Key: entityKey}} }),
		"duplicate entity key": mutate(func(c *wire.DeviceCandidate) {
			c.Entities = []wire.CandidateEntity{{Key: entityKey, DeviceClass: classMP}, {Key: entityKey, DeviceClass: classMP}}
		}),
		"too many entities": mutate(func(c *wire.DeviceCandidate) {
			c.Entities = make([]wire.CandidateEntity, maxEntitiesPerDevice+1)
			for i := range c.Entities {
				c.Entities[i] = wire.CandidateEntity{Key: fmt.Sprintf("e%d", i), DeviceClass: classMP}
			}
		}),
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := New(testSite)
			if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{c}); err == nil {
				t.Fatalf("accepted a candidate with %s", name)
			}
			if n := len(r.Devices()); n != 0 {
				t.Fatalf("a refused report left %d device row(s)", n)
			}
		})
	}
}

// TestOversizeReportIsRefusedBeforeTheWorkIsDone bounds the work an untrusted
// report can cause. Without the cap, one frame within the transport's own byte
// limit still buys a derivation and two map entries per candidate.
func TestOversizeReportIsRefusedBeforeTheWorkIsDone(t *testing.T) {
	r := New(testSite)
	cands := make([]wire.DeviceCandidate, maxCandidatesPerReport+1)
	for i := range cands {
		cands[i] = candidate("roku-ecp", fmt.Sprintf("X%d", i))
	}
	if err := r.ApplyCandidates(relayA, cands); err == nil {
		t.Fatal("accepted a report over the candidate cap")
	}
	if n := len(r.Devices()); n != 0 {
		t.Fatalf("a refused oversize report left %d device row(s)", n)
	}
}

// TestDuplicateIdentityInOneReportIsRefused: two candidates for one
// (driver, native_id) is one device claimed twice (REL-153). Silently letting
// the second win would make the app peer's view depend on array order for a
// report that does not describe a set at all.
func TestDuplicateIdentityInOneReportIsRefused(t *testing.T) {
	r := New(testSite)
	err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{candidate("roku-ecp", "X1"), candidate("roku-ecp", "X1")})
	if err == nil {
		t.Fatal("accepted a report claiming one identity twice")
	}
}

// TestUnauthenticatedReportIsRefused: the intake will not key a view on an
// empty relay identity. A report with nowhere to belong could only be applied by
// guessing whose it was.
func TestUnauthenticatedReportIsRefused(t *testing.T) {
	r := New(testSite)
	if err := r.ApplyCandidates("", []wire.DeviceCandidate{candidate("roku-ecp", "X1")}); err == nil {
		t.Fatal("accepted a report carrying no authenticated relay identity")
	}
}

// TestIgnoredCandidateIsNotListed: an ignored candidate rides in the report
// (REL-110 requires it) but is deliberately not materialized. Listing a device
// an operator suppressed — and letting a command reach it — would make the
// suppression cosmetic.
func TestIgnoredCandidateIsNotListed(t *testing.T) {
	forever := wire.CandidateIgnoredForever
	ignored := candidate("roku-ecp", "X1")
	ignored.Status = wire.CandidateStatusIgnored
	ignored.IgnoredUntil = &forever

	r := New(testSite)
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{ignored, candidate("roku-ecp", "X2")}); err != nil {
		t.Fatalf("ApplyCandidates: %v", err)
	}
	devs := r.Devices()
	if len(devs) != 1 {
		t.Fatalf("got %d device(s), want 1 — an ignored candidate must not list", len(devs))
	}
	if want := deviceid.Device(testSite, "roku-ecp", "X2"); devs[0].ID != want {
		t.Errorf("listed device = %q, want the non-ignored %q", devs[0].ID, want)
	}
	if _, found := r.Entity(deviceid.Entity(testSite, "roku-ecp", "X1", entityKey)); found {
		t.Error("an ignored candidate's entity resolves — a command would reach a suppressed device")
	}
}

// TestReportReplacesOnlyTheReportingRelaysView is REL-111 scoped by REL-111a's
// identity keying: one relay's report replaces its own rows and nothing else.
//
// Make ApplyCandidates merge into the existing view instead of replacing it and
// the "departed" assertion fails; make it clear every relay's rows and the
// "other relay survives" assertion fails.
func TestReportReplacesOnlyTheReportingRelaysView(t *testing.T) {
	r := New(testSite)
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{candidate("roku-ecp", "A1"), candidate("roku-ecp", "A2")}); err != nil {
		t.Fatalf("relay A report: %v", err)
	}
	if err := r.ApplyCandidates(relayB, []wire.DeviceCandidate{candidate("roku-ecp", "B1")}); err != nil {
		t.Fatalf("relay B report: %v", err)
	}
	if n := len(r.Devices()); n != 3 {
		t.Fatalf("two relays reporting three distinct devices gave %d row(s), want 3", n)
	}

	// Relay A no longer sees A2.
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{candidate("roku-ecp", "A1")}); err != nil {
		t.Fatalf("relay A re-report: %v", err)
	}
	ids := map[string]bool{}
	for _, d := range r.Devices() {
		ids[d.ID] = true
	}
	if ids[deviceid.Device(testSite, "roku-ecp", "A2")] {
		t.Error("a device relay A stopped reporting survived — the report was folded in, not replaced")
	}
	if !ids[deviceid.Device(testSite, "roku-ecp", "B1")] {
		t.Error("relay B's device vanished when relay A reported — a report replaced another relay's view")
	}
	if !ids[deviceid.Device(testSite, "roku-ecp", "A1")] {
		t.Error("relay A's still-reported device vanished")
	}
}

// TestOneDeviceTwoRelaysIsOneRowAttributedToTheLatestReporter is REL-153: the
// identity tuple excludes the relay, so both relays' reports name the same row.
// The row records which relay reported it most recently, and survives either one
// dropping it while the other still reports it.
func TestOneDeviceTwoRelaysIsOneRowAttributedToTheLatestReporter(t *testing.T) {
	r := New(testSite)
	shared := candidate("roku-ecp", "SHARED")
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{shared}); err != nil {
		t.Fatalf("relay A report: %v", err)
	}
	if err := r.ApplyCandidates(relayB, []wire.DeviceCandidate{shared}); err != nil {
		t.Fatalf("relay B report: %v", err)
	}

	devs := r.Devices()
	if len(devs) != 1 {
		t.Fatalf("one device seen by two relays gave %d row(s), want 1 (REL-153)", len(devs))
	}
	if devs[0].RelayID != relayB {
		t.Errorf("relay_id = %q, want the most recent reporter %q", devs[0].RelayID, relayB)
	}

	// Relay B stops seeing it; relay A still does.
	if err := r.ApplyCandidates(relayB, nil); err != nil {
		t.Fatalf("relay B empty report: %v", err)
	}
	devs = r.Devices()
	if len(devs) != 1 {
		t.Fatalf("the device vanished when one of two reporters dropped it: %d row(s), want 1", len(devs))
	}
	if devs[0].RelayID != relayA {
		t.Errorf("relay_id = %q, want the remaining reporter %q", devs[0].RelayID, relayA)
	}

	// Both stop: now it is genuinely gone.
	if err := r.ApplyCandidates(relayA, nil); err != nil {
		t.Fatalf("relay A empty report: %v", err)
	}
	if n := len(r.Devices()); n != 0 {
		t.Fatalf("%d row(s) survive with no relay reporting them, want 0", n)
	}
}

// TestConflictingReportsResolveToTheMostRecentReporter is the answer to what is
// authoritative when two relays disagree about one device: REL-153's own rule,
// the most recent report.
func TestConflictingReportsResolveToTheMostRecentReporter(t *testing.T) {
	r := New(testSite)
	first := candidate("roku-ecp", "SHARED")
	first.Name = "As Relay A Sees It"
	second := candidate("roku-ecp", "SHARED")
	second.Name = "As Relay B Sees It"

	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{first}); err != nil {
		t.Fatalf("relay A report: %v", err)
	}
	if err := r.ApplyCandidates(relayB, []wire.DeviceCandidate{second}); err != nil {
		t.Fatalf("relay B report: %v", err)
	}
	if got := r.Devices()[0].Name; got != "As Relay B Sees It" {
		t.Errorf("name = %q, want the most recent reporter's %q", got, "As Relay B Sees It")
	}

	// And the older reporter reporting again takes it back — the rule is
	// recency, not first- or last-enrolled.
	if err := r.ApplyCandidates(relayA, []wire.DeviceCandidate{first}); err != nil {
		t.Fatalf("relay A re-report: %v", err)
	}
	if got := r.Devices()[0].Name; got != "As Relay A Sees It" {
		t.Errorf("name = %q, want %q after relay A reported again", got, "As Relay A Sees It")
	}
}
