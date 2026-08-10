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

/** A freshly inserted layer of `kind`, placed somewhere visible and valid.
 *
 * Every kind but `image` lands VALID: it has the fields wire.ValidateSlideLayers
 * requires, so inserting it and saving immediately produces a slide that
 * renders. An image cannot — its bytes must be chosen from the content origin —
 * so it lands deliberately incomplete and the properties panel asks for the one
 * missing thing rather than the editor inventing an asset_ref that resolves to
 * nothing. */
export function defaultLayer(kind: LayerKind): SlideLayer {
  switch (kind) {
    case "text":
      return { kind, x: 160, y: 420, w: 1600, h: 240, text: "New text", font_px: 96, color: "#FFFFFF", align: "left" };
    case "rect":
      return { kind, x: 240, y: 240, w: 800, h: 400, color: "#F368C4" };
    case "clock":
      return { kind, x: 1160, y: 120, w: 600, h: 200, text: "3:04 PM", font_px: 120, color: "#FFFFFF", align: "right" };
    case "image":
      return { kind, x: 240, y: 200, w: 960, h: 540 };
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

/** An empty slide. Ids are supplied by the caller rather than minted here so the
 * reducer stays pure (and a test can assert an exact model). */
export function newSlide(id: string): CastSlide {
  return { id, layers: [] };
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
  slides: [],
  slideIndex: 0,
  layerIndex: null,
  dirty: false,
};

/** The state a loaded cast starts the editor in. */
export function studioStateFromCast(cast: Cast): StudioState {
  return { name: cast.name, slides: cast.slides, slideIndex: 0, layerIndex: null, dirty: false };
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
  return { name: state.name, slides: state.slides };
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

    case "selectSlide": {
      if (action.index === state.slideIndex) return state;
      if (action.index < 0 || action.index >= state.slides.length) return state;
      // Moving to another slide drops the layer selection: layer indices are
      // per-slide, so carrying one across would select an unrelated layer.
      return { ...state, slideIndex: action.index, layerIndex: null };
    }

    case "addSlide": {
      const slides = [...state.slides, newSlide(action.id)];
      return { ...state, slides, slideIndex: slides.length - 1, layerIndex: null, dirty: true };
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
