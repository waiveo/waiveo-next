package store

import (
	"encoding/json"
	"sort"
	"testing"
)

// derivedmembers_internal_test.go pins the two things about the derived-member
// strip that behaviour alone cannot: the LIST of shapes it covers, and the
// fidelity of a body it did not have to touch.
//
// The list is the whole defect. The first cut covered a cast's
// `slides[].layers[]` and returned every other kind unchanged, so a playlist
// item's `items[].slide.layers[]` — the same authored layer stack, from the same
// media picker, through the same Store.Create — persisted an expiring url
// verbatim. assetrefs.go's own doc records the identical failure happening three
// times in three hand-written projections; this is the guard that stops the
// fourth.

// TestEveryAssetBearingKindHasADerivedStrip is the anti-drift assertion, in the
// shape placement_internal_test.go uses for the same class of problem: the set
// of kinds the strip knows about must be EXACTLY AssetBearingKinds.
//
// Both lists answer "which row kinds carry an authored layer stack or an asset
// reference?" — assetrefs.go for the three whole-workspace consumers, this file
// for the write path. A kind in one and not the other is a shape that is swept,
// exported and validated but writes a dead link into its own document (or the
// reverse). Adding a kind to AssetBearingKinds now fails here until its strip
// exists, which is the only version of this that cannot rot.
func TestEveryAssetBearingKindHasADerivedStrip(t *testing.T) {
	names := func(ks []Kind) []string {
		out := make([]string, 0, len(ks))
		for _, k := range ks {
			out = append(out, string(k))
		}
		sort.Strings(out)
		return out
	}
	stripped := make([]Kind, 0, len(derivedMemberStrippers))
	for k := range derivedMemberStrippers {
		stripped = append(stripped, k)
	}

	want := names(AssetBearingKinds)
	got := names(stripped)
	if len(want) != len(got) {
		t.Fatalf("derivedMemberStrippers covers %v; AssetBearingKinds is %v.\n"+
			"A kind that can name content can carry a derived `url`, and the two lists disagreeing means one shape is "+
			"swept and exported but writes an expiring url into its own row — the exact per-shape blind spot "+
			"assetrefs.go's doc says cost this codebase three copies of one bug.", got, want)
	}
	for i := range want {
		if want[i] != got[i] {
			t.Errorf("derivedMemberStrippers covers %v, AssetBearingKinds is %v — they must be the same set", got, want)
		}
	}
}

// TestAPlaylistInlineSlideLayerURLIsStripped is the shape the first cut missed,
// asserted at the source rather than only through the api.
func TestAPlaylistInlineSlideLayerURLIsStripped(t *testing.T) {
	const body = `{"name":"Lobby Loop","items":[` +
		`{"source":"asset","asset_ref":"sha256:aa"},` +
		`{"source":"slide","slide":{"layers":[` +
		`{"kind":"rect","color":"#101828"},` +
		`{"kind":"image","asset_ref":"sha256:bb","url":"https://o/content/bb?exp=1&sig=deadbeef"}]}}]}`

	out := stripDerivedMembers(KindPlaylist, json.RawMessage(body))

	var row struct {
		Items []struct {
			Source   string `json:"source"`
			AssetRef string `json:"asset_ref"`
			Slide    *struct {
				Layers []struct {
					Kind     string `json:"kind"`
					AssetRef string `json:"asset_ref"`
					URL      string `json:"url"`
				} `json:"layers"`
			} `json:"slide"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &row); err != nil {
		t.Fatalf("decode the stripped playlist: %v (body %s)", err, out)
	}
	if len(row.Items) != 2 || row.Items[1].Slide == nil || len(row.Items[1].Slide.Layers) != 2 {
		t.Fatalf("the strip reshaped the row: %s", out)
	}
	if got := row.Items[1].Slide.Layers[1].URL; got != "" {
		t.Errorf("items[1].slide.layers[1].url = %q, want it gone.\n"+
			"An inline slide's layer stack is the same authored stack a cast's slide carries, and since HV-1 the url a "+
			"media picker hands over is a signed capability that expires — persisting it is a dead link in the "+
			"operator's own document a day later.", got)
	}
	// The authored halves survive, or the strip has merely broken the row.
	if got := row.Items[1].Slide.Layers[1].AssetRef; got != "sha256:bb" {
		t.Errorf("the inline slide layer's asset_ref = %q, want sha256:bb — the derived half was dropped and the authored half with it", got)
	}
	if got := row.Items[0].AssetRef; got != "sha256:aa" {
		t.Errorf("the plain asset item's asset_ref = %q, want sha256:aa", got)
	}
}

// TestABodyWithNothingToStripIsReturnedUnchanged pins the fidelity claim on the
// side where it is absolute: a row carrying no derived member is not decoded and
// re-encoded at all, so nothing about its bytes can move.
//
// It matters because the store persists the exact bytes the api later serves. A
// strip that normalized every row on the way past would be rewriting the
// representation of every cast and playlist in the site to remove a member from
// a few of them — key order, whitespace and the HTML-escaping of a text layer
// included.
func TestABodyWithNothingToStripIsReturnedUnchanged(t *testing.T) {
	cases := map[string]struct {
		kind Kind
		body string
	}{
		"a cast whose layers carry no url":  {KindCast, `{"z":1,"slides":[{"layers":[{"kind":"text","text":"Tues & Weds <today>"}]}],"a":2}`},
		"a playlist of plain asset items":   {KindPlaylist, `{"z":1,"items":[{"source":"asset","asset_ref":"sha256:aa"}],"a":2}`},
		"an inline slide with no url":       {KindPlaylist, `{"items":[{"source":"slide","slide":{"layers":[{"kind":"rect","color":"#fff"}]}}]}`},
		"a kind that carries no layers":     {KindScreen, `{"z":1,"name":"Lobby","a":2}`},
		"a body this function cannot parse": {KindCast, `not json at all`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := string(stripDerivedMembers(tc.kind, json.RawMessage(tc.body))); got != tc.body {
				t.Errorf("the body came back rewritten:\n got %s\nwant %s", got, tc.body)
			}
		})
	}
}
