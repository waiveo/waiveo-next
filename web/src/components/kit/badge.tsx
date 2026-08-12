import type { ComponentProps, ReactNode } from "react";
import { Badge as UIBadge } from "@/components/ui/badge";

/**
 * Badge — the neutral count/label chip, and the counterpart to `StatusBadge`.
 *
 * The two are NOT interchangeable and the split is the point. `StatusBadge`
 * paints one of five *platform states* (ok/warn/error/off/pending) as flat fill
 * + icon + uppercase label, and green there means live. This one paints a piece
 * of *data* — an event schema, a duration, "Unsaved changes", a version — where
 * there is no state vocabulary to honour and an icon would be noise. Reaching
 * for `StatusBadge` to render a version number would put a green tick beside it
 * and claim something the value does not say.
 *
 * `tone` is deliberately not the `Status` union: `warn` and `err` here are
 * *emphasis*, not a device's health, and there is no `ok` tone at all — the
 * ok/green lane is reserved for live/ok (HORIZON), so a decorative chip may
 * never enter it. A gradient never fills a badge.
 *
 * `mono` is for a chip whose content is an identifier the operator is meant to
 * read literally (`state.v1`, a pack id, `12s`). It switches the face and drops
 * the display weight/tracking, matching how every other identifier in this
 * console is set.
 *
 * A badge is not interactive, so it carries no label requirement of its own —
 * its children ARE its text. It takes no `children`-less form for the same
 * reason: an empty chip is a coloured smudge with no meaning.
 */
export type BadgeTone = "neutral" | "accent" | "outline" | "warn" | "err";

const TONE: Record<BadgeTone, NonNullable<ComponentProps<typeof UIBadge>["variant"]>> = {
  neutral: "default",
  accent: "accent",
  outline: "outline",
  warn: "warning",
  err: "destructive",
};

export interface BadgeProps extends Omit<ComponentProps<typeof UIBadge>, "variant" | "face" | "children"> {
  /** Emphasis, never device health — see `StatusBadge` for that. Default `neutral`. */
  tone?: BadgeTone;
  /** Render an identifier-style chip in the mono face. */
  mono?: boolean;
  /** The visible text. A badge with nothing in it is a coloured smudge. */
  children: ReactNode;
}

export function Badge({ tone = "neutral", mono = false, children, ...rest }: BadgeProps) {
  return (
    <UIBadge variant={TONE[tone]} face={mono ? "mono" : "sans"} {...rest}>
      {children}
    </UIBadge>
  );
}
