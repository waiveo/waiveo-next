import { ArrowDown, ArrowUp, Clock, Image as ImageIcon, Square, Trash2, Type, type LucideIcon } from "lucide-react";
import { Button, EmptyState, KitIcon } from "@/components/kit";
import { LAYER_KINDS, type LayerKind, type SlideLayer } from "@/api";
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
};

const KIND_LABEL: Record<LayerKind, string> = {
  text: "Text",
  rect: "Rectangle",
  image: "Image",
  clock: "Clock",
};

export function InsertToolbar({ onInsert }: { onInsert: (kind: LayerKind) => void }) {
  return (
    <div role="group" aria-label="Insert a layer" className="flex flex-wrap gap-2">
      {LAYER_KINDS.map((kind) => (
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
    </div>
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
        description="Insert a text, rectangle, image or clock layer to start building it."
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
