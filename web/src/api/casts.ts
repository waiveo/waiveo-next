// api/1 — the CAST family: the authored native-slide documents the Studio edits.
//
// A cast is the authoring-side container for `source: "slide"` playlist content
// (parity plan Phase 2). Its slides carry the SAME layer stack the player/1 wire
// carries — `internal/shared/wire.Layer`, projected onto a lease by
// `internal/feeder/snapshot/screenprograms.go` and
// `internal/relay/schedulehost/schedulehost.go` — so an authored slide and a
// served slide are one shape end to end, never a re-encoding. That is why the
// types below transcribe the Go struct's JSON tags exactly rather than inventing
// a console-side vocabulary.
//
// ── Why the shapes live in exactly one module ───────────────────────────────
// The `/casts` routes have since shipped, and these types are the console's one
// transcription of their schema: the shapes, the path, and the module factory,
// all here. The Studio and the cast library import ONLY the typed module — no
// route page builds a path, a header, or a body — so a change to the served
// schema is an edit in this file and nowhere else. That confinement is what let
// the widget layer kinds be added in one place rather than in every surface
// that touches a layer.
//
// Everything else is already law: the module is built by the same `crud()`
// factory every other family uses, so it inherits Problem parsing, the ETag /
// If-Match precondition (no unconditional overwrite, API-022), the create-time
// Idempotency-Key, and cursor pagination without re-deriving one of them.

import { ApiClient } from "./client";
import { crud } from "./crud";
import type { ResourceModule } from "./resources";

/** The fixed pixel canvas every slide's geometry is expressed in — the same
 * constants the wire declares (`wire.SlideCanvasWidth/Height`). Players force
 * `scaleToFill` onto a 1920×1080 surface, so this is a real coordinate space and
 * not a design convention: an editor that authored against any other canvas
 * would place every layer wrong on the TV. */
export const SLIDE_CANVAS_WIDTH = 1920;
export const SLIDE_CANVAS_HEIGHT = 1080;

/** The nine v1 layer kinds (`wire.LayerKind*`), in the order the openapi enum
 * declares them. A closed set: the wire's validator refuses anything else, and
 * the projector DROPS a slide whose layers do not validate rather than serving
 * it malformed — so an editor that let an operator author a tenth kind would
 * produce a slide that silently never appears.
 *
 * The list must also stay COMPLETE, and that is the half this repo keeps getting
 * wrong — twice now, in the same way. The four live-widget kinds landed on the
 * wire, in the server-side resolver and in the player while this array still
 * held four; then `video` landed on the wire, in the projector and in the player
 * while this array still held eight. The second time was worse than an absent
 * feature: because `validateSlide` reads this array as the closed set, a cast
 * that ALREADY carried a video layer was reported "Unknown layer kind" by the
 * console, which holds the Studio's save gate — so an operator could not save
 * that cast at all, for a reason the server did not agree with. A kind the
 * server accepts and this array omits is not a missing capability, it is a
 * broken editor. */
export const LAYER_KINDS = ["text", "rect", "image", "clock", "date", "countdown", "weather", "entity", "video", "derive"] as const;
export type LayerKind = (typeof LAYER_KINDS)[number];

/** The four kinds whose content is LIVE — computed rather than typed in. Two are
 * computed by the player from its own clock (`date`, `countdown`), two are
 * resolved by the box at Lease issuance and drawn verbatim (`weather`,
 * `entity`); see `internal/slidelive`. They are grouped because that is exactly
 * the set the Studio's widget picker offers, and because a Studio preview can
 * only ever APPROXIMATE the second pair. */
export const WIDGET_LAYER_KINDS = ["date", "countdown", "weather", "entity"] as const;
export type WidgetLayerKind = (typeof WIDGET_LAYER_KINDS)[number];

/** The kinds the player draws as a Label — everything but the two content-bearing
 * kinds and `rect`. These are the layers that carry `font_px`, `color` and
 * `align`, so the properties panel offers those three for exactly this set. */
export const LABEL_LAYER_KINDS = ["text", "clock", "date", "countdown", "weather", "entity"] as const;

/** Whether a layer kind is drawn as a Label (and so carries text styling). */
export function isLabelKind(kind: LayerKind): boolean {
  return (LABEL_LAYER_KINDS as readonly string[]).includes(kind);
}

/** The two kinds whose substance is BYTES in the content origin —
 * `wire.LayerFetchesContent`. They are the pair, not `image` alone, and naming
 * the pair once is the point: every place that tested for `image` by hand is a
 * place `video` was forgotten (the player's own fetch loop carries the same
 * note). Both are authored as an `asset_ref` and fetched + content-address
 * verified by the player before anything is drawn. */
export const CONTENT_LAYER_KINDS = ["image", "video"] as const;
export type ContentLayerKind = (typeof CONTENT_LAYER_KINDS)[number];

/** Whether a layer kind's content is bytes the player must fetch. */
export function isContentKind(kind: LayerKind): kind is ContentLayerKind {
  return (CONTENT_LAYER_KINDS as readonly string[]).includes(kind);
}

/** The three things the OFF-APPLIANCE rasterizer can draw (`wire.DeriveKind*`).
 * `qr` has no native equivalent at all; `text` and `rect` are the STYLED forms
 * of the two native kinds, for when a layer needs a gradient, a drop shadow, a
 * rounded border or a font the device does not ship. */
export const DERIVE_KINDS = ["qr", "text", "rect"] as const;
export type DeriveKind = (typeof DERIVE_KINDS)[number];

/** A derive layer's backing. `solid` sits beside the two gradients so a spec can
 * state a flat fill under a shadow or a rounded border. */
export const DERIVE_FILL_KINDS = ["solid", "linear", "radial"] as const;
export type DeriveFillKind = (typeof DERIVE_FILL_KINDS)[number];

export interface DeriveFill {
  kind: DeriveFillKind;
  from: string;
  /** Required for the gradients, REJECTED on `solid` — the server refuses a
   * second stop a solid fill would ignore. */
  to?: string;
  /** `linear` only. CSS convention: 0 points up, 90 right. */
  angle_deg?: number;
}

export interface DeriveShadow {
  dx?: number;
  dy?: number;
  blur?: number;
  color?: string;
  /** An integer PERCENT, not a fraction: the spec is hashed to produce the
   * raster's content address, and a float would make that address depend on
   * decimal formatting. */
  opacity_pct?: number;
}

export interface DeriveBorder {
  width?: number;
  color?: string;
  radius?: number;
}

/** What the off-appliance rasterizer must draw for one `derive` layer —
 * `wire.DeriveSpec`, field for field. A closed, declarative vocabulary: the
 * renderer drives a real browser, so there is deliberately no way to send it
 * markup. */
export interface DeriveSpec {
  kind: DeriveKind;
  /** `qr`: the payload. */
  data?: string;
  /** `qr`: error-correction level; empty means M. */
  ec_level?: "L" | "M" | "Q" | "H";
  /** `text`: the LITERAL string. Never a format — a raster is a frozen picture,
   * so a clock or a countdown must stay a native live layer. */
  text?: string;
  font_px?: number;
  font_family?: string;
  /** A font file in the content origin, embedded by the renderer before drawing.
   * This is the member that makes a face the device does not ship possible. */
  font_asset_ref?: string;
  /** `text` only — a symbol is always centred and a rect fills its box. */
  align?: LayerAlign;
  /** `text` only. */
  valign?: "top" | "middle" | "bottom";
  /** Text colour for `text`; the DARK MODULE colour for `qr`. */
  color?: string;
  fill?: DeriveFill;
  shadow?: DeriveShadow;
  border?: DeriveBorder;
}

/** Whether a derive layer still needs the off-appliance renderer to run over it
 * — the console-side mirror of `wire.LayerDeriveState`.
 *
 * `pending`: no PNG has ever been produced, so the projection omits the layer
 * and the rest of the slide still draws. `stale`: a PNG exists but the spec or
 * the GEOMETRY has changed since — the raster is rendered at the layer's pixel
 * size, so a resize invalidates it exactly as an edit does — and the OLD picture
 * keeps being served until the tool catches up, because an edit nobody has
 * rendered yet must never blank a screen.
 *
 * The console cannot recompute the digest (it is a hash of the server's own
 * canonical encoding), so "stale" is only reported when the server said so. What
 * this function decides unaided is the one case that matters at authoring time:
 * a layer with no raster at all. */
export function deriveNeedsRender(layer: SlideLayer): boolean {
  return layer.kind === "derive" && !layer.asset_ref;
}

/** The weather template tokens the BOX substitutes (`slidelive`'s closed set).
 * Anything else in the template is literal, so a typo shows on the wall as
 * itself rather than blanking the widget. */
export const WEATHER_TOKENS = ["{temp}", "{tempc}", "{cond}"] as const;

/** The entity template token the box substitutes. An entity layer with no
 * template shows just this, which is why the wire does not require one. */
export const ENTITY_STATE_TOKEN = "{state}";

/** A text layer's horizontal alignment. Optional — an unset align renders at the
 * player's own default. */
export type LayerAlign = "left" | "center" | "right";

/**
 * One positioned native element in a slide, transcribed field-for-field from
 * `wire.Layer`. Array order is z-order: index 0 is drawn first (furthest back).
 *
 * `text` carries two different meanings by kind and that is the wire's design,
 * not a shortcut here: for `text` it is the literal string; for `clock` it is the
 * Go reference-time LAYOUT the player formats the current local time through
 * (`"15:04"`, `"3:04 PM"`, `"Mon 15:04"`) and re-renders every second. It is not
 * a strftime string — the producer authors it and the player interprets it under
 * that one convention, so the Studio offers layouts in that grammar.
 */
export interface SlideLayer {
  kind: LayerKind;
  x: number;
  y: number;
  w: number;
  h: number;
  /** The literal for `text`. A FORMAT for every generated kind: the Go
   * reference-time layout for `clock`/`date`, the D/H/M/S remaining-time layout
   * for `countdown` (optional), and the substitution template the BOX fills for
   * `weather` (required) / `entity` (optional). Unused by rect/image. */
  text?: string;
  /** `image`/`video`: the content-addressed `sha256:` ref the player verifies
   * against. The only AUTHORED half of a content-bearing layer. */
  asset_ref?: string;
  /** `image`/`video`: the content-origin fetch URL for those bytes. DERIVED —
   * a producer mints it from the asset_ref at projection time, so an authored
   * layer need not carry one (`wire.ValidateAuthoredSlideLayers`). */
  url?: string;
  /** `countdown`: the target instant as Unix epoch MILLISECONDS, UTC — an
   * absolute instant, so the player counts down without knowing the authoring
   * timezone. Never a local wall time and never seconds. */
  target_ms?: number;
  /** `entity`: the platform entity whose live state this widget shows. The
   * AUTHORED half of an entity widget. */
  entity_id?: string;
  // There is deliberately NO `value` member. `wire.Layer` carries one for
  // `weather`/`entity` — the display string the BOX resolves at Lease issuance —
  // but it is derived, never authored: `api/openapi.yaml`'s SlideLayer does not
  // declare it and the schema is `additionalProperties: false`, so a console
  // that modelled it could only ever send a 400. The Studio previews those two
  // kinds from their own template instead.
  /** Any Label-drawn kind: pixel font size. Optional styling. */
  font_px?: number;
  /** `rect`: the fill (required). A Label kind's foreground (optional). */
  color?: string;
  /** A Label kind's horizontal alignment. Optional. */
  align?: LayerAlign;
  /** `derive`: what the OFF-APPLIANCE rasterizer must draw. Required for that
   * kind and rejected on every other — a derive block hung on a text layer would
   * be ignored by every projection, which is a styling control that silently
   * does nothing. */
  derive?: DeriveSpec;
  /** `derive`: the spec digest the current `asset_ref` was rendered from. Written
   * by `waiveo-derive`, never authored — the Studio round-trips it untouched so
   * that saving an unrelated edit does not make every raster look stale. */
  derived_from?: string;
}

/** One slide of a cast: an ordered layer stack plus how long it holds the screen.
 * `duration_ms` is optional — an omitted duration means the playlist item's own
 * default applies rather than zero. */
export interface CastSlide {
  id: string;
  duration_ms?: number;
  layers: SlideLayer[];
}

/** A cast row. The api/1 baseline (id/scope_node/revision/timestamps) plus the
 * authored slides and the two cast-level settings.
 *
 * `default_duration_ms` is the cast's own dwell time for slides that state none:
 * a slidecast's slides overwhelmingly share one timing, so this is what makes
 * "hold every slide for eight seconds" one edit rather than one per slide. It is
 * the THIRD step of the resolution — slide `duration_ms`, then the referencing
 * playlist item's `duration_seconds`, then this, then the player's own default.
 * It is `number | null` because null is how a PATCH CLEARS it (the body
 * shallow-merges, so an omitted member means "leave it alone").
 *
 * `template` marks the cast a starting point rather than something a screen
 * plays. The server refuses a playlist item that references one, so the flag is
 * load-bearing rather than a console-side label. */
export interface Cast {
  id: string;
  scope_node: string;
  name: string;
  slides: CastSlide[];
  default_duration_ms?: number | null;
  template?: boolean;
  external_id?: string | null;
  labels?: Record<string, string>;
  revision: number;
  created_at: number;
  updated_at: number;
}

export interface CastCreate {
  id?: string;
  scope_node: string;
  name: string;
  slides: CastSlide[];
  default_duration_ms?: number | null;
  template?: boolean;
  external_id?: string | null;
  labels?: Record<string, string>;
}

export interface CastUpdate {
  name?: string;
  slides?: CastSlide[];
  /** `null` CLEARS the cast-wide default; omitting the member leaves it alone. */
  default_duration_ms?: number | null;
  template?: boolean;
  external_id?: string | null;
  labels?: Record<string, string>;
}

/** The cast family's api/1 surface — the uniform CRUD module, nothing bespoke.
 * Duplicate is a client-side read-then-create rather than a server verb, so it
 * needs no operation of its own. */
export type CastsModule = ResourceModule<Cast, CastCreate, CastUpdate>;

/** Build the cast module over an ApiClient. The ONE place `/casts` is named. */
export function createCastsModule(client: ApiClient): CastsModule {
  return crud<Cast, CastCreate, CastUpdate>(client, "/casts");
}

// ── Client-side slide validation ─────────────────────────────────────────────

/** One reason a slide would be refused. `index` names the offending layer, or is
 * `null` when the problem is the slide as a whole (an empty stack). */
export interface SlideProblem {
  index: number | null;
  message: string;
}

const HEX_COLOR = /^#[0-9a-fA-F]{6}$/;

/**
 * The console-side mirror of `wire.ValidateSlideLayers`, and it earns its
 * duplication: the projector does not REJECT an invalid slide back to the
 * author, it DROPS it — `screenprograms.go` and `schedulehost.go` skip a slide
 * whose layers fail validation, so the failure mode without this check is a
 * saved cast that simply never appears on the TV, with nothing on the authoring
 * surface to explain why. Running the same rules at the point of authorship
 * turns that silence into a message next to the layer that caused it.
 *
 * It is a mirror, not the authority. The wire's copy still runs server-side; this
 * one exists to be legible to the person who can fix the problem.
 *
 * Which copy it mirrors matters, and the answer is the AUTHORING gate
 * (`wire.ValidateAuthoredSlideLayers`), not the serving one. They differ in
 * exactly one rule — a content-bearing layer's `url` is derived by the producer
 * at projection time, so it is not required of an author — and a mirror stricter
 * than the server is not a safety margin: it reports a slide the server would
 * accept as invalid, which in the Studio HOLDS THE SAVE for the whole cast. That
 * is the same failure this function shipped with for `video` (an unknown-kind
 * error for a kind the server accepts), reached from the other direction.
 */
export function validateSlide(slide: CastSlide): SlideProblem[] {
  const problems: SlideProblem[] = [];
  if (slide.layers.length === 0) {
    problems.push({ index: null, message: "A slide needs at least one layer to render." });
    return problems;
  }
  slide.layers.forEach((l, i) => {
    const at = (message: string) => problems.push({ index: i, message });
    if (!(LAYER_KINDS as readonly string[]).includes(l.kind)) {
      at(`Unknown layer kind "${l.kind}".`);
      return;
    }
    if (l.w <= 0 || l.h <= 0) at("Width and height must be positive.");
    if (l.x < 0 || l.y < 0) at("Position must be on the canvas (x and y at least 0).");
    if (l.x + l.w > SLIDE_CANVAS_WIDTH || l.y + l.h > SLIDE_CANVAS_HEIGHT) {
      at(`Extends past the ${SLIDE_CANVAS_WIDTH}×${SLIDE_CANVAS_HEIGHT} canvas.`);
    }
    if (l.kind === "text" && !l.text) at("Text is required.");
    if (l.kind === "clock" && !l.text) at("A clock needs a time format.");
    // Both content-bearing kinds, one rule — `url` deliberately not demanded:
    // see this function's doc.
    if (isContentKind(l.kind) && !l.asset_ref) {
      at(l.kind === "video" ? "Pick a video from the media library." : "Pick an image from the media library.");
    }
    if (l.kind === "rect" && !l.color) at("A rectangle needs a fill colour.");
    // The live widgets. Each mirrors one arm of wire.ValidateSlideLayers, and
    // each is a rule an operator can break in the ordinary course of authoring —
    // inserting a countdown and never setting its target, clearing a weather
    // template, picking an entity and then deleting the device.
    if (l.kind === "date" && !l.text) at("A date needs a date format.");
    if (l.kind === "countdown" && !(l.target_ms && l.target_ms > 0)) {
      at("A countdown needs a target date and time.");
    }
    if (l.kind === "weather" && !l.text) at("A weather widget needs a display template, e.g. \u007Btemp\u007D\u00B0 \u007Bcond\u007D.");
    if (l.kind === "entity" && !l.entity_id) at("Choose which entity this widget shows.");
    // The rasterized fallback. These mirror `wire.ValidateDeriveSpec`'s arms —
    // and only the ones an operator can break while authoring, for the reason
    // this function's doc gives: a mirror STRICTER than the server holds the save
    // for a cast the server would accept.
    if (l.kind === "derive") {
      const d = l.derive;
      if (!d) {
        at("A rasterized layer needs something to draw.");
      } else if (!(DERIVE_KINDS as readonly string[]).includes(d.kind)) {
        at(`Unknown rasterized kind "${d.kind}".`);
      } else {
        if (d.kind === "qr" && !d.data) at("A QR code needs a link or text to encode.");
        if (d.kind === "text" && !d.text) at("Text is required.");
        if (d.kind === "rect" && !d.fill) at("A styled panel needs a fill.");
        if (d.fill && d.fill.kind !== "solid" && !d.fill.to) at("A gradient needs a second colour.");
        if (d.fill && d.fill.kind === "solid" && d.fill.to) at("A solid fill has no second colour.");
        if (d.border && d.border.width && !d.border.color) at("A border with a width needs a colour.");
        for (const [what, value] of [["Colour", d.color], ["Fill", d.fill?.from], ["Gradient end", d.fill?.to],
          ["Shadow colour", d.shadow?.color], ["Border colour", d.border?.color]] as const) {
          if (value && !HEX_COLOR.test(value)) at(`${what} "${value}" is not a #RRGGBB value.`);
        }
      }
    }
    // Every OTHER kind must not carry one: the server refuses it outright, so a
    // console that let one through would produce a 422 an operator cannot read.
    if (l.kind !== "derive" && l.derive) at("Only a rasterized layer carries a rasterizer spec.");
    if (l.color && !HEX_COLOR.test(l.color)) at(`Colour "${l.color}" is not a #RRGGBB value.`);
  });
  return problems;
}

/** Every problem across a whole cast, keyed by slide index — what the Studio's
 * save gate and the filmstrip's warning badges read. */
export function validateCastSlides(slides: CastSlide[]): Map<number, SlideProblem[]> {
  const out = new Map<number, SlideProblem[]>();
  slides.forEach((s, i) => {
    const problems = validateSlide(s);
    if (problems.length > 0) out.set(i, problems);
  });
  return out;
}
