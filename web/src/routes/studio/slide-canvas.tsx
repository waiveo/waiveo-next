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
import { ImageOff, VideoOff } from "lucide-react";
import { KitIcon } from "@/components/kit";
import {
  SLIDE_CANVAS_HEIGHT,
  SLIDE_CANVAS_WIDTH,
  isContentKind,
  isInteractiveLayer,
  navItemRects,
  type CastSlide,
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
    case "ping":
      // The EVENT NAME, not the label: two buttons on one slide are told apart
      // by what they fire, and that is also the string an automation author has
      // to match, so the layer list is where they should be able to read it off.
      return layer.ping_name ? `Button — fires "${layer.ping_name}"` : "Button — (no event name)";
    case "nav":
      return layer.items && layer.items.length > 0
        ? `Menu — ${layer.items.length} item${layer.items.length === 1 ? "" : "s"}`
        : "Menu — (no items yet)";
  }
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

/**
 * `asset_ref` → a fetch URL for those bytes, as of RIGHT NOW: the content
 * listing (`GET /content`), keyed for lookup.
 *
 * A content-bearing layer's URL is not part of the layer. It is minted by the
 * server per response and it EXPIRES (`internal/feeder/contenturl`), so the only
 * correct time to know one is the moment something is about to fetch it — which
 * is why the server refuses to store an authored one at all
 * (`internal/app/store/derivedmembers.go`) and why the canvas resolves through
 * this rather than reading `layer.url`.
 *
 * Reading `layer.url` is exactly what broke: the picker patched the listing's
 * url into the layer, the save persisted it, and reopening the cast a day later
 * drew a canvas of broken images against a url the origin had begun refusing.
 */
export type AssetUrls = ReadonlyMap<string, string>;

/** The fetch URL for a content-bearing layer's bytes, or undefined when there
 * are none to draw — no asset chosen yet, or one the library no longer holds
 * (an asset the retention sweep reclaimed). Both are undrawable, and both are
 * shown as the same "nothing here" outline. */
function assetUrlFor(layer: SlideLayer, assetUrls: AssetUrls | undefined): string | undefined {
  if (!layer.asset_ref) return undefined;
  return assetUrls?.get(layer.asset_ref);
}

/** One layer, drawn the way the player draws it. Canvas-space coordinates. */
export function LayerView({
  layer,
  now,
  assetUrls,
}: {
  layer: SlideLayer;
  now: Date;
  /** The content library, for a content-bearing layer's preview. See AssetUrls. */
  assetUrls?: AssetUrls | undefined;
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

  if (isContentKind(layer.kind)) {
    // Resolved from the content library, never read off the layer: the layer
    // holds the content-addressed `asset_ref`, and the url for those bytes is
    // minted per response and expires. See AssetUrls.
    const src = assetUrlFor(layer, assetUrls);
    // A content-bearing layer with no bytes to draw is a labelled outline rather
    // than nothing: it is a placed, selectable, movable object that simply is
    // not finished, and an invisible one could not be found again.
    if (!src) {
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
          src={src}
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
        src={src}
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
  assetUrls,
}: {
  slide: CastSlide;
  scale: number;
  className?: string;
  /** The content library, for content-bearing layers' previews. See AssetUrls. */
  assetUrls?: AssetUrls | undefined;
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
          <LayerView key={i} layer={layer} now={now} assetUrls={assetUrls} />
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
  /** The content library, for content-bearing layers' previews. See AssetUrls. */
  assetUrls?: AssetUrls | undefined;
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
  assetUrls,
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
        <SlideStage slide={slide} scale={scale} assetUrls={assetUrls} className="pointer-events-none absolute left-0 top-0" />

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
