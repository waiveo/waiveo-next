import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type CSSProperties,
  type PointerEvent as ReactPointerEvent,
} from "react";
import { ImageOff, QrCode, VideoOff } from "lucide-react";
import { KitIcon } from "@/components/kit";
import {
  SLIDE_CANVAS_HEIGHT,
  SLIDE_CANVAS_WIDTH,
  deriveNeedsRender,
  isContentKind,
  isInteractiveLayer,
  navItemRects,
  type CastSlide,
  type DeriveSpec,
  type SlideLayer,
} from "@/api";
import { cn } from "@/lib/utils";
import { RESIZE_HANDLES, moveLayerBy, resizeLayerBy, staleRasterKey, type ResizeHandle } from "./cast-model";
import { COUNTDOWN_DEFAULT_LAYOUT, formatCountdownLayout, formatGoTimeLayout } from "./go-time-layout";

/**
 * The Studio's canvas: the 1920×1080 slide, scaled to fit, with the selected
 * layer draggable and resizable.
 *
 * The scaling is the load-bearing detail. Every coordinate in the model is in
 * CANVAS space (wire.SlideCanvasWidth/Height) because that is the surface the
 * player draws on; the editor shows it at whatever fraction of that fits the
 * viewport. So the visuals are drawn at 1:1 inside a `transform: scale()`
 * wrapper — a font size of 96 is 96 canvas pixels, exactly as the Roku will draw
 * it, and no per-layer arithmetic can drift from that — while the CHROME
 * (selection outline, resize grips) is drawn OUTSIDE the scaled wrapper in
 * screen pixels, so grips stay grabbable at any zoom instead of shrinking with
 * the artwork.
 *
 * Pointer deltas are divided by the same scale on the way back into the model,
 * and a drag is computed from the box the layer had when the drag STARTED rather
 * than accumulated frame to frame — accumulating rounds every frame, and the
 * rounding error is exactly what makes a layer creep away from the cursor.
 */

/**
 * The content origin's `asset_ref` → fetch-`url` map, or `null` when the origin
 * has not answered — either because the listing is still in flight or because
 * the read FAILED.
 *
 * The Studio needs it because `url` is DERIVED, not authored: the wire calls it
 * "present on a SERVED slide", producers mint it at projection time, and no
 * authored cast should carry one at all. Every producer writes the ref alone —
 * `waiveo-derive` writes `asset_ref` + `derived_from` and nothing else, a pack
 * import writes what the pack declared, an API caller writes what it likes —
 * and a canvas that keyed off `url` showed every one of those as if it had no
 * bytes at all. For a derive layer that meant the fake approximation and a
 * NEEDS RENDER badge on a layer that HAD been rendered, forever.
 *
 * The server now refuses to STORE an authored one at all
 * (`internal/app/store/derivedmembers.go`), because a content url is signed and
 * expiring (`internal/feeder/contenturl`): the picker used to patch the
 * listing's url into the layer, the save persisted it, and reopening the cast a
 * day later drew a canvas of broken images against a url the origin had begun
 * refusing. The Studio drops the member on load too
 * (`cast-model.withoutDerivedLayerFields`), so inside the editor the listing is
 * the only answer there is.
 *
 * `null` is one value for two situations on purpose, because they have the same
 * consequence: the origin's answer is UNKNOWN, so nothing may be reported
 * missing. Collapsing "failed" into "loaded and empty" is the second half of the
 * same defect — the canvas then tells an operator whose box is briefly
 * unreachable that the retention sweep ate every asset in the cast, and they go
 * and re-upload or re-render bytes that were never gone.
 */
export type AssetUrls = ReadonlyMap<string, string> | null;

/**
 * The layers whose drawn raster is known to be OUT OF DATE, keyed
 * `${slideId}#${layerIndex}`.
 *
 * A derive layer's PNG is rendered at its exact spec and geometry, so editing
 * either makes the picture on the canvas a picture of the previous design. The
 * layer keeps drawing it — never blanking a screen (or an editor) over an edit
 * nobody has rendered yet is the same discipline the projection applies — but
 * drawing it with nothing said is a lie about a finished layer, which is the
 * class this whole file keeps closing. The badge says which truth it is.
 *
 * The console cannot compute this from a layer alone: `derived_from` is a hash
 * of the server's own canonical encoding, and a second implementation of that
 * encoding here is exactly the drifting copy this codebase keeps paying for.
 * What it CAN see without one is the operator's own edit, which is the case they
 * hit every time they nudge a font size — so staleness is computed in the
 * Studio, by comparing the draft against the cast as it was read
 * (cast-model.staleRasterKeys), and passed in.
 */
export type StaleRasters = ReadonlySet<string> | null;

/** What the canvas can draw for one layer's bytes. */
interface ResolvedAsset {
  /** The URL to fetch, when there is one. */
  url: string | undefined;
  /** True only when the origin's listing HAS answered and does not carry the
   * layer's ref — the bytes were swept, or never uploaded. Never true while the
   * listing is in flight, and never true when the read failed. */
  missing: boolean;
}

/**
 * Resolve a layer's bytes from the content origin's own listing.
 *
 * The LISTING is authoritative, and an authored `url` is at most a fallback for
 * when the listing is unknown. That order is the whole point: `url` is a DERIVED
 * member that producers mint at projection time, so one sitting on an authored
 * layer is a value nothing has re-checked — written by an older console, carried
 * in from a workspace export, or (on the branch that makes content urls signed
 * and expiring) already dead. Preferring it over the listing draws from the
 * expired url AND reports `missing: false`, so there is no badge either: worse
 * than either drawing nothing or saying so.
 *
 * When the origin has not answered, an authored url is better than nothing and
 * cannot be contradicted by a listing we do not have — so it is used, and
 * nothing is reported missing.
 */
function resolveLayerAsset(layer: SlideLayer, urls: AssetUrls): ResolvedAsset {
  if (!layer.asset_ref) return { url: undefined, missing: false };
  if (urls === null) return { url: layer.url, missing: false };
  const url = urls.get(layer.asset_ref);
  return { url, missing: url === undefined };
}

/** A ticking clock, shared by every clock layer on the stage (one timer, not one
 * per layer). The preview ticks for the same reason the player does: a frozen
 * clock is the single most common way a slide looks right in an editor and wrong
 * on the wall. */
function useNow(active: boolean): Date {
  const [now, setNow] = useState(() => new Date());
  useEffect(() => {
    if (!active) return;
    const id = window.setInterval(() => setNow(new Date()), 1000);
    return () => window.clearInterval(id);
  }, [active]);
  return now;
}

/** The CSS `justify-content` an `align` maps to (the text box is a flex row). */
function justifyFor(align: SlideLayer["align"]): CSSProperties["justifyContent"] {
  return align === "center" ? "center" : align === "right" ? "flex-end" : "flex-start";
}

/** A one-line human description of a layer, for the layer list, the canvas hit
 * target's accessible name, and the properties panel heading. */
export function describeLayer(layer: SlideLayer): string {
  switch (layer.kind) {
    case "text":
      return layer.text ? `Text — ${layer.text}` : "Text — (empty)";
    case "clock":
      return `Clock — ${layer.text ?? "(no format)"}`;
    case "rect":
      return `Rectangle — ${layer.color ?? "(no colour)"}`;
    case "image":
      return layer.asset_ref ? `Image — ${layer.asset_ref.replace(/^sha256:/, "").slice(0, 10)}…` : "Image — (none chosen)";
    case "video":
      return layer.asset_ref ? `Video — ${layer.asset_ref.replace(/^sha256:/, "").slice(0, 10)}…` : "Video — (none chosen)";
    case "date":
      return `Date — ${layer.text ?? "(no format)"}`;
    case "countdown":
      // The TARGET, not the layout: two countdowns on one slide differ by what
      // they count to, and a list that showed both as "HH:MM:SS" would be
      // useless for telling them apart.
      return layer.target_ms
        ? `Countdown — to ${new Date(layer.target_ms).toLocaleString()}`
        : "Countdown — (no target set)";
    case "weather":
      return `Weather — ${layer.text ?? "(no template)"}`;
    case "entity":
      return layer.entity_id ? `Entity — ${layer.entity_id}` : "Entity — (none chosen)";
    case "ping":
      // The EVENT NAME, not the label: two buttons on one slide are told apart
      // by what they fire, and that is also the string an automation author has
      // to match, so the layer list is where they should be able to read it off.
      return layer.ping_name ? `Button — fires "${layer.ping_name}"` : "Button — (no event name)";
    case "nav":
      return layer.items && layer.items.length > 0
        ? `Menu — ${layer.items.length} item${layer.items.length === 1 ? "" : "s"}`
        : "Menu — (no items yet)";
    case "derive": {
      const d = layer.derive;
      const what = d?.kind === "qr" ? `QR — ${d.data ?? "(nothing to encode)"}`
        : d?.kind === "text" ? `Styled text — ${d.text || "(empty)"}`
        : d?.kind === "rect" ? "Styled panel"
        : "Rasterized — (nothing to draw)";
      return deriveNeedsRender(layer) ? `${what} — needs render` : what;
    }
  }
}

/** The CSS that APPROXIMATES a derive spec in the editor.
 *
 * It is an approximation and the canvas says so (see the NEEDS RENDER badge):
 * the authoritative pixels come from Chromium running off-appliance, and the
 * browser drawing this preview is not necessarily the same Chromium, does not
 * have the embedded font, and cannot encode a QR symbol. What it CAN do
 * faithfully is the geometry and the styling — which is the whole reason an
 * operator is looking at the canvas — so gradient, radius, border and shadow are
 * built from the same members the page builder reads.
 *
 * Once a raster EXISTS the canvas stops approximating and draws the real PNG,
 * so what an operator checks before shipping is the actual picture. */
function deriveApproxStyle(spec: DeriveSpec | undefined): CSSProperties {
  if (!spec) return {};
  const out: CSSProperties = {};
  const f = spec.fill;
  if (f?.kind === "linear") out.background = `linear-gradient(${f.angle_deg ?? 0}deg, ${f.from} 0%, ${f.to} 100%)`;
  else if (f?.kind === "radial") out.background = `radial-gradient(circle at 50% 50%, ${f.from} 0%, ${f.to} 100%)`;
  else if (f) out.background = f.from;
  const b = spec.border;
  if (b?.width) out.border = `${b.width}px solid ${b.color ?? "#FFFFFF"}`;
  if (b?.radius) out.borderRadius = b.radius;
  const sh = spec.shadow;
  if (sh) {
    const pct = (sh.opacity_pct ?? 50) / 100;
    out.boxShadow = `${sh.dx ?? 0}px ${sh.dy ?? 0}px ${sh.blur ?? 0}px rgba(0,0,0,${pct})`;
  }
  return out;
}

/** The string a live widget SHOWS in the editor's preview.
 *
 * `date` and `countdown` are exact: the player computes them from its own clock
 * through the very grammars this file's formatters mirror, so what the canvas
 * draws is what the wall draws (modulo the second it is read in).
 *
 * `weather` and `entity` cannot be. Their value is substituted by the BOX at
 * Lease issuance (internal/slidelive) from a forecast service and the device
 * plane, neither of which the console is asking here. So the preview renders the
 * author's template with the value tokens replaced by a STAND-IN in the shape of
 * a real answer — "72° Clear", "on" — which shows the layout, the font and the
 * box the widget will need, and never pretends to be today's weather. That is
 * the same honesty the box itself applies when a source cannot answer, where it
 * substitutes an em dash rather than blanking the slide. */
function liveWidgetPreview(layer: SlideLayer, now: Date): string {
  switch (layer.kind) {
    case "clock":
    case "date":
      return formatGoTimeLayout(layer.text ?? "", now);
    case "countdown":
      return formatCountdownLayout(layer.text || COUNTDOWN_DEFAULT_LAYOUT, (layer.target_ms ?? 0) - now.getTime());
    case "weather":
      return (layer.text ?? "")
        .replaceAll("{temp}", "72")
        .replaceAll("{tempc}", "22")
        .replaceAll("{cond}", "Clear");
    case "entity":
      return (layer.text || "{state}").replaceAll("{state}", "on");
    default:
      return layer.text ?? "";
  }
}

/** The kinds whose preview changes every second, and so make the stage tick. A
 * date only turns over at midnight, but it costs the same one shared timer and
 * a slide left open across midnight would otherwise show yesterday. */
const TICKING_KINDS = ["clock", "date", "countdown"];

/** The badge a layer wears when what is drawn is not what the layer says.
 *
 * It is one component because the three truths are one vocabulary and an
 * operator has to be able to tell them apart at a glance: NEEDS RENDER (nothing
 * was ever produced), NEEDS RE-RENDER (a picture is drawn, of the previous
 * design), BYTES MISSING (a picture was produced and the origin is not serving
 * it). Reporting any of the three as another sends the operator to do work that
 * is either already done or will not help. */
function LayerBadge({ label }: { label: string }) {
  return (
    <span
      data-slot="layer-derive-badge"
      className="absolute left-2 top-2 rounded bg-black/70 px-2 py-1 text-white"
      style={{ fontSize: 28 }}
    >
      {label}
    </span>
  );
}

/** One layer, drawn the way the player draws it. Canvas-space coordinates. */
export function LayerView({
  layer,
  now,
  assetUrls = null,
  stale = false,
}: {
  layer: SlideLayer;
  now: Date;
  /** The content origin's ref→url listing, for a content-bearing layer's
   * preview. See AssetUrls. */
  assetUrls?: AssetUrls;
  /** The drawn raster is a picture of a previous spec or geometry. See
   * StaleRasters — the canvas cannot decide this for itself. */
  stale?: boolean;
}) {
  const box: CSSProperties = {
    position: "absolute",
    left: layer.x,
    top: layer.y,
    width: layer.w,
    height: layer.h,
  };

  if (layer.kind === "rect") {
    return <div data-slot="layer-rect" aria-hidden="true" style={{ ...box, backgroundColor: layer.color }} />;
  }

  const asset = resolveLayerAsset(layer, assetUrls);

  if (layer.kind === "derive") {
    // A RENDERED derive layer is drawn as exactly what it becomes on the wire —
    // an image — so the canvas stops approximating the moment the real pixels
    // exist. That is not a nicety: the whole point of the rasterizer is styling
    // this browser cannot reproduce, so an approximation that never gave way to
    // the truth would be the last thing an operator saw before shipping.
    //
    // The URL is RESOLVED, never assumed: `waiveo-derive` writes the asset_ref
    // and its digest and nothing else, so a canvas that waited for an authored
    // `url` waited for something no rasterizer run has ever produced.
    if (asset.url) {
      // A STALE raster is still drawn — never blanking a layer over an edit
      // nobody has rendered yet is the same discipline the projection applies —
      // but it is drawn WITH the badge. Silently showing a picture of the
      // previous font size is the "lie about a finished layer" this file's
      // badges exist to end, seen from the one side that had no badge at all.
      return (
        <div data-slot="layer-derive" aria-hidden="true" style={box} className="relative">
          <img src={asset.url} alt="" style={{ width: "100%", height: "100%", objectFit: "contain" }} />
          {stale ? <LayerBadge label="NEEDS RE-RENDER" /> : null}
        </div>
      );
    }
    const spec = layer.derive;
    // The badge states which of the two truths this is. NEEDS RENDER means no
    // raster has ever been produced (deriveNeedsRender — the same predicate the
    // layer list and the properties panel read). BYTES MISSING means one WAS
    // produced and the content origin is not serving it: swept, or never
    // uploaded. Reporting the second as the first would send an operator to run
    // a tool that has already run.
    const missing = asset.missing;
    return (
      <div data-slot="layer-derive-preview" aria-hidden="true" style={{ ...box, ...deriveApproxStyle(spec) }}
        className="flex items-center justify-center overflow-hidden">
        {spec?.kind === "text" ? (
          <span
            style={{
              color: spec.color ?? "#FFFFFF",
              fontSize: spec.font_px ?? 64,
              lineHeight: 1.15,
              textAlign: spec.align ?? "left",
              width: "100%",
              padding: "0 8px",
              whiteSpace: "pre-wrap",
            }}
          >
            {spec.text ?? ""}
          </span>
        ) : spec?.kind === "qr" ? (
          <span style={{ color: spec.color ?? "#111827" }}>
            <KitIcon icon={QrCode} decorative className="size-32" />
          </span>
        ) : null}
        {deriveNeedsRender(layer) || missing ? (
          <LayerBadge label={missing ? "BYTES MISSING" : "NEEDS RENDER"} />
        ) : null}
      </div>
    );
  }

  if (isContentKind(layer.kind)) {
    // A content-bearing layer with no bytes CHOSEN yet is drawn as a labelled
    // outline rather than nothing: it is a placed, selectable, movable object
    // that simply is not finished, and an invisible one could not be found
    // again.
    //
    // A layer that NAMES bytes the origin is not serving gets the same outline,
    // because it is equally undrawable — but it wears the badge, because it is
    // not the same situation and the remedy is not the same. "Nothing chosen"
    // is finished by picking bytes; "the origin has no such digest" means the
    // bytes were swept or never uploaded, and an operator who reads that as the
    // first goes looking for a picker that will not help. The `missing` signal
    // was computed here and never read, which made the two indistinguishable —
    // and it is never true while the origin's answer is unknown, so a slow or
    // failed read cannot produce this badge.
    if (!asset.url) {
      return (
        <div
          data-slot={`layer-${layer.kind}-empty`}
          aria-hidden="true"
          style={box}
          className="relative flex items-center justify-center border-4 border-dashed border-[color:var(--wv-border)] bg-[color:var(--wv-surface-2)]"
        >
          <KitIcon icon={layer.kind === "video" ? VideoOff : ImageOff} decorative className="size-16 text-muted-foreground" />
          {asset.missing ? <LayerBadge label="BYTES MISSING" /> : null}
        </div>
      );
    }
    if (layer.kind === "video") {
      // The first frame, not playback. The player loops the clip for the
      // slide's dwell time, but the Studio is a LAYOUT surface: a still frame
      // answers "is this the right clip, and does it fit the box" without
      // spinning up a decoder per thumbnail — the filmstrip draws every slide
      // through this same component, so autoplaying here would start one video
      // per slide the moment a cast is opened.
      return (
        <video
          data-slot="layer-video"
          src={asset.url}
          muted
          playsInline
          preload="metadata"
          style={{ ...box, objectFit: "contain" }}
        />
      );
    }
    return (
      <img
        data-slot="layer-image"
        src={asset.url}
        alt=""
        style={{ ...box, objectFit: "contain" }}
      />
    );
  }

  if (layer.kind === "nav") {
    // A menu is drawn item by item, in the SAME cells the player focuses
    // (`navItemRects`, mirrored three ways — see its doc). Drawing it as one box
    // with the item labels crammed inside would look fine in the editor and
    // nothing like the wall.
    //
    // Each cell carries a dashed outline, which is the editor's standing
    // shorthand for "this is a region, not paint" (the unfilled image/video
    // placeholders use the same one). It is the only honest preview of focus:
    // exactly one item is focused on the TV at a time and which one depends on
    // what the viewer last pressed, so showing a solid ring on one of them here
    // would assert something the editor cannot know.
    const rects = navItemRects(layer);
    return (
      <div data-slot="layer-nav" aria-hidden="true" style={box}>
        {(layer.items ?? []).map((item, i) => {
          const [ix, iy, iw, ih] = rects[i] ?? [layer.x, layer.y, layer.w, layer.h];
          return (
            <div
              key={i}
              data-slot="layer-nav-item"
              style={{
                position: "absolute",
                left: ix - layer.x,
                top: iy - layer.y,
                width: iw,
                height: ih,
                color: layer.color ?? "#FFFFFF",
                fontSize: layer.font_px ?? 44,
                display: "flex",
                alignItems: "center",
                justifyContent: layer.align ? justifyFor(layer.align) : "center",
                overflow: "hidden",
                border: "2px dashed rgba(139,92,246,0.9)",
                boxSizing: "border-box",
              }}
            >
              {item.label}
            </div>
          );
        })}
      </div>
    );
  }

  // Every remaining kind is a Label on the player — text, clock, date,
  // countdown, weather, entity, and a `ping` button's label — differing only in
  // where the string comes from.
  // One branch for all six is deliberate: they share the font, colour, alignment
  // and box behaviour exactly, and six near-identical JSX blocks is six places
  // for the preview to drift from the wall.
  const content = layer.kind === "text" || layer.kind === "ping" ? (layer.text ?? "") : liveWidgetPreview(layer, now);
  return (
    <div
      data-slot={`layer-${layer.kind}`}
      style={{
        ...box,
        // An INTERACTIVE layer gets a dashed outline so an operator can see at a
        // glance which parts of the slide a viewer can press — including the
        // ones that carry no visual hint of it at all, like an `entity` reading
        // somebody made interactive by giving it an event name. Keyed on
        // `isInteractiveLayer` rather than on the kind for exactly that reason:
        // a kind test would outline the buttons and leave every interactive
        // widget looking inert, while the player focuses all of them.
        ...(isInteractiveLayer(layer)
          ? { outline: "2px dashed rgba(139,92,246,0.9)", outlineOffset: "-2px" }
          : {}),
        color: layer.color ?? "#FFFFFF",
        fontSize: layer.font_px ?? 48,
        lineHeight: 1.15,
        textAlign: layer.align ?? "left",
        display: "flex",
        alignItems: "center",
        justifyContent: justifyFor(layer.align),
        overflow: "hidden",
        whiteSpace: "pre-wrap",
      }}
    >
      {content}
    </div>
  );
}

/** The slide, drawn at `scale`, with no editing chrome. Used by the canvas and
 * by the filmstrip's thumbnails, so a thumbnail can never disagree with the
 * canvas about what a slide looks like. */
export function SlideStage({
  slide,
  scale,
  className,
  assetUrls = null,
  staleRasters = null,
}: {
  slide: CastSlide;
  scale: number;
  className?: string;
  /** The content origin's ref→url listing, for content-bearing layers'
   * previews. See AssetUrls. */
  assetUrls?: AssetUrls;
  /** Which layers' drawn rasters are out of date. See StaleRasters. The KEY is
   * built here, not by the caller's loop, because this is the one place that
   * holds both the slide's id and the layer's index. */
  staleRasters?: StaleRasters;
}) {
  const ticking = slide.layers.some((l) => TICKING_KINDS.includes(l.kind));
  const now = useNow(ticking);
  return (
    <div
      data-slot="slide-stage"
      className={cn("relative overflow-hidden bg-black", className)}
      style={{ width: SLIDE_CANVAS_WIDTH * scale, height: SLIDE_CANVAS_HEIGHT * scale }}
    >
      <div
        className="absolute left-0 top-0 origin-top-left"
        style={{
          width: SLIDE_CANVAS_WIDTH,
          height: SLIDE_CANVAS_HEIGHT,
          transform: `scale(${scale})`,
        }}
      >
        {slide.layers.map((layer, i) => (
          <LayerView
            key={i}
            layer={layer}
            now={now}
            assetUrls={assetUrls}
            stale={staleRasters?.has(staleRasterKey(slide.id, i)) ?? false}
          />
        ))}
      </div>
    </div>
  );
}

/**
 * The composition guides: centre lines, thirds, and a title-safe inset.
 *
 * All three are drawn as PERCENTAGES of the stage, so one component is correct
 * at every zoom without knowing the scale — and none of them snaps anything.
 * That is the whole design: a guide that moves a layer is a behaviour with a
 * right answer and several wrong ones (what counts as near, whether a resize
 * snaps too, whether the operator can suppress it mid-drag), and this editor's
 * drag path is the part its test suite proves hardest. These are a straightedge
 * held up to the artwork, nothing more.
 *
 * The title-safe inset is the one that is not decoration. A television overscans
 * — a real panel can crop several percent off every edge — so text laid flush to
 * the canvas border can be legitimately unreadable on the wall while looking
 * perfect in the editor. 5% is the broadcast convention.
 */
function CanvasGuides() {
  const line = "absolute bg-[color:var(--wv-accent)]/25";
  return (
    <div data-slot="canvas-guides" aria-hidden="true" className="pointer-events-none absolute inset-0 z-[5]">
      {/* Thirds. */}
      <div className={cn(line, "left-1/3 top-0 h-full w-px")} />
      <div className={cn(line, "left-2/3 top-0 h-full w-px")} />
      <div className={cn(line, "left-0 top-1/3 h-px w-full")} />
      <div className={cn(line, "left-0 top-2/3 h-px w-full")} />
      {/* Centre, brighter than the thirds — it is the one an operator lines
          things up against most. */}
      <div className="absolute left-1/2 top-0 h-full w-px bg-[color:var(--wv-accent)]/55" />
      <div className="absolute left-0 top-1/2 h-px w-full bg-[color:var(--wv-accent)]/55" />
      {/* Title-safe. */}
      <div className="absolute inset-[5%] border border-dashed border-[color:var(--wv-warn)]/50" />
    </div>
  );
}

/** Where a grip sits on the selection box, as a fraction of its width/height. */
const HANDLE_ANCHOR: Record<ResizeHandle, { fx: number; fy: number; cursor: string }> = {
  nw: { fx: 0, fy: 0, cursor: "nwse-resize" },
  n: { fx: 0.5, fy: 0, cursor: "ns-resize" },
  ne: { fx: 1, fy: 0, cursor: "nesw-resize" },
  e: { fx: 1, fy: 0.5, cursor: "ew-resize" },
  se: { fx: 1, fy: 1, cursor: "nwse-resize" },
  s: { fx: 0.5, fy: 1, cursor: "ns-resize" },
  sw: { fx: 0, fy: 1, cursor: "nesw-resize" },
  w: { fx: 0, fy: 0.5, cursor: "ew-resize" },
};

const HANDLE_NAME: Record<ResizeHandle, string> = {
  nw: "top left",
  n: "top",
  ne: "top right",
  e: "right",
  se: "bottom right",
  s: "bottom",
  sw: "bottom left",
  w: "left",
};

export interface SlideCanvasProps {
  slide: CastSlide;
  selectedIndex: number | null;
  onSelect: (index: number | null) => void;
  /** A drag finished a frame: the layer's new absolute canvas geometry. */
  onGeometry: (index: number, geometry: Pick<SlideLayer, "x" | "y" | "w" | "h">) => void;
  /**
   * A pointer gesture started or finished.
   *
   * A drag emits one `onGeometry` per pointermove — hundreds for a long one —
   * and the host's history has no other way to know that they were all ONE
   * thing the operator did. Announcing the edges is what makes a 200px drag a
   * single undo step rather than 200 of them, and it is exact where a timer
   * is not: an operator who holds the button still for a second while lining
   * a layer up has not performed two drags.
   */
  onGesture?: (phase: "begin" | "end") => void;
  /** The content origin's ref→url listing (see AssetUrls). */
  assetUrls?: AssetUrls;
  /** Which layers' drawn rasters are out of date (see StaleRasters). */
  staleRasters?: StaleRasters;
  /**
   * The zoom, when the HOST owns it.
   *
   * Left out, the canvas measures its column and fits — which is the right
   * answer for anything that just wants a slide drawn at whatever size it has.
   * The Studio's viewport is not that: it has an explicit zoom the operator sets
   * from the tool rail and the keyboard, it can go past 100% (where the stage is
   * larger than the frame and the viewport scrolls), and its fit is against the
   * viewport's HEIGHT as well as its width. None of that can be expressed by a
   * component measuring itself, and two components each deciding the scale is
   * the way the pointer arithmetic drifts from the drawing.
   *
   * Given, it is used verbatim — including a scale above 1 — and the measuring
   * effect does not run at all.
   */
  scale?: number;
  /** Classes for the stage's own frame, for a host that draws its own (the
   * Studio's TV bezel supplies the border, so it turns this one off). */
  stageClassName?: string;
  /** Draw the composition guides over the artwork (see CanvasGuides). */
  showGuides?: boolean;
}

/** What a drag in progress is holding: which layer, which grip (null = move),
 * where the pointer went down, and the box the layer had at that instant. */
interface DragState {
  index: number;
  handle: ResizeHandle | null;
  startX: number;
  startY: number;
  origin: SlideLayer;
}

export function SlideCanvas({
  slide,
  selectedIndex,
  onSelect,
  onGeometry,
  onGesture,
  assetUrls = null,
  staleRasters = null,
  scale: controlledScale,
  stageClassName,
  showGuides = false,
}: SlideCanvasProps) {
  const frameRef = useRef<HTMLDivElement>(null);
  const [measuredScale, setMeasuredScale] = useState(1);
  const scale = controlledScale ?? measuredScale;
  // The live scale for the pointer handlers, which are bound once: reading it
  // from a ref keeps them from being torn down and rebound on every resize.
  const scaleRef = useRef(scale);
  scaleRef.current = scale;
  const dragRef = useRef<DragState | null>(null);
  const [dragging, setDragging] = useState(false);
  // Whether the host owns the zoom, read inside the effect below rather than
  // listed as a dependency: the effect must not re-run (and re-measure) merely
  // because a controlled scale changed.
  const controlledRef = useRef(controlledScale !== undefined);
  controlledRef.current = controlledScale !== undefined;

  // Fit the canvas to the column, WHEN the host has not given a scale.
  // `clientWidth` is 0 in a non-layout environment (jsdom), where a scale of 0
  // would collapse the stage and make every pointer delta infinite — so an
  // unmeasurable frame falls back to 1:1 rather than to nothing.
  useLayoutEffect(() => {
    const measure = () => {
      if (controlledRef.current) return;
      const width = frameRef.current?.clientWidth ?? 0;
      setMeasuredScale(width > 0 ? Math.min(width / SLIDE_CANVAS_WIDTH, 1) : 1);
    };
    measure();
    window.addEventListener("resize", measure);
    return () => window.removeEventListener("resize", measure);
  }, []);

  const beginDrag = useCallback(
    (e: ReactPointerEvent, index: number, handle: ResizeHandle | null) => {
      const layer = slide.layers[index];
      if (!layer) return;
      // Stop the stage's own "click the background to deselect" from firing, and
      // stop the browser from starting a text/image drag of the artwork.
      e.stopPropagation();
      e.preventDefault();
      onSelect(index);
      // …and put focus where the operator just pressed, BY HAND, because that
      // `preventDefault` is exactly what stops the browser doing it. Without
      // this, clicking a layer selected it and left focus wherever it was — so
      // the selection ring and the focus ring disagreed, and nothing on the
      // canvas could be operated from the keyboard after a click. (The nudge
      // itself is bound at the document and acts on the SELECTION, so it works
      // either way now; this is about the two rings telling the same story, and
      // about Tab continuing from where the operator is looking.)
      // Found by index rather than from the event target, because a grip is a
      // SIBLING of the hit box and not a child of it — `closest` from a grip
      // would leave focus on a tabIndex -1 handle that Tab cannot return to.
      frameRef.current
        ?.querySelector<HTMLElement>(`[data-slot="layer-hit"][data-layer-index="${index}"]`)
        ?.focus();
      dragRef.current = { index, handle, startX: e.clientX, startY: e.clientY, origin: layer };
      setDragging(true);
      // AFTER the selection, which is not part of the gesture and which ends
      // whatever coalescing run preceded it.
      onGesture?.("begin");
    },
    [slide, onSelect, onGesture],
  );

  // The move/up listeners live on the WINDOW, not the grip: a fast drag outruns
  // the 10px grip constantly, and a listener bound to it would drop the gesture
  // the moment the pointer left. (Pointer capture would also work, but window
  // listeners behave identically whether or not the environment implements it.)
  useEffect(() => {
    const onMove = (e: PointerEvent) => {
      const drag = dragRef.current;
      if (!drag) return;
      const dx = (e.clientX - drag.startX) / scaleRef.current;
      const dy = (e.clientY - drag.startY) / scaleRef.current;
      const next = drag.handle
        ? resizeLayerBy(drag.origin, drag.handle, dx, dy)
        : moveLayerBy(drag.origin, dx, dy);
      onGeometry(drag.index, { x: next.x, y: next.y, w: next.w, h: next.h });
    };
    const onUp = () => {
      if (!dragRef.current) return;
      dragRef.current = null;
      setDragging(false);
      onGesture?.("end");
    };
    window.addEventListener("pointermove", onMove);
    window.addEventListener("pointerup", onUp);
    window.addEventListener("pointercancel", onUp);
    return () => {
      window.removeEventListener("pointermove", onMove);
      window.removeEventListener("pointerup", onUp);
      window.removeEventListener("pointercancel", onUp);
    };
  }, [onGeometry, onGesture]);

  // NOTE: the arrow-key nudge and Delete are NOT bound here. They were, on this
  // element, and that made them unreachable: `beginDrag` preventDefaults its own
  // pointerdown (so the browser does not drag the artwork), which also stops the
  // browser moving focus — so clicking a layer selected it without focusing it
  // and the arrows went nowhere. They are bound at the DOCUMENT now and act on
  // the SELECTION (studio-shortcuts.matchNudge), which is the thing the operator
  // can see. This element stays focusable, and focus now follows a click, so the
  // selection ring and the focus ring agree.

  const selected = selectedIndex === null ? undefined : slide.layers[selectedIndex];

  return (
    <div
      ref={frameRef}
      data-slot="slide-canvas"
      className={cn(controlledScale === undefined ? "w-full min-w-0" : "shrink-0")}
    >
      <div
        className={cn(
          "relative mx-auto overflow-hidden rounded-panel border border-border bg-black",
          stageClassName,
        )}
        style={{ width: SLIDE_CANVAS_WIDTH * scale, height: SLIDE_CANVAS_HEIGHT * scale }}
        onPointerDown={() => onSelect(null)}
      >
        {/* The artwork, at 1:1 inside the scale transform. */}
        <SlideStage
          slide={slide}
          scale={scale}
          assetUrls={assetUrls}
          staleRasters={staleRasters}
          className="pointer-events-none absolute left-0 top-0"
        />

        {showGuides ? <CanvasGuides /> : null}

        {/* The chrome, in screen pixels. One hit target per layer, topmost last
            so the z-order the operator sees is the z-order they click. */}
        {slide.layers.map((layer, i) => (
          <div
            key={i}
            role="button"
            tabIndex={0}
            data-slot="layer-hit"
            data-layer-index={i}
            data-selected={i === selectedIndex}
            aria-pressed={i === selectedIndex}
            aria-label={`Layer ${i + 1}: ${describeLayer(layer)}`}
            onPointerDown={(e) => beginDrag(e, i, null)}
            onClick={(e) => {
              e.stopPropagation();
              onSelect(i);
            }}
            className={cn(
              "absolute outline-none",
              i === selectedIndex
                ? "border-2 border-[color:var(--wv-accent-text)]"
                : "border border-transparent hover:border-[color:var(--wv-accent-text)]/60",
              dragging && i === selectedIndex ? "cursor-grabbing" : "cursor-grab",
              "focus-visible:ring-2 focus-visible:ring-ring",
            )}
            style={{
              left: layer.x * scale,
              top: layer.y * scale,
              width: layer.w * scale,
              height: layer.h * scale,
            }}
          />
        ))}

        {/* The selected layer's grips, siblings of the hit targets (never nested
            inside one — a control inside a control is neither clickable nor
            announceable). */}
        {selected && selectedIndex !== null
          ? RESIZE_HANDLES.map((handle) => {
              const anchor = HANDLE_ANCHOR[handle];
              return (
                <div
                  key={handle}
                  role="button"
                  tabIndex={-1}
                  data-slot="resize-handle"
                  data-handle={handle}
                  aria-label={`Resize layer ${selectedIndex + 1} from the ${HANDLE_NAME[handle]}`}
                  onPointerDown={(e) => beginDrag(e, selectedIndex, handle)}
                  className="absolute z-10 size-2.5 -translate-x-1/2 -translate-y-1/2 rounded-[3px] border border-[color:var(--wv-bg)] bg-[color:var(--wv-accent-text)]"
                  style={{
                    left: (selected.x + selected.w * anchor.fx) * scale,
                    top: (selected.y + selected.h * anchor.fy) * scale,
                    cursor: anchor.cursor,
                  }}
                />
              );
            })
          : null}
      </div>
    </div>
  );
}
