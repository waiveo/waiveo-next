package identity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
)

// corpus_test.go (Task 5) is the differential-oracle driver over the frozen
// relay/1 corpus for the operational store's generation-apply behavior: a
// new generation swaps in atomically — LastAppliedGeneration ever reflects
// the OLD {generation, hash} pair or the NEW one, never a cross of the two
// (REL-055/056) — and re-applying a generation whose content is byte-identical
// to what is already persisted is idempotent (REL-070): repeating the same
// persist is a no-op, never a torn or duplicated write. Both cases replay the
// exact fixture values the frozen corpus files declare, never invented ones —
// the same differential-oracle discipline conformance/drivers/relay1 applies
// to the wire protocol, applied here to the operational store underneath it.

const (
	rel056Corpus = "../../../conformance/corpora/relay-1/REL-056-valid-generation-apply-atomic-swap.json"
	rel070Corpus = "../../../conformance/corpora/relay-1/REL-070-valid-generation-reapply-idempotent-noop.json"
)

// genHash is the {generation, hash} shape both corpus cases carry at several
// dotted paths (relay_last_applied, state_snapshot.body, persisted_last_applied).
type genHash struct {
	Generation int64  `json:"generation"`
	Hash       string `json:"hash"`
}

// rel056Doc decodes exactly the REL-056 fields this driver reads.
type rel056Doc struct {
	Input struct {
		RelayLastApplied genHash `json:"relay_last_applied"`
		StateSnapshot    struct {
			Body genHash `json:"body"`
		} `json:"state_snapshot"`
	} `json:"input"`
	Expected struct {
		ApplyAtomic bool `json:"apply_atomic"`
		StateAck    struct {
			Body struct {
				AppliedGeneration int64 `json:"applied_generation"`
			} `json:"body"`
		} `json:"state_ack"`
		PersistedLastApplied genHash `json:"persisted_last_applied"`
	} `json:"expected"`
}

// rel070Doc decodes exactly the REL-070 fields this driver reads.
type rel070Doc struct {
	Input struct {
		RelayLastApplied genHash `json:"relay_last_applied"`
		StateSnapshot    struct {
			Body genHash `json:"body"`
		} `json:"state_snapshot"`
	} `json:"input"`
	Expected struct {
		TreatedAsNoop bool `json:"treated_as_noop"`
		StateAck      struct {
			Body struct {
				AppliedGeneration int64 `json:"applied_generation"`
			} `json:"body"`
		} `json:"state_ack"`
		PersistedLastAppliedHashUnchanged string `json:"persisted_last_applied_hash_unchanged"`
	} `json:"expected"`
}

func loadRel056(t *testing.T) rel056Doc {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(rel056Corpus))
	if err != nil {
		t.Fatalf("reading REL-056 corpus: %v", err)
	}
	var doc rel056Doc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decoding REL-056 corpus: %v", err)
	}
	return doc
}

func loadRel070(t *testing.T) rel070Doc {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(rel070Corpus))
	if err != nil {
		t.Fatalf("reading REL-070 corpus: %v", err)
	}
	var doc rel070Doc
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decoding REL-070 corpus: %v", err)
	}
	return doc
}

// TestCorpusREL056GenerationApplyAtomicSwap replays REL-056: the relay has
// last-applied generation 41, then applies the corpus's generation-42
// snapshot. The corpus's own apply_atomic:true is the oracle's claim under
// test: a concurrent reader hammering LastAppliedGeneration while the swap
// commits must NEVER observe a cross pair (old generation with the new hash,
// or vice versa) — only the fully-old pair or the fully-new one, exactly as
// the corpus's before/after values name them. The final persisted state and
// the derived state.ack must match the corpus's expected block exactly.
func TestCorpusREL056GenerationApplyAtomicSwap(t *testing.T) {
	doc := loadRel056(t)
	if !doc.Expected.ApplyAtomic {
		t.Fatal("corpus self-check: REL-056 expects apply_atomic:true")
	}
	oldPair := doc.Input.RelayLastApplied
	newPair := doc.Input.StateSnapshot.Body

	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.SetLastAppliedGeneration(oldPair.Generation, oldPair.Hash); err != nil {
		t.Fatalf("seed SetLastAppliedGeneration(%d): %v", oldPair.Generation, err)
	}
	if gen, hash, ok, err := store.LastAppliedGeneration(); err != nil || !ok || gen != oldPair.Generation || hash != oldPair.Hash {
		t.Fatalf("seeded LastAppliedGeneration() = (%d, %q, %v, %v), want (%d, %q, true, nil)", gen, hash, ok, err, oldPair.Generation, oldPair.Hash)
	}

	// A reader goroutine polls LastAppliedGeneration concurrently with the
	// swap, recording every distinct pair it observes. REL-056's atomicity
	// claim is that this set is a subset of {oldPair, newPair} — never a
	// generation from one and a hash from the other.
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		observed = map[genHash]bool{}
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
				t.Errorf("concurrent LastAppliedGeneration(): %v", err)
				return
			}
			if !ok {
				// The store was already seeded with oldPair before this reader
				// started; last-applied must never transiently disappear
				// during an atomic swap (REL-055/056) — a momentarily-absent
				// row is itself a torn state, not merely "nothing to observe
				// yet".
				t.Errorf("LastAppliedGeneration() ok=false mid-swap; last-applied vanished (torn, REL-056)")
				return
			}
			mu.Lock()
			observed[genHash{Generation: gen, Hash: hash}] = true
			mu.Unlock()
		}
	}()

	if err := store.SetLastAppliedGeneration(newPair.Generation, newPair.Hash); err != nil {
		t.Fatalf("swap SetLastAppliedGeneration(%d): %v", newPair.Generation, err)
	}
	close(stop)
	wg.Wait()

	for pair := range observed {
		if pair != oldPair && pair != newPair {
			t.Fatalf("observed torn last-applied pair %+v during the swap; want only %+v or %+v (REL-056 atomic swap)", pair, oldPair, newPair)
		}
	}

	gen, hash, ok, err := store.LastAppliedGeneration()
	if err != nil {
		t.Fatalf("final LastAppliedGeneration(): %v", err)
	}
	if !ok {
		t.Fatal("final LastAppliedGeneration() ok = false, want true")
	}
	if gen != doc.Expected.PersistedLastApplied.Generation || hash != doc.Expected.PersistedLastApplied.Hash {
		t.Fatalf("final persisted last-applied = (%d, %q), want corpus persisted_last_applied (%d, %q)",
			gen, hash, doc.Expected.PersistedLastApplied.Generation, doc.Expected.PersistedLastApplied.Hash)
	}
	if gen != doc.Expected.StateAck.Body.AppliedGeneration {
		t.Fatalf("final persisted generation = %d, want corpus state_ack.body.applied_generation %d", gen, doc.Expected.StateAck.Body.AppliedGeneration)
	}

	// §52 no-media re-check scoped to this corpus's own fixture: REL-056's
	// state_snapshot carries a full content section with asset_ref/url
	// fields, yet applying it must still leave the operational store holding
	// only the same seven small operational tables — never a table sized to
	// hold that section's asset bytes.
	assertNoMediaTable(t, store)
}

// TestCorpusREL070GenerationReapplyIdempotentNoop replays REL-070: the relay
// has already applied generation 42; the corpus's generation-43 snapshot
// carries sections byte-identical to generation 42 and so produces the same
// hash. Applying it — and re-applying it again, exactly as a caller retrying
// an unacknowledged state.ack would — must persist the corpus's expected
// {generation, hash} with the hash UNCHANGED across both calls (REL-070): no
// torn or duplicated state from repeating the same persist.
func TestCorpusREL070GenerationReapplyIdempotentNoop(t *testing.T) {
	doc := loadRel070(t)
	if !doc.Expected.TreatedAsNoop {
		t.Fatal("corpus self-check: REL-070 expects treated_as_noop:true")
	}
	priorPair := doc.Input.RelayLastApplied
	reapplyPair := doc.Input.StateSnapshot.Body
	if reapplyPair.Hash != priorPair.Hash {
		t.Fatalf("corpus self-check: REL-070's reapply hash %q must equal the prior hash %q (content-identical reapply)", reapplyPair.Hash, priorPair.Hash)
	}

	store, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.SetLastAppliedGeneration(priorPair.Generation, priorPair.Hash); err != nil {
		t.Fatalf("seed SetLastAppliedGeneration(%d): %v", priorPair.Generation, err)
	}

	// First apply of the content-identical generation-43 snapshot.
	if err := store.SetLastAppliedGeneration(reapplyPair.Generation, reapplyPair.Hash); err != nil {
		t.Fatalf("first reapply SetLastAppliedGeneration(%d): %v", reapplyPair.Generation, err)
	}
	gen, hash, ok, err := store.LastAppliedGeneration()
	if err != nil || !ok {
		t.Fatalf("LastAppliedGeneration() after first reapply = (ok=%v, err=%v)", ok, err)
	}
	if gen != doc.Expected.StateAck.Body.AppliedGeneration || hash != doc.Expected.PersistedLastAppliedHashUnchanged {
		t.Fatalf("after first reapply, persisted = (%d, %q), want (%d, %q)",
			gen, hash, doc.Expected.StateAck.Body.AppliedGeneration, doc.Expected.PersistedLastAppliedHashUnchanged)
	}

	// Re-applying the SAME generation+hash again (e.g. a retried state.ack)
	// must be a true no-op: identical end state, no error, nothing torn or
	// duplicated by repeating the write.
	if err := store.SetLastAppliedGeneration(reapplyPair.Generation, reapplyPair.Hash); err != nil {
		t.Fatalf("second (idempotent) reapply SetLastAppliedGeneration(%d): %v", reapplyPair.Generation, err)
	}
	gen2, hash2, ok2, err := store.LastAppliedGeneration()
	if err != nil || !ok2 {
		t.Fatalf("LastAppliedGeneration() after second reapply = (ok=%v, err=%v)", ok2, err)
	}
	if gen2 != gen || hash2 != hash {
		t.Fatalf("second reapply changed persisted state to (%d, %q), want unchanged (%d, %q) — REL-070 idempotent no-op", gen2, hash2, gen, hash)
	}
	if hash2 != priorPair.Hash {
		t.Fatalf("persisted hash after idempotent reapply = %q, want unchanged %q (persisted_last_applied_hash_unchanged)", hash2, priorPair.Hash)
	}

	assertNoMediaTable(t, store)
}

// assertNoMediaTable is the §52 operational-footprint check scoped to a
// specific corpus test: the store must hold exactly the known small
// operational tables (identity, last-applied, clock floor, telemetry queue +
// its sidecars, player-session state) and nothing capable of holding
// asset/media bytes, even after exercising a generation-apply swap whose own
// corpus fixture carries a full content section with asset_ref/url fields. This mirrors
// TestOpenCreatesExactOperationalTableSet's table set (identity_test.go);
// repeating it here ties the assertion to THIS corpus's own apply path,
// rather than only a freshly opened store.
func assertNoMediaTable(t *testing.T, store *Store) {
	t.Helper()
	tables, err := store.tableNames()
	if err != nil {
		t.Fatalf("tableNames(): %v", err)
	}
	sort.Strings(tables)

	want := []string{
		"app_peer_trust_pin",
		"clock_floor",
		"desired_state_verification_key",
		"last_applied_generation",
		"player_channel_tokens",
		"player_redeemed_grants",
		"relay_identity",
		"telemetry_loss_marker",
		"telemetry_queue",
		"telemetry_seq_high_water",
	}
	if len(tables) != len(want) {
		t.Fatalf("tables after generation apply = %v, want exactly %v (§52: no asset/media table)", tables, want)
	}
	for i := range want {
		if tables[i] != want[i] {
			t.Fatalf("tables after generation apply = %v, want exactly %v (§52: no asset/media table)", tables, want)
		}
	}
}
