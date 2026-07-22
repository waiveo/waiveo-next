import type { ReactNode } from "react";
import { cn } from "@/lib/utils";

/**
 * PageHeader — the platform's page identity. Every core page and every extension
 * page rendered through the kit gets its title here, set in the display face
 * (Bricolage Grotesque).
 *
 * The `hero` variant paints the `--grad-hero` band, but ONLY through the
 * `.wv-hero` utility, which lays the HORIZON scrim between the ramp and its
 * content. Text therefore sits on a scrim layer, never on the raw gradient —
 * the brand's text-never-on-raw-gradient rule expressed structurally, not as a
 * per-page habit.
 *
 * Placement matters as much as the scrim. `--grad-hero` is a 150deg ramp
 * (ember top-left → indigo tail bottom-right) and `--grad-hero-scrim` is
 * bottom-weighted (its 0.62 dark end at the bottom). Both protections live in
 * the LOWER band, which is exactly where HORIZON §4 places the hero headline
 * (warm-white holds 7.9:1 over the indigo lower-third). So the hero band is
 * taller than its content and bottom-anchors it (`justify-end` / `sm:items-end`
 * with a big-top/small-bottom pad): the ember sky sits ABOVE the copy, and the
 * headline lands where the scrim is darkest and the ramp is coolest — never
 * centered over the weakly-scrimmed ember stop.
 */
export interface PageHeaderProps {
  title: string;
  description?: string;
  /** Trailing actions (e.g. the one primary Button per view). */
  actions?: ReactNode;
  variant?: "default" | "hero";
  className?: string;
  children?: ReactNode;
}

export function PageHeader({
  title,
  description,
  actions,
  variant = "default",
  className,
  children,
}: PageHeaderProps) {
  const hero = variant === "hero";
  return (
    <header
      data-slot="page-header"
      data-variant={variant}
      className={cn(
        "flex flex-col gap-4 sm:flex-row sm:justify-between",
        hero
          ? // .wv-hero lays --grad-hero AND its scrim; the band is taller than its
            // content and bottom-anchors it so the headline lands in the protected
            // lower band (dark scrim end + indigo tail), never the ember top.
            "wv-hero rounded-panel min-h-[9.5rem] justify-end px-6 pt-12 pb-6 sm:min-h-[11rem] sm:items-end sm:px-8 sm:pt-14 sm:pb-7"
          : "border-b border-border pb-5 sm:items-center",
        className,
      )}
    >
      <div className="flex flex-col gap-1.5">
        <h1
          className={cn(
            "font-display text-[26px] leading-tight font-semibold tracking-[-0.01em]",
            hero && "text-[color:var(--grad-hero-fg)]",
          )}
        >
          {title}
        </h1>
        {description ? (
          <p
            className={cn(
              "max-w-2xl text-sm",
              hero ? "text-[color:var(--grad-hero-fg)]/85" : "text-muted-foreground",
            )}
          >
            {description}
          </p>
        ) : null}
        {children}
      </div>
      {actions ? <div className="flex shrink-0 items-center gap-2">{actions}</div> : null}
    </header>
  );
}
