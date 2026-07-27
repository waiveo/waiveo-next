package desiredstate

import (
	"encoding/json"
	"fmt"

	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// VerifyAndApply is the transport-independent verify/persist half of Pull:
// given an already-fetched state.snapshot body plus the raw `sections`
// bytes exactly as they arrived on the wire, it runs the full REL gate
// sequence — REL-060 structural completeness on the raw bytes, hash
// recompute (REL-053), signature verification against the persisted
// enrollment-anchored trust anchor (REL-071/072), generation monotonicity
// (REL-052) — and, only when every gate passes, persists
// {generation, hash, screen_programs} as ONE atomic apply-unit
// (REL-055/056, store.ApplyGeneration; re-applying an already-applied
// generation is REL-070's no-op).
//
// Extracted from Pull so the persistent-connection transport
// (internal/relay/relayconn) can apply a state.snapshot FRAME through the
// exact same gates the HTTP pull path uses — one verify chain, however the
// bytes arrived. On ANY failure it returns a zero Applied and a typed error
// (the package's Err* values); nothing is applied and the persisted
// last-applied generation is left exactly as it was.
func VerifyAndApply(store *identity.Store, body wire.StateSnapshotBody, rawSections json.RawMessage) (Applied, error) {
	if store == nil {
		return Applied{}, fmt.Errorf("desiredstate: VerifyAndApply: store must not be nil")
	}

	pub, ok, err := store.DesiredStateVerificationKey()
	if err != nil {
		return Applied{}, fmt.Errorf("desiredstate: VerifyAndApply: read desired_state_verification_key: %w", err)
	}
	if !ok {
		return Applied{}, ErrNoTrustAnchor
	}

	// 0. REL-060 structural completeness: the raw `sections` object MUST
	// carry all seven keys. This gate runs on the original wire bytes,
	// BEFORE hash recompute or signature verification — a Go-decoded
	// Sections struct always materializes all seven fields (missing ones as
	// zero values), so an omitted key is only observable here. A snapshot
	// missing any key is rejected outright; nothing is applied.
	if err := wire.ValidateSectionsComplete(rawSections); err != nil {
		return Applied{}, fmt.Errorf("%w: %v", ErrSectionsIncomplete, err)
	}

	// 1. Recompute `hash` from the received `sections` using the SAME
	// canonicalization the feeder used (wire.HashSections — the single
	// shared helper internal/feeder/snapshot also calls, so the two sides
	// cannot drift apart). A snapshot whose `hash` doesn't match its
	// `sections` is rejected before signature verification is even
	// attempted.
	recomputedHash, err := wire.HashSections(body.Sections)
	if err != nil {
		return Applied{}, fmt.Errorf("desiredstate: VerifyAndApply: recompute hash: %w", err)
	}
	if recomputedHash != body.Hash {
		return Applied{}, ErrSnapshotHashMismatch
	}

	// 2. Verify `signature` against the persisted trust anchor (REL-071):
	// decode with wire.DecodeSignature, verify with signhash.Verify over
	// the shared wire.SignedScopeBytes(generation, hash) — the exact bytes
	// the feeder signed (internal/feeder/snapshot.signGenerationHash).
	sigBytes, err := wire.DecodeSignature(body.Signature)
	if err != nil {
		return Applied{}, fmt.Errorf("%w: signature did not decode: %v", ErrSnapshotSignatureInvalid, err)
	}
	canon, err := wire.SignedScopeBytes(body.Generation, body.Hash)
	if err != nil {
		return Applied{}, fmt.Errorf("desiredstate: VerifyAndApply: build signed scope: %w", err)
	}
	if !signhash.Verify(pub, canon, sigBytes) {
		return Applied{}, ErrSnapshotSignatureInvalid
	}

	// 3. Enforce generation monotonicity (REL-052): a generation lower
	// than the persisted last-applied one is rejected outright. An equal
	// generation is not rejected — it is REL-070's no-op case, handled by
	// ApplyGeneration's own idempotent upsert below (persisting the same
	// {generation, hash} again is a no-op by construction).
	lastGen, _, hasLast, err := store.LastAppliedGeneration()
	if err != nil {
		return Applied{}, fmt.Errorf("desiredstate: VerifyAndApply: read last-applied generation: %w", err)
	}
	if hasLast && body.Generation < lastGen {
		return Applied{}, ErrGenerationRegressed
	}

	applied, err := extractApplied(body.Generation, body.Sections)
	if err != nil {
		return Applied{}, fmt.Errorf("desiredstate: VerifyAndApply: %w", err)
	}

	// 4. Persist {generation, hash} AND the applied screen_programs as ONE
	// atomic apply-unit (REL-055/056) — see Pull's original comment for the
	// torn-write hazard store.ApplyGeneration closes. Only reached once
	// hash + signature have both verified and the generation has not
	// regressed. Re-applying the same already-applied generation is a no-op
	// by construction (REL-070) — the row is upserted to the same values.
	programsJSON, err := json.Marshal(applied.ScreenPrograms)
	if err != nil {
		return Applied{}, fmt.Errorf("desiredstate: VerifyAndApply: marshal applied screen_programs: %w", err)
	}
	if err := store.ApplyGeneration(body.Generation, body.Hash, programsJSON); err != nil {
		return Applied{}, fmt.Errorf("desiredstate: VerifyAndApply: persist applied generation: %w", err)
	}

	return applied, nil
}
