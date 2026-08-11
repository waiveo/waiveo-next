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

/** The eleven layer kinds (`wire.LayerKind*`), in the order the openapi enum
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
export const LAYER_KINDS = ["text", "rect", "image", "clock", "date", "countdown", "weather", "entity", "video", "ping", "nav", "derive"] as const;
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
 * `align`, so the properties panel offers those three for exactly this set.
 *
 * The two INTERACTIVE kinds are in it because the player draws them with the
 * identical Label path: a `ping` is its label, and a `nav` draws one label per
 * item through the same styling (PhotonScene.renderSlide). A button an operator
 * cannot set the size or colour of is a button they cannot make readable across
 * a room, which is the only place these are ever used. */
export const LABEL_LAYER_KINDS = ["text", "clock", "date", "countdown", "weather", "entity", "ping", "nav"] as const;

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

/** The two INTERACTIVE kinds — the first layers whose value flows from the
 * screen back to the box. A `ping` is a button the viewer focuses with the
 * remote and presses OK on, raising a durable `screen.interaction` event an
 * automation can trigger on; a `nav` is a D-pad menu whose items jump to another
 * slide of the same cast. */
export const INTERACTIVE_LAYER_KINDS = ["ping", "nav"] as const;

/** Whether a layer is FOCUSABLE on the player — the console-side mirror of
 * `wire.LayerIsInteractive`, and it must mirror BOTH of its arms.
 *
 * A `nav` is focusable by kind (its items are the targets). Everything else is
 * focusable exactly when it carries a `ping_name`, which is legal on ANY kind
 * and is what turns an ordinary widget into an interactive one. Testing the KIND
 * alone here would draw no focus affordance on the very layers an operator most
 * needs to be told are now pressable — the entity reading they just made
 * interactive — while the player happily focuses them. */
export function isInteractiveLayer(layer: SlideLayer): boolean {
  return layer.kind === "nav" || Boolean(layer.ping_name);
}

/** A ping name must be a slug: the value travels to a rules/1 `event` trigger's
 * `match` constraint, where an automation author retypes it by hand and
 * "Front Desk " would never match "front desk". Mirrors `wire.ValidPingName`. */
export const PING_NAME_PATTERN = /^[a-z0-9][a-z0-9_.-]*$/;
export const PING_NAME_MAX = 64;

/** A nav layer's menu is bounded — the items share the layer's box, so past this
 * many the per-item region is too small to read across a room. Mirrors
 * `wire.maxNavItems`. */
export const NAV_ITEMS_MAX = 8;

/** The smallest side, in canvas pixels, a FOCUSABLE region may have — a
 * pressable layer's own box, or one cell of a menu. Mirrors
 * `wire.MinInteractiveSide`.
 *
 * It is a legibility floor: the canvas is shown on a wall and driven by a remote
 * from across a room, and below roughly this size a focus outline cannot be told
 * apart at viewing distance. The Studio's canvas is scaled DOWN, which is
 * precisely what makes the mistake easy to make here and impossible to see. */
export const MIN_INTERACTIVE_SIDE = 48;

/** One entry of a `nav` layer's menu (`wire.NavItem`). Both members are
 * required: a label-less item is an unreadable target, and a target-less one is
 * a menu entry that accepts a press and performs nothing. */
export interface NavItem {
  label: string;
  /** The cast-local id (`CastSlide.id`) of the slide pressing OK jumps to. */
  target_slide_id: string;
}

/** Lay a nav layer's items out inside its own box: one `[x, y, w, h]` per item,
 * in item order. The console-side mirror of `wire.NavItemRects`, which the
 * PLAYER also transcribes (PhotonScene.wvNavItemRects) — the three must agree,
 * because this one decides where the Studio DRAWS an item and the player's
 * decides where it FOCUSES one, and a disagreement puts the focus ring somewhere
 * other than the label it belongs to.
 *
 * The axis follows the box's own aspect (wider than tall is a row, otherwise a
 * column), so the drawn geometry alone decides orientation and there is no
 * second authored field to contradict it. The last item absorbs the
 * integer-division remainder so the items exactly fill the box. */
export function navItemRects(layer: SlideLayer): Array<[number, number, number, number]> {
  const items = layer.items ?? [];
  const n = items.length;
  if (n <= 0) return [];
  const out: Array<[number, number, number, number]> = [];
  if (layer.w >= layer.h) {
    const cell = Math.floor(layer.w / n);
    for (let i = 0; i < n; i++) {
      out.push([layer.x + cell * i, layer.y, i === n - 1 ? layer.w - cell * (n - 1) : cell, layer.h]);
    }
    return out;
  }
  const cell = Math.floor(layer.h / n);
  for (let i = 0; i < n; i++) {
    out.push([layer.x, layer.y + cell * i, layer.w, i === n - 1 ? layer.h - cell * (n - 1) : cell]);
  }
  return out;
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
  /** The stable name a press on this layer reports. Required for `ping`, and
   * OPTIONAL ON EVERY OTHER KIND — carrying one is what makes an ordinary widget
   * focusable and pressable (`wire.LayerIsInteractive`). It is the value a
   * rules/1 `event` trigger matches on. */
  ping_name?: string;
  /** `nav`: the ordered menu. Legal on no other kind. */
  items?: NavItem[];
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
    // The two INTERACTIVE kinds.
    if (l.kind === "ping") {
      if (!l.text) at("A button needs a label people can read before pressing it.");
      if (!l.ping_name) at("A button needs an event name, so an automation has something to trigger on.");
    }
    if (l.kind === "nav") {
      if (!l.items || l.items.length === 0) at("A menu needs at least one item.");
      if (l.items && l.items.length > NAV_ITEMS_MAX) at(`A menu holds at most ${NAV_ITEMS_MAX} items.`);
      (l.items ?? []).forEach((item, n) => {
        if (!item.label) at(`Menu item ${n + 1} needs a label.`);
        if (!item.target_slide_id) at(`Menu item ${n + 1} needs a slide to jump to.`);
      });
    }
    // The two interactive MEMBERS, checked on EVERY layer rather than inside
    // the kind branches above — the same split `wire.validateSlideLayers`
    // makes, and for the same reason. A `ping_name` is legal on any kind, so
    // checking its grammar only under `ping` leaves every interactive widget
    // ungated; `items` are legal only on `nav`, so their presence elsewhere can
    // only be caught out here. Each is the MIRROR direction of the other, which
    // is exactly the direction this repo's past half-fixes missed.
    if (l.ping_name && (!PING_NAME_PATTERN.test(l.ping_name) || l.ping_name.length > PING_NAME_MAX)) {
      at(`Event name "${l.ping_name}" must be lower-case letters, digits, "_", "-" or "." (up to ${PING_NAME_MAX}).`);
    }
    if (l.items && l.items.length > 0 && l.kind !== "nav") at("Only a menu layer can have menu items.");
    // The legibility floor, over BOTH arms of "interactive" — the same rule and
    // the same two definitions the wire applies (`wire.MinInteractiveSide` over
    // `LayerIsInteractive`/`NavItemRects`). Mirrored here because the editor's
    // canvas is scaled down: a region that looks perfectly clickable in a
    // 600-pixel-wide editor is a thumbnail on the wall, and without this the
    // first the operator hears of it is a 422 on save.
    if (isInteractiveLayer(l)) {
      if (l.kind === "nav") {
        navItemRects(l).forEach(([, , iw, ih], n) => {
          if (iw < MIN_INTERACTIVE_SIDE || ih < MIN_INTERACTIVE_SIDE) {
            at(`Menu item ${n + 1} is ${iw}×${ih} — too small to see focus on across a room (minimum ${MIN_INTERACTIVE_SIDE}px). Make the menu bigger or carry fewer items.`);
          }
        });
      } else if (l.w < MIN_INTERACTIVE_SIDE || l.h < MIN_INTERACTIVE_SIDE) {
        at(`A pressable layer must be at least ${MIN_INTERACTIVE_SIDE}×${MIN_INTERACTIVE_SIDE} so a viewer can see focus on it and aim a remote at it.`);
      }
    }
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
        // TYPOGRAPHY is text-only, and the server REFUSES it elsewhere rather
        // than ignoring it (`wire.ValidateDeriveSpec`): the renderer writes the
        // size and family into the text rule only and embeds the face for a text
        // run only. This mirror exists because the console's own controls cannot
        // produce the violation — a spec's kind is fixed at insert — but a cast
        // authored through the API or shipped in a pack can, and then Save would
        // send a body the server answers with an opaque 422.
        if (d.kind !== "text") {
          if (d.font_px) at("Font size only applies to rasterized text.");
          if (d.font_family) at("Font family only applies to rasterized text.");
          if (d.font_asset_ref) at("A custom font file only applies to rasterized text.");
        }
        if (d.kind === "rect" && d.color) at("A styled panel has no foreground colour — set its fill instead.");
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

/**
 * Every nav item in `slides` whose target names no slide of the SAME cast, as
 * problems keyed by the slide the menu sits on.
 *
 * This rule cannot live in `validateSlide`: it sees one slide and has no idea
 * what the other slides are called. It is the console mirror of
 * `datamodel.checkNavTargets` (`CAST_NAV_TARGET_UNKNOWN`), which is the server's
 * cast-level gate — and mirroring it here is what turns a 422 on save into a
 * message beside the item while the operator is still building the menu.
 *
 * It matters most on the ordinary edit that breaks it: deleting a slide another
 * slide's menu points at. Nothing about that deletion looks wrong, and without
 * this the next save fails with a server error naming an item the operator has
 * long since stopped thinking about.
 */
export function validateNavTargets(slides: CastSlide[]): Map<number, SlideProblem[]> {
  const declared = new Set(slides.map((s) => s.id).filter((id) => id !== ""));
  const out = new Map<number, SlideProblem[]>();
  slides.forEach((slide, si) => {
    slide.layers.forEach((l, li) => {
      if (l.kind !== "nav") return;
      (l.items ?? []).forEach((item, n) => {
        // An EMPTY target is already reported by validateSlide; reporting it
        // again here would show two messages for one mistake.
        if (!item.target_slide_id || declared.has(item.target_slide_id)) return;
        const list = out.get(si) ?? [];
        list.push({
          index: li,
          message: `Menu item ${n + 1} jumps to a slide ("${item.target_slide_id}") this cast no longer has.`,
        });
        out.set(si, list);
      });
    });
  });
  return out;
}

/** Every problem across a whole cast, keyed by slide index — what the Studio's
 * save gate and the filmstrip's warning badges read. */
export function validateCastSlides(slides: CastSlide[]): Map<number, SlideProblem[]> {
  const out = new Map<number, SlideProblem[]>();
  slides.forEach((s, i) => {
    const problems = validateSlide(s);
    if (problems.length > 0) out.set(i, problems);
  });
  // The CAST-level rules are merged in here rather than left for a caller to
  // remember: every surface that gates on slide validity reads this one function
  // (the save gate, the filmstrip badges, the properties panel), so a rule added
  // beside it instead of into it would be enforced by whichever of them happened
  // to be updated.
  for (const [i, navProblems] of validateNavTargets(slides)) {
    out.set(i, [...(out.get(i) ?? []), ...navProblems]);
  }
  return out;
}
