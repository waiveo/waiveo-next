import type { ReactElement, ReactNode } from "react";
import {
  Tooltip as UITooltip,
  TooltipContent,
  TooltipTrigger,
} from "@/components/ui/tooltip";

/**
 * Tooltip — hover/focus help on a control that is already named.
 *
 * # A tip is a DESCRIPTION, never a name
 *
 * This is the rule the whole component is shaped around. The trigger must carry
 * its own accessible name before it is wrapped — an icon-only kit `Button` has
 * one because `ButtonProps` refuses a nameless icon button at the type level.
 * The tip is wired as `aria-describedby`, so a screen reader reads the name and
 * then the description; a tooltip used AS the name disappears the moment the
 * pointer leaves, which is a control with no name for anyone not hovering it.
 *
 * A tip that merely repeats the button's own `aria-label` is therefore fine and
 * is the common case: the sighted mouse user has no other way to learn what a
 * glyph means, and the screen-reader user hears the label either way.
 *
 * # Why this replaces `title=`
 *
 * The native `title` attribute is what the console reached for before this
 * existed, and it is a poor substitute: it never appears on a touch device, it
 * cannot be reached by keyboard at all, it is unstyled and unthemed, and its
 * ~1s browser delay is long enough that operators do not discover it. This
 * opens on hover AND on keyboard focus, is painted from the theme tokens (so it
 * is legible in Dusk and Daybreak alike), and dismisses on Escape.
 *
 * # Never put an action in one
 *
 * The content is text. A tooltip is not reachable by pointer without triggering
 * a race between the tip closing and the pointer arriving, so anything clickable
 * inside it is unusable — that is a popover, and `Combobox` is the kit's example
 * of one.
 */
export interface TooltipProps {
  /** The help text. Required: an empty tip is a control that flickers a blank box. */
  tip: ReactNode;
  /** Exactly one already-named element — the tip's trigger. */
  children: ReactElement;
  side?: "top" | "right" | "bottom" | "left";
  /** Hover dwell before the tip opens, in ms. Keyboard focus opens immediately. */
  delayDuration?: number;
}

export function Tooltip({ tip, children, side = "top", delayDuration = 200 }: TooltipProps) {
  return (
    <UITooltip delayDuration={delayDuration}>
      <TooltipTrigger asChild>{children}</TooltipTrigger>
      <TooltipContent side={side}>{tip}</TooltipContent>
    </UITooltip>
  );
}
