import { describe, expect, it } from "vitest";
import {
  MIN_INTERACTIVE_SIDE,
  NAV_ITEMS_MAX,
  isInteractiveLayer,
  navItemRects,
  validateCastSlides,
  validateNavTargets,
  validateSlide,
  type CastSlide,
  type SlideLayer,
} from "./casts";

/**
 * The console's mirror of the wire's INTERACTIVE-layer rules.
 *
 * The mirror earns its duplication the same way `validateSlide` as a whole does:
 * the projector does not reject an invalid slide back to the author, it DROPS
 * it, so without a console copy the failure mode is a saved cast that never
 * appears with nothing on the authoring surface to explain why. What these tests
 * guard is that the mirror stays a mirror in BOTH directions — stricter than the
 * server holds the save on a slide the server would accept, looser lets a save
 * fail with a server error the operator cannot act on.
 */

function base(kind: SlideLayer["kind"]): SlideLayer {
  return { kind, x: 100, y: 100, w: 400, h: 120 };
}

function slideWith(...layers: SlideLayer[]): CastSlide {
  return { id: "s1", layers };
}

describe("ping layers", () => {
  it("needs both a label and an event name", () => {
    const full: SlideLayer = { ...base("ping"), text: "Press OK", ping_name: "call_service" };
    expect(validateSlide(slideWith(full))).toHaveLength(0);

    // The absent members are OMITTED, not set to undefined: every optional
    // member of wire.Layer is omitempty, so "no label" is the absence of the
    // key. `exactOptionalPropertyTypes` makes the distinction a type error,
    // which is the right shape for a model whose wire form is omitempty.
    expect(validateSlide(slideWith({ ...base("ping"), ping_name: "call_service" }))).toHaveLength(1);
    expect(validateSlide(slideWith({ ...base("ping"), text: "Press OK" }))).toHaveLength(1);
  });

  it("checks the event-name grammar on EVERY kind, not only on ping", () => {
    // The mirror direction. A ping_name is legal on any kind — that is the whole
    // interactive-widget mechanism — so a check living only in the `ping` branch
    // would let a name no automation can ever match through on all ten others.
    const problems = validateSlide(slideWith({ ...base("text"), text: "hi", ping_name: "Front Desk" }));
    expect(problems).toHaveLength(1);
    expect(problems[0]?.message).toMatch(/lower-case/i);

    expect(
      validateSlide(slideWith({ ...base("text"), text: "hi", ping_name: "front_desk" })),
    ).toHaveLength(0);
  });

  it("marks any layer carrying an event name as interactive", () => {
    expect(isInteractiveLayer({ ...base("entity"), entity_id: "e", ping_name: "toggle" })).toBe(true);
    expect(isInteractiveLayer(base("nav"))).toBe(true);
    expect(isInteractiveLayer({ ...base("text"), text: "hi" })).toBe(false);
  });
});

describe("nav layers", () => {
  const item = { label: "Rooms", target_slide_id: "rooms" };

  it("needs at least one complete item", () => {
    expect(validateSlide(slideWith({ ...base("nav"), w: 600, items: [item] }))).toHaveLength(0);
    expect(validateSlide(slideWith({ ...base("nav"), items: [] }))).toHaveLength(1);
    expect(
      validateSlide(slideWith({ ...base("nav"), items: [{ label: "", target_slide_id: "rooms" }] })),
    ).toHaveLength(1);
    expect(
      validateSlide(slideWith({ ...base("nav"), items: [{ label: "Rooms", target_slide_id: "" }] })),
    ).toHaveLength(1);
  });

  it("refuses menu items on any other kind", () => {
    // The other mirror direction: `items` are legal ONLY on nav, so their
    // presence elsewhere can only be caught outside the per-kind branches.
    const problems = validateSlide(slideWith({ ...base("rect"), color: "#112233", items: [item] }));
    expect(problems.some((p) => /menu layer/i.test(p.message))).toBe(true);
  });

  it("bounds the item count", () => {
    const many = Array.from({ length: NAV_ITEMS_MAX + 1 }, () => item);
    expect(validateSlide(slideWith({ ...base("nav"), w: 1800, items: many })).length).toBeGreaterThan(0);
  });

  it("lays items out along the box's longer axis, last one absorbing the remainder", () => {
    const horizontal: SlideLayer = { kind: "nav", x: 100, y: 200, w: 900, h: 100, items: [item, item, item] };
    expect(navItemRects(horizontal)).toEqual([
      [100, 200, 300, 100],
      [400, 200, 300, 100],
      [700, 200, 300, 100],
    ]);

    const vertical: SlideLayer = { kind: "nav", x: 10, y: 20, w: 200, h: 600, items: [item, item] };
    expect(navItemRects(vertical)).toEqual([
      [10, 20, 200, 300],
      [10, 320, 200, 300],
    ]);

    const remainder: SlideLayer = { kind: "nav", x: 0, y: 0, w: 100, h: 10, items: [item, item, item] };
    const rects = navItemRects(remainder);
    expect(rects[2]?.[0] + rects[2]?.[2]).toBe(100);
  });
});

describe("the focus legibility floor", () => {
  it("refuses a pressable layer smaller than the minimum", () => {
    const tiny: SlideLayer = {
      kind: "rect", x: 0, y: 0, w: MIN_INTERACTIVE_SIDE - 1, h: 200, color: "#112233", ping_name: "hotspot",
    };
    expect(validateSlide(slideWith(tiny)).length).toBeGreaterThan(0);

    const ok: SlideLayer = { ...tiny, w: MIN_INTERACTIVE_SIDE };
    expect(validateSlide(slideWith(ok))).toHaveLength(0);
  });

  it("refuses a menu whose CELLS fall below the minimum even though the box does not", () => {
    // The case a whole-layer check misses entirely: a 300px menu is comfortably
    // big, and its eight items are 37px each.
    const crowded: SlideLayer = {
      kind: "nav", x: 0, y: 0, w: 300, h: 120,
      items: Array.from({ length: 8 }, () => ({ label: "x", target_slide_id: "rooms" })),
    };
    const problems = validateSlide(slideWith(crowded));
    expect(problems.some((p) => /Menu item/.test(p.message))).toBe(true);
  });
});

describe("nav targets across the cast", () => {
  const menu = (target: string): SlideLayer => ({
    kind: "nav", x: 0, y: 0, w: 600, h: 120, items: [{ label: "Go", target_slide_id: target }],
  });
  const text: SlideLayer = { kind: "text", x: 0, y: 0, w: 200, h: 80, text: "hi" };

  it("accepts a jump to any slide of the same cast, including a later one", () => {
    const slides: CastSlide[] = [
      { id: "home", layers: [menu("rooms")] },
      { id: "rooms", layers: [text] },
    ];
    expect(validateNavTargets(slides).size).toBe(0);
    expect(validateCastSlides(slides).size).toBe(0);
  });

  it("reports a jump to a slide the cast no longer has", () => {
    // The ordinary edit that breaks it: delete the slide a menu points at.
    const slides: CastSlide[] = [{ id: "home", layers: [menu("rooms")] }];
    const problems = validateNavTargets(slides);
    expect(problems.get(0)).toHaveLength(1);
    expect(problems.get(0)?.[0]?.index).toBe(0);
    // And it must reach the surface that holds the save gate, not just the
    // helper — a rule enforced beside validateCastSlides rather than inside it
    // would be applied by whichever caller happened to be updated.
    expect(validateCastSlides(slides).get(0)?.some((p) => /no longer has/.test(p.message))).toBe(true);
  });

  it("does not double-report an EMPTY target", () => {
    const slides: CastSlide[] = [{ id: "home", layers: [menu("")] }];
    expect(validateNavTargets(slides).size).toBe(0);
    // The layer-shape gate still catches it.
    expect(validateSlide(slides[0]!).length).toBeGreaterThan(0);
  });
});
