// @vitest-environment node
//
// The Studio's editing model. These assert the MODEL a save would ship, which is
// the thing an editor gets wrong: geometry that escapes the canvas, a z-order
// reorder that scrambles the stack, a delete that strands the selection.

import { describe, expect, it } from "vitest";
import { LAYER_KINDS, validateSlide, type Cast, type CastSlide, type SlideLayer } from "@/api";
import {
  EMPTY_STUDIO_STATE,
  MIN_LAYER_SIZE,
  clampLayer,
  currentSlide,
  defaultLayer,
  moveLayerBy,
  newSlide,
  reorder,
  resizeLayerBy,
  selectedLayer,
  studioReducer,
  studioStateFromCast,
  studioStateToUpdate,
  type StudioAction,
  type StudioState,
} from "./cast-model";

const rect = (over: Partial<SlideLayer> = {}): SlideLayer => ({
  kind: "rect",
  x: 100,
  y: 100,
  w: 400,
  h: 200,
  color: "#123456",
  ...over,
});

function castOf(slides: CastSlide[], name = "Lobby loop"): Cast {
  return {
    id: "01J8Z3K4N5P6Q7R8S9T0V1W2X3",
    scope_node: "01J8Z0ROOT0000000000000000",
    name,
    slides,
    revision: 1,
    created_at: 0,
    updated_at: 0,
  };
}

/** Run a sequence of actions from a start state — the way the editor actually
 * arrives at a model, rather than one action in isolation. */
function run(state: StudioState, ...actions: StudioAction[]): StudioState {
  return actions.reduce(studioReducer, state);
}

describe("geometry", () => {
  it("keeps a layer inside the 1920x1080 canvas when dragged past the edge", () => {
    const moved = moveLayerBy(rect({ x: 1600, y: 900 }), 500, 500);
    expect(moved).toMatchObject({ x: 1920 - 400, y: 1080 - 200 });
  });

  it("keeps a layer inside the canvas when dragged past the top-left", () => {
    expect(moveLayerBy(rect({ x: 10, y: 10 }), -500, -500)).toMatchObject({ x: 0, y: 0 });
  });

  it("rounds fractional drags to whole pixels (the wire's geometry is integer)", () => {
    expect(moveLayerBy(rect(), 12.4, -7.6)).toMatchObject({ x: 112, y: 92 });
  });

  it("pins the opposite edge on a resize", () => {
    // Dragging the WEST grip right by 100 must move the left edge only: the
    // right edge (x+w = 500) stays exactly where it was.
    const r = resizeLayerBy(rect(), "w", 100, 0);
    expect(r.x).toBe(200);
    expect(r.x + r.w).toBe(500);
  });

  it("grows from the south-east grip without moving the origin", () => {
    const r = resizeLayerBy(rect(), "se", 200, 100);
    expect(r).toMatchObject({ x: 100, y: 100, w: 600, h: 300 });
  });

  it("refuses to collapse a layer below the minimum grabbable size", () => {
    const r = resizeLayerBy(rect(), "e", -10_000, 0);
    expect(r.w).toBe(MIN_LAYER_SIZE);
    expect(r.x).toBe(100);
  });

  it("stops a resize at the canvas edge rather than off it", () => {
    const r = resizeLayerBy(rect({ x: 1500, y: 900 }), "se", 5000, 5000);
    expect(r.x + r.w).toBe(1920);
    expect(r.y + r.h).toBe(1080);
  });

  it("clamps an oversized layer down to the canvas", () => {
    expect(clampLayer(rect({ x: 0, y: 0, w: 5000, h: 5000 }))).toMatchObject({ w: 1920, h: 1080, x: 0, y: 0 });
  });
});

describe("reorder", () => {
  it("moves an element and shifts the rest", () => {
    expect(reorder(["a", "b", "c"], 0, 2)).toEqual(["b", "c", "a"]);
    expect(reorder(["a", "b", "c"], 2, 0)).toEqual(["c", "a", "b"]);
  });

  it("is a no-op (same array) for an out-of-range or identical target", () => {
    const list = ["a", "b"];
    expect(reorder(list, 0, 0)).toBe(list);
    expect(reorder(list, 0, 5)).toBe(list);
    expect(reorder(list, -1, 1)).toBe(list);
  });
});

describe("default layers", () => {
  it("inserts every kind but image already valid, so insert-then-save renders", () => {
    for (const kind of ["text", "rect", "clock"] as const) {
      const l = defaultLayer(kind);
      expect(l.kind).toBe(kind);
      expect(l.x + l.w).toBeLessThanOrEqual(1920);
      expect(l.y + l.h).toBeLessThanOrEqual(1080);
    }
    expect(defaultLayer("text").text).toBeTruthy();
    expect(defaultLayer("clock").text).toBeTruthy();
    expect(defaultLayer("rect").color).toMatch(/^#[0-9A-Fa-f]{6}$/);
  });

  it("leaves an image layer without bytes — the picker supplies them", () => {
    expect(defaultLayer("image").asset_ref).toBeUndefined();
  });
});

describe("studioReducer — slides", () => {
  const base = studioStateFromCast(castOf([{ id: "s1", layers: [rect()] }, { id: "s2", layers: [] }]));

  it("loads a cast clean (not dirty) with the first slide selected", () => {
    expect(base).toMatchObject({ slideIndex: 0, layerIndex: null, dirty: false });
    expect(base.slides).toHaveLength(2);
  });

  it("adds a slide, selects it, and marks the draft dirty", () => {
    const next = run(base, { type: "addSlide", id: "s3" });
    expect(next.slides.map((s) => s.id)).toEqual(["s1", "s2", "s3"]);
    expect(next.slideIndex).toBe(2);
    expect(next.dirty).toBe(true);
    // The seeded layer is selected, so the properties panel opens on the thing
    // the operator is about to change.
    expect(next.layerIndex).toBe(0);
  });

  // The reason this matters is atomicity, not tidiness: a PATCH carries the
  // WHOLE slides array and the server refuses the whole body if any member is
  // undrawable (openapi CastSlide.layers minItems 1, then checkCastSlides →
  // wire.ValidateAuthoredSlideLayers). A zero-layer add therefore made the next
  // save fail for every OTHER slide too.
  it("mints a slide that is VALID by construction — add-then-save is savable", () => {
    const fresh = newSlide("s9");
    expect(fresh.layers.length).toBeGreaterThan(0);
    expect(validateSlide(fresh)).toEqual([]);
    const next = run(base, { type: "addSlide", id: "s3" });
    expect(validateSlide(next.slides[2]!)).toEqual([]);
  });

  it("duplicates a slide by VALUE — editing the copy leaves the original alone", () => {
    const dup = run(base, { type: "duplicateSlide", index: 0, id: "s1-copy" });
    expect(dup.slides.map((s) => s.id)).toEqual(["s1", "s1-copy", "s2"]);
    expect(dup.slideIndex).toBe(1);
    const edited = run(dup, { type: "selectLayer", index: 0 }, { type: "patchLayer", index: 0, patch: { color: "#000000" } });
    expect(edited.slides[1]?.layers[0]?.color).toBe("#000000");
    expect(edited.slides[0]?.layers[0]?.color).toBe("#123456");
  });

  it("deleting the selected last slide leaves the selection on a real slide", () => {
    const onLast = run(base, { type: "selectSlide", index: 1 });
    const next = run(onLast, { type: "deleteSlide", index: 1 });
    expect(next.slides).toHaveLength(1);
    expect(next.slideIndex).toBe(0);
    expect(currentSlide(next)?.id).toBe("s1");
  });

  it("deleting a slide BEFORE the selected one keeps the same slide selected", () => {
    const onSecond = run(base, { type: "selectSlide", index: 1 });
    const next = run(onSecond, { type: "deleteSlide", index: 0 });
    expect(currentSlide(next)?.id).toBe("s2");
  });

  it("reordering follows the slide that moved", () => {
    const next = run(base, { type: "moveSlide", from: 0, to: 1 });
    expect(next.slides.map((s) => s.id)).toEqual(["s2", "s1"]);
    expect(currentSlide(next)?.id).toBe("s1");
  });

  it("clearing a duration REMOVES the key (omitted means 'use the default', not zero)", () => {
    const withDuration = run(base, { type: "setSlideDuration", index: 0, durationMs: 8000 });
    expect(withDuration.slides[0]?.duration_ms).toBe(8000);
    const cleared = run(withDuration, { type: "setSlideDuration", index: 0, durationMs: null });
    expect("duration_ms" in (cleared.slides[0] as object)).toBe(false);
  });

  it("switching slides drops the layer selection (indices are per-slide)", () => {
    const selected = run(base, { type: "selectLayer", index: 0 });
    expect(selected.layerIndex).toBe(0);
    expect(run(selected, { type: "selectSlide", index: 1 }).layerIndex).toBeNull();
  });
});

describe("studioReducer — layers", () => {
  const base = studioStateFromCast(castOf([{ id: "s1", layers: [rect(), rect({ x: 800 })] }]));

  it("inserts a layer ON TOP of the stack and selects it", () => {
    const next = run(base, { type: "insertLayer", layer: defaultLayer("text") });
    const layers = currentSlide(next)?.layers ?? [];
    expect(layers).toHaveLength(3);
    expect(layers[2]?.kind).toBe("text");
    expect(next.layerIndex).toBe(2);
    expect(selectedLayer(next)?.kind).toBe("text");
  });

  it("clamps an inserted layer that would not fit", () => {
    const next = run(base, { type: "insertLayer", layer: { kind: "rect", x: 1900, y: 1000, w: 800, h: 400, color: "#ffffff" } });
    const l = selectedLayer(next);
    expect((l?.x ?? 0) + (l?.w ?? 0)).toBeLessThanOrEqual(1920);
  });

  it("moving a layer marks the draft dirty and clamps to the canvas", () => {
    const next = run(base, { type: "moveLayer", index: 0, dx: -5000, dy: 40 });
    expect(currentSlide(next)?.layers[0]).toMatchObject({ x: 0, y: 140 });
    expect(next.dirty).toBe(true);
  });

  it("a move that changes nothing does not dirty the draft", () => {
    const pinned = run(base, { type: "moveLayer", index: 0, dx: -5000, dy: -5000 });
    const again = studioReducer(pinned, { type: "moveLayer", index: 0, dx: -10, dy: -10 });
    expect(again).toBe(pinned);
  });

  it("reordering a layer changes z-order and keeps the moved layer selected", () => {
    const selected = run(base, { type: "selectLayer", index: 0 });
    const next = run(selected, { type: "reorderLayer", from: 0, to: 1 });
    // Index 0 is drawn first (furthest back); the moved layer is now on top.
    expect(currentSlide(next)?.layers[1]?.x).toBe(100);
    expect(next.layerIndex).toBe(1);
  });

  it("deleting a layer clears the selection rather than pointing past the end", () => {
    const selected = run(base, { type: "selectLayer", index: 1 });
    const next = run(selected, { type: "deleteLayer", index: 1 });
    expect(currentSlide(next)?.layers).toHaveLength(1);
    expect(next.layerIndex).toBeNull();
    expect(selectedLayer(next)).toBeUndefined();
  });

  it("refuses to select a layer index that does not exist", () => {
    expect(studioReducer(base, { type: "selectLayer", index: 9 })).toBe(base);
  });

  // Every optional member of wire.Layer is `omitempty`, so "no image chosen" is
  // the ABSENCE of asset_ref/url, not a present key holding nothing. A patch
  // that wrote `undefined` would serialise a key the server's strict decoder
  // does not expect — and clearing only `asset_ref` would leave a url pointing
  // at bytes the layer no longer names.
  it("an undefined patch value REMOVES the key from the saved body", () => {
    const withImage = studioStateFromCast(
      castOf([
        {
          id: "s1",
          layers: [{ kind: "image", x: 0, y: 0, w: 640, h: 360, asset_ref: "sha256:aa11", url: "/content/aa11" }],
        },
      ]),
    );
    const cleared = run(withImage, {
      type: "patchLayer",
      index: 0,
      patch: { asset_ref: undefined, url: undefined },
    });
    const layer = studioStateToUpdate(cleared).slides?.[0]?.layers?.[0] ?? {};
    expect("asset_ref" in layer).toBe(false);
    expect("url" in layer).toBe(false);
  });
});

describe("save body + dirty tracking", () => {
  const base = studioStateFromCast(castOf([{ id: "s1", layers: [rect()] }]));

  it("ships exactly the draft name and slides", () => {
    const edited = run(base, { type: "rename", name: "Front window" }, { type: "insertLayer", layer: defaultLayer("clock") });
    const body = studioStateToUpdate(edited);
    expect(body.name).toBe("Front window");
    expect(body.slides?.[0]?.layers.map((l) => l.kind)).toEqual(["rect", "clock"]);
  });

  it("a rename to the SAME name is not an edit", () => {
    expect(studioReducer(base, { type: "rename", name: base.name })).toBe(base);
  });

  it("a save adopts the server copy, clears dirty, and keeps the operator in place", () => {
    // One action, not two: adding a slide already lands a selected text layer
    // on it, so this is the same "added a slide and typed on it" state the test
    // has always described.
    const edited = run(base, { type: "addSlide", id: "s2" });
    expect(edited).toMatchObject({ slideIndex: 1, layerIndex: 0, dirty: true });
    const saved = studioReducer(edited, {
      type: "saved",
      cast: castOf([{ id: "s1", layers: [rect()] }, { id: "s2", layers: [defaultLayer("text")] }]),
    });
    expect(saved.dirty).toBe(false);
    expect(saved.slideIndex).toBe(1);
    expect(saved.layerIndex).toBe(0);
  });

  it("a save whose server copy dropped the selected layer clears the selection", () => {
    const edited = run(base, { type: "selectLayer", index: 0 });
    const saved = studioReducer(edited, { type: "saved", cast: castOf([{ id: "s1", layers: [] }]) });
    expect(saved.layerIndex).toBeNull();
  });

  it("the empty start state holds nothing and is not dirty", () => {
    expect(EMPTY_STUDIO_STATE).toMatchObject({ slides: [], dirty: false });
    expect(currentSlide(EMPTY_STUDIO_STATE)).toBeUndefined();
  });
});

/**
 * What INSERTING each kind produces — asserted against the same rules the server
 * and the projector apply (`validateSlide` mirrors
 * `wire.ValidateAuthoredSlideLayers`).
 *
 * This is the guard on a specific, quiet failure: a default that is missing its
 * kind's required field lands a layer that looks fine on the canvas, holds the
 * save gate for a reason the operator did not cause, and — if it ever reached
 * the wire — would be DROPPED at serve time with nothing anywhere saying why.
 */
describe("inserting a layer of each kind", () => {
  const NOW = Date.parse("2026-08-10T09:15:00");

  it("lands every kind DRAWABLE except the four that must name something first", () => {
    for (const kind of LAYER_KINDS) {
      const problems = validateSlide({ id: "s", layers: [defaultLayer(kind, NOW)] });
      if (kind === "image" || kind === "video" || kind === "entity" || kind === "nav") {
        // Deliberately incomplete: a content-bearing layer's bytes come from
        // the content origin, an entity's subject from the device plane, and a
        // menu item's target from the cast this layer is being inserted INTO —
        // none of which defaultLayer can know, and inventing any of them would
        // produce a layer that resolves to nothing on the wall (for `nav`,
        // specifically, a menu item that highlights, accepts a press and does
        // nothing). Iterating LAYER_KINDS rather than a hand-written list is
        // what makes this test see a NEW kind at all — it is how `video` was
        // caught arriving with no default.
        expect(problems, `${kind} must land incomplete, not invalid for some other reason`).toHaveLength(1);
      } else {
        expect(problems, `${kind} must insert ready to draw`).toHaveLength(0);
      }
    }
  });

  it("gives a countdown a target in the FUTURE of the instant it was inserted", () => {
    const layer = defaultLayer("countdown", NOW);
    // Midnight tonight: strictly ahead, so the widget counts visibly from the
    // moment it lands rather than reading 00:00:00 like a broken one. Derived
    // from the passed-in instant, which is what makes this assertable at all.
    expect(layer.target_ms).toBe(Date.parse("2026-08-11T00:00:00"));
  });

  it("gives a weather widget a template the box can actually substitute into", () => {
    // A template with no token would render as its own literal text forever —
    // a widget that is, on the wall, indistinguishable from a text layer.
    expect(defaultLayer("weather", NOW).text).toMatch(/\{temp\}/);
  });
});
