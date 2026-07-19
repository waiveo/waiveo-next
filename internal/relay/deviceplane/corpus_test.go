package deviceplane

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/maaxton/waiveo-next/internal/rules/registry"
)

// This file (Task 5) is the end-to-end oracle for the whole device plane: it
// loads the frozen REL-110 corpus case once, drives it through the real
// Store (Task 2) and the real CommandSurface (Tasks 3/4) exactly as a relay
// would — build the candidate report, then execute each dispatched command —
// and diffs every output against the corpus's own expected shapes. Tasks 2
// and 3's own test files already gate the store and the command surface in
// isolation; this driver is the one place the whole REL-110 case is proven
// end-to-end in a single pass, the way the plan's oracle demands.

// rel110Case is the full REL-110 corpus document, decoded once for this
// driver: the candidate report input, the dispatched commands + fixture
// vocab, and every expected output field this task diffs against.
type rel110Case struct {
	CaseID string   `json:"case_id"`
	ReqIDs []string `json:"req_ids"`
	Input  struct {
		DeviceCandidatesReport  json.RawMessage   `json:"device_candidates_report"`
		Commands                []json.RawMessage `json:"commands"`
		TargetEntityDeviceClass string            `json:"target_entity_device_class"`
		MediaPlayerCommands     []string          `json:"media_player_commands"`
	} `json:"input"`
	Expected struct {
		CandidateViewReplacesPriorView          bool              `json:"candidate_view_replaces_prior_view"`
		Results                                 []json.RawMessage `json:"results"`
		UnresolvedCommandAttemptedAgainstDevice bool              `json:"unresolved_command_attempted_against_device"`
	} `json:"expected"`
}

func loadRel110Case(t *testing.T) rel110Case {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(rel110Corpus))
	if err != nil {
		t.Fatalf("reading REL-110 corpus: %v", err)
	}
	var c rel110Case
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatalf("decoding REL-110 corpus: %v", err)
	}
	return c
}

// buildStoreFromCorpusReport builds a Store whose Report() reproduces the
// corpus's device_candidates_report byte-for-byte, driving it only through
// the Store's own public mutators (Observe/Ignore) — never by poking fields
// — so this is a genuine end-to-end proof of Task 2, not a fixture replay.
// The corpus's report already reflects a re-observed steady state (both
// candidates' last_seen equal their final observation), so this driver
// observes each match, then re-observes it at the reported last_seen to
// exercise the same forward-only bump path Task 2's own tests cover.
func buildStoreFromCorpusReport(t *testing.T, report json.RawMessage) *Store {
	t.Helper()

	var wire struct {
		RelayID string `json:"relay_id"`
		Body    struct {
			Candidates []struct {
				Match        json.RawMessage `json:"match"`
				Provenance   Provenance      `json:"provenance"`
				Status       Status          `json:"status"`
				IgnoredUntil *string         `json:"ignored_until"`
				FirstSeen    int64           `json:"first_seen"`
				LastSeen     int64           `json:"last_seen"`
			} `json:"candidates"`
		} `json:"body"`
	}
	if err := json.Unmarshal(report, &wire); err != nil {
		t.Fatalf("decoding corpus device_candidates_report: %v", err)
	}

	s := NewStore(wire.RelayID)
	for _, c := range wire.Body.Candidates {
		m, err := ParseMatch(c.Match)
		if err != nil {
			t.Fatalf("parsing corpus candidate match %s: %v", c.Match, err)
		}
		s.Observe(m, c.Provenance, c.FirstSeen)
		if c.LastSeen != c.FirstSeen {
			s.Observe(m, c.Provenance, c.LastSeen)
		}
		switch c.Status {
		case StatusAdopted:
			s.Adopt(m.Key())
		case StatusIgnored:
			s.Ignore(m.Key(), c.IgnoredUntil)
		}
	}
	return s
}

// assertShapeEqual marshals got and compares it, as a generic JSON tree, to
// wantRaw — robust to field order / whitespace while asserting full shape
// and values (used throughout this driver for corpus diffs).
func assertShapeEqual(t *testing.T, label string, got any, wantRaw json.RawMessage) {
	t.Helper()
	gotRaw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("%s: marshal: %v", label, err)
	}
	var gotTree, wantTree any
	if err := json.Unmarshal(gotRaw, &gotTree); err != nil {
		t.Fatalf("%s: re-decode ours: %v", label, err)
	}
	if err := json.Unmarshal(wantRaw, &wantTree); err != nil {
		t.Fatalf("%s: decode corpus: %v", label, err)
	}
	if !reflect.DeepEqual(gotTree, wantTree) {
		t.Errorf("%s: shape mismatch:\n got=%s\nwant=%s", label, gotRaw, wantRaw)
	}
}

// TestREL110CorpusEndToEnd is the oracle the plan names: load the frozen
// REL-110 case, build the candidate store from its device_candidates_report
// and prove Report() replaces (not folds into) the prior view, then execute
// every dispatched command through the CommandSurface — built from the
// corpus's own target_entity_device_class + media_player_commands fixture
// vocab via the reused registry.FixtureRegistry — and diff every
// device.command_result against expected.results, including the
// unresolved_command_attempted_against_device:false invariant (REL-110,
// REL-111, REL-112, REL-113, REL-006).
func TestREL110CorpusEndToEnd(t *testing.T) {
	c := loadRel110Case(t)

	if c.CaseID != "REL-110-valid-device-candidate-and-command" {
		t.Fatalf("loaded case_id = %q, want the REL-110 case", c.CaseID)
	}

	// --- device.candidates: full-set report (REL-110/111) ---

	store := buildStoreFromCorpusReport(t, c.Input.DeviceCandidatesReport)
	firstReport := store.Report()
	assertShapeEqual(t, "device_candidates_report", firstReport, c.Input.DeviceCandidatesReport)

	if !c.Expected.CandidateViewReplacesPriorView {
		t.Fatal("corpus expects candidate_view_replaces_prior_view:true")
	}
	// Prove replacement, not accumulation (REL-111): observing a brand-new
	// candidate and re-reporting must yield the ORIGINAL two members plus
	// exactly the one new member — never a growing delta log, and a second
	// call to Report() must independently reproduce the same full set (not
	// consume it).
	again := store.Report()
	assertShapeEqual(t, "device_candidates_report (idempotent re-report)", again, c.Input.DeviceCandidatesReport)

	extra, err := ParseMatch(json.RawMessage(`{"macOui":"AABBCC"}`))
	if err != nil {
		t.Fatalf("parsing probe match: %v", err)
	}
	store.Observe(extra, ProvenanceDiscovered, 1)
	grown := store.Report()
	if len(grown.Body.Candidates) != len(firstReport.Body.Candidates)+1 {
		t.Fatalf("after observing one new candidate, report has %d candidates, want %d (full-set replace, REL-111)",
			len(grown.Body.Candidates), len(firstReport.Body.Candidates)+1)
	}
	for _, want := range firstReport.Body.Candidates {
		found := false
		for _, got := range grown.Body.Candidates {
			if got.Match.Key() == want.Match.Key() {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("grown report lost prior candidate %s — REL-111 requires the full prior view", want.Match.Key())
		}
	}

	// --- device.command / device.command_result (REL-112/113/006) ---

	if len(c.Input.Commands) != len(c.Expected.Results) {
		t.Fatalf("corpus has %d commands but %d expected results", len(c.Input.Commands), len(c.Expected.Results))
	}

	controller := &recordingController{}
	resolve := func(entityID string) (string, bool) {
		// The corpus dispatches every command against the one adopted entity
		// its commands name; anything else resolves to no device class.
		for _, raw := range c.Input.Commands {
			var cmd struct {
				Body struct {
					EntityID string `json:"entity_id"`
				} `json:"body"`
			}
			_ = json.Unmarshal(raw, &cmd)
			if cmd.Body.EntityID == entityID {
				return c.Input.TargetEntityDeviceClass, true
			}
		}
		return "", false
	}
	surface := NewCommandSurface(controller, registry.FixtureRegistry{}, resolve)

	for i, rawCmd := range c.Input.Commands {
		var cmd DeviceCommand
		if err := json.Unmarshal(rawCmd, &cmd); err != nil {
			t.Fatalf("decoding corpus command[%d]: %v", i, err)
		}

		wasUnresolvedVocab := !registry.FixtureRegistry{}.CommandExists(c.Input.TargetEntityDeviceClass, cmd.Body.Command)
		callsBefore := len(controller.calls)

		got := surface.Execute(cmd)
		assertShapeEqual(t, "device.command_result["+cmd.ID+"]", got, c.Expected.Results[i])

		if wasUnresolvedVocab && len(controller.calls) != callsBefore {
			t.Errorf("command[%d] %q was unresolved but the controller was still called — REL-113 forbids touching the device", i, cmd.Body.Command)
		}
	}

	// The corpus's own oracle field: the unresolved command must never have
	// reached the physical device (REL-113).
	if c.Expected.UnresolvedCommandAttemptedAgainstDevice {
		t.Fatal("corpus expects unresolved_command_attempted_against_device:false")
	}
	unresolvedInResults := 0
	for _, raw := range c.Expected.Results {
		var res struct {
			Body struct {
				OK    bool `json:"ok"`
				Error *struct {
					Code string `json:"code"`
				} `json:"error"`
			} `json:"body"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			t.Fatalf("decoding expected result: %v", err)
		}
		if !res.Body.OK && res.Body.Error != nil && res.Body.Error.Code == "COMMAND_UNRESOLVED" {
			unresolvedInResults++
		}
	}
	if unresolvedInResults == 0 {
		t.Fatal("corpus expected no COMMAND_UNRESOLVED result — test fixture drift, expected at least one (the blast command)")
	}
	// Exactly one command dispatched to the controller: the resolved launch,
	// never the unresolved blast (REL-113).
	if len(controller.calls) != len(c.Input.Commands)-unresolvedInResults {
		t.Errorf("controller dispatch count = %d, want %d (resolved commands only)",
			len(controller.calls), len(c.Input.Commands)-unresolvedInResults)
	}

	// --- coverage note: REL-116/117/118 do not exist in contracts/relay-1.md
	// (grepped: no match) — the device plane section (### Device plane) runs
	// REL-110 through REL-115 only, immediately followed by "### Player-
	// credential authority" (REL-120+). There is no device-plane clause left
	// uncovered by Tasks 1-5; nothing to log here beyond this note.
}
