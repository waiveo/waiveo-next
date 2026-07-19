package deviceplane

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
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

// buildRel110Store builds the exact candidate set the REL-110 corpus reports:
// one pending SSDP discovery and one mDNS discovery ignored forever, both
// last re-observed at 1752537600000, via the Store's own mutators.
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

	// First-observed order is the report order: ssdp then mdns.
	s.Observe(ssdp, ProvenanceDiscovered, 1752537000000)
	s.Observe(mdns, ProvenanceDiscovered, 1752530000000)
	// Both re-observed later — bumps last_seen, leaves first_seen.
	s.Observe(ssdp, ProvenanceDiscovered, 1752537600000)
	s.Observe(mdns, ProvenanceDiscovered, 1752537600000)

	forever := IgnoredForever
	s.Ignore(mdns.Key(), &forever)
	return s
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
	m, _ := ParseMatch(json.RawMessage(`{"ssdp":"urn:roku-com:device:player:1"}`))

	s.Observe(m, ProvenanceDiscovered, 1000)
	s.Observe(m, ProvenanceDiscovered, 2000)

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
	m, _ := ParseMatch(json.RawMessage(`{"mdns":"_googlecast._tcp"}`))

	s.Observe(m, ProvenanceDiscovered, 5000)
	s.Observe(m, ProvenanceDiscovered, 3000) // older — must not regress

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
	pending, _ := ParseMatch(json.RawMessage(`{"ssdp":"a"}`))
	adopted, _ := ParseMatch(json.RawMessage(`{"ssdp":"b"}`))
	ignored, _ := ParseMatch(json.RawMessage(`{"ssdp":"c"}`))

	s.Observe(pending, ProvenanceDiscovered, 1)
	s.Observe(adopted, ProvenanceDiscovered, 1)
	s.Observe(ignored, ProvenanceDiscovered, 1)
	s.Adopt(adopted.Key())
	forever := IgnoredForever
	s.Ignore(ignored.Key(), &forever)

	for _, c := range s.Report().Body.Candidates {
		wantIgnored := c.Status == StatusIgnored
		gotHasUntil := c.IgnoredUntil != nil
		if wantIgnored != gotHasUntil {
			t.Errorf("candidate %s: status=%q ignored_until-present=%v, want present iff ignored",
				c.Match.Key(), c.Status, gotHasUntil)
		}
	}
}

// TestIgnoredUntilAbsentWhenNotIgnored asserts a non-ignored status marshals
// ignored_until as JSON null (the corpus's absent form) even if a stale
// pointer had lingered — Report enforces the iff invariant on emit.
func TestIgnoredUntilClearedOnAdopt(t *testing.T) {
	s := NewStore(rel110RelayID)
	m, _ := ParseMatch(json.RawMessage(`{"ssdp":"x"}`))
	s.Observe(m, ProvenanceDiscovered, 1)
	forever := IgnoredForever
	s.Ignore(m.Key(), &forever)
	s.Adopt(m.Key()) // adopting an ignored candidate must clear ignored_until

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
	a, _ := ParseMatch(json.RawMessage(`{"ssdp":"a"}`))
	b, _ := ParseMatch(json.RawMessage(`{"mdns":"_b._tcp"}`))

	s.Observe(a, ProvenanceDiscovered, 1)
	if got := len(s.Report().Body.Candidates); got != 1 {
		t.Fatalf("first report: %d candidates, want 1", got)
	}

	s.Observe(b, ProvenanceDiscovered, 2)
	rep := s.Report()
	if len(rep.Body.Candidates) != 2 {
		t.Fatalf("second report: %d candidates, want the full set of 2", len(rep.Body.Candidates))
	}
	keys := map[string]bool{}
	for _, c := range rep.Body.Candidates {
		keys[c.Match.Key()] = true
	}
	if !keys[a.Key()] || !keys[b.Key()] {
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
