import { describe, it, expect } from "vitest";
import type { SlideLayer } from "@/api";
import { focusRingRect, focusTargets, spatialNeighbor } from "./focus";

/**
 * The D-pad, proved against the BrightScript it transcribes.
 *
 * Every case names the rule in `player-v3/components/PhotonScene.brs` it mirrors.
 * A simulator that walks a menu differently from the wall is worse than no
 * simulator, so these are the cases where "sensible" and "what the player does"
 * come apart.
 */

const layer = (over: Partial<SlideLayer> = {}): SlideLayer => ({
  kind: "text",
  x: 0,
  y: 0,
  w: 200,
  h: 100,
  text: "Hi",
  ...over,
});

describe("which regions are focusable, and in what order", () => {
  it("registers a ping_name on ANY kind, not only on `ping`", () => {
    // wire.LayerIsInteractive's second arm, and PhotonScene.brs:834-855 puts the
    // registration OUTSIDE the kind chain for exactly this reason: "putting it in
    // the `ping` branch is the half-implementation this codebase keeps shipping".
    const targets = focusTargets([
      layer({ kind: "entity", entity_id: "e1", ping_name: "lobby_state" }),
      layer({ kind: "rect", color: "#101020", ping_name: "hotspot" }),
      layer({ kind: "image", asset_ref: "sha256:aa11", ping_name: "poster" }),
    ]);
    expect(targets.map((t) => t.press)).toEqual([
      { kind: "ping", pingName: "lobby_state" },
      { kind: "ping", pingName: "hotspot" },
      { kind: "ping", pingName: "poster" },
    ]);
  });

  it("ignores a layer with no ping_name", () => {
    expect(focusTargets([layer(), layer({ kind: "rect", color: "#fff" })])).toEqual([]);
  });

  it("makes each nav ITEM its own target, in item order", () => {
    const nav = layer({
      kind: "nav",
      x: 0,
      y: 0,
      w: 600,
      h: 100,
      items: [
        { label: "Menu", target_slide_id: "s2" },
        { label: "Hours", target_slide_id: "s3" },
        { label: "Map", target_slide_id: "s4" },
      ],
    });
    const targets = focusTargets([nav]);
    expect(targets).toHaveLength(3);
    // The rects are wire.NavItemRects': a row, because the box is wider than
    // tall, with the LAST item absorbing the integer-division remainder.
    expect(targets.map((t) => t.rect)).toEqual([
      [0, 0, 200, 100],
      [200, 0, 200, 100],
      [400, 0, 200, 100],
    ]);
    expect(targets[1].press).toEqual({ kind: "nav", label: "Hours", targetSlideId: "s3" });
  });

  it("does NOT add a whole-layer target for a nav that also carries a ping_name", () => {
    // PhotonScene.brs:846 — `kind <> "nav"`. A whole-layer target "would sit over
    // every item and steal their focus". This is the arm a reimplementation
    // written from the wire types alone gets wrong, because ping_name is legal on
    // every kind and nav is not obviously an exception.
    const targets = focusTargets([
      layer({
        kind: "nav",
        w: 600,
        h: 100,
        ping_name: "whole_menu",
        items: [
          { label: "A", target_slide_id: "s2" },
          { label: "B", target_slide_id: "s3" },
        ],
      }),
    ]);
    expect(targets).toHaveLength(2);
    expect(targets.every((t) => t.press.kind === "nav")).toBe(true);
  });

  it("registers targets in Z-order, so focus lands on the first thing placed", () => {
    // renderSlide: `setFocusIndex(0)` — "First in Z-ORDER rather than nearest a
    // corner: the layer stack's order is the one thing the author controls."
    // Index 0 here is the BOTTOM layer, even though it is on the right.
    const targets = focusTargets([
      layer({ kind: "ping", x: 1600, y: 900, w: 200, h: 100, text: "Back", ping_name: "back" }),
      layer({ kind: "ping", x: 100, y: 100, w: 200, h: 100, text: "Help", ping_name: "help" }),
    ]);
    expect(targets[0].press).toEqual({ kind: "ping", pingName: "back" });
  });

  it("carries the layer index, so the ring can be drawn on the right layer", () => {
    const targets = focusTargets([layer(), layer({ kind: "ping", text: "Go", ping_name: "go" })]);
    expect(targets[0]).toMatchObject({ layerIndex: 1, itemIndex: null });
  });

  it("survives a nav layer with no items at all", () => {
    expect(focusTargets([layer({ kind: "nav", w: 600, h: 100 })])).toEqual([]);
  });
});

describe("wvSpatialNeighbor — where an arrow moves", () => {
  /** Three buttons in a row, plus one below the middle. */
  const row = focusTargets([
    layer({ kind: "ping", x: 100, y: 500, w: 200, h: 100, text: "L", ping_name: "l" }),
    layer({ kind: "ping", x: 800, y: 500, w: 200, h: 100, text: "M", ping_name: "m" }),
    layer({ kind: "ping", x: 1500, y: 500, w: 200, h: 100, text: "R", ping_name: "r" }),
    layer({ kind: "ping", x: 800, y: 800, w: 200, h: 100, text: "D", ping_name: "d" }),
  ]);

  it("moves right to the nearest centre strictly beyond the current one", () => {
    expect(spatialNeighbor(row, 0, "right")).toBe(1);
    expect(spatialNeighbor(row, 1, "right")).toBe(2);
  });

  it("moves left symmetrically", () => {
    expect(spatialNeighbor(row, 2, "left")).toBe(1);
  });

  it("returns null at the end of a row, so the key is NOT swallowed", () => {
    // The player returns false from onKeyEvent here, deliberately: "so Home still
    // exits". The preview reads this null the same way and lets the browser have
    // the key.
    expect(spatialNeighbor(row, 2, "right")).toBeNull();
    expect(spatialNeighbor(row, 0, "left")).toBeNull();
  });

  it("moves down to a target below", () => {
    expect(spatialNeighbor(row, 1, "down")).toBe(3);
  });

  it("weights ACROSS-axis distance double, so in-line beats merely close", () => {
    // The scoring is `along + across * 2`. From L (centre 200,550):
    //   - `far` is directly in line, 1000px along, 0 across  → score 1000
    //   - `near` is 400px along but 350px across             → score 1100
    // A plain nearest-neighbour search picks `near`; the player picks `far`, and
    // this doubling is the whole reason a D-pad feels like a D-pad.
    const targets = focusTargets([
      layer({ kind: "ping", x: 100, y: 500, w: 200, h: 100, text: "L", ping_name: "l" }),
      layer({ kind: "ping", x: 500, y: 850, w: 200, h: 100, text: "near", ping_name: "near" }),
      layer({ kind: "ping", x: 1100, y: 500, w: 200, h: 100, text: "far", ping_name: "far" }),
    ]);
    expect(spatialNeighbor(targets, 0, "right")).toBe(2);
  });

  it("breaks a tie toward the EARLIER registered target", () => {
    // The BrightScript compares with `<` while scanning in registration order, so
    // an equal score never displaces the incumbent. Two buttons mirrored about
    // the source's centre score identically going down.
    const targets = focusTargets([
      layer({ kind: "ping", x: 860, y: 100, w: 200, h: 100, text: "src", ping_name: "src" }),
      layer({ kind: "ping", x: 460, y: 600, w: 200, h: 100, text: "first", ping_name: "first" }),
      layer({ kind: "ping", x: 1260, y: 600, w: 200, h: 100, text: "second", ping_name: "second" }),
    ]);
    expect(spatialNeighbor(targets, 0, "down")).toBe(1);
  });

  it("ignores a target whose centre is not strictly beyond the current one", () => {
    // Two boxes at the same centre-y: neither is up or down from the other.
    const targets = focusTargets([
      layer({ kind: "ping", x: 100, y: 500, w: 200, h: 100, text: "a", ping_name: "a" }),
      layer({ kind: "ping", x: 800, y: 500, w: 200, h: 100, text: "b", ping_name: "b" }),
    ]);
    expect(spatialNeighbor(targets, 0, "down")).toBeNull();
    expect(spatialNeighbor(targets, 0, "up")).toBeNull();
  });

  it("returns null for an out-of-range index rather than throwing", () => {
    expect(spatialNeighbor(row, -1, "right")).toBeNull();
    expect(spatialNeighbor(row, 99, "right")).toBeNull();
    expect(spatialNeighbor([], 0, "right")).toBeNull();
  });
});

describe("showFocusRing — the clamp that makes an edge ring three-sided", () => {
  it("pads the target by 8px on every side when there is room", () => {
    expect(focusRingRect([100, 100, 200, 100], 1920, 1080)).toEqual([92, 92, 216, 116]);
  });

  it("CLAMPS at the left/top edge, losing that side's outline the way the wall does", () => {
    // A target flush at x=0 pads to x=-8, which SceneGraph draws off-screen — the
    // ring silently becomes three-sided. A preview drawing four sides here would
    // hide the exact defect an author most needs to see, since "that is where an
    // author is most likely to put a menu".
    expect(focusRingRect([0, 0, 200, 100], 1920, 1080)).toEqual([0, 0, 208, 108]);
  });

  it("CLAMPS at the right/bottom edge", () => {
    expect(focusRingRect([1720, 980, 200, 100], 1920, 1080)).toEqual([1712, 972, 208, 108]);
  });

  it("reports nothing to draw when the clamp leaves no ring", () => {
    expect(focusRingRect([0, 0, 0, 0], 0, 0)).toBeNull();
  });
});
