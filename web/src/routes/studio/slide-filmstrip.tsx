import { AlertTriangle, ChevronLeft, ChevronRight, Copy, Plus, Trash2 } from "lucide-react";
import { Button, KitIcon } from "@/components/kit";
import { SLIDE_CANVAS_WIDTH, type CastSlide, type SlideProblem } from "@/api";
import { cn } from "@/lib/utils";
import { SlideStage, type AssetUrls, type StaleRasters } from "./slide-canvas";

/**
 * The slide filmstrip — the cast's slides in play order, as live thumbnails.
 *
 * The thumbnails are the SAME renderer the canvas uses (SlideStage), just at a
 * smaller scale, so a thumbnail can never quietly disagree with the canvas about
 * what a slide looks like. A slide the projector would refuse wears a warning
 * badge here rather than only in the properties panel: the strip is where an
 * operator scans a whole cast, and a broken slide is invisible on the TV, so
 * this is the one place the problem is discoverable before it ships.
 *
 * Reorder is by explicit move-left / move-right buttons rather than drag. Drag
 * reordering needs a pointer, and the strip is the one part of the Studio that
 * must stay operable with a keyboard alone — a canvas can reasonably demand a
 * mouse; a list cannot.
 */

/** Thumbnail width in screen pixels; the scale follows from the fixed canvas. */
const THUMB_WIDTH = 176;
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
  /** The content origin's ref→url listing, passed straight to SlideStage: a
   * thumbnail that resolved bytes differently from the canvas would be a second
   * answer to the same question. */
  assetUrls?: AssetUrls;
  /** Which layers' drawn rasters are out of date, for the same reason: a
   * thumbnail is the same renderer, so it must not present a raster as current
   * that the canvas badges as stale. */
  staleRasters?: StaleRasters;
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
  assetUrls = null,
  staleRasters = null,
}: SlideFilmstripProps) {
  return (
    <section aria-label="Slides" className="flex min-w-0 flex-col gap-2">
      <div className="flex items-center justify-between gap-2">
        <h2 className="text-sm font-semibold">
          Slides <span className="text-muted-foreground">({slides.length})</span>
        </h2>
        <Button size="sm" variant="secondary" icon={Plus} onClick={onAdd}>
          Add slide
        </Button>
      </div>

      <ol className="wv-table-region flex list-none gap-3 overflow-x-auto pb-2">
        {slides.map((slide, i) => {
          const problems = problemsBySlide.get(i);
          const active = i === activeIndex;
          return (
            <li key={slide.id} className="flex shrink-0 flex-col gap-1">
              <button
                type="button"
                data-slot="filmstrip-slide"
                data-active={active}
                aria-current={active ? "true" : undefined}
                aria-label={`Slide ${i + 1}${problems ? " — needs attention" : ""}`}
                onClick={() => onSelect(i)}
                className={cn(
                  "relative overflow-hidden rounded-card border-2 outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring",
                  active ? "border-[color:var(--wv-accent-text)]" : "border-border hover:border-[color:var(--wv-accent-text)]/60",
                )}
              >
                <SlideStage slide={slide} scale={THUMB_SCALE} assetUrls={assetUrls} staleRasters={staleRasters} />
                <span className="absolute left-1 top-1 rounded-[6px] bg-[color:var(--wv-bg)]/80 px-1.5 text-[11px] font-semibold">
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
                  icon={ChevronLeft}
                  aria-label={`Move slide ${i + 1} earlier`}
                  disabled={i === 0}
                  onClick={() => onMove(i, i - 1)}
                />
                <Button
                  size="icon"
                  variant="ghost"
                  icon={Copy}
                  aria-label={`Duplicate slide ${i + 1}`}
                  onClick={() => onDuplicate(i)}
                />
                <Button
                  size="icon"
                  variant="ghost"
                  icon={Trash2}
                  aria-label={`Delete slide ${i + 1}`}
                  onClick={() => onDelete(i)}
                />
                <Button
                  size="icon"
                  variant="ghost"
                  icon={ChevronRight}
                  aria-label={`Move slide ${i + 1} later`}
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
