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
        "flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between",
        hero
          ? // .wv-hero lays --grad-hero AND its scrim; content sits above the scrim.
            "wv-hero rounded-panel px-6 py-8 sm:px-8 sm:py-10"
          : "border-b border-border pb-5",
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
