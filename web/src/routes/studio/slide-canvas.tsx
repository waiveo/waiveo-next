import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type CSSProperties,
  type KeyboardEvent as ReactKeyboardEvent,
  type PointerEvent as ReactPointerEvent,
} from "react";
import { ImageOff, QrCode, VideoOff } from "lucide-react";
import { KitIcon } from "@/components/kit";
import {
  SLIDE_CANVAS_HEIGHT,
  SLIDE_CANVAS_WIDTH,
  deriveNeedsRender,
  isContentKind,
  type CastSlide,
  type DeriveSpec,
  type SlideLayer,
} from "@/api";
import { cn } from "@/lib/utils";
import { RESIZE_HANDLES, moveLayerBy, resizeLayerBy, type ResizeHandle } from "./cast-model";
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

/** How far an arrow key moves the selected layer, in canvas pixels. */
const NUDGE = 8;
/** …and with Alt held, for placing something precisely. */
const NUDGE_FINE = 1;

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

/** One layer, drawn the way the player draws it. Canvas-space coordinates. */
export function LayerView({ layer, now }: { layer: SlideLayer; now: Date }) {
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

  if (layer.kind === "derive") {
    // A RENDERED derive layer is drawn as exactly what it becomes on the wire —
    // an image — so the canvas stops approximating the moment the real pixels
    // exist. That is not a nicety: the whole point of the rasterizer is styling
    // this browser cannot reproduce, so an approximation that never gave way to
    // the truth would be the last thing an operator saw before shipping.
    if (layer.url) {
      return <img data-slot="layer-derive" src={layer.url} alt="" style={{ ...box, objectFit: "contain" }} />;
    }
    const spec = layer.derive;
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
        <span
          data-slot="layer-derive-badge"
          className="absolute left-2 top-2 rounded bg-black/70 px-2 py-1 text-white"
          style={{ fontSize: 28 }}
        >
          NEEDS RENDER
        </span>
      </div>
    );
  }

  if (isContentKind(layer.kind)) {
    // A content-bearing layer with no bytes chosen yet is drawn as a labelled
    // outline rather than nothing: it is a placed, selectable, movable object
    // that simply is not finished, and an invisible one could not be found
    // again.
    if (!layer.url) {
      return (
        <div
          data-slot={`layer-${layer.kind}-empty`}
          aria-hidden="true"
          style={box}
          className="flex items-center justify-center border-4 border-dashed border-[color:var(--wv-border)] bg-[color:var(--wv-surface-2)]"
        >
          <KitIcon icon={layer.kind === "video" ? VideoOff : ImageOff} decorative className="size-16 text-muted-foreground" />
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
          src={layer.url}
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
        src={layer.url}
        alt=""
        style={{ ...box, objectFit: "contain" }}
      />
    );
  }

  // Every remaining kind is a Label on the player — text, clock, date,
  // countdown, weather, entity — differing only in where the string comes from.
  // One branch for all six is deliberate: they share the font, colour, alignment
  // and box behaviour exactly, and six near-identical JSX blocks is six places
  // for the preview to drift from the wall.
  const content = layer.kind === "text" ? (layer.text ?? "") : liveWidgetPreview(layer, now);
  return (
    <div
      data-slot={`layer-${layer.kind}`}
      style={{
        ...box,
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
}: {
  slide: CastSlide;
  scale: number;
  className?: string;
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
          <LayerView key={i} layer={layer} now={now} />
        ))}
      </div>
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
  /** A keyboard move, by a canvas-space delta. */
  onNudge: (index: number, dx: number, dy: number) => void;
  /** A keyboard resize, by a canvas-space delta on one grip. */
  onResizeBy: (index: number, handle: ResizeHandle, dx: number, dy: number) => void;
  onDelete: (index: number) => void;
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
  onNudge,
  onResizeBy,
  onDelete,
}: SlideCanvasProps) {
  const frameRef = useRef<HTMLDivElement>(null);
  const [scale, setScale] = useState(1);
  // The live scale for the pointer handlers, which are bound once: reading it
  // from a ref keeps them from being torn down and rebound on every resize.
  const scaleRef = useRef(scale);
  scaleRef.current = scale;
  const dragRef = useRef<DragState | null>(null);
  const [dragging, setDragging] = useState(false);

  // Fit the canvas to the column. `clientWidth` is 0 in a non-layout environment
  // (jsdom), where a scale of 0 would collapse the stage and make every pointer
  // delta infinite — so an unmeasurable frame falls back to 1:1 rather than to
  // nothing.
  useLayoutEffect(() => {
    const measure = () => {
      const width = frameRef.current?.clientWidth ?? 0;
      setScale(width > 0 ? Math.min(width / SLIDE_CANVAS_WIDTH, 1) : 1);
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
      dragRef.current = { index, handle, startX: e.clientX, startY: e.clientY, origin: layer };
      setDragging(true);
    },
    [slide, onSelect],
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
    };
    window.addEventListener("pointermove", onMove);
    window.addEventListener("pointerup", onUp);
    window.addEventListener("pointercancel", onUp);
    return () => {
      window.removeEventListener("pointermove", onMove);
      window.removeEventListener("pointerup", onUp);
      window.removeEventListener("pointercancel", onUp);
    };
  }, [onGeometry]);

  const onLayerKeyDown = useCallback(
    (e: ReactKeyboardEvent, index: number) => {
      const step = e.altKey ? NUDGE_FINE : NUDGE;
      const delta: Record<string, [number, number]> = {
        ArrowLeft: [-step, 0],
        ArrowRight: [step, 0],
        ArrowUp: [0, -step],
        ArrowDown: [0, step],
      };
      const d = delta[e.key];
      if (d) {
        e.preventDefault();
        // Shift turns the arrows into a resize from the bottom-right grip, so
        // the whole geometry is reachable without a pointer.
        if (e.shiftKey) onResizeBy(index, "se", d[0], d[1]);
        else onNudge(index, d[0], d[1]);
        return;
      }
      if (e.key === "Delete" || e.key === "Backspace") {
        e.preventDefault();
        onDelete(index);
      }
    },
    [onNudge, onResizeBy, onDelete],
  );

  const selected = selectedIndex === null ? undefined : slide.layers[selectedIndex];

  return (
    <div ref={frameRef} data-slot="slide-canvas" className="w-full min-w-0">
      <div
        className="relative mx-auto overflow-hidden rounded-panel border border-border bg-black"
        style={{ width: SLIDE_CANVAS_WIDTH * scale, height: SLIDE_CANVAS_HEIGHT * scale }}
        onPointerDown={() => onSelect(null)}
      >
        {/* The artwork, at 1:1 inside the scale transform. */}
        <SlideStage slide={slide} scale={scale} className="pointer-events-none absolute left-0 top-0" />

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
            onKeyDown={(e) => onLayerKeyDown(e, i)}
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
