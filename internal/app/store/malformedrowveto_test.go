package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/maaxton/waiveo-next/internal/app/store"
	"github.com/maaxton/waiveo-next/internal/datamodel"
	"github.com/maaxton/waiveo-next/internal/shared/wire"
)

// malformedrowveto_test.go pins ONE property of every write path this branch
// touches: a malformed row ALREADY IN THE STORE must not veto an unrelated write.
//
// # Why this needs a test rather than an argument
//
// validateAfterWrite (scheduling.go) judges a write by re-validating the WHOLE
// row-set of the kind it touched. That is what makes a cross-row rule
// enforceable — a cast delete is refused while a playlist still names it — and
// it means, unmodified, that any row no current validator accepts refuses every
// future write of its kind. CREATE, UPDATE and DELETE alike; and with DELETE
// gone, there is no repair path left through the API at all, only raw SQL
// against the database file.
//
// So an authoring gate added AFTER rows exist is not a local change. It is a
// change to whether the surface still works, and the rows it retroactively
// condemns are real: a `source: "slide"` playlist item's inline layer stack
// passes no authoring gate on this branch, so `POST /playlists` answers 201 for
// a zero-layer inline slide, for a `derive` layer with no spec, and for an
// unknown layer kind. A workspace restore and a seed bundle deliver the same
// shapes. This branch adds an off-appliance renderer that has to cope with every
// one of them; what it must not do is make them fatal to the store.
//
// # What this branch does about the malformed rows themselves
//
// Not this. The defences live where the damage was: GET /derive/pending omits a
// layer it knows is undrawable rather than serving a work order that cannot be
// filled, and internal/derive refuses a spec-less layer and renders every unit
// under a recover(), so one bad row costs its own layer and never the pass. An
// inline AUTHORING gate is being added on the interactive-layers track, together
// with the prior-fault diff (priorfaults.go) that makes adding one safe; this
// branch deliberately does not ship a second copy of either.
//
// This test is what keeps that division honest. If a gate is ever added here
// without the diff underneath it, these three operations start failing, and they
// fail with a message naming `items[0].slide.layers` of a body that has no slide
// in it — the field path is row-relative, so the fault reported belongs to a
// different row entirely.

const (
	vetoScopeNodeID  = "01J9D0VET0SC0PEN0DEF1XTRES"
	vetoPlantedID    = "01J9D1VET0P1ANTEDBADR0WAAA"
	vetoBystanderID  = "01J9D2VET0BYSTANDERR0WBBBB"
	vetoNewPlaylistD = "01J9D3VET0NEWAT0DAYR0WCCCC"
)

// vetoValidPlaylist is a playlist nothing objects to, at either end of the merge:
// one `asset` item, which is the shape every branch's validators agree on.
func vetoValidPlaylist(id, name string) datamodel.Playlist {
	return datamodel.Playlist{
		ID: id, ScopeNode: vetoScopeNodeID, Name: name,
		Items: []datamodel.PlaylistItem{{
			Source: "asset", AssetRef: "sha256:" +
				"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			ContentType: "image",
		}},
	}
}

// plantMalformedPlaylist writes a playlist body straight into the file through a
// second connection, bypassing every validator the store interposes.
//
// Planting rather than creating is deliberate even where a create would work
// today: the row under test is one an OLDER build stored, and a fixture that
// depends on the current build accepting it stops testing anything the moment
// the gate it is defending against arrives.
func plantMalformedPlaylist(t *testing.T, dsn, id, name string, items string) {
	t.Helper()
	body := `{"id":"` + id + `","scope_node":"` + vetoScopeNodeID + `","name":"` + name + `",` +
		`"items":` + items + `,"revision":1,"created_at":1,"updated_at":1}`
	withRawDB(t, dsn, func(db *sql.DB) {
		if _, err := db.Exec(
			`INSERT INTO playlists (id, revision, external_id, labels, scope_node, created_at, updated_at, body)
			 VALUES (?, 1, '', '{}', ?, 1, 1, ?)`, id, vetoScopeNodeID, body); err != nil {
			t.Fatalf("plant %s: %v", id, err)
		}
	})
}

// TestAMalformedStoredRowVetoesNoUnrelatedWrite drives all three operations
// against a store holding a planted row the derive track's own renderer is built
// to survive: a `source: "slide"` item whose inline layer stack is EMPTY.
func TestAMalformedStoredRowVetoesNoUnrelatedWrite(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "feeder-store.db")

	s := openStoreAt(t, dsn)
	if _, err := s.Create(ctx, store.KindScopeNode, mustJSON(t, datamodel.ScopeNode{
		ID: vetoScopeNodeID, Kind: "org", Name: "Veto Fixture Org",
		AccountState: "active", Entitlements: json.RawMessage(`{}`),
	})); err != nil {
		t.Fatalf("seed the scope node: %v", err)
	}
	bystander, err := s.Create(ctx, store.KindPlaylist,
		mustJSON(t, vetoValidPlaylist(vetoBystanderID, "Innocent Bystander")))
	if err != nil {
		t.Fatalf("create the bystander playlist: %v", err)
	}

	plantMalformedPlaylist(t, dsn, vetoPlantedID, "Stored By An Older Build",
		`[{"source":"slide","slide":{"layers":[]}}]`)

	// CREATE — a playlist that has nothing to do with the planted row.
	if _, err := s.Create(ctx, store.KindPlaylist,
		mustJSON(t, vetoValidPlaylist(vetoNewPlaylistD, "Authored Today"))); err != nil {
		t.Fatalf("CREATE of an unrelated, valid playlist = %v, want success — one stored row "+
			"the current validators refuse must not veto a write that did not touch it, and the "+
			"refusal names an item index of a body that carries no slide at all", err)
	}

	// UPDATE — a rename of the bystander.
	renamed := vetoValidPlaylist(vetoBystanderID, "Renamed Bystander")
	if _, err := s.Update(ctx, store.KindPlaylist, bystander.ID, bystander.Revision,
		mustJSON(t, renamed)); err != nil {
		t.Fatalf("UPDATE of an unrelated playlist = %v, want success", err)
	}

	// DELETE — the repair path. This is the one that matters most: refuse it and
	// an operator cannot get out of the state through the API at all.
	cur, found, err := s.Get(ctx, store.KindPlaylist, vetoBystanderID)
	if err != nil || !found {
		t.Fatalf("re-read the bystander: %v (found=%v)", err, found)
	}
	if err := s.Delete(ctx, store.KindPlaylist, cur.ID, cur.Revision); err != nil {
		t.Fatalf("DELETE of an unrelated playlist = %v, want success — DELETE is the only repair "+
			"path there is, and a store that refuses it is recoverable only with raw SQL", err)
	}
}

// TestEveryMalformedInlineShapeTheRendererMustSurviveIsStorable is the input
// half of the derive track's guarantees, stated as a fact about the store rather
// than assumed by the tool.
//
// internal/derive's refusal-plus-recover and GET /derive/pending's omission are
// each written for a specific malformed shape. This is what says those shapes
// really can be in a store — so if a future change makes one of them unstorable,
// the guard written for it becomes untested by construction rather than
// silently, and the two stay honest about which side is doing the work.
func TestEveryMalformedInlineShapeTheRendererMustSurviveIsStorable(t *testing.T) {
	ctx := context.Background()

	shapes := []struct {
		name  string
		items string
	}{
		{"a zero-layer inline slide", `[{"source":"slide","slide":{"layers":[]}}]`},
		{"a derive layer with no spec", `[{"source":"slide","slide":{"layers":[{"kind":"derive","x":0,"y":0,"w":400,"h":400}]}}]`},
		{"an unknown layer kind", `[{"source":"slide","slide":{"layers":[{"kind":"hologram","x":0,"y":0,"w":100,"h":100}]}}]`},
		{"geometry off the canvas", `[{"source":"slide","slide":{"layers":[{"kind":"rect","x":1900,"y":0,"w":100,"h":100,"color":"#ffffff"}]}}]`},
	}

	for _, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "feeder-store.db")
			s := openStoreAt(t, dsn)
			if _, err := s.Create(ctx, store.KindScopeNode, mustJSON(t, datamodel.ScopeNode{
				ID: vetoScopeNodeID, Kind: "org", Name: "Veto Fixture Org",
				AccountState: "active", Entitlements: json.RawMessage(`{}`),
			})); err != nil {
				t.Fatalf("seed the scope node: %v", err)
			}
			plantMalformedPlaylist(t, dsn, vetoPlantedID, "Stored By An Older Build", sh.items)

			// The row is readable, and the store still takes writes over it.
			if _, found, err := s.Get(ctx, store.KindPlaylist, vetoPlantedID); err != nil || !found {
				t.Fatalf("the planted row is not readable: %v (found=%v)", err, found)
			}
			if _, err := s.Create(ctx, store.KindPlaylist,
				mustJSON(t, vetoValidPlaylist(vetoNewPlaylistD, "Authored Today"))); err != nil {
				t.Fatalf("a write over the planted row = %v, want success", err)
			}
		})
	}
}

// TestAWriteThatItselfBreaksARuleIsStillRefused is the mirror direction, and it
// is the one this repo keeps getting wrong: the guard is relaxed and then it
// stops firing on the thing it was for. Tolerating an INHERITED fault must not
// tolerate a fault the caller is submitting right now.
func TestAWriteThatItselfBreaksARuleIsStillRefused(t *testing.T) {
	ctx := context.Background()
	dsn := filepath.Join(t.TempDir(), "feeder-store.db")

	s := openStoreAt(t, dsn)
	if _, err := s.Create(ctx, store.KindScopeNode, mustJSON(t, datamodel.ScopeNode{
		ID: vetoScopeNodeID, Kind: "org", Name: "Veto Fixture Org",
		AccountState: "active", Entitlements: json.RawMessage(`{}`),
	})); err != nil {
		t.Fatalf("seed the scope node: %v", err)
	}
	plantMalformedPlaylist(t, dsn, vetoPlantedID, "Stored By An Older Build",
		`[{"source":"slide","slide":{"layers":[]}}]`)

	// A cast slide's layer stack IS gated at authoring time, on every branch.
	bad := datamodel.Cast{
		ID: "01J9D4VET0BADCASTR0WDDDDDD", ScopeNode: vetoScopeNodeID, Name: "Bad Cast",
		Slides: []datamodel.CastSlide{{ID: "s1", Layers: []wire.Layer{
			{Kind: "hologram", X: 0, Y: 0, W: 100, H: 100},
		}}},
	}
	if _, err := s.Create(ctx, store.KindCast, mustJSON(t, bad)); err == nil {
		t.Fatal("a write submitting its OWN invalid layer stack was accepted; tolerating inherited " +
			"faults must never tolerate introduced ones")
	}
}
