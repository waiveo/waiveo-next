package api

// The in-transaction playlist asset guard (playlistAssetGuards, scheduling.go).
//
// These are INTERNAL tests deliberately. The interleaving the guard closes —
// content reclaimed between the api's pre-write asset check and the store write
// it gates — cannot be requested over HTTP, which is exactly why it is dangerous
// and exactly why it has never been observed. Reproducing it needs the guard set
// assembled the way the handler assembles it, with the reclamation placed inside
// the write transaction where a retention sweep holding the store's write lock
// lands it.

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/feeder/origin"
)

// mustJSONBody marshals a row fixture, failing the test rather than the write.
func mustJSONBody(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return b
}

// asValidationError is errors.As for the store's validation error, named so the
// assertion above reads as the claim it is making.
func asValidationError(err error, target **store.ValidationError) bool {
	return errors.As(err, target)
}

// guardTestScopeNode is a canonical ULID a playlist row can be placed under. The
// store validates scheduling rows against each other, not against the scope tree,
// so a well-formed node id is all a standalone playlist needs.
const guardTestScopeNode = "01J8ZG000000000000000000S1"

// guardTestPlaylistID is the playlist row these cases write.
const guardTestPlaylistID = "01J8ZG000000000000000000P1"

// newGuardTestFixture returns a dir-backed content origin holding one asset, an
// empty store, and the playlist body naming that asset — the state a client is in
// the instant before its playlist create reaches the store.
func newGuardTestFixture(t *testing.T) (*origin.Store, *store.Store, string, []byte) {
	t.Helper()
	content, err := origin.Open(t.TempDir())
	if err != nil {
		t.Fatalf("origin.Open: %v", err)
	}
	assetRef, err := content.Add([]byte("waiveo-next: content reclaimed mid-transaction"))
	if err != nil {
		t.Fatalf("origin.Add: %v", err)
	}
	st, err := store.Open(":memory:", store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	// The node the fixture playlist is placed at. A row's scope_node is a
	// reference the store resolves (DAT-006) and its refusal is ALSO a
	// REFERENCE_INVALID, so a fixture that skipped this would make the guard's own
	// refusal indistinguishable from a fixture mistake — and would make the
	// guard-disabled case below fail for a reason that has nothing to do with the
	// guard.
	if _, err := st.Create(context.Background(), store.KindScopeNode, mustJSONBody(t, datamodel.ScopeNode{
		ID: guardTestScopeNode, Kind: "org", Name: "Fixture Org",
		AccountState: "active", Entitlements: json.RawMessage(`{}`),
	})); err != nil {
		t.Fatalf("seed the fixture scope node: %v", err)
	}

	body := mustJSONBody(t, datamodel.Playlist{
		ID: guardTestPlaylistID, ScopeNode: guardTestScopeNode, Name: "Guarded",
		Items: []datamodel.PlaylistItem{{Source: "asset", AssetRef: assetRef}},
	})
	return content, st, assetRef, body
}

// reclaimDuringWrite is the retention sweep's deletion, expressed as a WriteGuard
// so it lands INSIDE the write transaction. Guards run in order, so passing this
// ahead of the asset guard reproduces the exact interleaving: the api's pre-write
// check has already passed, the row has not been written yet, and the asset
// disappears.
func reclaimDuringWrite(content *origin.Store, assetRef string) store.WriteGuard {
	return func([]store.Resource) error {
		return content.Remove(strings.TrimPrefix(assetRef, "sha256:"))
	}
}

// TestPlaylistAssetGuardRefusesAWriteWhoseAssetIsReclaimedMidTransaction is the
// guard doing its job: the write is refused and nothing is persisted.
func TestPlaylistAssetGuardRefusesAWriteWhoseAssetIsReclaimedMidTransaction(t *testing.T) {
	content, st, assetRef, body := newGuardTestFixture(t)
	srv := &server{content: content}

	guards := append([]store.WriteGuard{reclaimDuringWrite(content, assetRef)},
		playlistAssetGuards(srv, body)...)

	_, err := st.Create(context.Background(), store.KindPlaylist, body, guards...)
	if err == nil {
		t.Fatal("a playlist referencing content reclaimed inside its own write transaction was STORED: " +
			"the client is answered 201 and every screen that plays it fetches a 404")
	}
	var verr *store.ValidationError
	if !asValidationError(err, &verr) {
		t.Fatalf("write failed with %v, want a *store.ValidationError the api renders as 422/VALIDATION_FAILED", err)
	}
	if len(verr.Errors) != 1 || verr.Errors[0].Code != "REFERENCE_INVALID" {
		t.Fatalf("validation errors = %+v, want one REFERENCE_INVALID naming the asset", verr.Errors)
	}
	if !strings.Contains(verr.Errors[0].Message, assetRef) {
		t.Fatalf("error message %q does not name the missing asset %q", verr.Errors[0].Message, assetRef)
	}

	if _, found, err := st.Get(context.Background(), store.KindPlaylist, guardTestPlaylistID); err != nil || found {
		t.Fatalf("the refused write left a row behind: found=%v err=%v", found, err)
	}
}

// TestPlaylistAssetGuardDisabledStoresADanglingReference is the same case with
// the guard REMOVED from the guard set — the state this codebase was in before it
// existed. It must fail: a playlist is stored naming content the origin cannot
// serve.
//
// Without this case the one above proves only that some write failed. With it,
// the difference between a stored dangling reference and a refusal is attributed
// to the guard and to nothing else — same store, same origin, same body, same
// reclamation, one guard's worth of difference.
func TestPlaylistAssetGuardDisabledStoresADanglingReference(t *testing.T) {
	content, st, assetRef, body := newGuardTestFixture(t)

	guards := []store.WriteGuard{reclaimDuringWrite(content, assetRef)}

	if _, err := st.Create(context.Background(), store.KindPlaylist, body, guards...); err != nil {
		t.Fatalf("write with the guard disabled = %v, want it to succeed (that is the bug being pinned)", err)
	}
	if _, found, err := st.Get(context.Background(), store.KindPlaylist, guardTestPlaylistID); err != nil || !found {
		t.Fatalf("expected the unguarded write to store the row: found=%v err=%v", found, err)
	}
	if content.Has(strings.TrimPrefix(assetRef, "sha256:")) {
		t.Fatal("the stand-in reclamation did not remove the asset; this case is not reproducing the race")
	}
	// The row is stored, the bytes are gone: every screen this playlist reaches
	// fetches a 404. That is what the guard prevents.
}

// TestPlaylistWriteGuardsAreAssembledForThePlaylistKind proves the guard is
// reached through the SAME assembly the handlers use (resource.writeGuards),
// rather than merely existing as a function.
func TestPlaylistWriteGuardsAreAssembledForThePlaylistKind(t *testing.T) {
	content, _, assetRef, body := newGuardTestFixture(t)
	rs := &resource{srv: &server{content: content}, cfg: playlistsConfig()}

	guards := rs.writeGuards(parseFields(body), "", body)
	if len(guards) != 1 {
		t.Fatalf("playlist write guards = %d, want exactly 1 (the asset guard; this body carries no external_id)", len(guards))
	}
	if err := guards[0](nil); err != nil {
		t.Fatalf("the assembled guard rejected a present asset: %v", err)
	}
	if err := content.Remove(strings.TrimPrefix(assetRef, "sha256:")); err != nil {
		t.Fatalf("origin.Remove: %v", err)
	}
	if err := guards[0](nil); err == nil {
		t.Fatal("the assembled guard accepted a playlist naming content the origin no longer holds")
	}
}

// TestEveryResourceWriteAssemblesItsWriteGuards guards the half a behavioural
// test cannot reach: that EVERY verb which opens a resource write goes through
// resource.writeGuards.
//
// A regression that dropped the call from one of them — reverting the patch path
// to rs.externalIDGuards, say — would leave every existing test in this package
// green, because the external_id rules it would still enforce are the ones every
// other case is about, and the asset guard's absence is invisible until a
// retention sweep and a playlist write land in the same microsecond on a real
// box. So this reads the source: any function that calls store.Create or
// store.Update must also assemble its guards.
//
// Stating it as "whatever writes" rather than naming create and patch is what
// makes it survive the next verb somebody adds. It catches the call being
// deleted, not the call being made unreachable — the same limit
// TestMainStartsTheConsoleBinding records for its own AST check.
func TestEveryResourceWriteAssemblesItsWriteGuards(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "api.go", nil, 0)
	if err != nil {
		t.Fatalf("parse api.go: %v", err)
	}
	writers := 0
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil {
			continue
		}
		writes, guards := false, false
		ast.Inspect(fn, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "Create", "Update":
				// store.Create / store.Update, reached as rs.srv.store.X.
				if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "store" {
					writes = true
				}
			case "writeGuards":
				guards = true
			}
			return true
		})
		if !writes {
			continue
		}
		writers++
		if !guards {
			t.Errorf("api.go: %s opens a resource write without calling rs.writeGuards — "+
				"the per-kind write guards, including the playlist asset guard, would not run on that path", fn.Name.Name)
		}
	}
	if writers < 2 {
		t.Fatalf("found %d resource-write function(s) in api.go, want at least the create and patch paths; "+
			"this check is looking for the wrong thing and would pass on anything", writers)
	}
}
