import {
  AlignCenterHorizontal,
  AlignCenterVertical,
  AlignEndHorizontal,
  AlignEndVertical,
  AlignStartHorizontal,
  AlignStartVertical,
  BringToFront,
  ChevronDown,
  ChevronUp,
  SendToBack,
  type LucideIcon,
} from "lucide-react";
import { KitIcon } from "@/components/kit";
import { cn } from "@/lib/utils";
import type { AlignTarget } from "./cast-model";

/**
 * Align and z-order for the selected layer — the compact glyph bar every design
 * tool puts at the top of its properties column.
 *
 * Both commands also exist in the Arrange menu and on the keyboard, and all
 * three routes call the same handlers. This one exists because a menu is where
 * a capability goes to be undiscoverable: an operator nudging a title into the
 * middle of a slide with the arrow keys will find a centre button sitting six
 * inches from the X field, and will not go looking under a menu for it.
 *
 * Alignment is against the CANVAS — see `alignPatch`, and the reason (a single
 * selection has nothing else to align to).
 *
 * There is no distribute, and no align-to-selection, because there is no
 * multi-select: `StudioState.layerIndex` is one index. That is a real gap, but
 * it is a gap in the EDIT MODEL, and a row of buttons that needed two layers
 * would be permanently disabled.
 */

const ALIGN_BUTTONS: Array<{ target: AlignTarget; icon: LucideIcon; label: string }> = [
  { target: "left", icon: AlignStartVertical, label: "Align left" },
  { target: "hcenter", icon: AlignCenterVertical, label: "Align horizontal centre" },
  { target: "right", icon: AlignEndVertical, label: "Align right" },
  { target: "top", icon: AlignStartHorizontal, label: "Align top" },
  { target: "vmiddle", icon: AlignCenterHorizontal, label: "Align vertical middle" },
  { target: "bottom", icon: AlignEndHorizontal, label: "Align bottom" },
];

export type OrderCommand = "front" | "forward" | "backward" | "back";

const ORDER_BUTTONS: Array<{ command: OrderCommand; icon: LucideIcon; label: string }> = [
  { command: "front", icon: BringToFront, label: "Bring to front" },
  { command: "forward", icon: ChevronUp, label: "Bring forward" },
  { command: "backward", icon: ChevronDown, label: "Send backward" },
  { command: "back", icon: SendToBack, label: "Send to back" },
];

export interface ArrangeControlsProps {
  onAlign: (target: AlignTarget) => void;
  onOrder: (command: OrderCommand) => void;
  /** False when the layer is already frontmost — both "forward" commands become
   * inert rather than pushing an undo step that changes nothing. */
  canRaise: boolean;
  canLower: boolean;
}

export function ArrangeControls({ onAlign, onOrder, canRaise, canLower }: ArrangeControlsProps) {
  return (
    <div className="flex flex-col gap-1.5">
      <span className="text-[11px] font-semibold uppercase tracking-[0.06em] text-muted-foreground">Arrange</span>
      <div role="group" aria-label="Align on the canvas" className="flex items-center gap-0.5">
        {ALIGN_BUTTONS.map((b) => (
          <GlyphButton key={b.target} icon={b.icon} label={b.label} onClick={() => onAlign(b.target)} />
        ))}
      </div>
      <div role="group" aria-label="Layer order" className="flex items-center gap-0.5">
        {ORDER_BUTTONS.map((b) => (
          <GlyphButton
            key={b.command}
            icon={b.icon}
            label={b.label}
            disabled={b.command === "front" || b.command === "forward" ? !canRaise : !canLower}
            onClick={() => onOrder(b.command)}
          />
        ))}
      </div>
    </div>
  );
}

function GlyphButton({
  icon,
  label,
  onClick,
  disabled = false,
}: {
  icon: LucideIcon;
  label: string;
  onClick: () => void;
  disabled?: boolean;
}) {
  return (
    <button
      type="button"
      aria-label={label}
      title={label}
      disabled={disabled}
      onClick={onClick}
      className={cn(
        "flex size-7 items-center justify-center rounded border border-border bg-[color:var(--wv-surface-2)]",
        "text-muted-foreground outline-none transition-colors",
        "hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring",
        "disabled:pointer-events-none disabled:opacity-40",
      )}
    >
      <KitIcon icon={icon} decorative className="size-3.5" />
    </button>
  );
}
