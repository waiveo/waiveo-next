package api_test

// This is the make-it-real end-to-end oracle: it wires the app store, the api/1
// authoring handler, the feeder's store-derived signed desired-state
// (snapshot.BuildFromStore), the relay's desired-state apply gate (the hash +
// signature + generation-monotonicity verification the relay's desiredstate.Pull
// performs), and the relay's schedule resolver (schedulehost) IN-PROCESS — no
// network — and proves the whole loop: an authored schedule edit made over the
// api handler changes the program the relay resolves for a screen (A -> B).
//
// A green build with a loop that does NOT propagate an authored change is exactly
// the failure this test guards against: if BuildFromStore stamped a constant
// generation, or the api write did not reach the store, or the resolver read a
// stale section, the display would not flip and this test would fail.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/feeder/signing"
	"github.com/maaxton/waiveo-next/internal/feeder/snapshot"
	"github.com/maaxton/waiveo-next/internal/relay/schedulehost"
	"github.com/maaxton/waiveo-next/internal/shared/signhash"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// Seed fixture ids (mirroring internal/app/store/seed.go's demo — fixture ULIDs,
// no secrets): the screen node the demo schedule governs, and the content daypart
// (06:00-22:00) an authored edit blanks.
const (
	e2eScreenNodeID     = "01J8Z4DEM0SCREENF1RSTPH0TN"
	e2eContentDaypartID = "01J8Z7DEM0DAYPARTC0NTENT01"
)

// e2eContentInstant is a fixed wall-clock instant inside the seed demo's content
// daypart (06:00-22:00 America/Chicago): noon Chicago on a fixed date. Resolution
// is clock-driven off the site node's own tz (DAT-033/034), so pinning the
// instant makes program A / B deterministic regardless of when the test runs.
func e2eContentInstant(t *testing.T) int64 {
	t.Helper()
	loc, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatalf("load America/Chicago: %v", err)
	}
	return time.Date(2026, 1, 15, 12, 0, 0, 0, loc).UnixMilli()
}

// TestAuthoringLoopAPIEditChangesResolvedProgram is the end-to-end oracle: seed
// the demo, resolve program A through the full desired-state path, author a
// schedule change over the api handler (blank the content daypart), and re-resolve
// at the SAME instant — the resolved program MUST change (A: display:content with
// the playlist asset -> B: display:blank with no content), and the store
// generation MUST strictly advance across the edit.
func TestAuthoringLoopAPIEditChangesResolvedProgram(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// Seed the demo (site + screen + playlist + two-daypart schedule) as authored
	// store rows — the same rows a fresh make dev resolves a program from.
	img := []byte("waiveo-next-authoring-loop-e2e-content")
	assetRef := signhash.ContentID(img)
	if err := e.store.SeedDemo(ctx, assetRef); err != nil {
		t.Fatalf("SeedDemo: %v", err)
	}

	// The feeder's desired-state signing identity (a throwaway per-test key).
	id, err := signing.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("signing.LoadOrCreate: %v", err)
	}

	nowMs := e2eContentInstant(t)

	// --- Program A: resolve the seeded schedule through the full desired-state path.
	displayA, contentA, _, genA := resolveThroughDesiredState(t, e.store, img, id, 0, nowMs)
	if displayA != "content" {
		t.Fatalf("program A display = %q, want content (seed content daypart at noon Chicago)", displayA)
	}
	if len(contentA) == 0 {
		t.Fatalf("program A carried no content, want the seeded playlist asset")
	}
	if contentA[0].AssetRef != assetRef {
		t.Fatalf("program A asset_ref = %q, want the seeded playlist asset %q", contentA[0].AssetRef, assetRef)
	}

	// --- Author a schedule change over the api handler: blank the content daypart.
	// GET the daypart's current ETag, then PATCH display_power on -> blank under
	// If-Match — the api/1 optimistic-concurrency convention (the store validates +
	// bumps revision + generation atomically before the write commits).
	getResp, _ := e.do(t, http.MethodGet, "/api/v1/dayparts/"+e2eContentDaypartID, nil, nil)
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET content daypart: status %d", getResp.StatusCode)
	}
	etag := getResp.Header.Get("ETag")
	if etag == "" {
		t.Fatalf("GET content daypart returned no ETag")
	}
	patch := mustJSON(t, map[string]string{"display_power": "blank"})
	patchResp, raw := e.do(t, http.MethodPatch, "/api/v1/dayparts/"+e2eContentDaypartID, patch,
		map[string]string{"If-Match": etag})
	if patchResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH content daypart display_power->blank: status %d, body %s", patchResp.StatusCode, raw)
	}

	// --- Program B: re-resolve at the SAME instant after the authored edit.
	displayB, contentB, _, genB := resolveThroughDesiredState(t, e.store, img, id, genA, nowMs)

	// The store generation strictly advanced across the write (REL-052 monotonicity).
	if !(genB > genA) {
		t.Fatalf("store generation did not advance across the authored edit: %d -> %d", genA, genB)
	}

	// THE ORACLE: the authored edit changed the resolved program A -> B.
	if displayB == displayA {
		t.Fatalf("resolved display did not change across the authored edit: still %q — the loop did not propagate", displayB)
	}
	if displayB != "blank" {
		t.Fatalf("program B display = %q, want blank (content daypart authored to blank)", displayB)
	}
	if len(contentB) != 0 {
		t.Fatalf("program B carried content %v, want none (the daypart is now blank)", contentB)
	}
}

// resolveThroughDesiredState runs the full make-it-real path a relay drives, in
// process and with no network: it derives the store's desired state
// (store.DesiredState), signs it (snapshot.BuildFromStore), verifies it with the
// exact gate the relay's desiredstate.Pull enforces before any section reaches a
// screen — the sections hash to the snapshot's own hash (REL-053), the signature
// verifies under the feeder's signing key (REL-075), and the generation has not
// regressed below the last-applied one (REL-052) — then parses the carried
// schedule section into a data-model RowStore (schedulehost.BuildStore) and
// resolves the governed screen's program at nowMs (schedulehost.ProjectLease),
// exactly as the relay's resolveAndServe does. lastGen is the previously-applied
// generation the monotonicity check runs against (0 for the first apply).
func resolveThroughDesiredState(t *testing.T, st *store.Store, img []byte, id *signing.Identity, lastGen, nowMs int64) (display string, content []wire.LeaseContent, programRevision string, generation int64) {
	t.Helper()

	ds, err := st.DesiredState(context.Background())
	if err != nil {
		t.Fatalf("DesiredState: %v", err)
	}
	snap, err := snapshot.BuildFromStore(ds, img, "https://origin.example", id, nil)
	if err != nil {
		t.Fatalf("BuildFromStore: %v", err)
	}

	// Apply gate: the same verification the relay's desiredstate.Pull performs,
	// reproduced in-process (no /state/pull network hop).
	recomputed, err := wire.HashSections(snap.Sections)
	if err != nil {
		t.Fatalf("HashSections: %v", err)
	}
	if recomputed != snap.Hash {
		t.Fatalf("snapshot hash %q != recomputed %q (REL-053)", snap.Hash, recomputed)
	}
	canon, err := wire.SignedScopeBytes(snap.Generation, snap.Hash)
	if err != nil {
		t.Fatalf("SignedScopeBytes: %v", err)
	}
	sig, err := wire.DecodeSignature(snap.Signature)
	if err != nil {
		t.Fatalf("DecodeSignature: %v", err)
	}
	if !signhash.Verify(id.SigningPub(), canon, sig) {
		t.Fatalf("snapshot signature did not verify under the feeder key (REL-075)")
	}
	if snap.Generation < lastGen {
		t.Fatalf("snapshot generation %d regressed below last-applied %d (REL-052)", snap.Generation, lastGen)
	}

	// Resolve the governed screen's program from the verified schedule section.
	rowStore, errs := schedulehost.BuildStore(snap.Sections.Schedule)
	if len(errs) != 0 {
		t.Fatalf("schedulehost.BuildStore reported errors over the authored schedule: %+v", errs)
	}
	if !schedulehost.Governs(rowStore, e2eScreenNodeID) {
		t.Fatalf("the seeded schedule does not govern screen %s", e2eScreenNodeID)
	}
	display, _, content, programRevision, err = schedulehost.ProjectLease(rowStore, e2eScreenNodeID, nowMs, snap.Sections.RevocationAndSite.ContentOrigin)
	if err != nil {
		t.Fatalf("ProjectLease: %v", err)
	}
	return display, content, programRevision, snap.Generation
}
