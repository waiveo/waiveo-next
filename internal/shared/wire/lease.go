package wire

import (
	"encoding/json"
	"fmt"
	"strings"
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

// The `slide` layer kinds. This is a CLOSED set: ValidateSlideLayers rejects
// any other kind, so a producer cannot ship a layer a player has no draw path
// for. A new kind is added here — and to the validator's per-kind rules, and to
// a player's renderer — deliberately, one at a time.
//
// The first four are the v1 kinds (native slide rendering, parity milestone 2).
// The last four are the LIVE WIDGETS the legacy system drew on essentially every
// slide, and they are the whole reason this path is native rather than a
// server-rasterized PNG: a flat image freezes them. They split by WHERE the
// value that makes them live comes from, and that split is the design:
//
//   - date and countdown are derived from the CLOCK, so a player computes them
//     itself and re-computes them on its own tick. Nothing about them needs the
//     box, and routing them through the box would make them as stale as the last
//     Lease.
//   - weather and entity are derived from data only the BOX has (an upstream
//     forecast service; the device plane's observed entity state). Those are
//     resolved SERVER-side onto Layer.Value at Lease issuance
//     (internal/slidelive, called from internal/relay/playerserver) and the
//     player simply draws the text it is handed. A player that fetched weather
//     itself would need outbound internet per screen, a credential per screen,
//     and a second implementation of every unit and format decision.
//
// The ninth, video, is the only MOVING element a slide can carry, and it splits
// a third way again: like image (and unlike every widget) its substance is bytes
// in the content origin, so it is authored as an asset_ref and fetched +
// content-address-verified by the player before it is ever presented. See
// LayerKindVideo and LayerFetchesContent.
//
// Forward compatibility is by SKIPPING, not by refusal: a player that predates
// one of these kinds draws the layers it knows and silently skips the rest, so a
// slide authored with a countdown still shows its title and image on an older
// player. That is why an unknown kind is a producer-side error (rejected here)
// but a player-side no-op.
const (
	LayerKindText  = "text"  // a literal string drawn as a Label
	LayerKindRect  = "rect"  // a filled rectangle
	LayerKindImage = "image" // a content-addressed image drawn as a Poster
	LayerKindClock = "clock" // a Label refreshed with the formatted current time

	LayerKindDate      = "date"      // a Label showing the formatted current date
	LayerKindCountdown = "countdown" // a Label counting down to TargetMS
	LayerKindWeather   = "weather"   // a Label showing server-resolved current conditions
	LayerKindEntity    = "entity"    // a Label showing a platform entity's server-resolved state

	// LayerKindVideo is a content-addressed video drawn as a positioned Video
	// node — the ONE moving element a slide can carry, and the second kind
	// (with image) whose bytes come from the content origin rather than from
	// the layer itself.
	//
	// It is deliberately modelled as image's twin, not as its own species: an
	// authored video layer carries exactly the same two content fields an image
	// layer does (AssetRef authored, URL derived at projection time), passes the
	// same required-field rules, and is placed in the same canvas with the same
	// geometry. Everything that differs is downstream of the wire — a player
	// creates a Video node instead of a Poster and loops it for the slide's
	// dwell — so nothing here needs to know about codecs, streaming or duration.
	// See LayerFetchesContent for the one predicate that names the pair, which
	// is what keeps the two content projections' URL derivation from drifting
	// into deriving one and not the other.
	LayerKindVideo = "video"
)

// LayerFetchesContent reports whether a layer of this kind names bytes in the
// content origin — i.e. whether it carries an `asset_ref` a player must fetch
// and content-address-verify, and therefore whether a projection must derive
// its `url`.
//
// It exists because that question is asked in FOUR places that must answer it
// identically: this package's own per-kind required-field rules
// (validateSlideLayers), and the URL derivation in each of the two content
// projections (internal/feeder/snapshot.resolveLayers and
// internal/relay/schedulehost.resolveLayers), plus any later producer. When
// `image` was the only such kind, each of those spelled `l.Kind ==
// LayerKindImage` inline, and adding `video` to the validator without adding it
// to both projections would have produced exactly this codebase's signature
// defect: a layer the store accepts, whose url is never minted, so
// ValidateSlideLayers drops the whole slide at serve time and the screen shows
// nothing with nothing anywhere saying why. One predicate makes that
// impossible to do by halves.
func LayerFetchesContent(kind string) bool {
	return kind == LayerKindImage || kind == LayerKindVideo
}

// slideLayerKinds is the closed kind set above as a slice, in declaration
// order — the ONE enumeration ValidateSlideLayers both tests membership against
// and names in its rejection message, so a kind added to the constants above and
// forgotten here cannot pass validation while claiming the message lists every
// legal value.
var slideLayerKinds = []string{
	LayerKindText, LayerKindRect, LayerKindImage, LayerKindClock,
	LayerKindDate, LayerKindCountdown, LayerKindWeather, LayerKindEntity,
	LayerKindVideo,
}

// isSlideLayerKind reports whether kind is one of the closed set.
func isSlideLayerKind(kind string) bool {
	for _, k := range slideLayerKinds {
		if k == kind {
			return true
		}
	}
	return false
}

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

	// Text is the literal string for a `text` layer, and a FORMAT for every
	// kind whose content is generated rather than authored:
	//
	//   - `clock`: the layout the player renders the current LOCAL time
	//     through, refreshed every second.
	//   - `date`: the layout the player renders the current LOCAL date through.
	//     Same convention, same formatter, different cadence — a date changes
	//     once a day, so nothing about it needs to come from the box.
	//   - `countdown`: an OPTIONAL remaining-time layout (see below); an empty
	//     Text means the player's default `HH:MM:SS`.
	//   - `weather` / `entity`: a template the SERVER substitutes into (see
	//     Value); the player never interprets it.
	//
	// A clock/date format is a Go reference-time layout — `"15:04:05"`,
	// `"3:04 PM"`, `"Monday, January 2"` — NOT a strftime string: the producer
	// authors it and the player interprets it under that one convention, so it
	// is fixed HERE rather than left for the two ends to each guess (a mismatch
	// renders a clock as literal garbage).
	//
	// A countdown layout is deliberately NOT a Go reference-time layout: Go has
	// no reference DURATION, and reusing the time layout would make `15` mean
	// "hour of day" on a value that has no hour of day. Its grammar is its own
	// tiny closed token set, longest match first, everything else literal:
	// `DD`/`D` days, `HH`/`H` hours, `MM`/`M` minutes, `SS`/`S` seconds — the
	// doubled form zero-padded to two digits, the single form unpadded. Hours,
	// minutes and seconds are the remainder within the next larger unit ONLY
	// when that larger unit appears in the layout; a layout of just `"HH:MM:SS"`
	// therefore counts total hours rather than silently dropping whole days.
	//
	// Unused by `rect`/`image`.
	Text string `json:"text,omitempty"`
	// AssetRef and URL address an `image` or `video` layer's bytes exactly as a
	// plain `image`/`video` content item does (DAT-041 discipline): AssetRef is
	// the content-addressed `sha256:` reference a player verifies the fetched
	// bytes against, URL the direct content-origin fetch target (never a
	// relay-hosted one, REL-140). Both are required together for either kind —
	// they are the SAME pair of fields under the same rules, which is why
	// LayerFetchesContent names the two kinds once instead of each consumer
	// testing for `image` and remembering to add `video`.
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
	// TargetMS is a `countdown` layer's target instant: Unix epoch
	// MILLISECONDS, UTC — the same absolute-instant unit every other time on
	// this wire uses (Lease.IssuedAt/ValidUntil), NOT a local wall time and NOT
	// seconds. An absolute instant is what lets the player count down without
	// knowing the authoring timezone: it subtracts its own clock and formats the
	// remainder. Required (and strictly positive) for `countdown`, unused
	// elsewhere. A target already in the past renders as all zeroes rather than
	// as a negative — a finished countdown reads "00:00:00", and validation does
	// NOT reject a past target, because a slide outliving its own event is a
	// scheduling matter, not a malformed layer.
	TargetMS int64 `json:"target_ms,omitempty"`
	// EntityID names the platform entity an `entity` layer displays the current
	// state of — a data-model/1 entity id, the same identifier a device
	// inventory entry's `entities[].entity_id` (REL-063) and a rules/1 state
	// trigger's subject carry. Required for `entity`, unused elsewhere. It is
	// the AUTHORED half of an entity layer; the displayed half is Value.
	EntityID string `json:"entity_id,omitempty"`
	// Value is the SERVER-RESOLVED display text of a `weather` or `entity`
	// layer: the string the player draws verbatim, with no interpretation at
	// all. It is the counterpart of an `image` layer's URL — a field the
	// AUTHORED row never carries and a projection fills in (there, from the
	// content origin; here, from the box's live data). See internal/slidelive,
	// which is the one place that fills it, and internal/relay/playerserver,
	// which calls it at Lease issuance so every poll carries a fresh value.
	//
	// It is deliberately NOT required by ValidateSlideLayers. The authored layer
	// legitimately has none, the projections that carry a slide through
	// (feeder/snapshot, relay/schedulehost) pass it through unresolved, and
	// requiring it would mean a forecast service being unreachable BLANKS THE
	// WHOLE SLIDE — losing the title, the image and the clock to fix nothing.
	// The resolver instead guarantees a non-empty value (its own unavailable
	// placeholder) so the widget degrades to a dash while the rest of the slide
	// keeps drawing.
	Value string `json:"value,omitempty"`
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
//   - Every layer's Kind is one of the kinds in slideLayerKinds — the closed
//     set above.
//   - Every layer's geometry fits the canvas: W and H are strictly positive
//     (a zero-area layer draws nothing), X and Y are non-negative, and the
//     layer's far edge (X+W, Y+H) stays within SlideCanvasWidth×
//     SlideCanvasHeight. A layer partly off-canvas is refused rather than
//     left for a player to clip.
//   - The required fields for the layer's own kind are present: `text` needs
//     Text; `image` and `video` — the two content-bearing kinds — each need
//     BOTH AssetRef and URL (one with one but not the other cannot be both
//     verified and fetched, DAT-041); `clock` needs
//     a non-empty format in Text; `rect` needs Color; `date` needs a non-empty
//     date layout in Text (same reason a clock does — an empty layout draws
//     nothing, which is a producer bug, not a blank widget); `countdown` needs
//     a strictly positive TargetMS (its layout in Text is optional, and a
//     missing target has no default that could be anything but wrong);
//     `weather` needs a non-empty template in Text; and `entity` needs an
//     EntityID naming its subject. What is deliberately NOT required is Value
//     on a `weather`/`entity` layer — see Value's own doc: it is filled at
//     Lease issuance, an authored layer never carries one, and demanding it
//     here would let an unreachable forecast service drop an entire slide.
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
	return validateSlideLayers(layers, true)
}

// ValidateAuthoredSlideLayers is the SAME gate applied one step earlier: to
// layers as an OPERATOR authored them, before any producer has projected them
// onto the wire.
//
// Exactly one rule differs, and it differs because of when it is asked. A
// content-bearing layer's `url` (image or video, LayerFetchesContent) is not
// authored — it is DERIVED at projection time from the content origin the
// snapshot was built against (internal/feeder/snapshot and
// internal/relay/schedulehost both mint it from the layer's own asset_ref), so
// at authoring time there is no url to require and demanding one would make
// every such layer unstorable. Everything else — the closed kind set, the
// canvas geometry, the other per-kind required fields, the `#RRGGBB` colors —
// is asked identically, because those ARE authored and a slide that would not
// draw should be refused by the operator's own create rather than silently
// dropped from a screen's program hours later.
//
// The two entry points share ONE implementation (validateSlideLayers below)
// rather than restating the rules, which is the whole reason this lives here
// beside ValidateSlideLayers: a second copy of the geometry or the per-kind
// required fields is exactly the drift the exported gate was introduced to
// remove, and it would drift in the direction that hurts most — an authoring
// gate that accepts what the serving gate later drops shows an operator a
// stored slide that never reaches a screen, with nothing anywhere saying why.
func ValidateAuthoredSlideLayers(layers []Layer) error {
	return validateSlideLayers(layers, false)
}

// validateSlideLayers is the one implementation behind both exported gates.
// requireContentURL distinguishes the SERVE-time caller (a url has been minted
// and a content-bearing layer without one cannot be fetched) from the
// AUTHORING-time caller (the url is derived later, so only the
// content-addressed asset_ref is authored) — see ValidateAuthoredSlideLayers.
// It governs both content-bearing kinds (LayerFetchesContent), which is why it
// is no longer named for `image` alone.
func validateSlideLayers(layers []Layer, requireContentURL bool) error {
	if len(layers) == 0 {
		return fmt.Errorf("wire: slide has no layers")
	}
	for i, l := range layers {
		if !isSlideLayerKind(l.Kind) {
			return fmt.Errorf("wire: slide layer %d: unknown kind %q (want one of %s)",
				i, l.Kind, strings.Join(slideLayerKinds, "/"))
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
		case LayerKindImage, LayerKindVideo:
			// The two content-bearing kinds share one rule because they carry
			// one pair of fields — see LayerFetchesContent. The kind is named in
			// the message so an operator is told which layer is at fault, not
			// merely that "a content layer" is.
			if l.AssetRef == "" {
				return fmt.Errorf("wire: slide layer %d (%s): asset_ref is required", i, l.Kind)
			}
			if requireContentURL && l.URL == "" {
				return fmt.Errorf("wire: slide layer %d (%s): both asset_ref and url are required, got asset_ref=%q url=%q", i, l.Kind, l.AssetRef, l.URL)
			}
		case LayerKindRect:
			if l.Color == "" {
				return fmt.Errorf("wire: slide layer %d (rect): color is required", i)
			}
		case LayerKindDate:
			if l.Text == "" {
				return fmt.Errorf("wire: slide layer %d (date): a non-empty date format (text) is required", i)
			}
		case LayerKindCountdown:
			// Strictly positive: 0 is the zero value of an absent field, not the
			// Unix epoch as an authored target, and a countdown to "unset" would
			// silently render as a finished one (all zeroes) forever.
			if l.TargetMS <= 0 {
				return fmt.Errorf("wire: slide layer %d (countdown): a positive target_ms (Unix epoch ms) is required, got %d", i, l.TargetMS)
			}
		case LayerKindWeather:
			if l.Text == "" {
				return fmt.Errorf("wire: slide layer %d (weather): a non-empty display template (text) is required", i)
			}
		case LayerKindEntity:
			if l.EntityID == "" {
				return fmt.Errorf("wire: slide layer %d (entity): entity_id is required", i)
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
