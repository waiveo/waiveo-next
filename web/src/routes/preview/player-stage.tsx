import { useEffect, useRef, type CSSProperties } from "react";
import { SLIDE_CANVAS_HEIGHT, SLIDE_CANVAS_WIDTH, navItemRects, type SlideLayer } from "@/api";
import { cn } from "@/lib/utils";
import { COUNTDOWN_DEFAULT_LAYOUT, formatCountdownLayout, formatGoTimeLayout } from "@/routes/studio/go-time-layout";
import type { AssetUrls } from "@/routes/studio/slide-canvas";
import { FOCUS_RING_COLOR, FOCUS_RING_THICKNESS, focusRingRect } from "./focus";
import type { FocusTarget, ProjectedSlide } from "./playback";

/**
 * PlayerStage — a slide drawn the way `player-v3` draws it, not the way the
 * Studio draws it.
 *
 * # Why this is not `SlideStage`
 *
 * `SlideStage` is an EDITOR canvas and it is right to be one: it centres text in
 * its box so a half-placed layer is findable, wraps long strings so an operator
 * can read what they typed, letterboxes images so a picture is never distorted
 * while it is being positioned, outlines every interactive layer so the
 * pressable parts are visible at a glance, and badges the layers whose bytes are
 * missing. Every one of those is an authoring affordance, and every one of them
 * is a thing the television does NOT do.
 *
 * A preview that reused it would be a picture of the editor, shown to an
 * operator asking what the wall will look like. So this file exists, and each
 * rule below is the BrightScript's rule with the source named:
 *
 * | This stage | player-v3 | Studio canvas does |
 * |---|---|---|
 * | images/video stretch to the box | `loadDisplayMode = "scaleToFill"` (PhotonScene.brs:772) | letterboxes (`objectFit: contain`) |
 * | labels sit at the TOP of their box | `createSlideLabel` sets no `vertAlign` (:944) | vertically centres |
 * | labels are ONE line, clipped | a SceneGraph `Label` does not wrap | `white-space: pre-wrap` |
 * | nav items centre both ways, no cell borders | `horizAlign`/`vertAlign` centre (:816) | dashed outline per cell |
 * | exactly ONE focus ring, 6px, 8px pad | `showFocusRing` (:1281) | dashed outline on every interactive layer |
 * | missing bytes → grey box + em dash | `createDegradedLayer` (:997) | dashed border, icon, BYTES MISSING badge |
 * | video plays, looped | `v.loop = true` (:934) | frozen first frame |
 * | no badges of any kind | — | NEEDS RENDER / BYTES MISSING |
 *
 * # What it still cannot be
 *
 * A browser drawing a television. The typeface is not `font:SystemFontFile`, the
 * SceneGraph `Label` default size and colour are not knowable from here, and
 * `weather`/`entity` carry a `value` the box resolves at Lease issuance that the
 * console is not asking for. Those are in `fidelity.ts`, which the surface SHOWS.
 * The line this file holds is: obey every rule a browser can obey, and let the
 * ledger carry the rest. Never split the difference.
 */

/** The player's placeholder for a layer whose bytes the device does not hold —
 * `wvDegradedLayerColor`/`wvDegradedLayerTextColor`/`wvDegradedLayerText`
 * (PhotonScene.brs:1038/1042/1030). The em dash is deliberately the SAME glyph
 * `slidelive.Unavailable` uses, "so a slide degrades one way rather than two". */
const DEGRADED_FILL = "#2B2B2B";
const DEGRADED_INK = "#8A8A8A";
const DEGRADED_GLYPH = "—";

/** `wvDegradedLayerFontPx(h)` — a third of the box height, clamped 24..96 "so a
 * 40px strip still shows something and a full-canvas layer does not render a
 * 360px dash". */
export function degradedGlyphPx(h: number): number {
  return Math.min(96, Math.max(24, Math.floor(h / 3)));
}

/** The size this stage draws a Label at when the layer states no `font_px`.
 *
 * It is a GUESS and the ledger says so. The player builds a Font node only when
 * `font_px > 0` and otherwise leaves the SceneGraph `Label`'s own default, which
 * is not a number this repository can read. 48 is what the Studio canvas picks,
 * and matching it at least means the preview and the editor agree with each
 * other about a layer neither of them can be right about. */
const ASSUMED_DEFAULT_FONT_PX = 48;

/** What a `weather` or `entity` layer shows here.
 *
 * The player draws `layer.value` verbatim — the string the BOX resolved at Lease
 * issuance — and performs no lookup of its own. The console is not asking the
 * forecast service or the device plane, so this substitutes a value shaped like
 * a real answer, exactly as the Studio does and for the same reason: it shows
 * the layout, the font and the box the widget needs, and never pretends to be
 * today's weather. The ledger declares it as a stand-in. */
function standInValue(layer: SlideLayer): string {
  if (layer.kind === "weather") {
    return (layer.text ?? "").replaceAll("{temp}", "72").replaceAll("{tempc}", "22").replaceAll("{cond}", "Clear");
  }
  return (layer.text || "{state}").replaceAll("{state}", "on");
}

/** `horizAlign` is assigned only for the three legal values; anything else
 * leaves the Label's default, which is left (`createSlideLabel`:966-969). */
function horizAlign(align: SlideLayer["align"]): CSSProperties["textAlign"] {
  return align === "center" ? "center" : align === "right" ? "right" : "left";
}

/** The shared shape of every Label this stage draws.
 *
 * Top-aligned and single-line, which is the whole point: `createSlideLabel` sets
 * `translation`, `width`, `height`, `text`, optionally a font and a colour, and
 * `horizAlign` — and never `vertAlign`, so the glyphs sit at the top of the box.
 * A SceneGraph Label does not wrap either. Text that overflows is clipped, on
 * the wall and here. */
function labelStyle(layer: SlideLayer): CSSProperties {
  return {
    position: "absolute",
    left: layer.x,
    top: layer.y,
    width: layer.w,
    height: layer.h,
    color: layer.color ?? "#FFFFFF",
    fontSize: layer.font_px ?? ASSUMED_DEFAULT_FONT_PX,
    lineHeight: 1.15,
    textAlign: horizAlign(layer.align),
    whiteSpace: "nowrap",
    overflow: "hidden",
  };
}

/** The degraded placeholder, drawn as the player builds it: an opaque neutral
 * rectangle with a centred em dash sized to the box. */
function DegradedLayer({ layer }: { layer: SlideLayer }) {
  return (
    <div
      data-slot="player-layer-degraded"
      aria-hidden="true"
      style={{
        position: "absolute",
        left: layer.x,
        top: layer.y,
        width: layer.w,
        height: layer.h,
        backgroundColor: DEGRADED_FILL,
        color: DEGRADED_INK,
        fontSize: degradedGlyphPx(layer.h),
        display: "flex",
        alignItems: "center",
        justifyContent: "center",
      }}
    >
      {DEGRADED_GLYPH}
    </div>
  );
}

interface LayerProps {
  layer: SlideLayer;
  now: Date;
  assetUrls: AssetUrls;
  /** False while the transport is paused, so a video holds its frame instead of
   * running on under a paused transport — the one deliberate divergence in this
   * file, and it exists because a paused preview whose video kept playing would
   * read as a broken pause button. */
  playing: boolean;
}

/** One projected layer. The kinds are ordered as `renderSlide`'s chain orders
 * them, so the two can be read side by side. */
function PlayerLayer({ layer, now, assetUrls, playing }: LayerProps) {
  const box: CSSProperties = { position: "absolute", left: layer.x, top: layer.y, width: layer.w, height: layer.h };

  if (layer.kind === "rect") {
    return <div data-slot="player-layer-rect" aria-hidden="true" style={{ ...box, backgroundColor: layer.color }} />;
  }

  if (layer.kind === "image" || layer.kind === "video") {
    // `assetUrls === null` means the origin's answer is UNKNOWN — in flight, or
    // the read failed. Drawing the player's degraded placeholder there would
    // assert the device could not fetch the bytes, which is a claim about a
    // question nobody has answered. The surface says the origin is unreachable
    // in its own status line; here the layer is simply left black, which is what
    // it looks like before anything has loaded.
    if (assetUrls === null) {
      return <div data-slot="player-layer-unknown" aria-hidden="true" style={box} />;
    }
    const url = layer.asset_ref ? assetUrls.get(layer.asset_ref) : undefined;
    // No bytes at the origin is exactly the case the player degrades: it could
    // not fetch or verify, so it draws the placeholder rather than a hole,
    // because "an absent Poster is indistinguishable on a wall from a slide that
    // was authored without one".
    if (!url) return <DegradedLayer layer={layer} />;

    if (layer.kind === "video") {
      return (
        <PlayerVideo url={url} style={box} playing={playing} />
      );
    }
    // scaleToFill: the box is filled, aspect ratio ignored. This is the single
    // most consequential difference from the editor canvas, which letterboxes —
    // a portrait photo in a landscape box previews with black bars in the Studio
    // and ships STRETCHED.
    return <img data-slot="player-layer-image" src={url} alt="" style={{ ...box, objectFit: "fill" }} />;
  }

  if (layer.kind === "nav") {
    // One Label per item, in the cells `wire.NavItemRects` computes — the same
    // rects the player focuses, so the ring lands on the label it belongs to.
    // No cell borders: the wall draws none, and the editor's dashed outlines are
    // an authoring affordance.
    const rects = navItemRects(layer);
    return (
      <div data-slot="player-layer-nav" aria-hidden="true" style={box}>
        {(layer.items ?? []).map((item, i) => {
          const r = rects[i] ?? [layer.x, layer.y, layer.w, layer.h];
          return (
            <div
              key={i}
              data-slot="player-layer-nav-item"
              style={{
                position: "absolute",
                left: r[0] - layer.x,
                top: r[1] - layer.y,
                width: r[2],
                height: r[3],
                color: layer.color ?? "#FFFFFF",
                fontSize: layer.font_px ?? ASSUMED_DEFAULT_FONT_PX,
                // An item's cell is COMPUTED, not authored, so the player centres
                // it both ways unless the author stated an alignment — "left
                // aligning by default would pin every label to a boundary the
                // author never drew". vertAlign is centre here and nowhere else.
                display: "flex",
                alignItems: "center",
                justifyContent: layer.align === "left" ? "flex-start" : layer.align === "right" ? "flex-end" : "center",
                whiteSpace: "nowrap",
                overflow: "hidden",
              }}
            >
              {item.label}
            </div>
          );
        })}
      </div>
    );
  }

  // Every remaining kind is a Label. The player draws them through ONE
  // createSlideLabel call and differs only in where the string comes from.
  const text =
    layer.kind === "text" || layer.kind === "ping"
      ? (layer.text ?? "")
      : layer.kind === "clock" || layer.kind === "date"
        ? formatGoTimeLayout(layer.text ?? "", now)
        : layer.kind === "countdown"
          ? formatCountdownLayout(layer.text || COUNTDOWN_DEFAULT_LAYOUT, (layer.target_ms ?? 0) - now.getTime())
          : standInValue(layer);

  return (
    <div data-slot={`player-layer-${layer.kind}`} style={labelStyle(layer)}>
      {text}
    </div>
  );
}

/** A slide video. Its own component because the play/pause has to be driven
 * imperatively — a `<video>` element's playback state is not a prop — and
 * because `autoPlay` alone does not resume after a pause. */
function PlayerVideo({ url, style, playing }: { url: string; style: CSSProperties; playing: boolean }) {
  const ref = useRef<HTMLVideoElement>(null);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    if (playing) {
      // A rejected play() is normal (autoplay policy, a torn-down element) and
      // must not become an unhandled rejection in the console.
      void el.play().catch(() => {});
    } else {
      el.pause();
    }
  }, [playing]);
  return (
    <video
      data-slot="player-layer-video"
      ref={ref}
      src={url}
      muted
      loop
      playsInline
      autoPlay
      // scaleToFill, as with an image — the player's Video node fills its box.
      style={{ ...style, objectFit: "fill" }}
    />
  );
}

/** The one focus ring, drawn as the player draws it: four rectangles around a
 * padded, canvas-clamped box. Four rather than a CSS outline so the clamp is
 * VISIBLE — an outline would still draw four sides after the clamp had removed
 * one, which is the exact defect `showFocusRing`'s clamp exists to expose. */
function FocusRing({ rect }: { rect: [number, number, number, number] }) {
  const [x, y, w, h] = rect;
  const th = FOCUS_RING_THICKNESS;
  const bar = (s: CSSProperties) => ({ position: "absolute" as const, backgroundColor: FOCUS_RING_COLOR, ...s });
  return (
    <div data-slot="player-focus-ring" aria-hidden="true">
      <div style={bar({ left: x, top: y, width: w, height: th })} />
      <div style={bar({ left: x, top: y + h - th, width: w, height: th })} />
      <div style={bar({ left: x, top: y, width: th, height: h })} />
      <div style={bar({ left: x + w - th, top: y, width: th, height: h })} />
    </div>
  );
}

export interface PlayerStageProps {
  slide: ProjectedSlide;
  /** Screen pixels per canvas pixel. The artwork is drawn at 1:1 inside a
   * `transform: scale()` so a font size of 96 is 96 canvas pixels — the same
   * discipline the Studio canvas documents, and the reason preview geometry can
   * be trusted at any size. */
  scale: number;
  now: Date;
  assetUrls: AssetUrls;
  playing: boolean;
  /** The focused region, or `null` when the slide carries none or interaction is
   * off. Exactly one, because the wall has exactly one. */
  focused?: FocusTarget | null;
  /** The 5% broadcast title-safe inset. Off by default: it is a straightedge, and
   * a preview's job is to show the picture. */
  titleSafe?: boolean;
  className?: string;
}

export function PlayerStage({
  slide,
  scale,
  now,
  assetUrls,
  playing,
  focused = null,
  titleSafe = false,
  className,
}: PlayerStageProps) {
  const ring = focused ? focusRingRect(focused.rect, SLIDE_CANVAS_WIDTH, SLIDE_CANVAS_HEIGHT) : null;
  return (
    <div
      data-slot="player-stage"
      className={cn("relative overflow-hidden bg-black", className)}
      style={{ width: SLIDE_CANVAS_WIDTH * scale, height: SLIDE_CANVAS_HEIGHT * scale }}
    >
      <div
        className="absolute left-0 top-0 origin-top-left"
        style={{ width: SLIDE_CANVAS_WIDTH, height: SLIDE_CANVAS_HEIGHT, transform: `scale(${scale})` }}
      >
        {slide.layers.map((layer, i) => (
          <PlayerLayer key={i} layer={layer} now={now} assetUrls={assetUrls} playing={playing} />
        ))}
        {ring ? <FocusRing rect={ring} /> : null}
        {titleSafe ? (
          <div
            data-slot="player-title-safe"
            aria-hidden="true"
            className="pointer-events-none absolute inset-[5%] border border-dashed border-[color:var(--wv-warn)]/60"
          />
        ) : null}
      </div>
    </div>
  );
}
