import { useState } from "react";
import {
  ArrowDown,
  ArrowUp,
  CalendarDays,
  Clock,
  CloudSun,
  Image as ImageIcon,
  LayoutGrid,
  Square,
  Timer,
  ToggleLeft,
  Trash2,
  Type,
  type LucideIcon,
} from "lucide-react";
import { Button, EmptyState, KitIcon, Modal } from "@/components/kit";
import { WIDGET_LAYER_KINDS, type LayerKind, type SlideLayer, type WidgetLayerKind } from "@/api";
import { cn } from "@/lib/utils";
import { describeLayer } from "./slide-canvas";

/**
 * The insert toolbar and the layer list — the two halves of "what is on this
 * slide, and in what order".
 *
 * The list is presented TOP-FIRST while the model is bottom-first, and the
 * inversion is deliberate: the wire's array order is z-order with index 0
 * furthest back (`wire.Layer`, "array order = z-order"), but every design tool
 * an operator has used shows the frontmost layer at the top of the list. Showing
 * the raw array order would mean "move up" pushed a layer BEHIND the one above
 * it. The inversion is confined to this component — every index that crosses the
 * boundary in either direction is a model index.
 */

const KIND_ICON: Record<LayerKind, LucideIcon> = {
  text: Type,
  rect: Square,
  image: ImageIcon,
  clock: Clock,
  date: CalendarDays,
  countdown: Timer,
  weather: CloudSun,
  entity: ToggleLeft,
};

const KIND_LABEL: Record<LayerKind, string> = {
  text: "Text",
  rect: "Rectangle",
  image: "Image",
  clock: "Clock",
  date: "Date",
  countdown: "Countdown",
  weather: "Weather",
  entity: "Entity state",
};

/** The kinds the toolbar offers DIRECTLY: the static ones plus the clock, which
 * predates the widget concept and is the layer an operator reaches for most.
 * Everything else lives behind the widget picker — a flat row of eight buttons
 * is a worse door than four plus one, and the live kinds are the four that need
 * explaining before they are chosen. */
const DIRECT_KINDS: LayerKind[] = ["text", "rect", "image", "clock"];

/** What each live widget IS, in the terms that decide whether it is the one you
 * want. Every line says where the value comes from, because that is the whole
 * difference between these four and a text layer — and, for the two the box
 * resolves, the reason the canvas can only ever show a stand-in. */
const WIDGET_BLURB: Record<WidgetLayerKind, string> = {
  date: "Today's date, drawn by the screen from its own clock and re-rendered as the day turns.",
  countdown: "Time remaining until an instant you choose. The screen counts it down itself, so it keeps ticking even if the box is unreachable.",
  weather: "Current conditions for this site, fetched by the box and refreshed on every screen poll.",
  entity: "The live state of one device — on, off, playing — as the box last observed it.",
};

export interface InsertToolbarProps {
  onInsert: (kind: LayerKind) => void;
}

/**
 * The insert toolbar, and behind its last button the WIDGET PICKER.
 *
 * The picker exists because the four live kinds are not interchangeable with a
 * text layer in the way `rect` and `image` are: each one is a promise about
 * where a value comes from, and two of them (`weather`, `entity`) cannot be
 * previewed truthfully because the box resolves them at serve time. A modal is
 * the surface that has room to SAY that at the moment of choosing, which a
 * toolbar button does not — and picking the wrong one is otherwise invisible
 * until it is on a wall.
 */
export function InsertToolbar({ onInsert }: InsertToolbarProps) {
  const [pickerOpen, setPickerOpen] = useState(false);

  return (
    <>
      <div role="group" aria-label="Insert a layer" className="flex flex-wrap gap-2">
        {DIRECT_KINDS.map((kind) => (
          <Button
            key={kind}
            size="sm"
            variant="secondary"
            icon={KIND_ICON[kind]}
            className="wv-touch"
            onClick={() => onInsert(kind)}
          >
            {KIND_LABEL[kind]}
          </Button>
        ))}
        <Button
          size="sm"
          variant="secondary"
          icon={LayoutGrid}
          className="wv-touch"
          onClick={() => setPickerOpen(true)}
        >
          Add widget
        </Button>
      </div>

      <Modal
        title="Add a widget"
        description="A widget draws itself from live data instead of from text you type. Pick one and it lands on the slide ready to position."
        open={pickerOpen}
        onOpenChange={setPickerOpen}
        footer={
          <Button variant="secondary" onClick={() => setPickerOpen(false)}>
            Cancel
          </Button>
        }
      >
        <ul aria-label="Widgets" className="flex flex-col gap-2">
          {WIDGET_LAYER_KINDS.map((kind) => (
            <li key={kind}>
              <button
                type="button"
                data-slot="widget-choice"
                data-widget-kind={kind}
                // The whole card is the target (a two-line choice is easier to
                // hit than its heading), so the accessible NAME has to be set
                // explicitly: derived from the content it would be "Date Today's
                // date, drawn by the screen from its own clock…", which is what
                // a screen reader would announce as the choice and leaves no way
                // to refer to "Date" at all. The blurb is linked as the
                // DESCRIPTION instead, which is what it is.
                aria-label={KIND_LABEL[kind]}
                aria-describedby={`widget-choice-${kind}-desc`}
                // The modal closes on the SAME click that inserts, because the
                // inserted layer is selected and its kind-specific fields are
                // what the operator needs next — leaving the picker open would
                // cover the panel that asks for them.
                onClick={() => {
                  onInsert(kind);
                  setPickerOpen(false);
                }}
                className="flex w-full min-w-0 items-start gap-3 rounded-card border border-border p-3 text-left outline-none transition-colors hover:bg-accent focus-visible:ring-2 focus-visible:ring-ring"
              >
                <KitIcon icon={KIND_ICON[kind]} decorative className="mt-0.5 size-5 shrink-0" />
                <span className="min-w-0">
                  <span className="block text-sm font-medium">{KIND_LABEL[kind]}</span>
                  <span id={`widget-choice-${kind}-desc`} className="block text-[13px] text-muted-foreground">
                    {WIDGET_BLURB[kind]}
                  </span>
                </span>
              </button>
            </li>
          ))}
        </ul>
      </Modal>
    </>
  );
}

export interface LayerListProps {
  layers: SlideLayer[];
  selectedIndex: number | null;
  onSelect: (index: number) => void;
  /** Reorder in MODEL indices (0 = furthest back). */
  onReorder: (from: number, to: number) => void;
  onDelete: (index: number) => void;
}

export function LayerList({ layers, selectedIndex, onSelect, onReorder, onDelete }: LayerListProps) {
  if (layers.length === 0) {
    return (
      <EmptyState
        title="This slide is empty"
        description="Insert a text, rectangle, image or clock layer — or add a live widget — to start building it."
      />
    );
  }

  // Front-to-back for display; `index` below is always the MODEL index.
  const ordered = layers.map((layer, index) => ({ layer, index })).reverse();

  return (
    <ul aria-label="Layers" className="flex flex-col gap-1">
      {ordered.map(({ layer, index }) => {
        const selected = index === selectedIndex;
        const isFront = index === layers.length - 1;
        const isBack = index === 0;
        return (
          <li key={index} className="flex min-w-0 items-center gap-1">
            <button
              type="button"
              data-slot="layer-row"
              data-layer-index={index}
              aria-pressed={selected}
              onClick={() => onSelect(index)}
              className={cn(
                "wv-touch flex min-w-0 flex-1 items-center gap-2 rounded-input px-2 text-left text-[13px] outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring",
                selected ? "bg-[color:var(--wv-nav-active-bg)] text-[color:var(--wv-nav-active-fg)]" : "hover:bg-accent",
              )}
            >
              <KitIcon icon={KIND_ICON[layer.kind]} decorative className="size-4 shrink-0" />
              <span className="min-w-0 truncate">{describeLayer(layer)}</span>
            </button>
            <Button
              size="icon"
              variant="ghost"
              icon={ArrowUp}
              aria-label={`Bring ${describeLayer(layer)} forward`}
              disabled={isFront}
              onClick={() => onReorder(index, index + 1)}
            />
            <Button
              size="icon"
              variant="ghost"
              icon={ArrowDown}
              aria-label={`Send ${describeLayer(layer)} backward`}
              disabled={isBack}
              onClick={() => onReorder(index, index - 1)}
            />
            <Button
              size="icon"
              variant="ghost"
              icon={Trash2}
              aria-label={`Delete ${describeLayer(layer)}`}
              onClick={() => onDelete(index)}
            />
          </li>
        );
      })}
    </ul>
  );
}
