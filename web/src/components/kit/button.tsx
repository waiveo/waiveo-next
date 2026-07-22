import type { ComponentProps, ReactNode } from "react";
import type { LucideIcon } from "lucide-react";
import { Button as UIButton } from "@/components/ui/button";
import { KitIcon } from "./kit-icon";

/**
 * Button — THE action idiom, and the only sanctioned way for a page or the
 * ui-schema renderer to render one. It wraps the vendored shadcn base (the one
 * file the kit lets reach `@/components/ui`), so its variants ARE the vendored
 * variants: there is no second copy of the Tailwind utility strings to drift
 * from. The `default` variant is the ONE primary action per view — it carries
 * `--grad-accent` with AA-holding ink; every other variant stays flat.
 *
 * A button MUST have an accessible name. The type refuses one with neither
 * visible text `children` nor an explicit `aria-label` (the icon-only case), so
 * a screen reader never lands on a nameless control. An `icon` is decorative by
 * construction (rendered through KitIcon, aria-hidden); the label carries the
 * meaning. Buttons default to `type="button"` so one dropped into a form never
 * submits it by accident — pass `type="submit"` for the submitting action.
 */
type ButtonBase = Omit<ComponentProps<typeof UIButton>, "children" | "aria-label"> & {
  /** Optional leading glyph — decorative; the accessible name carries the meaning. */
  icon?: LucideIcon;
};

export type ButtonProps = ButtonBase &
  (
    | { children: ReactNode; "aria-label"?: string }
    | { "aria-label": string; children?: ReactNode }
  );

export function Button({ icon: Icon, children, className, asChild = false, type, ...rest }: ButtonProps) {
  // asChild renders the caller's own element (e.g. a router Link) through Radix
  // Slot, which requires exactly one child — so no icon injection and no native
  // `type` there; the slotted element owns both.
  if (asChild) {
    return (
      <UIButton asChild className={className} {...rest}>
        {children}
      </UIButton>
    );
  }
  return (
    <UIButton type={type ?? "button"} className={className} {...rest}>
      {Icon ? <KitIcon icon={Icon} decorative /> : null}
      {children}
    </UIButton>
  );
}
