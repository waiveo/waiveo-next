package datamodel

import (
	"encoding/json"
	"testing"
)

// playlistcontenttype_test.go drives the DAT-041 `content_type` rules on a
// playlist item — the field that decides whether a scheduled asset PLAYS as a
// video or is drawn as a still image.
//
// The stakes are why this is validated at all rather than left permissive: the
// value rides untouched from this row, through both content projections, onto
// the player/1 Lease content item's `type`, which is what a player switches its
// renderer on and what the relay's content-type filter admits an item by. A bad
// value there does not produce an error anywhere — it produces a screen showing
// nothing.

// playlistWithItem is one well-formed playlist row carrying a single item, as
// raw JSON — the form ValidateRows actually consumes (a raw bundle), so these
// cases exercise the decode as well as the rules.
func playlistWithItem(item string) json.RawMessage {
	return json.RawMessage(`{"id":"01J8ZPLAYLIST00000000000001","scope_node":"01J8ZNODE0000000000000001","name":"p","items":[` + item + `],"revision":1,"created_at":1,"updated_at":1}`)
}

// TestPlaylistItemContentTypeVocabulary: a stated content_type must be one of
// the closed set, and the two legal values are accepted.
func TestPlaylistItemContentTypeVocabulary(t *testing.T) {
	cases := []struct {
		name    string
		item    string
		wantErr bool
	}{
		{"image is legal", `{"source":"asset","asset_ref":"sha256:aa","content_type":"image"}`, false},
		{"video is legal", `{"source":"asset","asset_ref":"sha256:aa","content_type":"video"}`, false},
		{"absent is legal (projected as image, REL-061a)", `{"source":"asset","asset_ref":"sha256:aa"}`, false},
		{"an unknown type is refused", `{"source":"asset","asset_ref":"sha256:aa","content_type":"audio"}`, true},
		// `slide` is a real content type on the WIRE, which is exactly why it
		// must be refused here: it is not something an asset item can be, and
		// accepting it would produce a Lease item claiming to carry layers it
		// has none of.
		{"the wire's slide type is refused on an asset item", `{"source":"asset","asset_ref":"sha256:aa","content_type":"slide"}`, true},
		{"case matters", `{"source":"asset","asset_ref":"sha256:aa","content_type":"Video"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, errs := ValidateRows(RawRows{Playlists: []json.RawMessage{playlistWithItem(tc.item)}})
			got := hasErr(errs, "PLAYLIST_ITEM_CONTENT_TYPE_INVALID", "items[0].content_type")
			if got != tc.wantErr {
				t.Errorf("content_type rejected = %v, want %v; errs = %+v", got, tc.wantErr, errs)
			}
		})
	}
}

// TestPlaylistItemContentTypeOnlyOnAnAssetItem: a content_type on a source whose
// content type is decided by the source itself is refused rather than stored.
//
// This is the accepts-work-it-never-performs guard. A `cast` item projects to
// slide items whatever this field says, so storing "video" on one would record
// an operator intent that no projection reads and no screen honours — and the
// operator would have no way to tell, because the write succeeds and the screen
// keeps showing slides.
func TestPlaylistItemContentTypeOnlyOnAnAssetItem(t *testing.T) {
	for _, item := range []struct {
		name string
		json string
	}{
		{"cast", `{"source":"cast","cast_id":"01J8ZCAST00000000000000001","content_type":"video"}`},
		{"playable", `{"source":"playable","pack_id":"acme","content_id":"c","content_type":"video"}`},
		{"slide", `{"source":"slide","slide":{"layers":[{"kind":"rect","x":0,"y":0,"w":10,"h":10,"color":"#ffffff"}]},"content_type":"video"}`},
	} {
		t.Run(item.name, func(t *testing.T) {
			_, errs := ValidateRows(RawRows{Playlists: []json.RawMessage{playlistWithItem(item.json)}})
			if !hasErr(errs, "PLAYLIST_ITEM_CONTENT_TYPE_INVALID", "items[0].content_type") {
				t.Errorf("a %s item carrying content_type was accepted; errs = %+v", item.name, errs)
			}
		})
	}
}

// TestPlaylistItemContentTypeReportsEveryBadItem: a playlist is a document an
// operator edits whole, so every failing item is named at once — the same
// multi-error contract (API-013) checkCastSlides honours for a cast's slides.
// A validator that stopped at the first would make fixing a ten-item playlist a
// ten-round trip.
func TestPlaylistItemContentTypeReportsEveryBadItem(t *testing.T) {
	pl := json.RawMessage(`{"id":"01J8ZPLAYLIST00000000000001","scope_node":"01J8ZNODE0000000000000001","name":"p","items":[` +
		`{"source":"asset","asset_ref":"sha256:aa","content_type":"audio"},` +
		`{"source":"asset","asset_ref":"sha256:bb","content_type":"video"},` +
		`{"source":"asset","asset_ref":"sha256:cc","content_type":"gif"}` +
		`],"revision":1,"created_at":1,"updated_at":1}`)
	_, errs := ValidateRows(RawRows{Playlists: []json.RawMessage{pl}})
	if !hasErr(errs, "PLAYLIST_ITEM_CONTENT_TYPE_INVALID", "items[0].content_type") {
		t.Errorf("item 0's bad content_type was not reported; errs = %+v", errs)
	}
	if !hasErr(errs, "PLAYLIST_ITEM_CONTENT_TYPE_INVALID", "items[2].content_type") {
		t.Errorf("item 2's bad content_type was not reported — validation stopped early; errs = %+v", errs)
	}
	if hasErr(errs, "PLAYLIST_ITEM_CONTENT_TYPE_INVALID", "items[1].content_type") {
		t.Errorf("the valid item 1 was reported as failing; errs = %+v", errs)
	}
}

// TestPlaylistItemContentTypeRoundTripsOmitEmpty: the field is additive, so an
// item that states none marshals with no `content_type` key at all — the
// byte-identical-to-before property every additive field in this codebase
// carries, and the reason adding it changes no existing snapshot's hash.
func TestPlaylistItemContentTypeRoundTripsOmitEmpty(t *testing.T) {
	raw, err := json.Marshal(PlaylistItem{Source: PlaylistSourceAsset, AssetRef: "sha256:aa"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := m["content_type"]; ok {
		t.Errorf("an item with no content_type marshaled the key anyway: %s", raw)
	}

	raw, err = json.Marshal(PlaylistItem{Source: PlaylistSourceAsset, AssetRef: "sha256:aa", ContentType: PlaylistContentTypeVideo})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back PlaylistItem
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("round-trip Unmarshal: %v", err)
	}
	if back.ContentType != PlaylistContentTypeVideo {
		t.Errorf("content_type did not round-trip: got %q", back.ContentType)
	}
}
