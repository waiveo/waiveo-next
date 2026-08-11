// The Studio's undo/redo, driven as a pure state machine.
//
// The thing this feature gets wrong is never "does ⌘Z restore something" — it is
// HOW MUCH one press restores. So most of what is here is about step
// boundaries: a 200-frame drag is one step, a typed word is one step, two
// deletes are two, and a burst that stopped and started again is two. Each of
// those is asserted by counting steps AND by undoing and reading the document
// back, because a stack of the right depth holding the wrong states passes the
// first check on its own.

import { describe, expect, it } from "vitest";
import type { Cast, CastSlide, SlideLayer } from "@/api";
import { defaultLayer, type StudioState } from "./cast-model";
import {
  COALESCE_WINDOW_MS,
  EMPTY_STUDIO_HISTORY,
  MAX_HISTORY_DEPTH,
  isTextEntryTarget,
  matchHistoryShortcut,
  redoLabel,
  studioHistoryReducer,
  undoLabel,
  type StudioHistory,
  type StudioHistoryAction,
} from "./edit-history";

const rect = (over: Partial<SlideLayer> = {}): SlideLayer => ({
  kind: "rect",
  x: 100,
  y: 100,
  w: 400,
  h: 200,
  color: "#123456",
  ...over,
});

const text = (over: Partial<SlideLayer> = {}): SlideLayer => ({
  kind: "text",
  x: 200,
  y: 400,
  w: 800,
  h: 160,
  text: "Welcome",
  font_px: 96,
  color: "#FFFFFF",
  ...over,
});

function castOf(slides: CastSlide[], over: Partial<Cast> = {}): Cast {
  return {
    id: "01J8Z3K4N5P6Q7R8S9T0V1W2X3",
    scope_node: "01J8Z0ROOT0000000000000000",
    name: "Lobby loop",
    slides,
    revision: 1,
    created_at: 0,
    updated_at: 0,
    ...over,
  };
}

/** One slide with a rect at index 0 and a text at index 1. */
const ONE_SLIDE = castOf([{ id: "slide-1", layers: [rect(), text()] }]);
const TWO_SLIDES = castOf([
  { id: "slide-1", layers: [rect(), text()] },
  { id: "slide-2", layers: [text({ text: "Second" })] },
]);

function opened(cast: Cast): StudioHistory {
  return studioHistoryReducer(EMPTY_STUDIO_HISTORY, { type: "loaded", cast });
}

function run(history: StudioHistory, ...actions: StudioHistoryAction[]): StudioHistory {
  return actions.reduce(studioHistoryReducer, history);
}

/** The layer the canvas is showing, by index. */
function layerAt(state: StudioState, index: number): SlideLayer | undefined {
  return state.slides[state.slideIndex]?.layers[index];
}

/**
 * A REALISTIC pointer drag: the gesture's two edges plus one absolute-geometry
 * patch per animation frame, exactly as slide-canvas emits them (every frame
 * recomputes from the box the layer had at pointer-down, which is why the
 * patches are absolute rather than incremental).
 */
function dragBy(
  history: StudioHistory,
  index: number,
  origin: SlideLayer,
  dx: number,
  dy: number,
  frames: number,
  options: { at?: number; frameMs?: number; gesture?: boolean } = {},
): StudioHistory {
  const at0 = options.at ?? 10_000;
  const frameMs = options.frameMs ?? 8;
  const gesture = options.gesture ?? true;
  let out = gesture ? run(history, { type: "gesture", phase: "begin", at: at0 }) : history;
  for (let frame = 1; frame <= frames; frame += 1) {
    out = run(out, {
      type: "patchLayer",
      index,
      patch: {
        x: origin.x + Math.round((dx * frame) / frames),
        y: origin.y + Math.round((dy * frame) / frames),
        w: origin.w,
        h: origin.h,
      },
      at: at0 + frame * frameMs,
    });
  }
  return gesture ? run(out, { type: "gesture", phase: "end", at: at0 + frames * frameMs + 1 }) : out;
}

describe("undo and redo", () => {
  it("takes back the last edit and puts it back", () => {
    const after = run(opened(ONE_SLIDE), { type: "rename", name: "Foyer" });
    expect(after.present.name).toBe("Foyer");

    const undone = run(after, { type: "undo" });
    expect(undone.present.name).toBe("Lobby loop");

    const redone = run(undone, { type: "redo" });
    expect(redone.present.name).toBe("Foyer");
  });

  it("restores the whole document a delete removed, not just its name", () => {
    const after = run(opened(TWO_SLIDES), { type: "deleteSlide", index: 0 });
    expect(after.present.slides.map((s) => s.id)).toEqual(["slide-2"]);

    const undone = run(after, { type: "undo" });
    expect(undone.present.slides.map((s) => s.id)).toEqual(["slide-1", "slide-2"]);
    expect(layerAt(undone.present, 1)).toMatchObject({ kind: "text", text: "Welcome" });
  });

  it("does nothing at the ends of the stack", () => {
    const fresh = opened(ONE_SLIDE);
    expect(run(fresh, { type: "undo" })).toBe(fresh);
    expect(run(fresh, { type: "redo" })).toBe(fresh);
    expect(undoLabel(fresh)).toBeNull();
    expect(redoLabel(fresh)).toBeNull();
  });

  it("drops the redone future the moment a new edit branches off it", () => {
    const undone = run(
      opened(ONE_SLIDE),
      { type: "rename", name: "Foyer" },
      { type: "undo" },
    );
    expect(redoLabel(undone)).not.toBeNull();

    const branched = run(undone, { type: "deleteLayer", index: 1 });
    expect(redoLabel(branched)).toBeNull();
    expect(run(branched, { type: "redo" })).toBe(branched);
  });

  it("restores the selection that was live when the edit happened", () => {
    // Select the text layer, edit it, then move the selection elsewhere. Undo
    // has to put the operator back in front of the thing it is changing.
    const after = run(
      opened(ONE_SLIDE),
      { type: "selectLayer", index: 1 },
      { type: "patchLayer", index: 1, patch: { text: "Open at 9" } },
      { type: "selectLayer", index: 0 },
    );
    expect(after.present.layerIndex).toBe(0);

    const undone = run(after, { type: "undo" });
    expect(undone.present.layerIndex).toBe(1);
    expect(layerAt(undone.present, 1)).toMatchObject({ text: "Welcome" });
  });
});

describe("what is and is not a step", () => {
  it("a selection change is not a step", () => {
    const after = run(
      opened(ONE_SLIDE),
      { type: "selectLayer", index: 1 },
      { type: "selectLayer", index: 0 },
      { type: "selectSlide", index: 0 },
    );
    expect(after.past).toHaveLength(0);
    expect(undoLabel(after)).toBeNull();
  });

  it("an action the model refused is not a step", () => {
    // A nudge into the wall: cast-model returns the same state, so there is
    // nothing to take back and the stack must not grow.
    const pinned = run(
      opened(castOf([{ id: "slide-1", layers: [rect({ x: 0, y: 0 })] }])),
      { type: "moveLayer", index: 0, dx: -50, dy: -50 },
      { type: "moveLayer", index: 0, dx: -50, dy: -50 },
    );
    expect(pinned.past).toHaveLength(0);
  });

  it("structural edits never merge, however fast they arrive", () => {
    const after = run(
      opened(ONE_SLIDE),
      { type: "insertLayer", layer: defaultLayer("rect"), at: 1000 },
      { type: "insertLayer", layer: defaultLayer("clock"), at: 1000 },
      { type: "deleteLayer", index: 0, at: 1000 },
    );
    expect(after.past).toHaveLength(3);
  });
});

describe("coalescing — one gesture is one step", () => {
  it("a 200-frame, 200px drag is ONE undo, back to where the layer started", () => {
    const opened0 = opened(ONE_SLIDE);
    const origin = layerAt(opened0.present, 1);
    expect(origin).toBeDefined();

    const dragged = dragBy(opened0, 1, origin as SlideLayer, 200, 90, 200);
    expect(layerAt(dragged.present, 1)).toMatchObject({ x: 400, y: 490 });
    expect(dragged.past).toHaveLength(1);

    const undone = run(dragged, { type: "undo" });
    expect(layerAt(undone.present, 1)).toMatchObject({ x: 200, y: 400 });
    // …and there is nothing left behind it, so a second press is not needed.
    expect(undoLabel(undone)).toBeNull();
  });

  it("a drag the operator paused in the middle of is still ONE step", () => {
    // Frames four seconds apart — many times the coalescing window. Only the
    // explicit gesture keeps this together, which is why the gesture exists.
    const opened0 = opened(ONE_SLIDE);
    const origin = layerAt(opened0.present, 1) as SlideLayer;
    const dragged = dragBy(opened0, 1, origin, 120, 0, 4, { frameMs: 4000 });
    expect(dragged.past).toHaveLength(1);
    expect(run(dragged, { type: "undo" }).present.slides[0]?.layers[1]).toMatchObject({ x: 200 });
  });

  it("two separate drags of the same layer are two steps", () => {
    const opened0 = opened(ONE_SLIDE);
    const first = dragBy(opened0, 1, layerAt(opened0.present, 1) as SlideLayer, 100, 0, 20, { at: 1000 });
    // Immediately after the pointer comes up — inside the window, so only the
    // gesture edges separate these.
    const second = dragBy(first, 1, layerAt(first.present, 1) as SlideLayer, 100, 0, 20, { at: 1200 });

    expect(second.past).toHaveLength(2);
    expect(layerAt(second.present, 1)).toMatchObject({ x: 400 });
    expect(layerAt(run(second, { type: "undo" }).present, 1)).toMatchObject({ x: 300 });
    expect(layerAt(run(second, { type: "undo" }, { type: "undo" }).present, 1)).toMatchObject({ x: 200 });
  });

  it("holds a drag together on the key alone, with no gesture to help", () => {
    // The gesture is the belt; the key is the braces — and this is why the key
    // is the patch's FIELDS rather than the fields whose values moved.
    //
    // A real pointer path does two things a "what changed" key cannot survive.
    // It STARTS on one axis: the first frames of almost every drag have no
    // vertical component at all, so the changed set is {x} and then becomes
    // {x,y}. And it CROSSES BACK over its own origin, where the changed set
    // briefly empties. Either flip would split one drag into several steps.
    const opened0 = opened(ONE_SLIDE);
    const origin = layerAt(opened0.present, 1) as SlideLayer;
    const path: Array<[number, number]> = [
      [10, 0],
      [24, 0],
      [40, 3],
      [70, 18],
      [40, 22],
      [0, 22],
      [-30, 40],
      [-30, 40],
    ];
    let history = opened0;
    path.forEach(([dx, dy], frame) => {
      history = run(history, {
        type: "patchLayer",
        index: 1,
        patch: { x: origin.x + dx, y: origin.y + dy, w: origin.w, h: origin.h },
        at: 5000 + frame * 8,
      });
    });

    expect(history.past).toHaveLength(1);
    expect(layerAt(history.present, 1)).toMatchObject({ x: origin.x - 30, y: origin.y + 40 });
    expect(layerAt(run(history, { type: "undo" }).present, 1)).toMatchObject({ x: origin.x, y: origin.y });
  });

  it("does not swallow the NEXT edit into a finished drag", () => {
    // Inside a gesture everything merges, so the gesture had better end. If the
    // canvas's pointerup went unreported the flag would stay raised and every
    // edit for the rest of the session would fold into that one drag — an undo
    // stack of exactly one step, which is the failure that looks like it works.
    const opened0 = opened(ONE_SLIDE);
    const dragged = dragBy(opened0, 1, layerAt(opened0.present, 1) as SlideLayer, 90, 0, 8, { at: 1000 });
    const andTyped = run(dragged, { type: "patchLayer", index: 1, patch: { text: "Changed" }, at: 1100 });

    expect(andTyped.past).toHaveLength(2);
    const undone = run(andTyped, { type: "undo" });
    expect(layerAt(undone.present, 1)).toMatchObject({ text: "Welcome", x: 290 });
  });

  it("does not let the next drag merge into the one that just ended", () => {
    // The gesture's END has to close the run. Without that, two drags a
    // fraction of a second apart share a key and a window and become one step.
    const opened0 = opened(ONE_SLIDE);
    const first = dragBy(opened0, 1, layerAt(opened0.present, 1) as SlideLayer, 60, 0, 6, { at: 1000 });
    const second = dragBy(first, 1, layerAt(first.present, 1) as SlideLayer, 60, 0, 6, { at: 1060 });
    expect(second.past).toHaveLength(2);
  });
});

describe("coalescing — typing", () => {
  it("a typed word is ONE step, back to the text that was there before", () => {
    let history = run(opened(ONE_SLIDE), { type: "selectLayer", index: 1 });
    const typed = "Open at 9";
    for (let i = 1; i <= typed.length; i += 1) {
      history = run(history, {
        type: "patchLayer",
        index: 1,
        patch: { text: typed.slice(0, i) },
        at: 2000 + i * 120,
      });
    }
    expect(layerAt(history.present, 1)).toMatchObject({ text: "Open at 9" });
    expect(history.past).toHaveLength(1);
    expect(layerAt(run(history, { type: "undo" }).present, 1)).toMatchObject({ text: "Welcome" });
  });

  it("a pause longer than the window starts a new step", () => {
    const history = run(
      opened(ONE_SLIDE),
      { type: "selectLayer", index: 1 },
      { type: "patchLayer", index: 1, patch: { text: "Open" }, at: 2000 },
      { type: "patchLayer", index: 1, patch: { text: "Open at" }, at: 2000 + COALESCE_WINDOW_MS },
      // One millisecond past the window: a second thought, a second step.
      { type: "patchLayer", index: 1, patch: { text: "Open at 9" }, at: 2000 + COALESCE_WINDOW_MS * 2 + 1 },
    );
    expect(history.past).toHaveLength(2);
    expect(layerAt(run(history, { type: "undo" }).present, 1)).toMatchObject({ text: "Open at" });
  });

  it("moving the selection ends the run, so the next field is its own step", () => {
    const history = run(
      opened(ONE_SLIDE),
      { type: "selectLayer", index: 1 },
      { type: "patchLayer", index: 1, patch: { text: "Open" }, at: 2000 },
      { type: "selectLayer", index: 0 },
      { type: "selectLayer", index: 1 },
      { type: "patchLayer", index: 1, patch: { text: "Open!" }, at: 2100 },
    );
    expect(history.past).toHaveLength(2);
  });

  it("two different fields are two steps even in the same instant", () => {
    // The X box and the Y box in the properties panel. Both patch geometry, so
    // a key that lumped all geometry together would merge them.
    const history = run(
      opened(ONE_SLIDE),
      { type: "patchLayer", index: 1, patch: { x: 260 }, at: 3000 },
      { type: "patchLayer", index: 1, patch: { y: 480 }, at: 3000 },
    );
    expect(history.past).toHaveLength(2);
    expect(layerAt(run(history, { type: "undo" }).present, 1)).toMatchObject({ x: 260, y: 400 });
  });

  it("editing a DIFFERENT layer is a different step", () => {
    const history = run(
      opened(ONE_SLIDE),
      { type: "patchLayer", index: 0, patch: { color: "#111111" }, at: 3000 },
      { type: "patchLayer", index: 1, patch: { color: "#222222" }, at: 3010 },
    );
    expect(history.past).toHaveLength(2);
  });

  it("editing the same field on a different SLIDE is a different step", () => {
    const history = run(
      opened(TWO_SLIDES),
      { type: "patchLayer", index: 0, patch: { color: "#111111" }, at: 3000 },
      { type: "selectSlide", index: 1 },
      { type: "patchLayer", index: 0, patch: { color: "#222222" }, at: 3010 },
    );
    expect(history.past).toHaveLength(2);
    const undone = run(history, { type: "undo" });
    expect(undone.present.slides[1]?.layers[0]).toMatchObject({ color: "#FFFFFF" });
    expect(undone.present.slides[0]?.layers[0]).toMatchObject({ color: "#111111" });
  });
});

describe("the boundary — load, save and dirty", () => {
  it("loading a different cast clears the stack in both directions", () => {
    const edited = run(
      opened(ONE_SLIDE),
      { type: "rename", name: "Foyer" },
      { type: "deleteLayer", index: 0 },
      { type: "undo" },
    );
    expect(edited.past.length).toBeGreaterThan(0);
    expect(edited.future.length).toBeGreaterThan(0);

    const other = run(edited, { type: "loaded", cast: TWO_SLIDES });
    expect(other.past).toHaveLength(0);
    expect(other.future).toHaveLength(0);
    expect(run(other, { type: "undo" }).present.slides).toHaveLength(2);
  });

  it("a save keeps the stack — undoing past it is allowed, and reports dirty", () => {
    const edited = run(opened(ONE_SLIDE), { type: "rename", name: "Foyer" });
    expect(edited.present.dirty).toBe(true);

    const saved = run(edited, { type: "saved", cast: castOf(ONE_SLIDE.slides, { name: "Foyer", revision: 2 }) });
    expect(saved.present.dirty).toBe(false);
    expect(saved.past).toHaveLength(1);

    const undone = run(saved, { type: "undo" });
    expect(undone.present.name).toBe("Lobby loop");
    // The draft is no longer what the box holds, so it is dirty again and the
    // operator can save the undone version. Reporting it clean would leave the
    // Save button disabled over an undo that cannot be persisted.
    expect(undone.present.dirty).toBe(true);
  });

  it("undoing back to where the session started is CLEAN, not falsely dirty", () => {
    const undone = run(opened(ONE_SLIDE), { type: "rename", name: "Foyer" }, { type: "undo" });
    expect(undone.present.dirty).toBe(false);

    const redone = run(undone, { type: "redo" });
    expect(redone.present.dirty).toBe(true);
  });

  it("survives a round trip of undo and redo across a save without losing the clean point", () => {
    const saved = run(
      opened(ONE_SLIDE),
      { type: "rename", name: "Foyer" },
      { type: "saved", cast: castOf(ONE_SLIDE.slides, { name: "Foyer", revision: 2 }) },
      { type: "rename", name: "Atrium" },
    );
    // Back to the saved name: that IS the box's copy, so it is clean.
    const back = run(saved, { type: "undo" });
    expect(back.present.name).toBe("Foyer");
    expect(back.present.dirty).toBe(false);

    const forward = run(back, { type: "redo" });
    expect(forward.present.dirty).toBe(true);

    const backAgain = run(forward, { type: "undo" });
    expect(backAgain.present.dirty).toBe(false);
  });
});

describe("depth", () => {
  it("keeps the newest steps and drops the oldest past the cap", () => {
    let history = opened(ONE_SLIDE);
    const overshoot = 5;
    for (let i = 0; i < MAX_HISTORY_DEPTH + overshoot; i += 1) {
      history = run(history, { type: "insertLayer", layer: defaultLayer("rect"), at: 1000 + i });
    }
    expect(history.past).toHaveLength(MAX_HISTORY_DEPTH);

    for (let i = 0; i < MAX_HISTORY_DEPTH; i += 1) history = run(history, { type: "undo" });
    // Two original layers plus the five inserts that fell off the bottom.
    expect(history.present.slides[0]?.layers).toHaveLength(2 + overshoot);
    expect(undoLabel(history)).toBeNull();
  });
});

describe("labels — what a press will revert", () => {
  const label = (...actions: StudioHistoryAction[]) => undoLabel(run(opened(TWO_SLIDES), ...actions));

  it("names the slide operations", () => {
    expect(label({ type: "deleteSlide", index: 0 })).toBe("delete slide");
    expect(label({ type: "addSlide", id: "slide-3" })).toBe("add slide");
    expect(label({ type: "duplicateSlide", index: 0, id: "slide-3" })).toBe("duplicate slide");
    expect(label({ type: "moveSlide", from: 0, to: 1 })).toBe("move slide");
  });

  it("names the layer operations", () => {
    expect(label({ type: "deleteLayer", index: 0 })).toBe("delete layer");
    expect(label({ type: "reorderLayer", from: 0, to: 1 })).toBe("reorder layers");
    expect(label({ type: "insertLayer", layer: defaultLayer("text") })).toBe("add text layer");
    expect(label({ type: "insertLayer", layer: defaultLayer("countdown") })).toBe("add countdown layer");
  });

  it("tells a move from a resize, which the patch shape alone cannot", () => {
    // Both arrive as a four-member geometry patch; only what MOVED separates
    // them, so this is the one label that has to look at the previous layer.
    expect(label({ type: "patchLayer", index: 0, patch: { x: 240, y: 100, w: 400, h: 200 } })).toBe("move layer");
    expect(label({ type: "patchLayer", index: 0, patch: { x: 100, y: 100, w: 640, h: 200 } })).toBe("resize layer");
  });

  it("names a content field by what it is", () => {
    expect(label({ type: "patchLayer", index: 1, patch: { text: "Hi" } })).toBe("edit the text");
    expect(label({ type: "patchLayer", index: 1, patch: { color: "#0A0A0A" } })).toBe("change the colour");
    expect(label({ type: "patchLayer", index: 1, patch: { font_px: 40 } })).toBe("change the font size");
    expect(label({ type: "rename", name: "Foyer" })).toBe("rename the cast");
  });

  it("carries the label across to redo, so it never names a different step", () => {
    const undone = run(opened(TWO_SLIDES), { type: "deleteSlide", index: 0 }, { type: "undo" });
    expect(redoLabel(undone)).toBe("delete slide");
    expect(undoLabel(undone)).toBeNull();
  });

  it("names the FIRST action of a coalesced run, not its last frame", () => {
    const opened0 = opened(ONE_SLIDE);
    const dragged = dragBy(opened0, 1, layerAt(opened0.present, 1) as SlideLayer, 300, 0, 40);
    expect(undoLabel(dragged)).toBe("move layer");
  });
});

describe("the keyboard chord", () => {
  const chord = (over: Partial<Parameters<typeof matchHistoryShortcut>[0]>) =>
    matchHistoryShortcut({ key: "z", metaKey: false, ctrlKey: false, shiftKey: false, altKey: false, ...over });

  it("matches undo on either platform's modifier", () => {
    expect(chord({ metaKey: true })).toBe("undo");
    expect(chord({ ctrlKey: true })).toBe("undo");
    expect(chord({ key: "Z", metaKey: true })).toBe("undo");
  });

  it("matches redo on shift-Z and on Ctrl+Y", () => {
    expect(chord({ metaKey: true, shiftKey: true })).toBe("redo");
    expect(chord({ ctrlKey: true, shiftKey: true })).toBe("redo");
    expect(chord({ key: "y", ctrlKey: true })).toBe("redo");
  });

  it("leaves Cmd+Y to the browser — on macOS that is the History window", () => {
    expect(chord({ key: "y", metaKey: true })).toBeNull();
  });

  it("ignores a bare Z, and anything carrying Alt", () => {
    expect(chord({})).toBeNull();
    expect(chord({ metaKey: true, altKey: true })).toBeNull();
    expect(chord({ ctrlKey: true, altKey: true })).toBeNull();
    expect(chord({ key: "a", metaKey: true })).toBeNull();
  });
});

describe("the focus guard", () => {
  const el = (html: string): Element => {
    const host = document.createElement("div");
    host.innerHTML = html;
    return host.firstElementChild as Element;
  };

  it("yields to fields that keep their own undo", () => {
    expect(isTextEntryTarget(el("<textarea></textarea>"))).toBe(true);
    expect(isTextEntryTarget(el("<input>"))).toBe(true);
    expect(isTextEntryTarget(el('<input type="text">'))).toBe(true);
    expect(isTextEntryTarget(el('<input type="number">'))).toBe(true);
    expect(isTextEntryTarget(el('<input type="search">'))).toBe(true);
    expect(isTextEntryTarget(el('<div contenteditable="true"></div>'))).toBe(true);
  });

  it("claims the keystroke everywhere else, including controls with no undo of their own", () => {
    expect(isTextEntryTarget(el('<input type="color">'))).toBe(false);
    expect(isTextEntryTarget(el('<input type="checkbox">'))).toBe(false);
    expect(isTextEntryTarget(el('<input type="range">'))).toBe(false);
    expect(isTextEntryTarget(el("<select></select>"))).toBe(false);
    expect(isTextEntryTarget(el('<div role="button" tabindex="0"></div>'))).toBe(false);
    expect(isTextEntryTarget(el("<button></button>"))).toBe(false);
    expect(isTextEntryTarget(null)).toBe(false);
  });
});
