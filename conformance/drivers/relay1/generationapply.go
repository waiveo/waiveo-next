package relay1

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/maaxton/waiveo-next/conformance/drivers/corpus"
	"github.com/maaxton/waiveo-next/conformance/drivers/report"
	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// rel056Input decodes exactly the REL-056 corpus fields this stage reads:
// the relay's prior last-applied {generation, hash} and the incoming
// state.snapshot's {generation, hash, sections}.
type rel056Input struct {
	RelayLastApplied struct {
		Generation int64  `json:"generation"`
		Hash       string `json:"hash"`
	} `json:"relay_last_applied"`
	StateSnapshot struct {
		Body struct {
			Generation int64           `json:"generation"`
			Hash       string          `json:"hash"`
			Sections   json.RawMessage `json:"sections"`
		} `json:"body"`
	} `json:"state_snapshot"`
}

// genHashPair is the {generation, hash} shape the concurrent atomicity probe
// below records observations as — mirrors internal/relay/identity's own
// corpus_test.go genHash type, one level up in the conformance driver.
type genHashPair struct {
	Generation int64
	Hash       string
}

// rel056OldScreenProgramsJSON is a driver-staged fixture standing in for the
// relay's ALREADY-APPLIED prior generation's screen_programs — the corpus's
// own `input` only names the prior generation's {generation, hash}
// (relay_last_applied), not its screen_programs content, so this stage
// synthesizes a distinct, clearly-not-the-new-generation program (a
// different screen_id/program_revision/priority than the corpus's incoming
// generation-42 program) so a torn cross-generation read is actually
// distinguishable from a clean one. Not asserted against any corpus field.
const rel056OldScreenProgramsJSON = `[{"screen_id":"01J8Z3K4N5P6Q7R8S9T0V1W2X1","program_revision":"rev-16","priority":"scheduled","display":"content","content":[{"asset_ref":"sha256:0000000000000000000000000000000000000000000000000000000000000000","url":"https://app.example/cas/rev16","expires_at":1752537000000}]}]`

// driveREL056 drives the durable atomic generation-apply swap (REL-056)
// directly against internal/relay/identity's operational store —
// Store.ApplyGeneration, the exact atomic-apply primitive
// internal/relay/desiredstate.Pull uses (see desiredstate.go's own doc on
// why the old two-call SetLastAppliedGeneration + SetServedScreenPrograms
// pair left a torn-write window a power-pull could hit).
//
// Two independent properties are proved against the REAL store, not
// re-implemented: (1) an in-flight swap is atomic from a concurrent reader's
// perspective — a goroutine hammering LastAppliedGeneration() while
// ApplyGeneration commits the new generation NEVER observes a torn
// {generation, hash} cross-pair, only the fully-old pair or the fully-new
// one (this is exactly the property an edge-rule evaluation in progress at
// the moment of a swap depends on — the corpus's own
// rule_..._run_in_progress_at_swap input: a torn read would let that
// evaluation mix fields from two different generations); and (2) once the
// swap has landed, the persisted screen_programs facet reads back as the
// NEW generation's array, never the prior generation's stale one — the
// three-field {generation, hash, screen_programs} lockstep REL-056 requires
// end-to-end.
func driveREL056(rep *report.Report, cases map[string]corpus.Case) {
	c, ok := corpus.ByID(cases, "REL-056")
	if !ok {
		rep.Fail("REL-056", contract, "case not found in frozen corpus")
		return
	}

	in, err := decodeREL056Input(c)
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode corpus input: %v", err))
		return
	}

	var diffs []report.Diff

	// sections_present (REL-060): the structural completeness gate runs on
	// the corpus's OWN raw sections bytes — the exact wire.ValidateSectionsComplete
	// gate desiredstate.Pull applies before any typed decode.
	if err := wire.ValidateSectionsComplete(in.StateSnapshot.Body.Sections); err != nil {
		diffs = append(diffs, report.Diff{Field: "sections_present (REL-060, all seven keys)", Expected: "all seven REL-060 keys present", Actual: err.Error()})
	}

	var sectionsMap map[string]json.RawMessage
	if err := json.Unmarshal(in.StateSnapshot.Body.Sections, &sectionsMap); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("decode corpus sections as a map: %v", err))
		return
	}
	newProgramsJSON := []byte(sectionsMap["screen_programs"])
	if len(newProgramsJSON) == 0 {
		rep.Fail(c.CaseID, contract, "corpus state_snapshot.body.sections carries no screen_programs")
		return
	}

	store, err := identity.Open(":memory:")
	if err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("open identity store: %v", err))
		return
	}
	defer store.Close()

	oldPair := genHashPair{Generation: in.RelayLastApplied.Generation, Hash: in.RelayLastApplied.Hash}
	newPair := genHashPair{Generation: in.StateSnapshot.Body.Generation, Hash: in.StateSnapshot.Body.Hash}

	// Seed the prior generation exactly as ApplyGeneration would have left it
	// from an earlier apply — the corpus's relay_last_applied precondition.
	if err := store.ApplyGeneration(oldPair.Generation, oldPair.Hash, []byte(rel056OldScreenProgramsJSON), nil); err != nil {
		rep.Fail(c.CaseID, contract, fmt.Sprintf("seed prior generation %d: %v", oldPair.Generation, err))
		return
	}

	// Property 1: in-flight atomicity. A reader goroutine polls
	// LastAppliedGeneration (a single SELECT over {generation, hash} — the
	// same query internal/relay/identity/corpus_test.go's own oracle test
	// uses) concurrently with the swap, recording every distinct pair it
	// observes. REL-056's atomicity claim is that this set is a subset of
	// {oldPair, newPair} — never a generation from one paired with the hash
	// of the other.
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		observed = map[genHashPair]bool{}
		readErrs []string
		stop     = make(chan struct{})
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			gen, hash, ok, err := store.LastAppliedGeneration()
			if err != nil {
				mu.Lock()
				readErrs = append(readErrs, err.Error())
				mu.Unlock()
				return
			}
			if !ok {
				mu.Lock()
				readErrs = append(readErrs, "LastAppliedGeneration() ok=false mid-swap; last-applied vanished (torn, REL-056)")
				mu.Unlock()
				return
			}
			mu.Lock()
			observed[genHashPair{Generation: gen, Hash: hash}] = true
			mu.Unlock()
		}
	}()

	if err := store.ApplyGeneration(newPair.Generation, newPair.Hash, newProgramsJSON, nil); err != nil {
		close(stop)
		wg.Wait()
		rep.Fail(c.CaseID, contract, fmt.Sprintf("apply new generation %d: %v", newPair.Generation, err))
		return
	}
	close(stop)
	wg.Wait()

	for _, e := range readErrs {
		diffs = append(diffs, report.Diff{Field: "apply_atomic (concurrent LastAppliedGeneration read)", Expected: "no read error", Actual: e})
	}
	for pair := range observed {
		if pair != oldPair && pair != newPair {
			diffs = append(diffs, report.Diff{Field: "in_progress_run_completes_against_single_generation", Expected: fmt.Sprintf("only %+v or %+v", oldPair, newPair), Actual: fmt.Sprintf("torn cross-generation pair %+v", pair)})
		}
	}

	// Property 2: post-swap lockstep. The persisted screen_programs facet
	// must read back as the NEW generation's array — never the prior
	// generation's stale one (the exact torn state a two-call
	// SetLastAppliedGeneration + SetServedScreenPrograms pair could leave a
	// power-pull in, which store.ApplyGeneration's single-statement commit
	// closes).
	gotPrograms, err := store.LastAppliedScreenPrograms()
	if err != nil {
		diffs = append(diffs, report.Diff{Field: "persisted screen_programs (post-swap read)", Expected: "readable", Actual: err.Error()})
	} else if string(gotPrograms) != string(newProgramsJSON) {
		diffs = append(diffs, report.Diff{Field: "persisted screen_programs (no torn cross-generation mix)", Expected: string(newProgramsJSON), Actual: string(gotPrograms)})
	}

	// state_ack.body.applied_generation + persisted_last_applied: the final
	// resting state after the swap.
	finalGen, finalHash, ok, err := store.LastAppliedGeneration()
	if err != nil || !ok {
		diffs = append(diffs, report.Diff{Field: "final LastAppliedGeneration", Expected: "present", Actual: fmt.Sprintf("ok=%v err=%v", ok, err)})
	}
	if wantGen, present := expectInt(c, "state_ack.body.applied_generation"); !present {
		diffs = append(diffs, report.Diff{Field: "state_ack.body.applied_generation", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
	} else if finalGen != wantGen {
		diffs = append(diffs, report.Diff{Field: "state_ack.body.applied_generation", Expected: wantGen, Actual: finalGen})
	}
	if wantGen, present := expectInt(c, "persisted_last_applied.generation"); !present {
		diffs = append(diffs, report.Diff{Field: "persisted_last_applied.generation", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
	} else if finalGen != wantGen {
		diffs = append(diffs, report.Diff{Field: "persisted_last_applied.generation", Expected: wantGen, Actual: finalGen})
	}
	wantHash := c.ExpectString("persisted_last_applied.hash")
	if wantHash == "" {
		diffs = append(diffs, report.Diff{Field: "persisted_last_applied.hash", Expected: "<declared in corpus expected block>", Actual: "absent from corpus fixture"})
	} else if finalHash != wantHash {
		diffs = append(diffs, report.Diff{Field: "persisted_last_applied.hash", Expected: wantHash, Actual: finalHash})
	}

	if len(diffs) > 0 {
		rep.Fail(c.CaseID, contract, "durable atomic generation-apply swap diverged", diffs...)
		return
	}
	rep.Pass(c.CaseID, contract,
		"rule_..._run_in_progress_at_swap / apply_time evaluation semantics: this stage proves the underlying store-read property an in-flight evaluation depends on (never a torn cross-generation {generation, hash} pair) directly against internal/relay/identity, rather than staging a real edge-rule evaluation mid-swap — the edge-rule engine's own consumption of a single generation's fields is outside this store-level oracle's surface.",
		"the prior generation's screen_programs content (rel056OldScreenProgramsJSON) is a driver-staged fixture standing in for relay_last_applied's un-specified prior program — chosen only to be distinguishable from the corpus's incoming generation-42 program, not asserted byte-equal to anything the corpus declares.",
	)
}

// decodeREL056Input decodes c's `input` block into rel056Input by
// round-tripping through JSON — c.Input is the corpus package's generic
// map[string]any, so this recovers the typed shape this stage reads.
func decodeREL056Input(c corpus.Case) (rel056Input, error) {
	raw, err := json.Marshal(c.Input)
	if err != nil {
		return rel056Input{}, fmt.Errorf("marshal corpus input: %w", err)
	}
	var in rel056Input
	if err := json.Unmarshal(raw, &in); err != nil {
		return rel056Input{}, fmt.Errorf("unmarshal corpus input: %w", err)
	}
	return in, nil
}
