import { SLIDE_CANVAS_HEIGHT, SLIDE_CANVAS_WIDTH } from "@/api";

/**
 * The Studio viewport's zoom arithmetic, as pure functions.
 *
 * It lives outside the component for the reason every other rule in this folder
 * does: a scale computed inside a `useLayoutEffect` can only be checked by
 * rendering the editor in a DOM that reports zero for every measurement, which
 * is precisely the environment in which a fit calculation is meaningless. Here
 * the cases that matter — an unmeasurable viewport, a viewport smaller than the
 * chrome around the canvas, the step ladder's ends — are ordinary function
 * calls.
 */

/**
 * The zoom ladder, as fractions.
 *
 * A ladder rather than a multiplier because the useful stops are not
 * geometrically spaced: an operator wants 100% and 50% exactly (a 1:1 pixel
 * check, and the half-scale that fits 1920 in a laptop), and a `× 1.25` walk
 * from an arbitrary fit scale lands on 47.3% and 59.1% and never on either.
 */
export const ZOOM_STEPS = [0.05, 0.1, 0.15, 0.25, 0.33, 0.5, 0.67, 0.75, 1, 1.5, 2, 3, 4] as const;

export const MIN_ZOOM = ZOOM_STEPS[0];
export const MAX_ZOOM = ZOOM_STEPS[ZOOM_STEPS.length - 1] as number;

/** The next stop above `scale`, or `scale` when it is already at the top.
 * "Above" is strict, so zooming in from a fit scale that happens to equal a stop
 * still moves. */
export function zoomInFrom(scale: number): number {
  return ZOOM_STEPS.find((s) => s > scale + 1e-6) ?? MAX_ZOOM;
}

/** The next stop below `scale`, or `scale` when it is already at the bottom. */
export function zoomOutFrom(scale: number): number {
  for (let i = ZOOM_STEPS.length - 1; i >= 0; i -= 1) {
    const s = ZOOM_STEPS[i] as number;
    if (s < scale - 1e-6) return s;
  }
  return MIN_ZOOM;
}

/**
 * The chrome the canvas is drawn inside, in screen pixels — the TV bezel's
 * padding and the stand below it, plus the breathing room around the whole
 * frame.
 *
 * Named here rather than measured because `fitToViewport` has to subtract it
 * BEFORE the artwork is laid out, and a fit that ignored it puts the stand and
 * the bottom of the bezel under the tool rail — which is exactly what "fit to
 * window" is supposed to make impossible.
 */
export const CANVAS_CHROME = {
  /** TvFrame's `px-4` on both sides. */
  bezelX: 32,
  /** TvFrame's `pt-4` + `pb-7`. */
  bezelY: 44,
  /** The stand's neck and base. */
  stand: 22,
  /** The viewport's own padding around the frame. */
  padX: 48,
  padY: 48,
} as const;

/**
 * The largest scale at which the whole framed canvas fits in a viewport of
 * `width` × `height` screen pixels.
 *
 * Two properties are deliberate. It never returns more than 1:1 — "fit" that
 * blew a 1920 canvas up to 140% on a 4K panel would show the operator a picture
 * no screen will ever draw, and the ladder's own 150%+ stops are there for
 * anyone who wants that. And an UNMEASURABLE viewport (0, or a negative
 * remainder after the chrome, which is what a phone-sized viewport gives)
 * returns exactly 1 rather than 0 or a fraction: a scale of 0 collapses the
 * stage and makes every pointer delta infinite, and the test environment
 * reports 0 for every dimension there is.
 */
export function fitToViewport(width: number, height: number): number {
  const availableX = width - CANVAS_CHROME.bezelX - CANVAS_CHROME.padX;
  const availableY = height - CANVAS_CHROME.bezelY - CANVAS_CHROME.stand - CANVAS_CHROME.padY;
  const fit = Math.min(availableX / SLIDE_CANVAS_WIDTH, availableY / SLIDE_CANVAS_HEIGHT, 1);
  // ONE guard, not two. There was an `if (availableX <= 0 || availableY <= 0)
  // return 1` above as well, and mutation testing showed it could be deleted
  // with no test failing anywhere — because a non-positive remainder always
  // produces a non-positive `fit`, so this line already covered every case the
  // other one did. Two guards for one property is not defence in depth; it is a
  // guard nothing can hold responsible.
  return fit <= 0 ? 1 : fit;
}
