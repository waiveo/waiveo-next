import type { ReactNode } from "react";
import type { LucideIcon } from "lucide-react";
import { Card } from "@/components/ui/card";
import { cn } from "@/lib/utils";
import { KitIcon } from "./kit-icon";

/**
 * StatCard — a single measured figure on the flat content layer. The value is
 * set in the mono face with tabular numerals (HORIZON: every measured value that
 * changes or aligns is JetBrains Mono, tabular), so a column of stat cards lines
 * up digit-for-digit. Never a gradient surface.
 */
export interface StatCardProps {
  label: string;
  value: ReactNode;
  /** Optional leading glyph — decorative; the label carries the meaning. */
  icon?: LucideIcon;
  /** A small caption under the value (e.g. a delta or a hint). */
  hint?: ReactNode;
  className?: string;
  /** Supplementary hover text (ui-schema/1 UIS-079) — see StatusBadge's own
   * note: it explains what is already visible, never carries it alone. */
  title?: string;
}

export function StatCard({ label, value, icon: Icon, hint, className, title }: StatCardProps) {
  return (
    <Card
      data-slot="stat-card"
      className={cn("min-w-0 gap-0 p-5", className)}
      {...(title === undefined ? {} : { title })}
    >
      <div className="flex min-w-0 items-center justify-between gap-3">
        <span className="min-w-0 text-[11px] font-semibold tracking-[0.10em] text-muted-foreground uppercase break-words">
          {label}
        </span>
        {Icon ? (
          <KitIcon icon={Icon} decorative className="size-4 shrink-0 text-muted-foreground" />
        ) : null}
      </div>
      <div className="mt-2 min-w-0 font-mono text-[32px] leading-none font-semibold break-words wv-tnum">
        {value}
      </div>
      {hint ? <div className="mt-2 text-sm text-muted-foreground">{hint}</div> : null}
    </Card>
  );
}
