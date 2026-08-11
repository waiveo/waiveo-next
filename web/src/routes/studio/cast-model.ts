// The Studio's editing MODEL — every mutation the editor can make to a cast,
// as pure functions over plain data.
//
// The whole editor is one reducer over this state. That is a deliberate choice
// rather than a stylistic one: the thing a slide editor gets wrong is not its
// pixels but its MODEL — a drag that quietly pushes a layer off the canvas, a
// reorder that scrambles z-order, a delete that leaves the selection pointing at
// a slide that no longer exists, a save that ships geometry the projector will
// refuse. Keeping all of that in pure functions means those are the things the
// tests can drive directly and exactly, instead of being inferred from what
// rendered.
//
// The canvas is FIXED at 1920×1080 (wire.SlideCanvasWidth/Height). Nothing here
// works in screen pixels — the view scales the canvas to fit and converts back
// before it calls in, so every coordinate in this file is canvas-space.

import {
  SLIDE_CANVAS_HEIGHT,
  SLIDE_CANVAS_WIDTH,
  type Cast,
  type CastSlide,
  type CastUpdate,
  type LayerKind,
  type SlideLayer,
} from "@/api";
import { COUNTDOWN_DEFAULT_LAYOUT } from "./go-time-layout";

/** The smallest a layer may be dragged down to. A layer with no area is
 * invisible AND unselectable — it cannot be grabbed again to fix it — so the
 * floor is an escape-hatch guarantee, not a style rule. */
export const MIN_LAYER_SIZE = 16;

/** The resize grips, named by the edge/corner they move (compass directions). */
export const RESIZE_HANDLES = ["nw", "n", "ne", "e", "se", "s", "sw", "w"] as const;
export type ResizeHandle = (typeof RESIZE_HANDLES)[number];

/** Clamp a layer's box into the canvas at a legible minimum size, and round it
 * to whole pixels — the wire's geometry is integer, so a fractional drag that
 * survived to the save body would be a type mismatch the server rejects. */
export function clampLayer(layer: SlideLayer): SlideLayer {
  const w = Math.round(Math.min(Math.max(layer.w, MIN_LAYER_SIZE), SLIDE_CANVAS_WIDTH));
  const h = Math.round(Math.min(Math.max(layer.h, MIN_LAYER_SIZE), SLIDE_CANVAS_HEIGHT));
  const x = Math.round(Math.min(Math.max(layer.x, 0), SLIDE_CANVAS_WIDTH - w));
  const y = Math.round(Math.min(Math.max(layer.y, 0), SLIDE_CANVAS_HEIGHT - h));
  return { ...layer, x, y, w, h };
}

/** Move a layer by a canvas-space delta. Clamped, so dragging at the edge slides
 * along it rather than pushing the layer off-canvas into a slide the projector
 * would drop. */
export function moveLayerBy(layer: SlideLayer, dx: number, dy: number): SlideLayer {
  return clampLayer({ ...layer, x: layer.x + dx, y: layer.y + dy });
}

/**
 * Resize a layer by dragging one grip. The OPPOSITE edge stays pinned — that is
 * what makes a resize feel like a resize rather than a move — so the box is
 * computed in edge coordinates and only converted back to x/y/w/h at the end.
 */
export function resizeLayerBy(
  layer: SlideLayer,
  handle: ResizeHandle,
  dx: number,
  dy: number,
): SlideLayer {
  let left = layer.x;
  let top = layer.y;
  let right = layer.x + layer.w;
  let bottom = layer.y + layer.h;

  if (handle.includes("w")) left = Math.min(left + dx, right - MIN_LAYER_SIZE);
  if (handle.includes("e")) right = Math.max(right + dx, left + MIN_LAYER_SIZE);
  if (handle.includes("n")) top = Math.min(top + dy, bottom - MIN_LAYER_SIZE);
  if (handle.includes("s")) bottom = Math.max(bottom + dy, top + MIN_LAYER_SIZE);

  left = Math.max(left, 0);
  top = Math.max(top, 0);
  right = Math.min(right, SLIDE_CANVAS_WIDTH);
  bottom = Math.min(bottom, SLIDE_CANVAS_HEIGHT);

  return clampLayer({ ...layer, x: left, y: top, w: right - left, h: bottom - top });
}

/** The default countdown target: the start of the NEXT local day.
 *
 * A countdown's `target_ms` must be a positive absolute instant or the layer is
 * refused, so an inserted countdown has to arrive with SOME target — and a
 * meaningless one (the epoch, or "now") would render as a finished countdown of
 * all zeroes, which reads on the canvas as a broken widget rather than as a
 * placeholder. Midnight tonight is a real, obviously-provisional instant that
 * counts down visibly the moment it is inserted, so the operator can see the
 * widget working while they go and set the date they actually meant. */
function nextMidnightMs(nowMs: number): number {
  const d = new Date(nowMs);
  d.setHours(24, 0, 0, 0);
  return d.getTime();
}

/** A freshly inserted layer of `kind`, placed somewhere visible and valid.
 *
 * Every kind but the two content-bearing ones and `entity` lands VALID: it has
 * the fields wire.ValidateAuthoredSlideLayers requires, so inserting it and
 * saving immediately produces a slide that renders. Those three cannot — an
 * image's or a video's bytes must be chosen from the content origin and an
 * entity's subject from the device plane — so each lands deliberately incomplete
 * and the properties panel asks for the one missing thing rather than the editor
 * inventing an `asset_ref` or an `entity_id` that resolves to nothing. The save
 * gate holds until it is chosen, which is the honest sequence: a slide
 * referencing a nonexistent entity would be accepted by the server and then draw
 * a dash forever.
 *
 * `nowMs` is a PARAMETER rather than a `Date.now()` read inside, for the same
 * reason `newSlide` takes its id: it is the one input here that is not a
 * constant, and passing it keeps "what does inserting a countdown produce" a
 * question a test can answer exactly. Only `countdown` consults it. */
export function defaultLayer(kind: LayerKind, nowMs: number = Date.now()): SlideLayer {
  switch (kind) {
    case "text":
      return { kind, x: 160, y: 420, w: 1600, h: 240, text: "New text", font_px: 96, color: "#FFFFFF", align: "left" };
    case "rect":
      return { kind, x: 240, y: 240, w: 800, h: 400, color: "#F368C4" };
    case "clock":
      return { kind, x: 1160, y: 120, w: 600, h: 200, text: "3:04 PM", font_px: 120, color: "#FFFFFF", align: "right" };
    case "image":
      return { kind, x: 240, y: 200, w: 960, h: 540 };
    case "video":
      // The same 16:9 box an image lands in, and for the same reason: it is the
      // shape the content almost always is, so the first thing the operator does
      // is position it rather than reshape it. Like an image it lands with no
      // asset_ref — bytes are chosen, never invented.
      return { kind, x: 240, y: 200, w: 960, h: 540 };
    case "date":
      // Same formatter and same layout grammar as the clock — a date IS a time
      // format on this wire — so the default is a layout, not a rendered string.
      return { kind, x: 160, y: 200, w: 1200, h: 160, text: "Monday, January 2", font_px: 84, color: "#FFFFFF", align: "left" };
    case "countdown":
      return {
        kind, x: 160, y: 600, w: 1200, h: 220,
        text: COUNTDOWN_DEFAULT_LAYOUT,
        target_ms: nextMidnightMs(nowMs),
        font_px: 140, color: "#FFFFFF", align: "left",
      };
    case "weather":
      // The template is what the BOX substitutes into ({temp} °F, {cond}); the
      // literal degree sign and spacing are the author's and survive verbatim,
      // which is why a default that already looks like a finished widget is the
      // right starting point rather than a bare token.
      return { kind, x: 1080, y: 100, w: 720, h: 160, text: "{temp}\u00B0 {cond}", font_px: 88, color: "#FFFFFF", align: "right" };
    case "entity":
      // No entity_id: see this function's doc. The template defaults to the
      // bare state token, which is also what the box assumes for an empty one.
      return { kind, x: 160, y: 860, w: 1200, h: 140, text: "{state}", font_px: 72, color: "#FFFFFF", align: "left" };
    case "ping":
      // A button lands COMPLETE — label and event name both set — so inserting
      // one and saving produces a slide that renders AND a press that reaches an
      // automation. The default name is a slug (wire.ValidPingName), because a
      // default that failed its own grammar would put an error on the panel the
      // instant the operator inserted it.
      //
      // Centered, and sized for a remote-driven button seen across a room rather
      // than for a caption.
      return {
        kind, x: 660, y: 780, w: 600, h: 140,
        text: "Press OK", ping_name: "button_1",
        font_px: 72, color: "#FFFFFF", align: "center",
      };
    case "nav":
      // A menu lands EMPTY of items on purpose — the one kind besides the
      // content-bearing pair and `entity` that cannot land valid. An item's
      // target must name a slide of this cast, and `defaultLayer` is a pure
      // function of a kind and a clock: it does not know which cast it is being
      // inserted into, let alone which slides that cast has. Inventing a target
      // would produce exactly the dead-end menu item the whole nav design
      // refuses. The panel asks for the items, with the cast's real slides in a
      // dropdown, and the save gate holds until at least one is chosen.
      //
      // Wide rather than tall, so the default orientation is the horizontal row
      // a signage menu almost always is (the layout axis follows the box).
      return { kind, x: 260, y: 900, w: 1400, h: 120, items: [], font_px: 44, color: "#FFFFFF" };
  }
}

/**
 * A change to some of a layer's fields, where an explicit `undefined` REMOVES
 * the field rather than writing it.
 *
 * That distinction is the wire's, not a nicety: every optional field on
 * `wire.Layer` is `omitempty`, so "no image chosen" is the ABSENCE of
 * `asset_ref`, not a present key holding nothing. Clearing an image by writing
 * `asset_ref: undefined` into the object would serialise a key the server's
 * strict decoder does not expect. `applyPatch` deletes instead.
 */
/** The fields a layer must always have — a patch may change one but can never
 * remove it, which is why they are split out below. */
type RequiredLayerField = "kind" | "x" | "y" | "w" | "h";
/** …and the `omitempty` ones, where `undefined` means "remove the key". */
type OptionalLayerField = Exclude<keyof SlideLayer, RequiredLayerField>;

export type LayerPatch = Partial<Pick<SlideLayer, RequiredLayerField>> & {
  [K in OptionalLayerField]?: SlideLayer[K] | undefined;
};

function applyPatch(layer: SlideLayer, patch: LayerPatch): SlideLayer {
  const next = { ...layer } as SlideLayer & Record<string, unknown>;
  for (const [key, value] of Object.entries(patch)) {
    if (value === undefined) delete next[key];
    else next[key] = value;
  }
  return next;
}

/**
 * A brand-new slide — VALID as authored, never an empty stack.
 *
 * A zero-layer slide is not a neutral starting point, it is a slide the whole
 * stack refuses: `CastSlide.layers` is `minItems: 1` in api/openapi.yaml and
 * request bodies are schema-checked at the door, the store gate refuses it
 * again (`wire.ValidateAuthoredSlideLayers`, CAST_SLIDE_LAYERS_INVALID), and a
 * PATCH is atomic — so one empty slide makes the save of every OTHER slide in
 * the cast fail too. Seeding one text layer costs the operator one edit (the
 * text is placeholder and selected) and removes an entire failure mode.
 *
 * `text` rather than `rect` because it is the layer that says what the slide
 * is for, and because an operator who wanted a shape will replace it in one
 * gesture either way.
 *
 * Ids are supplied by the caller rather than minted here so the reducer stays
 * pure (and a test can assert an exact model).
 */
export function newSlide(id: string): CastSlide {
  return { id, layers: [defaultLayer("text")] };
}

/** Move `from` to `to` in a copy of the list. Out-of-range indices are a no-op
 * (returning the SAME array) — a drag that ends outside the strip must not
 * silently move something to an end the operator did not aim at. */
export function reorder<T>(list: T[], from: number, to: number): T[] {
  if (from === to) return list;
  if (from < 0 || from >= list.length || to < 0 || to >= list.length) return list;
  const next = list.slice();
  const [moved] = next.splice(from, 1);
  next.splice(to, 0, moved as T);
  return next;
}

// ── The editor state machine ────────────────────────────────────────────────

/** Everything the Studio holds that is not the loaded server record: the draft
 * name and slides, where the operator is (which slide, which layer), and whether
 * that draft differs from what was last read or saved. */
export interface StudioState {
  name: string;
  /** The cast-wide default dwell time, or null for "no cast-wide default".
   *
   * `null` rather than `undefined` because it is what the PATCH SENDS: a body
   * shallow-merges over the stored row, so omitting the member means "leave it
   * alone" and there would be no way to turn a default back off. An editor whose
   * state could not express "cleared" would have a control that only ever goes
   * one way. */
  defaultDurationMs: number | null;
  slides: CastSlide[];
  /** The slide on the canvas. Always a valid index while `slides` is non-empty. */
  slideIndex: number;
  /** The selected layer WITHIN the current slide, or null for no selection. */
  layerIndex: number | null;
  /** Unsaved edits exist. Drives the save affordance AND the leave guard. */
  dirty: boolean;
}

export type StudioAction =
  /** A cast arrived from the server (initial load, or a re-read after a
   * conflict): adopt it wholesale and forget the draft. */
  | { type: "loaded"; cast: Cast }
  /** A save landed: adopt the server's canonical copy but KEEP the operator
   * where they were — a save that jumped the selection back to slide 1 would
   * punish saving often, which is the habit an editor wants. */
  | { type: "saved"; cast: Cast }
  | { type: "rename"; name: string }
  | { type: "setDefaultDuration"; durationMs: number | null }
  | { type: "selectSlide"; index: number }
  | { type: "addSlide"; id: string }
  | { type: "duplicateSlide"; index: number; id: string }
  | { type: "deleteSlide"; index: number }
  | { type: "moveSlide"; from: number; to: number }
  | { type: "setSlideDuration"; index: number; durationMs: number | null }
  | { type: "selectLayer"; index: number | null }
  | { type: "insertLayer"; layer: SlideLayer }
  | { type: "patchLayer"; index: number; patch: LayerPatch }
  | { type: "moveLayer"; index: number; dx: number; dy: number }
  | { type: "resizeLayer"; index: number; handle: ResizeHandle; dx: number; dy: number }
  | { type: "deleteLayer"; index: number }
  | { type: "reorderLayer"; from: number; to: number };

/** The initial state before anything has loaded — an empty draft, not dirty. */
export const EMPTY_STUDIO_STATE: StudioState = {
  name: "",
  defaultDurationMs: null,
  slides: [],
  slideIndex: 0,
  layerIndex: null,
  dirty: false,
};

/** The state a loaded cast starts the editor in.
 *
 * `default_duration_ms` normalizes to null through `?? null` because the server
 * expresses "no cast-wide default" two ways that mean the same thing: the member
 * absent (never set) and the member null (set once and then cleared, which is
 * how a shallow-merge PATCH clears it and therefore what a cleared row is
 * SERVED as). The editor holds one of those, not both. */
export function studioStateFromCast(cast: Cast): StudioState {
  return {
    name: cast.name,
    defaultDurationMs: cast.default_duration_ms ?? null,
    slides: cast.slides.map(withoutDerivedLayerFields),
    slideIndex: 0,
    layerIndex: null,
    dirty: false,
  };
}

/** A loaded slide with every layer's DERIVED members dropped, so the document
 * the editor holds contains only what an operator authored.
 *
 * Today that is one member: a content-bearing layer's `url`. It is minted by the
 * server per response and it EXPIRES (`internal/feeder/contenturl`), so a copy
 * saved with the cast is a link that dies — which is exactly what happened,
 * because the picker patched the listing's url into the layer and the save sent
 * it. The canvas resolves a live one from the content listing at render time
 * instead (`slide-canvas.tsx`'s AssetUrls).
 *
 * Applied on LOAD as well as on pick, because a row written before the server
 * began refusing to store one still carries it, and a read-modify-write would
 * otherwise echo it straight back on the next save. */
function withoutDerivedLayerFields(slide: CastSlide): CastSlide {
  if (!slide.layers.some((l) => l.url !== undefined)) return slide;
  return {
    ...slide,
    layers: slide.layers.map((l) => {
      if (l.url === undefined) return l;
      const authored = { ...l };
      delete authored.url;
      return authored;
    }),
  };
}

/** The current slide, or undefined when the cast has none yet. */
export function currentSlide(state: StudioState): CastSlide | undefined {
  return state.slides[state.slideIndex];
}

/** The selected layer, or undefined when nothing is selected. */
export function selectedLayer(state: StudioState): SlideLayer | undefined {
  const slide = currentSlide(state);
  if (!slide || state.layerIndex === null) return undefined;
  return slide.layers[state.layerIndex];
}

/** The PATCH body a save sends. The editor never hand-builds this — the one
 * place the draft becomes a wire body, so what the tests assert on is what the
 * server would receive. */
export function studioStateToUpdate(state: StudioState): CastUpdate {
  // `default_duration_ms` is ALWAYS present, carrying null when there is no
  // cast-wide default. Omitting it when null would leave a previously-set
  // default stored forever (the body shallow-merges), so "cleared it and saved"
  // would silently not clear it — a control that accepts the work and never
  // performs it.
  return { name: state.name, slides: state.slides, default_duration_ms: state.defaultDurationMs };
}

/** Replace the current slide's layers, marking the draft dirty. */
function withLayers(state: StudioState, layers: SlideLayer[], layerIndex: number | null): StudioState {
  const slide = currentSlide(state);
  if (!slide) return state;
  const slides = state.slides.slice();
  slides[state.slideIndex] = { ...slide, layers };
  return { ...state, slides, layerIndex, dirty: true };
}

/** Keep an index inside a list that just changed length (or point at nothing
 * when the list emptied). */
function clampIndex(index: number, length: number): number {
  if (length === 0) return 0;
  return Math.min(Math.max(index, 0), length - 1);
}

export function studioReducer(state: StudioState, action: StudioAction): StudioState {
  switch (action.type) {
    case "loaded":
      return studioStateFromCast(action.cast);

    case "saved":
      return {
        name: action.cast.name,
        defaultDurationMs: action.cast.default_duration_ms ?? null,
        slides: action.cast.slides,
        slideIndex: clampIndex(state.slideIndex, action.cast.slides.length),
        // The layer selection survives only if the layer is still there. A
        // server that normalised the stack must not leave the properties panel
        // editing a layer index that no longer exists.
        layerIndex:
          state.layerIndex !== null &&
          state.layerIndex < (action.cast.slides[clampIndex(state.slideIndex, action.cast.slides.length)]?.layers.length ?? 0)
            ? state.layerIndex
            : null,
        dirty: false,
      };

    case "rename":
      if (action.name === state.name) return state;
      return { ...state, name: action.name, dirty: true };

    case "setDefaultDuration": {
      // Rounded and floored at 1ms rather than 0: zero is not a dwell time, it
      // is the absent value, and the server's own schema refuses it (minimum 1).
      // Clearing is `null`, which is a different statement and reaches here as
      // one.
      const next = action.durationMs === null ? null : Math.max(1, Math.round(action.durationMs));
      if (next === state.defaultDurationMs) return state;
      return { ...state, defaultDurationMs: next, dirty: true };
    }

    case "selectSlide": {
      if (action.index === state.slideIndex) return state;
      if (action.index < 0 || action.index >= state.slides.length) return state;
      // Moving to another slide drops the layer selection: layer indices are
      // per-slide, so carrying one across would select an unrelated layer.
      return { ...state, slideIndex: action.index, layerIndex: null };
    }

    case "addSlide": {
      const slides = [...state.slides, newSlide(action.id)];
      // The seeded layer is SELECTED: a new slide's placeholder text is the
      // first thing anyone changes, and landing with the properties panel
      // already on it saves a click that has no decision in it.
      return { ...state, slides, slideIndex: slides.length - 1, layerIndex: 0, dirty: true };
    }

    case "duplicateSlide": {
      const source = state.slides[action.index];
      if (!source) return state;
      const copy: CastSlide = {
        ...source,
        id: action.id,
        // Layers are copied by value so editing the duplicate cannot reach back
        // into the original (they would otherwise share layer objects).
        layers: source.layers.map((l) => ({ ...l })),
      };
      const slides = state.slides.slice();
      slides.splice(action.index + 1, 0, copy);
      return { ...state, slides, slideIndex: action.index + 1, layerIndex: null, dirty: true };
    }

    case "deleteSlide": {
      if (action.index < 0 || action.index >= state.slides.length) return state;
      const slides = state.slides.filter((_, i) => i !== action.index);
      return {
        ...state,
        slides,
        slideIndex: clampIndex(state.slideIndex > action.index ? state.slideIndex - 1 : state.slideIndex, slides.length),
        layerIndex: null,
        dirty: true,
      };
    }

    case "moveSlide": {
      const slides = reorder(state.slides, action.from, action.to);
      if (slides === state.slides) return state;
      // Follow the slide that moved — the operator is dragging the thing they
      // are working on, and losing it under the cursor is disorienting.
      const slideIndex = state.slideIndex === action.from ? action.to : state.slideIndex;
      return { ...state, slides, slideIndex, dirty: true };
    }

    case "setSlideDuration": {
      const slide = state.slides[action.index];
      if (!slide) return state;
      const slides = state.slides.slice();
      if (action.durationMs === null) {
        // An omitted duration means "use the playlist item's default" — which is
        // a different statement from zero, so the key is REMOVED rather than set.
        const rest = { ...slide };
        delete rest.duration_ms;
        slides[action.index] = rest;
      } else {
        slides[action.index] = { ...slide, duration_ms: Math.max(0, Math.round(action.durationMs)) };
      }
      return { ...state, slides, dirty: true };
    }

    case "selectLayer": {
      const slide = currentSlide(state);
      if (action.index !== null && (!slide || action.index < 0 || action.index >= slide.layers.length)) {
        return state;
      }
      if (action.index === state.layerIndex) return state;
      return { ...state, layerIndex: action.index };
    }

    case "insertLayer": {
      const slide = currentSlide(state);
      if (!slide) return state;
      // Appended, so a new layer lands ON TOP of the stack (array order is
      // z-order) — inserting behind what you can see would look like nothing
      // happened.
      const layers = [...slide.layers, clampLayer(action.layer)];
      return withLayers(state, layers, layers.length - 1);
    }

    case "patchLayer": {
      const slide = currentSlide(state);
      const layer = slide?.layers[action.index];
      if (!slide || !layer) return state;
      const layers = slide.layers.slice();
      layers[action.index] = clampLayer(applyPatch(layer, action.patch));
      return withLayers(state, layers, state.layerIndex);
    }

    case "moveLayer": {
      const slide = currentSlide(state);
      const layer = slide?.layers[action.index];
      if (!slide || !layer) return state;
      const moved = moveLayerBy(layer, action.dx, action.dy);
      if (moved.x === layer.x && moved.y === layer.y) return state;
      const layers = slide.layers.slice();
      layers[action.index] = moved;
      return withLayers(state, layers, state.layerIndex);
    }

    case "resizeLayer": {
      const slide = currentSlide(state);
      const layer = slide?.layers[action.index];
      if (!slide || !layer) return state;
      const resized = resizeLayerBy(layer, action.handle, action.dx, action.dy);
      const layers = slide.layers.slice();
      layers[action.index] = resized;
      return withLayers(state, layers, state.layerIndex);
    }

    case "deleteLayer": {
      const slide = currentSlide(state);
      if (!slide || action.index < 0 || action.index >= slide.layers.length) return state;
      const layers = slide.layers.filter((_, i) => i !== action.index);
      return withLayers(state, layers, null);
    }

    case "reorderLayer": {
      const slide = currentSlide(state);
      if (!slide) return state;
      const layers = reorder(slide.layers, action.from, action.to);
      if (layers === slide.layers) return state;
      // Keep the moved layer selected: reordering is how z-order is authored,
      // and the operator usually wants to keep nudging the same layer.
      const layerIndex = state.layerIndex === action.from ? action.to : state.layerIndex;
      return withLayers(state, layers, layerIndex);
    }
  }
}
