import { ArrowDown, ArrowUp, LayoutGrid, Sparkles, Trash2, type LucideIcon } from "lucide-react";
import { Button, EmptyState, KitIcon, Modal } from "@/components/kit";
import { DERIVE_KINDS, WIDGET_LAYER_KINDS, type DeriveKind, type LayerKind, type SlideLayer } from "@/api";
import { cn } from "@/lib/utils";
import { describeLayer } from "./slide-canvas";
import { ToolButton } from "./studio-chrome";
// The glyphs, names and blurbs are the SHARED catalog — `/widgets` browses the
// same descriptions this toolbar inserts from, so the two can never disagree
// about what a Weather layer is or where its value comes from.
import {
  DERIVE_BLURB,
  DERIVE_LABEL,
  KIND_ICON,
  KIND_LABEL,
  WIDGET_BLURB,
} from "./layer-catalog";

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

/** The kinds the toolbar offers DIRECTLY: the static ones plus the clock, which
 * predates the widget concept and is the layer an operator reaches for most.
 * Everything else lives behind the widget picker — a flat row of nine buttons
 * is a worse door than five plus one, and the live kinds are the four that need
 * explaining before they are chosen.
 *
 * `video` belongs here rather than behind the picker for the same reason
 * `image` does: it is not a live widget, it is a piece of content chosen from
 * the library, and it is the second half of a pair an operator thinks of as one
 * choice ("put a file on the slide"). Its absence from this row was the whole of
 * the defect — the kind was on the wire, in the projector and in the player, and
 * unreachable from the only surface that authors one.
 *
 * The two INTERACTIVE kinds are here too, and deliberately not behind the widget
 * picker: that picker's whole premise is "this draws itself from live data
 * instead of from what you type", and neither of these does. A button and a menu
 * are objects you place, exactly like a rectangle — what is new about them is
 * what they DO, which is the panel's job to explain, not the picker's. */
const DIRECT_KINDS: LayerKind[] = ["text", "rect", "image", "video", "clock", "ping", "nav"];

export interface InsertToolbarProps {
  onInsert: (kind: LayerKind) => void;
  /** Open the widget chooser. Held by the HOST rather than by this component,
   * because the Insert MENU opens the same two dialogs and a picker whose only
   * switch was a toolbar button could not be reached from there. */
  onOpenWidgetPicker: () => void;
  onOpenDerivePicker: () => void;
}

/**
 * The tool rail's insert group. Glyph over caption, the way legacy's `.tool-btn`
 * draws one and the way every editor's toolbox does — an operator reaching for
 * "the text tool" is looking for a picture, and the caption is the confirmation
 * rather than the target.
 */
export function InsertToolbar({ onInsert, onOpenWidgetPicker, onOpenDerivePicker }: InsertToolbarProps) {
  return (
    // `min-w-0` + `overflow-x-auto`: at a narrow viewport the nine tools are
    // wider than the rail, and a group that could not shrink pushed the zoom
    // cluster off the end of the bar instead of scrolling under it.
    <div
      role="group"
      aria-label="Insert a layer"
      className="flex min-w-0 items-center gap-0.5 overflow-x-auto"
    >
      {DIRECT_KINDS.map((kind) => (
        <ToolButton key={kind} icon={KIND_ICON[kind]} label={KIND_LABEL[kind]} onClick={() => onInsert(kind)} />
      ))}
      {/* The two pickers keep their full names as their ACCESSIBLE names — the
          caption alone ("Widget") would not say that a press opens a chooser
          rather than dropping something on the slide. */}
      <ToolButton icon={LayoutGrid} label="Widget" name="Add widget" onClick={onOpenWidgetPicker} />
      <ToolButton icon={Sparkles} label="Raster" name="Add rasterized" onClick={onOpenDerivePicker} />
    </div>
  );
}

/**
 * The RASTERIZED chooser.
 *
 * A modal rather than three more toolbar buttons, because each of these is a
 * layer that will not draw until a tool the operator has to run themselves has
 * run — and a surface with room to say so at the moment of choosing is the
 * difference between that being understood and being discovered on a wall.
 */
export function DerivePickerModal({
  open,
  onOpenChange,
  onInsertDerive,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** Insert a rasterized layer of one derive kind. Separate from `onInsert`
   * because `derive` is not one thing: the spec decides what is drawn. */
  onInsertDerive: (kind: DeriveKind) => void;
}) {
  return (
    <>
      <Modal
        title="Add a rasterized layer"
        description="These are the things a screen cannot draw by itself — a QR code, a gradient, a drop shadow, a font the screen does not ship. They are rendered to a picture by waiveo-derive, which runs on your machine and not on the box, and the layer stays marked NEEDS RENDER on the canvas until you have run it."
        open={open}
        onOpenChange={onOpenChange}
        footer={
          <Button variant="secondary" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
        }
      >
        <ul aria-label="Rasterized layers" className="flex flex-col gap-2">
          {DERIVE_KINDS.map((kind) => (
            <li key={kind}>
              <button
                type="button"
                className="wv-touch flex w-full items-start gap-3 rounded-md border border-[color:var(--wv-border)] bg-[color:var(--wv-surface-2)] p-3 text-left hover:border-[color:var(--wv-accent)]"
                onClick={() => {
                  onInsertDerive(kind);
                  onOpenChange(false);
                }}
              >
                <KitIcon icon={Sparkles} decorative className="mt-0.5 size-5 text-[color:var(--wv-accent)]" />
                <span className="flex flex-col gap-1">
                  <span className="font-medium">{DERIVE_LABEL[kind]}</span>
                  <span className="text-sm text-muted-foreground">{DERIVE_BLURB[kind]}</span>
                </span>
              </button>
            </li>
          ))}
        </ul>
      </Modal>
    </>
  );
}

/**
 * The WIDGET chooser.
 *
 * It exists because the four live kinds are not interchangeable with a text
 * layer in the way `rect` and `image` are: each one is a promise about where a
 * value comes from, and two of them (`weather`, `entity`) cannot be previewed
 * truthfully because the box resolves them at serve time. A modal is the surface
 * that has room to SAY that at the moment of choosing, which a toolbar button
 * does not — and picking the wrong one is otherwise invisible until it is on a
 * wall.
 */
export function WidgetPickerModal({
  open,
  onOpenChange,
  onInsert,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onInsert: (kind: LayerKind) => void;
}) {
  return (
    <>
      <Modal
        title="Add a widget"
        description="A widget draws itself from live data instead of from text you type. Pick one and it lands on the slide ready to position."
        open={open}
        onOpenChange={onOpenChange}
        footer={
          <Button variant="secondary" onClick={() => onOpenChange(false)}>
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
                  onOpenChange(false);
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

/**
 * The layer stack, Photoshop-dense: one 26px row per layer, front at the top,
 * its actions revealed on hover or focus.
 *
 * The density is the point of the redesign and not a style preference — a slide
 * carries a dozen layers and the panel it lives in shares a column with History
 * and Properties, so rows the height of a page button meant scrolling to find
 * out what was on the slide. Revealing the three per-row actions on hover is
 * legacy's `.layer-actions { opacity: 0 }`, and they stay in the tab order and
 * appear on focus, so the reveal costs a keyboard operator nothing.
 *
 * What is deliberately NOT here: a lock toggle, a visibility eye and an editable
 * layer name, all three of which legacy has. None of the three is on the wire —
 * `SlideLayer` is a closed schema of seventeen keys and carries no `locked`,
 * `hidden` or `name` — so each would be a control whose state vanished on
 * reload. The row's title is DERIVED from the layer instead (`describeLayer`),
 * which is why it reads "Text — Welcome" rather than "Layer 4".
 */
export function LayerList({ layers, selectedIndex, onSelect, onReorder, onDelete }: LayerListProps) {
  if (layers.length === 0) {
    return (
      <EmptyState
        title="This slide is empty"
        description="Insert a text, rectangle, image, video or clock layer — or add a live widget — to start building it."
      />
    );
  }

  // Front-to-back for display; `index` below is always the MODEL index.
  const ordered = layers.map((layer, index) => ({ layer, index })).reverse();

  return (
    <ul aria-label="Layers" className="flex flex-col gap-px">
      {ordered.map(({ layer, index }) => {
        const selected = index === selectedIndex;
        const isFront = index === layers.length - 1;
        const isBack = index === 0;
        const described = describeLayer(layer);
        return (
          <li
            key={index}
            className={cn(
              "group flex min-w-0 items-center gap-1 rounded border pl-1 pr-0.5 transition-colors",
              selected
                ? "border-[color:var(--wv-accent)] bg-[color:var(--wv-nav-active-bg)]"
                : "border-transparent hover:border-border hover:bg-accent",
            )}
          >
            <button
              type="button"
              data-slot="layer-row"
              data-layer-index={index}
              aria-pressed={selected}
              onClick={() => onSelect(index)}
              className={cn(
                "flex h-[26px] min-w-0 flex-1 items-center gap-1.5 rounded-sm text-left text-[12px] outline-none",
                "focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring",
                selected ? "text-[color:var(--wv-nav-active-fg)]" : null,
              )}
            >
              <KitIcon icon={KIND_ICON[layer.kind]} decorative className="size-3.5 shrink-0" />
              <span className="min-w-0 truncate">{described}</span>
            </button>
            <span className="flex shrink-0 items-center gap-px opacity-0 transition-opacity group-hover:opacity-100 group-focus-within:opacity-100">
              <LayerAction
                icon={ArrowUp}
                label={`Bring ${described} forward`}
                disabled={isFront}
                onClick={() => onReorder(index, index + 1)}
              />
              <LayerAction
                icon={ArrowDown}
                label={`Send ${described} backward`}
                disabled={isBack}
                onClick={() => onReorder(index, index - 1)}
              />
              <LayerAction icon={Trash2} label={`Delete ${described}`} danger onClick={() => onDelete(index)} />
            </span>
          </li>
        );
      })}
    </ul>
  );
}

/** One of the three per-row glyph buttons. 18px, as legacy's `.layer-action-btn`
 * is — a page-sized icon button would be taller than the row it sits in. */
function LayerAction({
  icon,
  label,
  onClick,
  disabled = false,
  danger = false,
}: {
  icon: LucideIcon;
  label: string;
  onClick: () => void;
  disabled?: boolean;
  danger?: boolean;
}) {
  return (
    <button
      type="button"
      aria-label={label}
      title={label}
      disabled={disabled}
      onClick={onClick}
      className={cn(
        "flex size-[18px] items-center justify-center rounded-sm text-muted-foreground outline-none transition-colors",
        "focus-visible:ring-2 focus-visible:ring-ring disabled:pointer-events-none disabled:opacity-30",
        danger
          ? "hover:bg-[color:var(--wv-err-bg)] hover:text-[color:var(--wv-err)]"
          : "hover:bg-[color:var(--wv-surface)] hover:text-foreground",
      )}
    >
      <KitIcon icon={icon} decorative className="size-3" />
    </button>
  );
}
