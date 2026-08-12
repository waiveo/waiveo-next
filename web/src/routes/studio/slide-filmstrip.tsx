import { AlertTriangle, ChevronDown, ChevronUp, Copy, Plus, Trash2 } from "lucide-react";
import { Badge, Button, KitIcon } from "@/components/kit";
import { SLIDE_CANVAS_WIDTH, type CastSlide, type SlideProblem } from "@/api";
import { cn } from "@/lib/utils";
import { SlideStage, type AssetUrls, type StaleRasters } from "./slide-canvas";

/**
 * The slide rail — the cast's slides in play order, as live thumbnails, docked
 * down the left edge of the editor.
 *
 * The thumbnails are the SAME renderer the canvas uses (SlideStage), just at a
 * smaller scale, so a thumbnail can never quietly disagree with the canvas about
 * what a slide looks like. A slide the projector would refuse wears a warning
 * badge here rather than only in the properties panel: the rail is where an
 * operator scans a whole cast, and a broken slide is invisible on the TV, so
 * this is the one place the problem is discoverable before it ships.
 *
 * ── Why it runs down the side ───────────────────────────────────────────────
 * It was a horizontal strip above the canvas, which is what a page with a
 * scrolling content column can hold. A full-screen editor has the opposite
 * shape: height is the scarce axis for the canvas (a 16:9 stage in a 16:9
 * viewport is height-bound, always), and width is what a 1920-wide artwork wants
 * least of. Legacy runs its slides down a 200px left rail for exactly that
 * reason, and a vertical list also scrolls the way a deck of forty slides
 * actually gets read.
 *
 * Reorder is by explicit move-earlier / move-later buttons rather than drag.
 * Drag reordering needs a pointer, and the rail is the one part of the Studio
 * that must stay operable with a keyboard alone — a canvas can reasonably demand
 * a mouse; a list cannot. The buttons are labelled "earlier"/"later" rather than
 * "up"/"down" because what they change is PLAY ORDER, and that survives the
 * strip being turned on its side.
 */

/** Thumbnail width in screen pixels; the scale follows from the fixed canvas. */
const THUMB_WIDTH = 152;
const THUMB_SCALE = THUMB_WIDTH / SLIDE_CANVAS_WIDTH;

export interface SlideFilmstripProps {
  slides: CastSlide[];
  activeIndex: number;
  problemsBySlide: Map<number, SlideProblem[]>;
  onSelect: (index: number) => void;
  onAdd: () => void;
  onDuplicate: (index: number) => void;
  onDelete: (index: number) => void;
  onMove: (from: number, to: number) => void;
  /** The cast-wide fallback dwell, so a slide with no duration of its own can
   * say what it will actually hold for instead of just "not set". */
  defaultDurationMs?: number | null;
  /** The content origin's ref→url listing, passed straight to SlideStage: a
   * thumbnail that resolved bytes differently from the canvas would be a second
   * answer to the same question. */
  assetUrls?: AssetUrls;
  /** Which layers' drawn rasters are out of date, for the same reason: a
   * thumbnail is the same renderer, so it must not present a raster as current
   * that the canvas badges as stale. */
  staleRasters?: StaleRasters;
}

/** The dwell a slide will actually hold for, as a badge string.
 *
 * The resolution order is the wire's (DAT-042): the slide's own `duration_ms`,
 * then the cast default, then the playlist's and finally the player's — and the
 * last two are not knowable from inside the editor. So a slide relying on them
 * says "auto" rather than inventing a number, and a slide covered by the cast
 * default shows that number marked as inherited. Printing a bare "10s" for both
 * cases is the small lie that makes an operator wonder why changing the cast
 * default did nothing visible. */
export function dwellBadge(slide: CastSlide, defaultDurationMs: number | null | undefined): {
  text: string;
  inherited: boolean;
} {
  if (slide.duration_ms !== undefined) {
    return { text: `${Math.round(slide.duration_ms / 1000)}s`, inherited: false };
  }
  if (defaultDurationMs !== null && defaultDurationMs !== undefined) {
    return { text: `${Math.round(defaultDurationMs / 1000)}s`, inherited: true };
  }
  return { text: "auto", inherited: true };
}

export function SlideFilmstrip({
  slides,
  activeIndex,
  problemsBySlide,
  onSelect,
  onAdd,
  onDuplicate,
  onDelete,
  onMove,
  defaultDurationMs = null,
  assetUrls = null,
  staleRasters = null,
}: SlideFilmstripProps) {
  return (
    <section aria-label="Slides" className="flex min-h-0 min-w-0 flex-1 flex-col">
      <div className="flex h-8 shrink-0 items-center gap-1 border-b border-border px-2">
        <span className="flex-1 truncate text-[11px] font-semibold uppercase tracking-[0.06em]">
          Slides <span className="font-normal text-muted-foreground">({slides.length})</span>
        </span>
        <Button size="icon" variant="ghost" icon={Plus} aria-label="Add slide" className="size-6" onClick={onAdd} />
      </div>

      <ol className="flex min-h-0 flex-1 list-none flex-col gap-2 overflow-y-auto overflow-x-hidden p-2">
        {slides.map((slide, i) => {
          const problems = problemsBySlide.get(i);
          const active = i === activeIndex;
          const dwell = dwellBadge(slide, defaultDurationMs);
          return (
            <li
              key={slide.id}
              data-slot="slide-card"
              data-active={active}
              className={cn(
                "group flex shrink-0 flex-col gap-1 rounded-[10px] border-2 p-1.5 transition-colors",
                active
                  ? "border-[color:var(--wv-accent)] bg-[color:var(--wv-nav-active-bg)]"
                  : "border-transparent bg-[color:var(--wv-surface-2)] hover:bg-accent",
              )}
            >
              {/* Legacy's top bar: what the slide will hold for on the left, the
                  destructive action on the right, revealed on hover. */}
              <div className="flex items-center justify-between gap-1">
                {/* How long this slide holds. A duration is a value, not a
                    health state, so it is a neutral/warn chip rather than a
                    StatusBadge — and `mono` because it is read literally.
                    The `title` STAYS a native one: a kit Tooltip needs a
                    focusable trigger, and this chip is not interactive, so
                    wrapping it would add a tab stop per slide to a strip a
                    keyboard user already crosses four controls at a time. */}
                <Badge
                  data-slot="slide-dwell"
                  mono
                  tone={dwell.inherited ? "neutral" : "warn"}
                  title={
                    dwell.inherited
                      ? "Inherited — this slide sets no duration of its own"
                      : "This slide's own duration"
                  }
                  className="px-1.5 py-px text-[10px] tabular-nums"
                >
                  {dwell.text}
                </Badge>
                <Button
                  size="icon"
                  variant="ghost"
                  icon={Trash2}
                  aria-label={`Delete slide ${i + 1}`}
                  className="size-5 opacity-0 transition-opacity group-hover:opacity-100 focus-visible:opacity-100"
                  onClick={() => onDelete(i)}
                />
              </div>

              <button
                type="button"
                data-slot="filmstrip-slide"
                data-active={active}
                aria-current={active ? "true" : undefined}
                aria-label={`Slide ${i + 1}${problems ? " — needs attention" : ""}`}
                onClick={() => onSelect(i)}
                className={cn(
                  "relative overflow-hidden rounded-[6px] outline-none ring-offset-1 ring-offset-[color:var(--wv-surface)]",
                  "focus-visible:ring-2 focus-visible:ring-ring",
                )}
              >
                <SlideStage slide={slide} scale={THUMB_SCALE} assetUrls={assetUrls} staleRasters={staleRasters} />
                <span className="absolute left-1 top-1 rounded-[5px] bg-[color:var(--wv-bg)]/80 px-1.5 text-[11px] font-semibold tabular-nums">
                  {i + 1}
                </span>
                {problems ? (
                  <span
                    data-slot="slide-problem-badge"
                    title={problems[0]?.message}
                    className="absolute right-1 top-1 flex size-5 items-center justify-center rounded-full bg-[color:var(--wv-warn-bg)]"
                  >
                    <KitIcon icon={AlertTriangle} decorative className="size-3 text-[color:var(--wv-warn)]" />
                  </span>
                ) : null}
              </button>

              <div className="flex items-center justify-center gap-0.5">
                <Button
                  size="icon"
                  variant="ghost"
                  icon={ChevronUp}
                  aria-label={`Move slide ${i + 1} earlier`}
                  className="size-6"
                  disabled={i === 0}
                  onClick={() => onMove(i, i - 1)}
                />
                <Button
                  size="icon"
                  variant="ghost"
                  icon={Copy}
                  aria-label={`Duplicate slide ${i + 1}`}
                  className="size-6"
                  onClick={() => onDuplicate(i)}
                />
                <Button
                  size="icon"
                  variant="ghost"
                  icon={ChevronDown}
                  aria-label={`Move slide ${i + 1} later`}
                  className="size-6"
                  disabled={i === slides.length - 1}
                  onClick={() => onMove(i, i + 1)}
                />
              </div>
            </li>
          );
        })}
      </ol>
    </section>
  );
}
