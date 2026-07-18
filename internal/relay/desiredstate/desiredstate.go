// Package desiredstate implements the relay's client side of relay/1
// desired-state pull (REL-051): fetching the feeder's signed
// `state.snapshot` over `/state/pull` (internal/feeder/enroll's own
// handler), VERIFYING it against the relay's persisted, enrollment-anchored
// trust anchor (REL-071, `#28`), enforcing generation monotonicity
// (REL-052), and persisting `{generation, hash}` as last-applied (REL-055,
// internal/relay/identity).
//
// Only feeder-signed state applies: a snapshot whose `sections` doesn't
// hash to its own `hash`, or whose `signature` doesn't verify under the
// persisted `desired_state_verification_key`, is rejected outright —
// nothing is applied and last-applied is left untouched. This is the
// security-load-bearing gate a later player/1 server (Task 9) sits behind:
// Pull's returned Applied value is the only screen-program state that ever
// reaches a screen.
package desiredstate

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/maaxton/waiveo-next/internal/relay/identity"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// pullHTTPTimeout bounds the desired-state pull exchange — this is a
// co-located, same-host call in Wave-1 first-photon's loopback deployment
// (mirroring internal/relay/enroll's own bootstrap timeout), so a short
// timeout is generous, not tight.
const pullHTTPTimeout = 10 * time.Second

// Errors Pull returns for each of relay/1's typed rejection reasons. All
// are checked with errors.Is, and each leaves the persisted last-applied
// generation untouched — no section is EVER applied on any of these paths.
var (
	// ErrNoTrustAnchor is returned when the store holds no persisted
	// desired_state_verification_key yet (the relay has not enrolled) —
	// there is nothing to verify a snapshot's signature against.
	ErrNoTrustAnchor = errors.New("desiredstate: no desired_state_verification_key persisted — relay must enroll before pulling desired state")

	// ErrSnapshotHashMismatch is returned when a pulled snapshot's `hash`
	// does not equal sha256 over its own `sections` (REL-053) — the
	// snapshot is internally inconsistent (sections tampered with, or
	// corrupted in transit) and is rejected outright, before signature
	// verification is even attempted.
	ErrSnapshotHashMismatch = errors.New("desiredstate: snapshot hash does not match its sections")

	// ErrSnapshotSignatureInvalid is returned when a pulled snapshot's
	// `signature` does not verify under the persisted
	// desired_state_verification_key trust anchor (REL-071) — relay/1's
	// SNAPSHOT_SIGNATURE_INVALID (REL-072). This is the security property:
	// only feeder-signed state (signed by the exact key learned at
	// enrollment) ever applies.
	ErrSnapshotSignatureInvalid = errors.New("desiredstate: snapshot signature did not verify against the persisted trust anchor (SNAPSHOT_SIGNATURE_INVALID)")

	// ErrGenerationRegressed is returned when a pulled snapshot's
	// `generation` is lower than the relay's persisted last-applied
	// generation (REL-052) — desired-state generations are monotonically
	// non-decreasing; a lower one is rejected outright, never applied.
	ErrGenerationRegressed = errors.New("desiredstate: snapshot generation is lower than the persisted last-applied generation")
)

// Applied is the relay's locally-applied Wave-1 first-photon desired-state
// result: the one screen-program's one image content item a later player/1
// server (Task 9) serves to the screen, plus the generation it came from.
// The zero value (Applied{}) is what Pull returns alongside every rejection
// error above — never a partially-populated value.
//
// PairingGrants carries the verified snapshot's sections.pairing_grants
// (REL-067) unmodified — the pairing server (internal/relay/playerserver)
// resolves a PairingRequest's grant_selector against these, exactly as
// REL-121/REL-126 require. Because this whole struct is only ever produced
// by an already-hash-and-signature-verified snapshot, a caller holding an
// Applied value can trust its PairingGrants exactly as much as it trusts
// the rest of the struct — no separate verification step applies here.
type Applied struct {
	Generation      int64
	ScreenID        string
	ProgramRevision string
	Image           wire.ContentRef
	PairingGrants   []wire.PairingGrant
}

// Pull fetches the feeder's signed desired-state snapshot from
// feederBaseURL's `/state/pull` (internal/feeder/enroll's handler),
// verifies it against store's persisted desired_state_verification_key
// trust anchor (REL-071), enforces generation monotonicity (REL-052), and
// on success persists `{generation, hash}` as last-applied (REL-055,
// idempotent — re-pulling the same, already-applied generation is a
// no-op, REL-070) before returning the applied screen-program.
//
// On ANY verification failure (hash mismatch, signature invalid, or a
// regressed generation), Pull returns a zero Applied and a typed error
// (one of the Err* values above) — no section is applied, and the
// persisted last-applied generation is left exactly as it was.
func Pull(feederBaseURL string, store *identity.Store) (Applied, error) {
	if store == nil {
		return Applied{}, fmt.Errorf("desiredstate: Pull: store must not be nil")
	}

	pub, ok, err := store.DesiredStateVerificationKey()
	if err != nil {
		return Applied{}, fmt.Errorf("desiredstate: Pull: read desired_state_verification_key: %w", err)
	}
	if !ok {
		return Applied{}, ErrNoTrustAnchor
	}

	body, err := fetchSnapshot(feederBaseURL)
	if err != nil {
		return Applied{}, fmt.Errorf("desiredstate: Pull: fetch snapshot: %w", err)
	}

	// 1. Recompute `hash` from the received `sections` using the SAME
	// canonicalization the feeder used (wire.HashSections — the single
	// shared helper internal/feeder/snapshot also calls, so the two sides
	// cannot drift apart). A snapshot whose `hash` doesn't match its
	// `sections` is rejected before signature verification is even
	// attempted.
	recomputedHash, err := wire.HashSections(body.Sections)
	if err != nil {
		return Applied{}, fmt.Errorf("desiredstate: Pull: recompute hash: %w", err)
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
		return Applied{}, fmt.Errorf("desiredstate: Pull: build signed scope: %w", err)
	}
	if !signhash.Verify(pub, canon, sigBytes) {
		return Applied{}, ErrSnapshotSignatureInvalid
	}

	// 3. Enforce generation monotonicity (REL-052): a generation lower
	// than the persisted last-applied one is rejected outright. An equal
	// generation is not rejected — it is REL-070's no-op case, handled by
	// SetLastAppliedGeneration's own idempotent upsert below (persisting
	// the same {generation, hash} again is a no-op by construction).
	lastGen, _, hasLast, err := store.LastAppliedGeneration()
	if err != nil {
		return Applied{}, fmt.Errorf("desiredstate: Pull: read last-applied generation: %w", err)
	}
	if hasLast && body.Generation < lastGen {
		return Applied{}, ErrGenerationRegressed
	}

	applied, err := extractApplied(body.Generation, body.Sections)
	if err != nil {
		return Applied{}, fmt.Errorf("desiredstate: Pull: %w", err)
	}

	// 4/5. Persist {generation, hash} as last-applied (REL-055) and return
	// the applied screen-program. Only reached once hash + signature have
	// both verified and the generation has not regressed.
	if err := store.SetLastAppliedGeneration(body.Generation, body.Hash); err != nil {
		return Applied{}, fmt.Errorf("desiredstate: Pull: persist last-applied generation: %w", err)
	}

	return applied, nil
}

// fetchSnapshot performs relay/1's desired-state pull (`GET /state/pull`,
// REL-051) against the feeder at feederBaseURL, decoding its full
// state.snapshot body. /state/pull always returns a full snapshot in
// Wave-1 first-photon — internal/feeder/enroll's handler implements no
// `since_generation`/`state.unchanged` branch, so none is coded against
// here.
func fetchSnapshot(feederBaseURL string) (wire.StateSnapshotBody, error) {
	client := bootstrapClient()

	resp, err := client.Get(feederBaseURL + "/state/pull")
	if err != nil {
		return wire.StateSnapshotBody{}, fmt.Errorf("GET /state/pull: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return wire.StateSnapshotBody{}, fmt.Errorf("GET /state/pull: unexpected status %d", resp.StatusCode)
	}

	var body wire.StateSnapshotBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return wire.StateSnapshotBody{}, fmt.Errorf("decode /state/pull response: %w", err)
	}
	return body, nil
}

// bootstrapClient returns an http.Client for the desired-state pull
// exchange. It mirrors internal/relay/enroll's own bootstrapClient
// exactly: server-authenticated TLS with no trust anchor to validate the
// feeder's self-signed listener certificate against — REL-010/011's
// bootstrap exception, made concrete for Wave-1 first-photon's co-located
// feeder+relay loopback deployment. This is deliberately independent of
// REL-071's desired_state_verification_key trust anchor, which this
// package verifies the *snapshot payload's signature* against, not the
// TLS connection.
func bootstrapClient() *http.Client {
	return &http.Client{
		Timeout: pullHTTPTimeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // REL-010/011 bootstrap exception, see doc above
		},
	}
}

// extractApplied builds Pull's returned Applied from a verified snapshot's
// sections: Wave-1 first-photon's one screen-program showing one image
// (internal/feeder/snapshot.Build's own shape). A sections value that
// doesn't carry at least one screen-program with at least one content item
// is a malformed snapshot — refused with a plain error, since it is not one
// of relay/1's own typed rejection reasons (it would mean the feeder itself
// built a malformed sections, not that verification failed).
func extractApplied(generation int64, sections wire.Sections) (Applied, error) {
	if len(sections.ScreenPrograms) == 0 {
		return Applied{}, errors.New("verified sections carried no screen_programs")
	}
	prog := sections.ScreenPrograms[0]
	if len(prog.Content) == 0 {
		return Applied{}, fmt.Errorf("verified screen_program %q carried no content", prog.ScreenID)
	}

	return Applied{
		Generation:      generation,
		ScreenID:        prog.ScreenID,
		ProgramRevision: prog.ProgramRevision,
		Image:           prog.Content[0],
		PairingGrants:   sections.PairingGrants,
	}, nil
}
