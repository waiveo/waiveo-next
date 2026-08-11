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
//
// SlideID is the CAST-LOCAL id of the authored slide a `slide` item was
// projected from (data-model/1 DAT-043's CastSlide.id), carried onto the wire so
// a `nav` layer's items can name a jump target that survives projection
// (LayerKindNav): the player matches a NavItem.TargetSlideID against this field
// to find which content item to present.
//
// It is `omitempty` and set ONLY for a slide item that came from a cast, so
// every other item — and every slide from an inline playlist item or a generated
// alert, neither of which has a cast-local id — marshals with no `slide_id` key,
// byte-identical to every prior release and therefore covered by the same
// LeaseSignedBytes signature it always was.
//
// Matching by id rather than by array position is the whole point: a cast's
// slides are projected into the SAME content array as whatever else the playlist
// carries, so an index authored against the cast addresses the wrong item the
// moment an entry is inserted before it — the class of bug that shows a menu
// working in the editor and jumping to the wrong slide on the wall.
type LeaseContent struct {
	Type       string  `json:"type"`
	AssetRef   string  `json:"asset_ref"`
	URL        string  `json:"url"`
	ExpiresAt  int64   `json:"expires_at"`
	DurationMS int64   `json:"duration_ms,omitempty"`
	SlideID    string  `json:"slide_id,omitempty"`
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

	// LayerKindPing is a VIEWER-PRESSABLE BUTTON: a labelled region the viewer
	// moves the remote's focus onto and presses OK on, at which point the player
	// POSTs the press back to the platform (`POST /player/v1/interaction`) and
	// the relay raises a durable `screen.interaction` event an automation can
	// trigger on (internal/events/screen_interaction.go, rules/1 RUL-080).
	//
	// It is the FIRST layer kind whose value flows the other way. Every other
	// kind is something the box tells a screen to draw; this one is something a
	// screen tells the box. That direction is the whole capability class the
	// legacy system had and this one lacked: a "call for service" button on a
	// lobby panel, a "start the tour" button in a museum, a shift handover
	// acknowledgement. Its authored half is PingName — the stable name an
	// automation matches on — and its drawn half is Text, the label a human
	// reads before pressing it.
	//
	// PingName is NOT exclusive to this kind, and that is deliberate: any layer
	// may carry one, which makes any layer focusable and pressable (see
	// LayerIsInteractive). `ping` is simply the kind that REQUIRES one and draws
	// itself as a button — an `entity` widget or an `image` with a ping_name is
	// an interactive widget, the same mechanism wearing a different face. One
	// mechanism rather than two is what keeps "focusable" from meaning one thing
	// for buttons and another for widgets.
	LayerKindPing = "ping"

	// LayerKindNav is an on-screen MENU the viewer drives with the remote's
	// D-pad: an ordered set of items, each of which jumps to another slide of
	// the same cast when OK is pressed on it.
	//
	// Its items are laid out inside the layer's own box along its longer axis
	// (see NavItemRects) — a wide nav is a horizontal row, a tall one a vertical
	// column — so the authored geometry alone decides the orientation and there
	// is no second `layout` field for an author to set inconsistently with the
	// box they drew.
	//
	// A nav item targets a slide by the cast-local slide id (NavItem.TargetSlideID
	// -> LeaseContent.SlideID), never by an index into the Lease's content array:
	// a cast's slides are projected into that array alongside whatever else the
	// playlist carries, so an index authored against the cast would address the
	// wrong item — or nothing — the moment a playlist gained an entry before it.
	// The authoring gate additionally refuses a target naming no slide of the
	// cast (datamodel.checkCastSlides), so a menu item that could only ever
	// dead-end cannot be stored in the first place.
	LayerKindNav = "nav"
)

// A `qr` layer kind is deliberately ABSENT from this set. A QR code's substance
// is a raster — a module grid whose bytes must exist somewhere a player can
// fetch and content-address-verify, exactly as an `image` layer's do — and this
// deployment's rasterizer (waiveo-derive) is a separate track. The seam it lands
// on is already the one `image`/`video` use: a `qr` kind would be a THIRD member
// of LayerFetchesContent whose AssetRef/URL are minted by the derive service at
// projection time (internal/feeder/snapshot.resolveLayers and
// internal/relay/schedulehost.resolveLayers, the two call sites that predicate
// already names) from the layer's authored payload. Adding the kind before that
// producer exists would ship a kind an author can select, a validator accepts,
// and no projection can ever give bytes to — this repo's signature defect. It is
// left out until the rasterizer it needs is real.

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

// LayerIsInteractive reports whether a layer is FOCUSABLE — whether the player
// gives it a focus region the viewer can move the remote's D-pad onto and press
// OK on (interactive slide layers, parity milestones 1.5/3.7).
//
// Two things make a layer interactive, and they are different capabilities, not
// two spellings of one:
//
//   - a non-empty PingName, on ANY kind. Pressing OK POSTs the press back to the
//     platform, which raises a durable `screen.interaction` event. This is both
//     the `ping` kind's own behavior and what makes an ordinary widget (an
//     `entity` reading, an `image`, a `rect` used as a hotspot) an INTERACTIVE
//     widget — tracker row 3.7 — with no second mechanism.
//   - a `nav` kind, whose Items each get their own focus region within the
//     layer's box and jump to another slide of the cast on OK.
//
// It is a shared predicate for the same reason LayerFetchesContent is one: the
// question "is this layer focusable?" is asked by the validator (which layers may
// carry which interactive fields), by the player's focus-target builder, and by
// the Studio canvas that draws a focus affordance. Three inline spellings of it
// is how a kind becomes focusable in one of them and inert in the others —
// a button that highlights and does nothing, or one that works and shows no
// focus at all.
func LayerIsInteractive(l Layer) bool {
	return l.PingName != "" || l.Kind == LayerKindNav
}

// NavItem is one entry of a `nav` layer's menu (LayerKindNav): the label the
// viewer reads and the cast-local slide id pressing OK jumps to.
//
// Both members are required. A label-less item is an unreadable target, and a
// target-less item is a menu entry that accepts a press and performs nothing —
// the dead-end surface this repo keeps producing, refused here at the wire shape
// rather than discovered on a wall.
//
// Launching ANOTHER Roku channel (legacy's `launch_app` nav action) is not
// modelled: it needs a channel-id vocabulary, a launch capability declaration on
// the player, and a return path back to this channel, none of which exist here
// yet. A single-purpose `target_slide_id` is the honest subset — see the track
// report; the field to add later is a discriminated `action` object, which is why
// this shape names the target explicitly rather than calling it `to`.
type NavItem struct {
	Label         string `json:"label"`
	TargetSlideID string `json:"target_slide_id"`
}

// MinInteractiveSide is the smallest side, in canvas pixels, a FOCUSABLE region
// may have — a pressable layer's own box, or one cell of a nav's menu.
//
// It is a legibility floor, not a rendering limit. The canvas is 1920x1080 shown
// on a wall and driven by a remote from across a room: below roughly this size a
// focus outline is not distinguishable at viewing distance, so a viewer cannot
// tell what pressing OK would do. The Studio's canvas is scaled DOWN, which is
// what makes the mistake easy — a region that looks fine in a 600-pixel-wide
// editor is a thumbnail on the wall.
//
// It is enforced at the wire, over both arms of LayerIsInteractive, so an
// unusable control is refused at authoring time rather than discovered by
// somebody standing in front of the screen.
const MinInteractiveSide = 48

// maxNavItems bounds a nav layer's menu. The items share the layer's own box
// (NavItemRects), so every extra item shrinks all of them; past this count the
// per-item region on a 1920×1080 canvas is too small to read at viewing distance
// and too small to show focus on. Bounding it here — where the stack is
// validated — means a player never has to decide what to do with a menu it
// cannot draw.
const maxNavItems = 8

// NavItemRects lays a `nav` layer's items out inside the layer's own box and
// returns one rect per item, in item order.
//
// The layout axis follows the box's own aspect: a box wider than it is tall is a
// horizontal row of equal-width items, otherwise a vertical column of
// equal-height items. Deriving the axis from the geometry rather than from a
// separate authored `layout` field is what makes it impossible to author a tall
// box declared "horizontal" — the two could disagree, and the drawn result would
// be the geometry while the D-pad behaviour followed the declaration.
//
// It lives HERE, beside the Layer shape and the canvas constants, because two
// consumers must compute the identical rects: the PLAYER (which draws the items
// and hit-tests focus against them) and the STUDIO canvas (which shows the
// author what the menu will look like). A second copy is how an author places a
// menu that focuses somewhere other than where it draws. The player is
// BrightScript and cannot call this function, so it carries its own transcription
// (PhotonScene.wvNavItemRects) — pinned to this one by
// TestNavItemRectsMatchesPlayerTranscription, which reads the .brs source, rather
// than left to drift.
//
// Integer division leaves at most (n-1) pixels unallocated at the far edge; the
// LAST item absorbs the remainder so the row exactly fills the box rather than
// ending a few pixels short of it.
func NavItemRects(l Layer) [][4]int {
	n := len(l.Items)
	if n <= 0 {
		return nil
	}
	out := make([][4]int, n)
	if l.W >= l.H {
		step := l.W / n
		for i := 0; i < n; i++ {
			w := step
			if i == n-1 {
				w = l.W - step*(n-1)
			}
			out[i] = [4]int{l.X + step*i, l.Y, w, l.H}
		}
		return out
	}
	step := l.H / n
	for i := 0; i < n; i++ {
		h := step
		if i == n-1 {
			h = l.H - step*(n-1)
		}
		out[i] = [4]int{l.X, l.Y + step*i, l.W, h}
	}
	return out
}

// pingNameMaxLen bounds a ping name. It is an identifier an automation matches
// on and a durable event payload field, not prose.
const pingNameMaxLen = 64

// navLabelMaxLen and navTargetMaxLen bound a nav item's two members. The target
// bound matches the authored cast slide id's own maximum (api/openapi.yaml's
// CastSlide.id maxLength) — a target longer than any id could be can only ever
// name nothing.
const (
	navLabelMaxLen  = 64
	navTargetMaxLen = 64
)

// ValidPingName reports whether s is a well-formed ping name: 1..64 characters
// of lowercase ASCII letters, digits, `_`, `-` and `.`, starting with a letter or
// digit.
//
// It is deliberately a SLUG rather than free text. The value travels three
// places that all key on it — the interaction request body, the durable
// `screen.interaction` event payload, and a rules/1 `event` trigger's `match`
// constraint — and an automation author types it by hand into the third. Free
// text would make "Front Desk " and "front desk" two names that look identical
// in a console and never match, with nothing anywhere saying why the automation
// did not fire.
func ValidPingName(s string) bool {
	if s == "" || len(s) > pingNameMaxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case (c == '_' || c == '-' || c == '.') && i > 0:
		default:
			return false
		}
	}
	return true
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
	LayerKindPing, LayerKindNav,
}

// authoredOnlyLayerKinds are the kinds an OPERATOR may author but that never
// reach a player. Today there is exactly one — LayerKindDerive, the rasterized
// fallback (derive.go) — and it is authoring-only because a projection rewrites
// it into a plain `image` before the serve-time gate ever sees it, so the wire a
// screen reads stays the closed set every existing player already draws.
//
// It is a named set rather than an inline `kind == LayerKindDerive` in
// validateSlideLayers for the reason that inline test would be wrong the day a
// second such kind appears: the asymmetry between the two gates has to be
// statable in ONE place, and the wire tests assert it from both directions
// against this slice.
var authoredOnlyLayerKinds = []string{LayerKindDerive}

// isSlideLayerKind reports whether kind is one of the closed set. authoring
// widens the set by authoredOnlyLayerKinds: the same question, asked at the two
// times the two exported gates are asked it.
func isSlideLayerKind(kind string, authoring bool) bool {
	for _, k := range slideLayerKinds {
		if k == kind {
			return true
		}
	}
	if authoring {
		for _, k := range authoredOnlyLayerKinds {
			if k == kind {
				return true
			}
		}
	}
	return false
}

// legalLayerKinds names every kind a gate accepts, for its rejection message —
// the ONE enumeration the message is built from, so it can never claim to list
// values the gate does not actually take.
func legalLayerKinds(authoring bool) []string {
	if !authoring {
		return slideLayerKinds
	}
	out := make([]string, 0, len(slideLayerKinds)+len(authoredOnlyLayerKinds))
	out = append(out, slideLayerKinds...)
	out = append(out, authoredOnlyLayerKinds...)
	return out
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
	// PingName is the stable name a press on this layer reports (interactive
	// slide layers, parity milestones 1.5/3.7). A layer that carries one is
	// FOCUSABLE (LayerIsInteractive): the player gives it a focus region, and OK
	// POSTs `{ping_name, …}` to the relay, which raises a durable
	// `screen.interaction` event carrying this exact value — the value a rules/1
	// `event` trigger's `match` constrains on. Required for `ping`, OPTIONAL for
	// every other kind (that is what makes an ordinary widget an interactive one),
	// and a slug wherever present (ValidPingName).
	PingName string `json:"ping_name,omitempty"`
	// Items is a `nav` layer's ordered menu (NavItem). Required (1..8 entries)
	// for `nav`, and REJECTED on every other kind — a stack that carries menu
	// items on a `rect` is a producer that thinks it authored a menu, and
	// silently drawing a rectangle instead is worse than refusing it.
	Items []NavItem `json:"items,omitempty"`
	// Derive is the RASTERIZED-FALLBACK spec of a `derive` layer: what the
	// off-appliance renderer must draw for something no SceneGraph node can (a
	// gradient, a drop shadow, a rounded border, a custom face, a QR symbol).
	// Required for `derive`, unused by every other kind. See derive.go, which
	// owns the shape, the gate and the projection.
	Derive *DeriveSpec `json:"derive,omitempty"`
	// DerivedFrom is the DeriveDigest of the spec+geometry the layer's current
	// AssetRef was rendered from. It is what distinguishes a current raster from
	// one an operator has since edited past, and it is filled by the derive tool
	// alongside AssetRef, never authored by hand. Unused by every kind but
	// `derive`.
	DerivedFrom string `json:"derived_from,omitempty"`
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
//     A `ping` needs BOTH a label in Text and a PingName; a `nav` needs 1..8
//     Items, each with a label and a target_slide_id.
//   - The two INTERACTIVE members are checked on EVERY layer, not only on the
//     kind that requires them, because each can be wrong in the mirror
//     direction a per-kind switch cannot see: a PingName is legal on any kind
//     (that is what makes an ordinary widget interactive) so its grammar is
//     enforced wherever it appears, and Items are legal ONLY on `nav` so their
//     presence anywhere else is refused.
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
		// requireContentURL is true exactly at SERVE time, which is also exactly
		// when the authoring-only kinds are illegal — a `derive` layer that
		// reached this gate is one a projection failed to rewrite into an
		// `image`, and serving it would hand a player a kind it cannot draw.
		authoring := !requireContentURL
		if !isSlideLayerKind(l.Kind, authoring) {
			return fmt.Errorf("wire: slide layer %d: unknown kind %q (want one of %s)",
				i, l.Kind, strings.Join(legalLayerKinds(authoring), "/"))
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
		case LayerKindPing:
			// A ping is a BUTTON: it needs a label a human can read before
			// pressing it (Text) and the name the press reports (PingName). Both,
			// because either one alone produces a surface that is exactly half a
			// button — an unlabelled hotspot, or a labelled control that reports
			// nothing.
			if l.Text == "" {
				return fmt.Errorf("wire: slide layer %d (ping): a button label (text) is required", i)
			}
			if l.PingName == "" {
				return fmt.Errorf("wire: slide layer %d (ping): ping_name is required", i)
			}
		case LayerKindNav:
			if len(l.Items) == 0 {
				return fmt.Errorf("wire: slide layer %d (nav): at least one menu item is required", i)
			}
			if len(l.Items) > maxNavItems {
				return fmt.Errorf("wire: slide layer %d (nav): %d items exceeds the maximum of %d", i, len(l.Items), maxNavItems)
			}
			for j, it := range l.Items {
				if it.Label == "" || len(it.Label) > navLabelMaxLen {
					return fmt.Errorf("wire: slide layer %d (nav): item %d needs a label of 1..%d characters", i, j, navLabelMaxLen)
				}
				if it.TargetSlideID == "" || len(it.TargetSlideID) > navTargetMaxLen {
					return fmt.Errorf("wire: slide layer %d (nav): item %d needs a target_slide_id of 1..%d characters", i, j, navTargetMaxLen)
				}
			}
		case LayerKindDerive:
			// The spec is required; the ASSET is not. A derive layer with no
			// asset yet is the normal state of a freshly authored one — the
			// off-appliance renderer has not run — and refusing it here would
			// make it impossible to author the thing that the renderer is
			// supposed to find and render.
			if err := ValidateDeriveSpec(l.Derive); err != nil {
				return fmt.Errorf("wire: slide layer %d (derive): %w", i, err)
			}
			if l.AssetRef != "" && !isAssetRef(l.AssetRef) {
				return fmt.Errorf("wire: slide layer %d (derive): asset_ref %q is not a sha256:<64 hex> reference", i, l.AssetRef)
			}
			if l.DerivedFrom != "" && l.AssetRef == "" {
				return fmt.Errorf("wire: slide layer %d (derive): derived_from names the spec an asset was rendered from, but there is no asset_ref", i)
			}
		}

		// The two INTERACTIVE members are checked outside the per-kind switch,
		// on every layer, because that is where they can go wrong:
		//
		//   - `ping_name` is legal on ANY kind (LayerIsInteractive — an entity
		//     widget with one is an interactive widget), so its grammar cannot be
		//     enforced from the `ping` case alone. Checking it only there is the
		//     exact half-fix this codebase has shipped before: the REQUIRED
		//     direction covered and the OPTIONAL-on-other-kinds direction not, so
		//     a malformed name reaches an automation's match constraint on every
		//     kind but one.
		//   - `items` is legal ONLY on `nav`, so its absence is checked in the
		//     switch and its PRESENCE ELSEWHERE has to be checked here — the
		//     mirror direction, which is the one a per-kind switch structurally
		//     cannot see.
		// An interactive region must be big enough to SEE FOCUS ON and to aim a
		// remote at from across a room. This is where LayerIsInteractive and
		// NavItemRects earn their place as shared definitions rather than as
		// player-side details: the rule is one rule over "the regions a viewer can
		// focus", and those two say what those regions are for both arms of the
		// mechanism — the whole layer for anything carrying a ping name, each
		// laid-out CELL for a nav's items. A 12-pixel menu cell (eight items in a
		// hundred-pixel box) is authored in a second in an editor whose canvas is
		// scaled down, and is unreadable and unhittable on the wall it ships to.
		if LayerIsInteractive(l) {
			if l.Kind == LayerKindNav {
				for j, r := range NavItemRects(l) {
					if r[2] < MinInteractiveSide || r[3] < MinInteractiveSide {
						return fmt.Errorf("wire: slide layer %d (nav): item %d's region is %dx%d, below the %dpx minimum a viewer can focus and press; make the menu bigger or carry fewer items",
							i, j, r[2], r[3], MinInteractiveSide)
					}
				}
			} else if l.W < MinInteractiveSide || l.H < MinInteractiveSide {
				return fmt.Errorf("wire: slide layer %d (%s): a pressable layer is %dx%d, below the %dpx minimum a viewer can focus and press",
					i, l.Kind, l.W, l.H, MinInteractiveSide)
			}
		}

		if l.PingName != "" && !ValidPingName(l.PingName) {
			return fmt.Errorf("wire: slide layer %d (%s): ping_name %q must be 1..%d characters of lowercase a-z, 0-9, '_', '-' or '.', starting with a letter or digit",
				i, l.Kind, l.PingName, pingNameMaxLen)
		}
		if len(l.Items) > 0 && l.Kind != LayerKindNav {
			return fmt.Errorf("wire: slide layer %d (%s): only a nav layer may carry items", i, l.Kind)
		}

		// The derive members belong to the derive kind alone. A `derive` block
		// hung on a text layer would be silently ignored by every projection —
		// an operator styling a gradient that never appears, with nothing
		// anywhere saying why — which is this codebase's signature defect seen
		// from the authoring side rather than the serving side.
		if l.Kind != LayerKindDerive {
			if l.Derive != nil {
				return fmt.Errorf("wire: slide layer %d (%s): a derive spec is only used by a %s layer", i, l.Kind, LayerKindDerive)
			}
			if l.DerivedFrom != "" {
				return fmt.Errorf("wire: slide layer %d (%s): derived_from is only used by a %s layer", i, l.Kind, LayerKindDerive)
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

// Alert-slide composition constants (data-model/1 DAT-004c's literal `message`
// override). They are HERE, beside the Layer shape and the canvas bounds, and
// not in the projection that calls AlertSlideLayers, because they are geometry
// against SlideCanvasWidth×SlideCanvasHeight: a copy in the caller would be a
// second place that has to be right about the canvas.
const (
	// alertBackgroundColor is the full-bleed backing an alert is drawn on. A
	// deep red, because the one job of an alert override is to be visibly not
	// the content the screen was showing a second ago.
	alertBackgroundColor = "#8C1C13"
	// alertTextColor is the message foreground: near-white for contrast against
	// alertBackgroundColor at viewing distance.
	alertTextColor = "#FFFFFF"
	// alertTextFontPx is the message size. Sized for a wall panel read from
	// across a room, not for a desk monitor.
	alertTextFontPx = 96
	// alertTextInsetX / alertTextBandY / alertTextBandH place the message as a
	// centered band across the middle of the canvas, inset from both edges so a
	// panel with overscan does not clip the first and last characters.
	alertTextInsetX = 120
	alertTextBandY  = 400
	alertTextBandH  = 280
)

// AlertSlideLayers composes the ONE generated slide a literal-`message` screen
// override is shown as (data-model/1 DAT-004c/DAT-004d): a full-canvas colored
// backing with the message centered across it.
//
// It exists so that an alert needs no authored cast. The daily signage path is
// "put this cast on that screen", but the emergency path an operator or an
// automation reaches for — "the kitchen is closed", "evacuate via the east
// door" — has no time for an authoring round trip, and legacy's `show_alert`
// took a message string for exactly that reason. Generating the slide here, in
// the package that owns the Layer shape and the validator, means the generated
// stack passes ValidateSlideLayers by construction and reaches a player through
// the identical slide path an authored one does — no second rendering route,
// and no chance of a hand-built stack drifting out of the canvas.
//
// The message is used verbatim: it is authored operator text, and this is a
// layer's Text field, which a player draws as a literal string (never a format
// — see Layer.Text: only clock/date/countdown/weather/entity interpret theirs).
func AlertSlideLayers(message string) []Layer {
	return []Layer{
		{
			Kind:  LayerKindRect,
			X:     0,
			Y:     0,
			W:     SlideCanvasWidth,
			H:     SlideCanvasHeight,
			Color: alertBackgroundColor,
		},
		{
			Kind:   LayerKindText,
			X:      alertTextInsetX,
			Y:      alertTextBandY,
			W:      SlideCanvasWidth - 2*alertTextInsetX,
			H:      alertTextBandH,
			Text:   message,
			FontPx: alertTextFontPx,
			Color:  alertTextColor,
			Align:  "center",
		},
	}
}
