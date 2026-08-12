import { navItemRects, type SlideLayer } from "@/api";
import type { FocusTarget, PressOutcome } from "./playback";

/**
 * The D-pad, mirrored from the player.
 *
 * `nav` and `ping` are the only layers whose value flows from the wall back to
 * the box, and today the only way to find out whether an authored one works is
 * to stand in front of a television with a remote. This module is the console's
 * transcription of the three BrightScript rules that decide what a remote does:
 *
 *  1. WHICH regions are focusable, and in what order they are registered —
 *     `PhotonScene.brs:688 renderSlide`.
 *  2. WHERE an arrow key moves — `PhotonScene.brs:1359 wvSpatialNeighbor`.
 *  3. WHAT OK does — `PhotonScene.brs:1195 activateFocusedTarget`.
 *
 * It is a transcription and not an approximation: every rule below is the
 * BrightScript's rule, and where the BrightScript does something surprising
 * (registering a ping OUTSIDE the kind chain; skipping a nav layer's own ping
 * name; landing focus on index 0 in Z-order) this file does the surprising thing
 * too. A D-pad simulator that moved "sensibly" instead would be worse than none:
 * an operator would lay out a menu that walks correctly here and traps focus on
 * the wall.
 */

/**
 * Every focusable region on a projected slide, in the order the player registers
 * them — which is also the order that decides where focus LANDS.
 *
 * The registration order is layer order (Z-order), and within a `nav` layer,
 * item order. `renderSlide` says why focus starts at index 0 of this list:
 * "First in Z-ORDER rather than nearest a corner: the layer stack's order is the
 * one thing the author controls directly."
 *
 * Two rules here are the ones a reimplementation gets wrong, and both are the
 * player's:
 *
 *  - A `ping_name` is registered for EVERY drawn kind, not just `ping`. That is
 *    the whole interactive-widget mechanism (`wire.LayerIsInteractive`), and the
 *    player registers it outside the kind chain precisely because putting it
 *    inside the `ping` branch is "the half-implementation this codebase keeps
 *    shipping".
 *  - A `nav` layer carrying a `ping_name` does NOT get a whole-layer target: its
 *    items are already targets and "a whole-layer target on top of them would
 *    sit over every item and steal their focus".
 *
 * And one rule is about what is DRAWN: only a drawn layer may be focusable. On
 * the player an unrecognized kind sets `drawn = false` and is skipped, so it
 * registers nothing — a focus trap on the forward-compatibility path. Every kind
 * this console knows about draws, so the practical case is a layer with a
 * `ping_name` and an unknown kind, which cannot reach here through the
 * projection anyway (the serve gate refuses it).
 */
export function focusTargets(layers: readonly SlideLayer[]): FocusTarget[] {
  const targets: FocusTarget[] = [];

  layers.forEach((layer, layerIndex) => {
    if (layer.kind === "nav") {
      const rects = navItemRects(layer);
      (layer.items ?? []).forEach((item, itemIndex) => {
        const rect = rects[itemIndex];
        if (!rect) return;
        targets.push({
          layerIndex,
          itemIndex,
          rect,
          press: { kind: "nav", label: item.label, targetSlideId: item.target_slide_id },
        });
      });
      // Deliberately no fall-through to the ping registration below — see this
      // function's doc, and PhotonScene.brs:846 (`kind <> "nav"`).
      return;
    }

    if (layer.ping_name) {
      targets.push({
        layerIndex,
        itemIndex: null,
        rect: [layer.x, layer.y, layer.w, layer.h],
        press: { kind: "ping", pingName: layer.ping_name },
      });
    }
  });

  return targets;
}

/** The four directions a remote's D-pad offers. */
export type Direction = "up" | "down" | "left" | "right";

/**
 * Where an arrow key moves focus, or `null` when nothing lies that way.
 *
 * `wvSpatialNeighbor` (PhotonScene.brs:1359), rule for rule: consider only
 * targets whose CENTRE is strictly beyond the current centre in the pressed
 * direction, and pick the one minimising `along + across * 2`. Ties go to the
 * earliest registered target, because the BrightScript's comparison is `<`
 * rather than `<=` and it scans in registration order.
 *
 * The doubling of `across` is what makes the traversal feel like a D-pad rather
 * than a nearest-neighbour search: a target far away but directly in line beats
 * a closer one off to the side. Getting that weight wrong would produce a
 * simulator that walks a menu in a different order from the wall, which is worse
 * than not simulating it.
 *
 * Returning `null` matters as much as returning an index: the player returns
 * `false` from `onKeyEvent` when a direction has no neighbour, deliberately NOT
 * swallowing the key, "so Home still exits". The preview's own key handler reads
 * this null the same way — it lets the browser have the key.
 */
export function spatialNeighbor(
  targets: readonly FocusTarget[],
  from: number,
  direction: Direction,
): number | null {
  if (from < 0 || from >= targets.length) return null;
  const cur = targets[from].rect;
  const cx = cur[0] + cur[2] / 2;
  const cy = cur[1] + cur[3] / 2;

  let best: number | null = null;
  let bestScore = 0;

  for (let i = 0; i < targets.length; i++) {
    if (i === from) continue;
    const t = targets[i].rect;
    const tx = t[0] + t[2] / 2;
    const ty = t[1] + t[3] / 2;

    let ok = false;
    let along = 0;
    let across = 0;
    switch (direction) {
      case "right":
        ok = tx > cx;
        along = tx - cx;
        across = Math.abs(ty - cy);
        break;
      case "left":
        ok = tx < cx;
        along = cx - tx;
        across = Math.abs(ty - cy);
        break;
      case "down":
        ok = ty > cy;
        along = ty - cy;
        across = Math.abs(tx - cx);
        break;
      case "up":
        ok = ty < cy;
        along = cy - ty;
        across = Math.abs(tx - cx);
        break;
    }
    if (!ok) continue;
    const score = along + across * 2;
    if (best === null || score < bestScore) {
      best = i;
      bestScore = score;
    }
  }
  return best;
}

/**
 * The focus ring's own geometry, clamped to the canvas exactly as
 * `showFocusRing` (PhotonScene.brs:1281) clamps it.
 *
 * The clamp is not a detail. Its own comment: "a target flush against an edge
 * would otherwise place a bar at a negative coordinate, where SceneGraph draws
 * it off-screen and the outline silently becomes three-sided — focus that looks
 * like focus on every layer except the ones at the edges, which is where an
 * author is most likely to put a menu." A preview that drew an un-clamped ring
 * would show four sides where the wall shows three, and hide the very defect an
 * operator opened the preview to find.
 *
 * Returns `null` when the clamp leaves nothing to draw — the player hides the
 * ring in that case.
 */
export const FOCUS_RING_PAD = 8;
export const FOCUS_RING_THICKNESS = 6;
/** `0x8B5CF6FF` — the ring colour, as the scene declares it. */
export const FOCUS_RING_COLOR = "#8B5CF6";

export function focusRingRect(
  rect: [number, number, number, number],
  canvasWidth: number,
  canvasHeight: number,
): [number, number, number, number] | null {
  const pad = FOCUS_RING_PAD;
  let [rx, ry] = [rect[0] - pad, rect[1] - pad];
  let rw = rect[2] + pad * 2;
  let rh = rect[3] + pad * 2;
  if (rx < 0) {
    rw += rx;
    rx = 0;
  }
  if (ry < 0) {
    rh += ry;
    ry = 0;
  }
  if (rx + rw > canvasWidth) rw = canvasWidth - rx;
  if (ry + rh > canvasHeight) rh = canvasHeight - ry;
  if (rw <= 0 || rh <= 0) return null;
  return [rx, ry, rw, rh];
}

/** A human sentence for what a press would do, used by the interaction log. */
export function describePress(press: PressOutcome, slideName: (id: string) => string): string {
  return press.kind === "nav"
    ? `Menu item “${press.label}” → jumps to ${slideName(press.targetSlideId)}`
    : `Button fires screen.interaction with interaction = “${press.pingName}”`;
}
