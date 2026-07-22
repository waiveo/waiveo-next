import type { ReactNode } from "react";
import { Inbox } from "lucide-react";
import type { LucideIcon } from "lucide-react";
import { cn } from "@/lib/utils";
import { KitIcon } from "./kit-icon";

/**
 * EmptyState — the one empty/zero-data idiom. Calm and plain per the brand voice
 * ("Nothing scheduled for this daypart yet. Add content to light it up."), on
 * the flat content layer. An optional action nudges the next step.
 */
export interface EmptyStateProps {
  title: string;
  description?: string;
  /** Decorative glyph above the title. Defaults to an inbox. */
  icon?: LucideIcon;
  /** A call to action (e.g. a primary Button). */
  action?: ReactNode;
  className?: string;
}

export function EmptyState({
  title,
  description,
  icon = Inbox,
  action,
  className,
}: EmptyStateProps) {
  return (
    <div
      data-slot="empty-state"
      className={cn(
        "flex flex-col items-center justify-center gap-3 px-6 py-12 text-center",
        className,
      )}
    >
      <div className="flex size-12 items-center justify-center rounded-panel bg-[color:var(--wv-surface-2)]">
        <KitIcon icon={icon} decorative className="size-6 text-muted-foreground" />
      </div>
      <div className="flex flex-col gap-1">
        <p className="font-display text-[17px] font-semibold">{title}</p>
        {description ? (
          <p className="max-w-sm text-sm text-muted-foreground">{description}</p>
        ) : null}
      </div>
      {action ? <div className="mt-1">{action}</div> : null}
    </div>
  );
}
