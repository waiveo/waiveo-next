package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
	_ "modernc.org/sqlite"
)

const (
	refsScopeNode = "01J8ZJ000000000000000000S1"
	refsPlaylistA = "01J8ZJ000000000000000000A1"
	refsPlaylistB = "01J8ZJ000000000000000000B2"
	refsCastA     = "01J8ZJ000000000000000000C1"
)

func refsPlaylist(t *testing.T, id string, items ...datamodel.PlaylistItem) []byte {
	t.Helper()
	b, err := json.Marshal(datamodel.Playlist{
		ID: id, ScopeNode: refsScopeNode, Name: "Playlist " + id, Items: items,
	})
	if err != nil {
		t.Fatalf("marshal playlist: %v", err)
	}
	return b
}

// TestWithContentReferencesProjectsEveryPlaylistItem pins the projection: hex
// digests with the `sha256:` prefix stripped (the content origin's own key),
// every playlist row counted, pack `playable` items — which resolve inside a pack
// and reference no origin content — contributing nothing.
func TestWithContentReferencesProjectsEveryPlaylistItem(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:", store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	seedPlacementNode(t, st, refsScopeNode)

	if _, err := st.Create(ctx, store.KindPlaylist, refsPlaylist(t, refsPlaylistA,
		datamodel.PlaylistItem{Source: "asset", AssetRef: "sha256:aa11"},
		datamodel.PlaylistItem{Source: "playable", PackID: "acme.signage", ContentID: "hero"},
	)); err != nil {
		t.Fatalf("create playlist A: %v", err)
	}
	if _, err := st.Create(ctx, store.KindPlaylist, refsPlaylist(t, refsPlaylistB,
		datamodel.PlaylistItem{Source: "asset", AssetRef: "sha256:bb22"},
		// The same asset in two playlists is one reference, not two.
		datamodel.PlaylistItem{Source: "asset", AssetRef: "sha256:aa11"},
	)); err != nil {
		t.Fatalf("create playlist B: %v", err)
	}
	wantGen, err := st.Generation(ctx)
	if err != nil {
		t.Fatalf("read generation: %v", err)
	}

	var got store.ContentReferences
	if err := st.WithContentReferences(ctx, func(refs store.ContentReferences) error {
		got = refs
		return nil
	}); err != nil {
		t.Fatalf("WithContentReferences: %v", err)
	}

	if len(got.Digests) != 2 || !got.Digests["aa11"] || !got.Digests["bb22"] {
		t.Fatalf("digests = %v, want exactly {aa11, bb22} unprefixed", got.Digests)
	}
	if got.Generation != wantGen {
		t.Fatalf("generation = %d, want %d", got.Generation, wantGen)
	}
	if got.SourceRows != 2 {
		t.Fatalf("source rows = %d, want 2 — a reader cannot tell an empty workspace "+
			"from an unread table without this", got.SourceRows)
	}
}

// TestWithContentReferencesCountsACastsSlideImages is the store half of the
// data-loss defect: the reference set the retention sweep acts on read ONLY
// playlist rows' item.asset_ref, so an asset whose only reference is a cast's
// image layer came back unreferenced and was permanently deleted while every
// screen playing that cast went blank.
//
// The inline `source: "slide"` item in the same playlist is the identical hole
// on the playlist side — its content is its layer stack, so it carries no
// item-level asset_ref for the old projection to find.
func TestWithContentReferencesCountsACastsSlideImages(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:", store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	seedPlacementNode(t, st, refsScopeNode)

	if _, err := st.Create(ctx, store.KindPlaylist, refsPlaylist(t, refsPlaylistA,
		datamodel.PlaylistItem{Source: "asset", AssetRef: "sha256:aa11"},
		datamodel.PlaylistItem{Source: "slide", Slide: &datamodel.Slide{
			Layers: []wire.Layer{imageLayer("sha256:bb22")},
		}},
	)); err != nil {
		t.Fatalf("create the playlist: %v", err)
	}
	castBody, err := json.Marshal(datamodel.Cast{
		ID: refsCastA, ScopeNode: refsScopeNode, Name: "Lunch Menu",
		Slides: []datamodel.CastSlide{{ID: "photo", Layers: []wire.Layer{imageLayer("sha256:cc33")}}},
	})
	if err != nil {
		t.Fatalf("marshal the cast: %v", err)
	}
	if _, err := st.Create(ctx, store.KindCast, castBody); err != nil {
		t.Fatalf("create the cast: %v", err)
	}

	var got store.ContentReferences
	if err := st.WithContentReferences(ctx, func(refs store.ContentReferences) error {
		got = refs
		return nil
	}); err != nil {
		t.Fatalf("WithContentReferences: %v", err)
	}

	for _, want := range []string{"aa11", "bb22", "cc33"} {
		if !got.Digests[want] {
			t.Errorf("digest %q is missing from the reference set %v — the sweep would reclaim content a screen is playing", want, got.Digests)
		}
	}
	if got.SourceRows != 2 {
		t.Errorf("source rows = %d, want 2 (one playlist + one cast); a cast row the sweep never read is a cast whose "+
			"images it cannot see", got.SourceRows)
	}
}

// TestWithContentReferencesAbortsOnAnUndecodablePlaylist is the difference
// between a sweep that keeps live content and one that deletes it.
//
// A playlist row whose body will not decode has references this store cannot
// enumerate. Skipping it — which is what the workspace export does, correctly,
// because an export that drops one row is still an export — would report those
// references as ABSENT, and the caller of this method deletes what is absent. So
// this aborts, and the callback never runs.
func TestWithContentReferencesAbortsOnAnUndecodablePlaylist(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "refs.db")
	st, err := store.Open(path, store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	// The malformed row is inserted through a second connection because no write
	// path in this package can produce one: every writer validates. That is the
	// point — the row this guard defends against comes from corruption, a partial
	// restore, or a future writer, not from the api.
	db, err := sql.Open("sqlite", "file:"+path+"?_txlock=immediate&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open a second connection: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO playlists (id, revision, external_id, labels, scope_node, created_at, updated_at, body)
		 VALUES (?, 1, '', '{}', ?, 0, 0, ?)`,
		refsPlaylistA, refsScopeNode, `{"items": "this is not an array"}`); err != nil {
		t.Fatalf("insert a malformed playlist: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close the second connection: %v", err)
	}

	called := false
	err = st.WithContentReferences(ctx, func(store.ContentReferences) error {
		called = true
		return nil
	})
	if err == nil {
		t.Fatal("a playlist body that will not decode was accepted; its references would be reported absent and its content reclaimed")
	}
	if called {
		t.Fatal("the callback ran with a reference set the store could not vouch for")
	}
}

// TestWithContentReferencesHoldsTheWriteLock is the atomicity property the whole
// content sweep rests on: no resource write can commit between the reference read
// and whatever the callback does about it.
//
// Without it, a playlist naming an asset can be stored in the window between
// "nothing references this" and the unlink, and the api answers 201 for a
// playlist that resolves to a 404 on every screen.
//
// WHAT THIS DOES NOT CATCH, stated rather than assumed: the assertion is that a
// concurrent write does not complete within a generous grace period while the
// callback holds, and then does complete promptly once it releases. A machine on
// which an in-memory SQLite insert genuinely took longer than the grace period
// would pass the first half for the wrong reason — which is why the second half
// is asserted too, so a write that was merely slow rather than blocked shows up
// as a failure to finish after the release.
func TestWithContentReferencesHoldsTheWriteLock(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:", store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()
	seedPlacementNode(t, st, refsScopeNode)

	writerStarted := make(chan struct{})
	writerDone := make(chan error, 1)
	committedInside := false

	if err := st.WithContentReferences(ctx, func(store.ContentReferences) error {
		go func() {
			close(writerStarted)
			_, err := st.Create(ctx, store.KindPlaylist, refsPlaylist(t, refsPlaylistA,
				datamodel.PlaylistItem{Source: "asset", AssetRef: "sha256:cc33"}))
			writerDone <- err
		}()
		<-writerStarted
		select {
		case <-writerDone:
			committedInside = true
		case <-time.After(250 * time.Millisecond):
		}
		return nil
	}); err != nil {
		t.Fatalf("WithContentReferences: %v", err)
	}

	if committedInside {
		t.Fatal("a playlist write committed while the reference set was being acted on; " +
			"a sweep could delete an asset the very next committed row references")
	}
	select {
	case err := <-writerDone:
		if err != nil {
			t.Fatalf("the blocked write failed after the lock was released: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the write never completed after the lock was released; it was not merely blocked by it")
	}

	// And the write it was blocked on is visible to the next read, so the
	// serialization is a delay rather than a loss.
	var after store.ContentReferences
	if err := st.WithContentReferences(ctx, func(refs store.ContentReferences) error {
		after = refs
		return nil
	}); err != nil {
		t.Fatalf("second WithContentReferences: %v", err)
	}
	if !after.Digests["cc33"] {
		t.Fatalf("digests after the blocked write = %v, want it to include cc33", after.Digests)
	}
}

// TestWithContentReferencesPropagatesTheCallbackError pins that a failing
// callback is not swallowed: the sweep's own errors have to reach its caller,
// because "the sweep ran" and "the sweep worked" are the two things an operator
// needs to be able to tell apart.
func TestWithContentReferencesPropagatesTheCallbackError(t *testing.T) {
	st, err := store.Open(":memory:", store.WallClockMs)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	want := errTestCallback
	if got := st.WithContentReferences(context.Background(), func(store.ContentReferences) error {
		return want
	}); got != want {
		t.Fatalf("WithContentReferences returned %v, want the callback's own error %v", got, want)
	}
}

type testCallbackError struct{}

func (testCallbackError) Error() string { return "the callback failed" }

var errTestCallback = testCallbackError{}
