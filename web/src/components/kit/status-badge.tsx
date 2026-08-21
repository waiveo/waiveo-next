import type { ReactNode } from "react";
import { CircleCheck, TriangleAlert, CircleAlert, Power, Clock } from "lucide-react";
import type { LucideIcon } from "lucide-react";
import { cn } from "@/lib/utils";
import { KitIcon } from "./kit-icon";

/**
 * StatusBadge — the one status chip for the whole console. Status reads as flat
 * fill + icon + label (legible in greyscale), never a gradient wash: the brand
 * forbids a gradient signalling state. Per HORIZON the ok/green family is
 * RESERVED for live/ok — only `ok` maps to green here; nothing decorative does.
 * The label is uppercased at wide tracking (the one broadcast-callout gesture).
 */
export type Status = "ok" | "warn" | "error" | "off" | "pending";

const STATUS: Record<Status, { icon: LucideIcon; className: string }> = {
  // The ok/green lane — reserved for live/ok, per the brand rule.
  ok: {
    icon: CircleCheck,
    className: "bg-[color:var(--wv-ok-bg)] text-[color:var(--wv-ok)]",
  },
  warn: {
    icon: TriangleAlert,
    className: "bg-[color:var(--wv-warn-bg)] text-[color:var(--wv-warn)]",
  },
  error: {
    icon: CircleAlert,
    className: "bg-[color:var(--wv-err-bg)] text-[color:var(--wv-err)]",
  },
  off: {
    icon: Power,
    className: "bg-[color:var(--wv-off-bg)] text-[color:var(--wv-muted)]",
  },
  pending: {
    icon: Clock,
    className: "bg-[color:var(--wv-surface-2)] text-[color:var(--wv-muted)]",
  },
};

export interface StatusBadgeProps {
  status: Status;
  /** The visible label — status is flat + icon + label, so the text is required. */
  children: ReactNode;
  className?: string;
  /** Supplementary hover text explaining what this status MEANS (ui-schema/1
   * UIS-079). Never the only carrier of information needed to operate the page:
   * hover text is unreachable by touch and inconsistently surfaced by assistive
   * technology, so it explains what is already visible. */
  title?: string;
}

export function StatusBadge({ status, children, className, title }: StatusBadgeProps) {
  const { icon, className: tone } = STATUS[status];
  return (
    <span
      data-slot="status-badge"
      data-status={status}
      {...(title === undefined ? {} : { title })}
      className={cn(
        "inline-flex w-fit items-center gap-1.5 rounded-pill px-2 py-0.5",
        "text-[11px] font-semibold tracking-[0.10em] uppercase whitespace-nowrap",
        tone,
        className,
      )}
    >
      <KitIcon icon={icon} decorative className="size-3" />
      {children}
    </span>
  );
}
