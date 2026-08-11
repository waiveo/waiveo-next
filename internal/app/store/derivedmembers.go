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
// contenturl.ServeTTL, the Studio patches it into the layer, and the cast write
// persisted it verbatim. An operator building a cast today and reopening it
// tomorrow met a canvas of broken images and a properties panel showing a dead
// link — the authoring surface reproducing HV-1's own defect a day late,
// invisibly, because nothing a screen sees is affected.
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
// # Why the store rather than the api layer
//
// The same reason declaredmembers.go gives: every writer of a row goes through
// Store.Create/Store.Update — the api handlers, the workspace restore, the
// make-dev seed. A strip in the cast handler would leave a restored workspace
// carrying urls minted by whatever origin exported it, against a key the
// importing site does not hold.

// stripDerivedMembers returns body with kind's derived members removed. A body
// that carries none is returned unchanged, bytes and all — including a body this
// function cannot parse, which the very next step (parseBaseline) reports
// properly rather than having it surface here as a mangled row.
func stripDerivedMembers(kind Kind, body json.RawMessage) json.RawMessage {
	if kind != KindCast {
		return body
	}
	return stripCastLayerURLs(body)
}

// stripCastLayerURLs removes `url` from every layer of every slide of a cast
// row.
//
// It walks with json.RawMessage at every level it is not editing, so nothing
// outside the layer objects it actually changes is re-encoded: member order,
// number spelling (a duration_ms is not run through float64 and back), and every
// unrecognized member survive byte for byte. The store persists the exact bytes
// the api later serves, so a normalization that quietly rewrote the rest of the
// document would be changing the representation of every cast to remove a member
// from a few of them.
func stripCastLayerURLs(body json.RawMessage) json.RawMessage {
	row := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &row); err != nil {
		return body
	}
	rawSlides, ok := row["slides"]
	if !ok {
		return body
	}
	var slides []json.RawMessage
	if err := json.Unmarshal(rawSlides, &slides); err != nil {
		return body
	}

	changed := false
	for i, rawSlide := range slides {
		slide := map[string]json.RawMessage{}
		if err := json.Unmarshal(rawSlide, &slide); err != nil {
			continue
		}
		rawLayers, ok := slide["layers"]
		if !ok {
			continue
		}
		var layers []json.RawMessage
		if err := json.Unmarshal(rawLayers, &layers); err != nil {
			continue
		}
		slideChanged := false
		for j, rawLayer := range layers {
			layer := map[string]json.RawMessage{}
			if err := json.Unmarshal(rawLayer, &layer); err != nil {
				continue
			}
			if _, present := layer["url"]; !present {
				continue
			}
			delete(layer, "url")
			out, err := json.Marshal(layer)
			if err != nil {
				continue
			}
			layers[j] = out
			slideChanged = true
		}
		if !slideChanged {
			continue
		}
		encLayers, err := json.Marshal(layers)
		if err != nil {
			continue
		}
		slide["layers"] = encLayers
		encSlide, err := json.Marshal(slide)
		if err != nil {
			continue
		}
		slides[i] = encSlide
		changed = true
	}
	if !changed {
		return body
	}

	encSlides, err := json.Marshal(slides)
	if err != nil {
		return body
	}
	row["slides"] = encSlides
	out, err := json.Marshal(row)
	if err != nil {
		return body
	}
	return out
}
