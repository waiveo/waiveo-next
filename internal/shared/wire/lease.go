package wire

import (
	"encoding/json"
	"fmt"
)

// LeaseContent is one player/1 Lease `content` array entry for a plain
// `image`/`video` item (PLY-083): `{type, asset_ref, url, expires_at}`,
// plus the additive `duration_ms` (PLY-083b). This differs from relay/1's
// own ContentRef (REL-061), which carries no `type` field — a relay
// assigns `type` when converting a verified screen-program's content
// reference into a player/1 Lease content item (SetServedProgram, per-item
// from the entry's own ContentType, REL-061a).
//
// DurationMS mirrors relay/1's ContentRef.DurationMS (REL-061a) verbatim —
// SetServedProgram carries it unmodified from the persisted entry onto this
// field, `omitempty` so an item that carries none (the pre-PLY-083b wire
// shape) marshals with no `duration_ms` key, byte-identical to every prior
// release (mirroring ContentRef's own omitempty doc, REL-061a).
//
// Layers is the additive `slide` content type's positioned native elements
// (native slide rendering, parity milestone 2): a `type:"slide"` item carries
// the ordered layer stack a player draws directly (text/rect/image/clock, each
// placed in the fixed 1920×1080 canvas). It is `omitempty` and set ONLY for a
// slide item, so every pre-existing `image`/`video` content item — which never
// populates it — marshals with no `layers` key at all, byte-identical to every
// prior release and therefore covered by the SAME `LeaseSignedBytes` signature
// (PLY-090) it always was: the field rides that signature automatically,
// because LeaseSignedBytes marshals this whole struct, but contributes nothing
// to the signed bytes of an item that carries no layers. A relay populates it
// only after ValidateSlideLayers accepts the layers (a malformed slide is never
// served, internal/relay/playerserver.SetServedProgram).
type LeaseContent struct {
	Type       string  `json:"type"`
	AssetRef   string  `json:"asset_ref"`
	URL        string  `json:"url"`
	ExpiresAt  int64   `json:"expires_at"`
	DurationMS int64   `json:"duration_ms,omitempty"`
	Layers     []Layer `json:"layers,omitempty"`
}

// SlideCanvasWidth and SlideCanvasHeight are the fixed pixel canvas every
// `slide` layer is positioned within (native slide rendering, parity milestone
// 2). A player forces `scaleToFill` onto a surface of exactly this size, so a
// layer's geometry is authored against these bounds regardless of the panel's
// real resolution — the renderer targets exactly 1920×1080 and scales the whole
// composed surface to fit. ValidateSlideLayers rejects any layer that falls
// outside this box, so a producer can never ship geometry a player would have
// to clip or place off-screen.
const (
	SlideCanvasWidth  = 1920
	SlideCanvasHeight = 1080
)

// The four v1 `slide` layer kinds (native slide rendering, parity milestone 2).
// This is a CLOSED set: ValidateSlideLayers rejects any other kind, so a
// producer cannot ship a layer a player has no draw path for. Later kinds
// (video, date) are added here — and to the validator's per-kind rules —
// deliberately, one at a time, once a player advertises a renderer for them.
const (
	LayerKindText  = "text"  // a literal string drawn as a Label
	LayerKindRect  = "rect"  // a filled rectangle
	LayerKindImage = "image" // a content-addressed image drawn as a Poster
	LayerKindClock = "clock" // a Label refreshed with the formatted current time
)

// Layer is one positioned native element of a `slide` content item (native
// slide rendering, parity milestone 2). Layers are drawn in ARRAY ORDER — the
// slice index IS the z-order, later entries paint over earlier ones — so a
// producer orders the slice back-to-front.
//
// Geometry (X/Y/W/H) is in the fixed SlideCanvasWidth×SlideCanvasHeight canvas,
// top-left origin, and carries NO `omitempty`: a layer always states where it
// sits, and a `0` there is a real coordinate (the canvas's own edge), never an
// "unset" to be dropped from the wire. Every other member is kind-specific and
// `omitempty`, so a layer marshals only the fields its own kind uses — a `rect`
// carries no `text`, a `text` carries no `asset_ref`, and neither carries the
// zero-value keys of members it does not use. ValidateSlideLayers is the one
// place the per-kind required-field rules live; see its doc.
type Layer struct {
	Kind string `json:"kind"`
	X    int    `json:"x"`
	Y    int    `json:"y"`
	W    int    `json:"w"`
	H    int    `json:"h"`

	// Text is the literal string for a `text` layer, and the time-format
	// string for a `clock` layer (the format the player renders the current
	// time through, refreshed every second). Unused by `rect`/`image`.
	Text string `json:"text,omitempty"`
	// AssetRef and URL address an `image` layer's bytes exactly as a plain
	// `image` content item does (DAT-041 discipline): AssetRef is the
	// content-addressed `sha256:` reference a player verifies the fetched
	// bytes against, URL the direct content-origin fetch target (never a
	// relay-hosted one, REL-140). Both are required together for `image`.
	AssetRef string `json:"asset_ref,omitempty"`
	URL      string `json:"url,omitempty"`
	// FontPx is the pixel font size for a `text`/`clock` layer's Label. It is
	// optional styling — a layer that omits it renders at the player's own
	// default size — so ValidateSlideLayers does not require it.
	FontPx int `json:"font_px,omitempty"`
	// Color is a `#RRGGBB` hex string: a `rect`'s fill (required) or a
	// `text`/`clock`'s foreground (optional). Wherever present it MUST be a
	// well-formed hex color — a value a renderer could not parse is rejected
	// rather than silently drawn as black.
	Color string `json:"color,omitempty"`
	// Align is a `text` layer's horizontal alignment (`left`/`center`/`right`),
	// optional — an unset align renders at the player's own default. Unused by
	// the other kinds.
	Align string `json:"align,omitempty"`
}

// ValidateSlideLayers is the ONE gate a `slide` content item's layers pass
// before a relay ever serves them (native slide rendering, parity milestone 2).
// It is exported and lives here, in the wire package that owns the Layer shape,
// precisely so the single producer that populates a Lease's layers
// (internal/relay/playerserver.SetServedProgram, converting a persisted
// screen_programs slide item) and any later conformance driver validate against
// the IDENTICAL rules — a second, drifting copy of these checks is exactly the
// hazard sharing one exported function removes, the same discipline
// LeaseSignedBytes documents for the signed-scope bytes.
//
// A relay that finds a slide item whose layers this rejects DROPS the item
// rather than serving it: a player has no defined behavior for a malformed
// layer, so a slide that would not draw cleanly must never reach the wire in
// the first place. The rules, per the design:
//
//   - The layer stack is non-empty. A `slide` with no layers is nothing to
//     draw, and an empty stack is a producer bug, not a blank slide.
//   - Every layer's Kind is one of the four v1 kinds (LayerKindText/Rect/
//     Image/Clock) — the closed set above.
//   - Every layer's geometry fits the canvas: W and H are strictly positive
//     (a zero-area layer draws nothing), X and Y are non-negative, and the
//     layer's far edge (X+W, Y+H) stays within SlideCanvasWidth×
//     SlideCanvasHeight. A layer partly off-canvas is refused rather than
//     left for a player to clip.
//   - The required fields for the layer's own kind are present: `text` needs
//     Text; `image` needs BOTH AssetRef and URL (an image with one but not
//     the other cannot be both verified and fetched, DAT-041); `clock` needs
//     a non-empty format in Text; `rect` needs Color.
//   - Any Color that is present — the required `rect` fill, or an optional
//     `text`/`clock` foreground — is a well-formed `#RRGGBB` hex string. This
//     is validated wherever a color appears, not only where it is required:
//     the Layer doc fixes `#RRGGBB` as the format for every kind that carries
//     a color, and a value a renderer cannot parse is a defect on a `text`
//     layer exactly as it is on a `rect`.
//
// Note on unknown fields: this validates an already-decoded []Layer, so a
// wire-level "reject unknown JSON keys" belongs at the decode boundary that
// produced the slice (the snapshot decode), not here — a stray JSON member is
// dropped by encoding/json before a Layer value ever reaches this function.
// The scope of this gate is the semantic validity of the typed layers.
func ValidateSlideLayers(layers []Layer) error {
	if len(layers) == 0 {
		return fmt.Errorf("wire: slide has no layers")
	}
	for i, l := range layers {
		switch l.Kind {
		case LayerKindText, LayerKindRect, LayerKindImage, LayerKindClock:
			// A recognized kind; per-kind required fields checked below.
		default:
			return fmt.Errorf("wire: slide layer %d: unknown kind %q (want one of %q/%q/%q/%q)",
				i, l.Kind, LayerKindText, LayerKindRect, LayerKindImage, LayerKindClock)
		}

		if l.W <= 0 || l.H <= 0 {
			return fmt.Errorf("wire: slide layer %d (%s): w and h must be positive, got w=%d h=%d", i, l.Kind, l.W, l.H)
		}
		if l.X < 0 || l.Y < 0 {
			return fmt.Errorf("wire: slide layer %d (%s): x and y must be non-negative, got x=%d y=%d", i, l.Kind, l.X, l.Y)
		}
		if l.X+l.W > SlideCanvasWidth || l.Y+l.H > SlideCanvasHeight {
			return fmt.Errorf("wire: slide layer %d (%s): geometry x=%d y=%d w=%d h=%d extends past the %dx%d canvas",
				i, l.Kind, l.X, l.Y, l.W, l.H, SlideCanvasWidth, SlideCanvasHeight)
		}

		switch l.Kind {
		case LayerKindText:
			if l.Text == "" {
				return fmt.Errorf("wire: slide layer %d (text): text is required", i)
			}
		case LayerKindClock:
			if l.Text == "" {
				return fmt.Errorf("wire: slide layer %d (clock): a non-empty time format (text) is required", i)
			}
		case LayerKindImage:
			if l.AssetRef == "" || l.URL == "" {
				return fmt.Errorf("wire: slide layer %d (image): both asset_ref and url are required, got asset_ref=%q url=%q", i, l.AssetRef, l.URL)
			}
		case LayerKindRect:
			if l.Color == "" {
				return fmt.Errorf("wire: slide layer %d (rect): color is required", i)
			}
		}

		// A color, wherever present, must be a renderable #RRGGBB — required
		// for rect, optional for text/clock, hex either way (see the doc).
		if l.Color != "" && !isHexColor(l.Color) {
			return fmt.Errorf("wire: slide layer %d (%s): color %q is not a #RRGGBB hex string", i, l.Kind, l.Color)
		}
	}
	return nil
}

// isHexColor reports whether s is a `#RRGGBB` string: a leading '#' followed by
// exactly six hexadecimal digits (either case). This is the one color form the
// slide renderer parses, so ValidateSlideLayers rejects anything else outright
// rather than leaving a player to guess at a malformed value.
func isHexColor(s string) bool {
	if len(s) != 7 || s[0] != '#' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f', c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

// Lease is player/1's Lease shape (PLY-090) minus `signature` — exactly
// the fields a Lease's `signature` covers. Field declaration order matches
// PLY-090's own shape `{lease_id, screen_id, program_revision, priority,
// display, content, issued_at, valid_until, signature}` up to `signature`
// itself, which a caller appends (internal/relay/playerserver.LeaseResponse
// embeds this struct and adds Signature as its own trailing field, so
// JSON marshal order matches PLY-090 exactly).
type Lease struct {
	LeaseID         string         `json:"lease_id"`
	ScreenID        string         `json:"screen_id"`
	ProgramRevision string         `json:"program_revision"`
	Priority        string         `json:"priority"`
	Display         string         `json:"display"`
	Content         []LeaseContent `json:"content"`
	IssuedAt        int64          `json:"issued_at"`
	ValidUntil      int64          `json:"valid_until"`
}

// LeaseSignedBytes marshals lease into THE canonical bytes a Lease's
// `signature` covers (PLY-090) — struct-declaration-order JSON marshal,
// the same canonicalization convention HashSections/SignedScopeBytes
// already establish for relay/1's own snapshot signature.
//
// Both the relay (which signs a Lease at issuance, internal/relay/
// playerserver) and a player (which must recompute the identical bytes to
// verify one against its pinned trust anchor, a later task) MUST call this
// function rather than each marshaling a Lease independently — sharing it
// here is what keeps the two sides from drifting apart on the signed
// scope's byte representation, exactly as SignedScopeBytes does for
// relay/1's snapshot signature. Do not reimplement this elsewhere.
func LeaseSignedBytes(lease Lease) ([]byte, error) {
	b, err := json.Marshal(lease)
	if err != nil {
		return nil, fmt.Errorf("wire: LeaseSignedBytes: marshal lease: %w", err)
	}
	return b, nil
}
