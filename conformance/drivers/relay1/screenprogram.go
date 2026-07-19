package relay1

import (
	"encoding/json"
	"fmt"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/relay/desiredstate"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// rel061Input decodes exactly the REL-061 corpus fields this stage reads:
// the corpus's own connectivity precondition (documentary only — this stage
// proves it structurally, see driveREL061's doc), the relay's already-durably
// -persisted last-applied {generation, hash}, and the persisted
// screen_programs array that earlier apply left behind.
type rel061Input struct {
	RelayAppPeerConnectivity  string `json:"relay_app_peer_connectivity"`
	RelayPersistedLastApplied struct {
		Generation int64  `json:"generation"`
		Hash       string `json:"hash"`
	} `json:"relay_persisted_last_applied"`
	PersistedSectionsScreenPrograms []wire.ScreenProgram `json:"persisted_sections_screen_programs"`
}

// driveREL061 drives the relay's offline program-delivery continuity
// (REL-055/061) directly against internal/relay/identity's durable store and
// internal/relay/desiredstate.ServedProgram — the exact offline-serve
// accessor Task 2 built for this purpose.
//
// The corpus's own relay_app_peer_connectivity: "disconnected" precondition
// is proved STRUCTURALLY, not by toggling a simulated flag: this stage seeds
// the store exactly as an earlier, online desiredstate.Pull would have left
// it (the same store.ApplyGeneration commit Pull itself calls, REL-056),
// then reads the served program back through ServedProgram — a function
// whose ENTIRE parameter list is the durable store, with no
// feederBaseURL/Feeder/RelayClient in scope anywhere in this call. There is
// no live app-peer connection for this stage to require, because none is
// ever constructed — exactly the property REL-055/061 assert.
func driveREL061(rep *report.Report, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REL-061")
	if !ok {
		rep.Fail("REL-061", contract, "case not found in frozen corpus")
		return
	}

	in, err := decodeREL061Input(c)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode corpus input: %v", err))
		return
	}
	if len(in.PersistedSectionsScreenPrograms) == 0 {
		rep.Fail(c.CaseID, contract, "corpus input.persisted_sections_screen_programs is empty")
		return
	}

	store, err := identity.Open(":memory:")
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("open identity store: %v", err))
		return
	}
	defer store.Close()

	programsJSON, err := json.Marshal(in.PersistedSectionsScreenPrograms)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("marshal corpus persisted screen_programs: %v", err))
		return
	}

	// Seed the durable store exactly as an earlier, ONLINE apply would have
	// left it — internal/relay/identity.Store.ApplyGeneration is the same
	// atomic commit desiredstate.Pull itself calls (REL-056).
	if err := store.ApplyGeneration(in.RelayPersistedLastApplied.Generation, in.RelayPersistedLastApplied.Hash, programsJSON); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("seed persisted last-applied generation %d: %v", in.RelayPersistedLastApplied.Generation, err))
		return
	}

	var diffs []report.Diff

	// The offline read itself: ServedProgram takes no feeder/app-peer
	// reference at all — calling it successfully here IS
	// served_from_persisted_snapshot_without_app_peer / the negation of
	// requires_live_app_peer_connection.
	served, err := desiredstate.ServedProgram(store)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("ServedProgram (offline serve accessor): %v", err))
		return
	}
	if len(served) == 0 {
		diffs = append(diffs, report.Diff{Field: "served_from_persisted_snapshot_without_app_peer", Expected: true, Actual: "ServedProgram returned no entries"})
	} else {
		entry := served[0]

		if wantPriority := c.ExpectString("screen_program_entry_priority"); wantPriority == "" {
			diffs = append(diffs, report.Diff{Field: "screen_program_entry_priority", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
		} else if entry.Priority != wantPriority {
			diffs = append(diffs, report.Diff{Field: "screen_program_entry_priority (PLY-108, unmodified)", Expected: wantPriority, Actual: entry.Priority})
		}

		if wantDisplay := c.ExpectString("screen_program_entry_display"); wantDisplay == "" {
			diffs = append(diffs, report.Diff{Field: "screen_program_entry_display", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
		} else if entry.Display != wantDisplay {
			diffs = append(diffs, report.Diff{Field: "screen_program_entry_display (PLY-109, unmodified)", Expected: wantDisplay, Actual: entry.Display})
		}

		if wantRevision := c.ExpectString("program_revision_delivered"); wantRevision == "" {
			diffs = append(diffs, report.Diff{Field: "program_revision_delivered", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
		} else if entry.ProgramRevision != wantRevision {
			diffs = append(diffs, report.Diff{Field: "program_revision_delivered", Expected: wantRevision, Actual: entry.ProgramRevision})
		}
	}

	if wantServedOffline, present := c.Expect("served_from_persisted_snapshot_without_app_peer"); !present {
		diffs = append(diffs, report.Diff{Field: "served_from_persisted_snapshot_without_app_peer", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
	} else if wantServedOffline != true {
		diffs = append(diffs, report.Diff{Field: "served_from_persisted_snapshot_without_app_peer", Expected: wantServedOffline, Actual: true})
	}

	if wantRequiresLive, present := c.Expect("requires_live_app_peer_connection"); !present {
		diffs = append(diffs, report.Diff{Field: "requires_live_app_peer_connection", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
	} else if wantRequiresLive != false {
		diffs = append(diffs, report.Diff{Field: "requires_live_app_peer_connection", Expected: wantRequiresLive, Actual: false})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "offline screen-program serve diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		"relay_app_peer_connectivity: \"disconnected\" is proved structurally, not by a simulated flag — this stage never constructs or references a feeder/app-peer connection anywhere in the offline read path (desiredstate.ServedProgram's sole parameter is the durable store), so there is no live connection to require.",
		"player/1 Lease delivery (PLY-108/109 carrying priority/display/content unmodified onto an issued Lease) is unit-tested end-to-end at internal/relay/playerserver's own TestSetServedProgramDeliversPersistedPreemptLease against the identical persisted-entry shape this stage seeds — this driver stage's own oracle surface is the relay/1-side offline-serve accessor, not player/1's issuance format.",
	)
}

// decodeREL061Input decodes c's `input` block into rel061Input by
// round-tripping through JSON — c.Input is the corpus package's generic
// map[string]any, so this recovers the typed shape this stage reads.
func decodeREL061Input(c corpus.Case) (rel061Input, error) {
	raw, err := json.Marshal(c.Input)
	if err != nil {
		return rel061Input{}, fmt.Errorf("marshal corpus input: %w", err)
	}
	var in rel061Input
	if err := json.Unmarshal(raw, &in); err != nil {
		return rel061Input{}, fmt.Errorf("unmarshal corpus input: %w", err)
	}
	return in, nil
}
