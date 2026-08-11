// The Studio viewport's zoom arithmetic.
//
// Every case here is one the editor hits and the DOM cannot demonstrate: a
// viewport that reports zero (the test environment, and the first frame of a
// real one), a viewport too small to hold the chrome, and the two ends of the
// ladder where a naive "next index" walks off the array and returns undefined.

import { describe, expect, it } from "vitest";
import { SLIDE_CANVAS_HEIGHT, SLIDE_CANVAS_WIDTH } from "@/api";
import { CANVAS_CHROME, MAX_ZOOM, MIN_ZOOM, ZOOM_STEPS, fitToViewport, zoomInFrom, zoomOutFrom } from "./canvas-zoom";

describe("the zoom ladder", () => {
  it("steps to the next stop up and down", () => {
    expect(zoomInFrom(0.5)).toBe(0.67);
    expect(zoomOutFrom(0.5)).toBe(0.33);
  });

  it("moves off an arbitrary fit scale onto the nearest stop beyond it", () => {
    // The case a `× 1.25` multiplier gets right and an index lookup gets wrong:
    // 0.49 is not ON the ladder at all, so "the next index" has no meaning.
    expect(zoomInFrom(0.49)).toBe(0.5);
    expect(zoomOutFrom(0.49)).toBe(0.33);
  });

  it("holds at both ends rather than walking off the array", () => {
    expect(zoomInFrom(MAX_ZOOM)).toBe(MAX_ZOOM);
    expect(zoomOutFrom(MIN_ZOOM)).toBe(MIN_ZOOM);
    expect(zoomInFrom(99)).toBe(MAX_ZOOM);
    expect(zoomOutFrom(0.0001)).toBe(MIN_ZOOM);
  });

  it("keeps 100% and 50% as exact stops", () => {
    // An operator checking pixel sizes needs 1:1 to BE 1:1, and 50% is the stop
    // that fits 1920 on a laptop. A geometric walk lands on neither.
    expect(ZOOM_STEPS).toContain(1);
    expect(ZOOM_STEPS).toContain(0.5);
  });
});

describe("fit to window", () => {
  it("fits the framed canvas inside the viewport, chrome included", () => {
    const width = 1200;
    const height = 800;
    const fit = fitToViewport(width, height);
    // The whole composition — bezel, artwork, stand, and the viewport's own
    // padding — must come in under the box. This is the assertion that fails if
    // the chrome is forgotten, which is what puts the stand under the tool rail.
    expect(SLIDE_CANVAS_WIDTH * fit + CANVAS_CHROME.bezelX + CANVAS_CHROME.padX).toBeLessThanOrEqual(width);
    expect(
      SLIDE_CANVAS_HEIGHT * fit + CANVAS_CHROME.bezelY + CANVAS_CHROME.stand + CANVAS_CHROME.padY,
    ).toBeLessThanOrEqual(height);
  });

  it("is bound by whichever axis is tighter", () => {
    // A 16:9 canvas in a squarer viewport is HEIGHT-bound; a wide short one is
    // width-bound. Fitting on width alone is the common bug and it hides the
    // bottom of the slide.
    expect(fitToViewport(4000, 400)).toBeLessThan(fitToViewport(4000, 4000));
    expect(fitToViewport(500, 4000)).toBeLessThan(fitToViewport(4000, 4000));
  });

  it("subtracts the chrome on the HEIGHT axis too, not only the width", () => {
    // Mutation testing found this hole: the case above happened to pick a
    // width-bound viewport, so dropping the vertical chrome term entirely
    // changed nothing it asserted. A HEIGHT-bound viewport is the only shape
    // that can see it — and it is the shape that matters, because the thing the
    // vertical term protects is the stand disappearing under the tool rail.
    const height = 700;
    const fit = fitToViewport(4000, height);
    const used = SLIDE_CANVAS_HEIGHT * fit + CANVAS_CHROME.bezelY + CANVAS_CHROME.stand + CANVAS_CHROME.padY;
    expect(used).toBeLessThanOrEqual(height);
    // …and it really is height-bound at this shape, so the assertion above is
    // about the vertical term and not vacuous.
    expect(fit).toBeLessThan((4000 - CANVAS_CHROME.bezelX - CANVAS_CHROME.padX) / SLIDE_CANVAS_WIDTH);
  });

  it("never enlarges past 1:1", () => {
    expect(fitToViewport(8000, 6000)).toBe(1);
  });

  it("falls back to 1:1 when the viewport cannot be measured", () => {
    // jsdom reports 0 for every dimension, and so does the first frame of a real
    // layout. A scale of 0 collapses the stage and makes every pointer delta
    // infinite; a tiny fraction is just as unusable.
    expect(fitToViewport(0, 0)).toBe(1);
    expect(fitToViewport(0, 900)).toBe(1);
    expect(fitToViewport(1200, 0)).toBe(1);
  });

  it("falls back to 1:1 when the chrome alone is wider than the viewport", () => {
    // A phone-width viewport leaves a NEGATIVE remainder after the bezel and the
    // padding. Dividing by that gives a negative scale, which renders nothing at
    // all and inverts every drag.
    expect(fitToViewport(40, 40)).toBe(1);
  });
});
