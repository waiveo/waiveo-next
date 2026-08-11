package store

import (
	"encoding/json"
)

// derivedmembers.go is injectDeclaredMembers' opposite number: it REMOVES, on
// write, the members of an authored row that this platform DERIVES rather than
// stores.
//
// # The one member, and the failure that put it here
//
// A content-bearing slide layer's `url` (wire.LayerFetchesContent — image and
// video) is derived at projection time from the content origin the snapshot is
// built against. wire.ValidateAuthoredSlideLayers already says so in as many
// words, and deliberately neither requires it of an author nor removes it; the
// projections (internal/feeder/snapshot.resolveLayers,
// internal/relay/schedulehost.resolveLayers) overwrite whatever is there.
//
// So a stored `url` reached no screen — and was harmless for as long as it was a
// permanent address. It stopped being one when content URLs became SIGNED and
// EXPIRING (internal/feeder/contenturl). The console's media picker hands the
// Studio the url from `GET /content`, which is now minted with
// contenturl.ServeTTL, the Studio patches it into the layer, and the write
// persisted it verbatim. An operator building a cast today and reopening it
// tomorrow met a canvas of broken images and a properties panel showing a dead
// link — the authoring surface reproducing HV-1's own defect a day late,
// invisibly, because nothing a screen sees is affected.
//
// # Every shape that carries an authored layer stack, not just the one we found
//
// The defect was found on a cast, and the first cut of this file stripped
// `slides[].layers[]` and returned every other kind untouched. That closes it on
// casts and nowhere else. A playlist item with `source: "slide"` carries the
// SAME authored layer stack, at `items[].slide.layers[]` (DAT-041); it arrives
// through the same Store.Create/Store.Update path, from the same media picker,
// and it persisted the picked url verbatim — signature, deadline and all.
//
// assetrefs.go next door has already paid for this lesson: three hand-written
// per-shape projections all read a playlist item's `asset_ref` and NONE of them
// read a layer stack, "and three copies is how it stayed one blind spot in three
// places". A per-shape strip is the same mistake in the same rows. So the shapes
// are enumerated ONCE, in derivedMemberStrippers below, its key set is required
// to be exactly AssetBearingKinds (TestEveryAssetBearingKindHasADerivedStrip),
// and a new asset-bearing shape is therefore added to both files or to neither.
//
// The playlist half was latent when it was found — no shipped surface put a url
// in an inline slide — and it is fixed anyway, because it is a CROSS-TRACK
// hazard rather than a tidiness question: an importer writing casts through
// Store.Create inherits the strip for free, and the very same importer writing a
// playlist inline slide would not have.
//
// # Why stripping is the fix, rather than re-resolving on read
//
// Because the value is not merely stale, it is not the row's to hold. A derived
// field persisted beside its source has exactly two futures: it is ignored (dead
// weight that will mislead the next reader) or it is trusted (a second, silently
// diverging copy of an answer the origin owns). Refusing it at the write is the
// only version with one answer in it. The api's own listing is where a console
// gets a fetchable url, at the moment it needs one — see
// internal/app/api/content.go.
//
// A REJECTION was considered and not taken. `url` is a documented member of
// api/openapi.yaml's SlideLayer that a client may legitimately echo back from a
// GET, and 422-ing a round-tripped read would make the ordinary read-modify-write
// cycle fail on a field the client never set. Ignoring a derived member is the
// same posture injectDeclaredMembers takes from the other side: the server owns
// the representation, and it completes or reduces what a writer sends rather than
// bouncing it.
//
// # Why the store rather than the api layer, stated accurately
//
// Because Store.Create/Store.Update is the choke point every GENERIC row write
// passes through: the casts handler, the playlists handler, a declarative pack's
// row install, the make-dev seed. A strip in the cast handler would have to be
// remembered again in the playlist handler — which is precisely the blind spot
// described above, in miniature.
//
// That is the real reason, and it is narrower than the one this file used to
// give. The previous wording claimed "every writer of a row goes through
// Store.Create/Store.Update — the api handlers, the workspace restore, the
// make-dev seed", and the workspace restore is not one of them. A restore does
// not write rows at all: it stages a whole SQLite file and swaps it into place
// (internal/app/restoreswap), and the export half is equally outside the generic
// path — workspace.go says so outright ("Neither operation is a resource-family
// CRUD call, so neither goes through the generic Create/Update/Delete path").
//
// So the residue is stated rather than implied: a workspace exported before this
// strip existed, or exported from another site, restores carrying whatever urls
// ITS origin minted, under a key the importing site does not hold. No screen is
// affected (both projections re-mint from the asset_ref), and the operator-facing
// symptom is the same dead link in the properties panel. Closing it belongs to
// the restore path, which is the only code that sees those rows; it is not
// closed here, and it is not silently assumed closed either.

// derivedMemberStrippers is the whole enumeration of row shapes carrying a
// derived member, keyed by kind: the ONE place a new shape is added.
//
// Each entry reports whether it changed anything, so a body that carries no
// derived member is returned as the identical bytes rather than re-encoded (see
// stripDerivedMembers' doc for the precise fidelity claim).
//
// Its key set is required to equal store.AssetBearingKinds — the same
// enumeration assetrefs.go keeps for the same rows — because the two questions
// ("which kinds name content?" and "which kinds carry a derived url?") have the
// same answer for the same reason: both are asking which kinds carry an authored
// layer stack or an asset reference. Letting them drift apart is how a shape gets
// covered by one and missed by the other.
var derivedMemberStrippers = map[Kind]func(json.RawMessage) (json.RawMessage, bool){
	// A cast's layer stacks: `slides[].layers[]`.
	KindCast: func(body json.RawMessage) (json.RawMessage, bool) {
		return editEachElement(body, "slides", stripLayerStackURLs)
	},
	// A playlist's: `items[].slide.layers[]`, on `source: "slide"` items. An
	// item of any other source carries no `slide` member and is left alone by
	// editMember, so no source check is needed here — the shape IS the check.
	KindPlaylist: func(body json.RawMessage) (json.RawMessage, bool) {
		return editEachElement(body, "items", func(item json.RawMessage) (json.RawMessage, bool) {
			return editMember(item, "slide", stripLayerStackURLs)
		})
	},
}

// stripDerivedMembers returns body with kind's derived members removed. A kind
// that carries none — and a body this function cannot parse, which the very next
// step (parseBaseline) reports properly rather than having it surface here as a
// mangled row — is returned unchanged, bytes and all.
//
// # The fidelity claim, precisely
//
// A body that needed no edit is returned as the SAME bytes: nothing is decoded
// and re-encoded, so nothing about it can change.
//
// A body that did need one is rebuilt, and two spellings change. Neither
// changes a VALUE: every one of these round-trips to the identical decoded
// document, minus the `url` members this function set out to remove. But the
// two have different REACH, and an earlier version of this comment got that
// wrong in the safe-sounding direction by scoping both to "the objects on the
// edited path". Only the first is that narrow.
//
//  1. Member ORDER changes on the path, and only on the path. Each object
//     rebuilt along the way to an edited layer is re-encoded from a
//     map[string]json.RawMessage, and Go emits map keys sorted, so those
//     objects come back alphabetized. Everything off the path rides as
//     json.RawMessage — an untouched slide, an untouched item, an unrecognized
//     member nobody here knows about — and keeps its authored member order.
//
//  2. HTML-significant characters are re-escaped throughout the WHOLE body,
//     path or not. json.Marshal of a map[string]json.RawMessage COMPACTS each
//     child with escapeHTML on, and that compaction walks the child to its
//     leaves — so a `<`, `>` or `&` anywhere in the row comes back as its
//     six-character unicode escape. Probed on a cast body: with one url
//     stripped from slide 0, a top-level `"z_top":"a & b <tag>"`, the `name`
//     beside it, and untouched slide 1's own text all came back escaped.
//
// What is preserved exactly, and not merely approximately: every value, and
// every number's authored spelling — a `duration_ms` of `1.50` is compacted,
// never run through float64 and back, and comes out `1.50`.
//
// So: this is a re-spelling of the row, not a re-interpretation of it. Anything
// comparing these bytes to their authored form (a digest, a golden fixture, a
// byte-equality assertion) must expect the escapes across the whole row and the
// sorted keys on the edited path; anything DECODING them sees no difference.
func stripDerivedMembers(kind Kind, body json.RawMessage) json.RawMessage {
	strip, ok := derivedMemberStrippers[kind]
	if !ok {
		return body
	}
	out, changed := strip(body)
	if !changed {
		return body
	}
	return out
}

// stripLayerStackURLs removes `url` from every layer of the object's `layers`
// array — the one edit this file makes, applied wherever a layer stack is
// reached from.
func stripLayerStackURLs(obj json.RawMessage) (json.RawMessage, bool) {
	return editEachElement(obj, "layers", stripLayerURL)
}

// stripLayerURL removes the derived `url` member from one layer object.
func stripLayerURL(layer json.RawMessage) (json.RawMessage, bool) {
	obj := map[string]json.RawMessage{}
	if err := json.Unmarshal(layer, &obj); err != nil {
		return layer, false
	}
	if _, present := obj["url"]; !present {
		return layer, false
	}
	delete(obj, "url")
	return reencode(obj, layer)
}

// editEachElement applies edit to every element of the ARRAY member named key,
// and reports whether any element changed.
//
// An absent member, a member that is not an array, an element edit that changes
// nothing: all of them return the input bytes and false, so an unaffected row
// never gets re-encoded at all.
func editEachElement(raw json.RawMessage, key string, edit func(json.RawMessage) (json.RawMessage, bool)) (json.RawMessage, bool) {
	obj := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw, false
	}
	rawArray, present := obj[key]
	if !present {
		return raw, false
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(rawArray, &elems); err != nil {
		return raw, false
	}
	changed := false
	for i, elem := range elems {
		out, elemChanged := edit(elem)
		if !elemChanged {
			continue
		}
		elems[i] = out
		changed = true
	}
	if !changed {
		return raw, false
	}
	encoded, err := json.Marshal(elems)
	if err != nil {
		return raw, false
	}
	obj[key] = encoded
	return reencode(obj, raw)
}

// editMember applies edit to the single OBJECT member named key. An item with no
// such member (a playlist item that is not an inline slide) is left exactly as it
// was.
func editMember(raw json.RawMessage, key string, edit func(json.RawMessage) (json.RawMessage, bool)) (json.RawMessage, bool) {
	obj := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw, false
	}
	member, present := obj[key]
	if !present {
		return raw, false
	}
	out, changed := edit(member)
	if !changed {
		return raw, false
	}
	obj[key] = out
	return reencode(obj, raw)
}

// reencode marshals an edited object back to bytes, falling back to the original
// on the (unreachable) marshal failure — a strip that cannot re-encode must leave
// the row alone rather than replace it with nothing.
func reencode(obj map[string]json.RawMessage, original json.RawMessage) (json.RawMessage, bool) {
	out, err := json.Marshal(obj)
	if err != nil {
		return original, false
	}
	return out, true
}
