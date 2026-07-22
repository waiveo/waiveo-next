import type { LucideIcon } from "lucide-react";
import { cn } from "@/lib/utils";

/**
 * KitIcon — the single sanctioned way to render a lucide icon in the console.
 *
 * A11y is a build rule, not a convention: an icon is EITHER meaningful (a
 * required `label` names it for assistive tech) OR explicitly `decorative` (and
 * hidden). The type refuses an icon that is neither — you cannot ship an
 * unlabeled, non-decorative icon, so a screen reader never hits a nameless glyph.
 */
type KitIconCommon = {
  icon: LucideIcon;
  className?: string;
  size?: number | string;
  strokeWidth?: number | string;
};

export type KitIconProps =
  | (KitIconCommon & { label: string; decorative?: false })
  | (KitIconCommon & { decorative: true; label?: never });

export function KitIcon(props: KitIconProps) {
  const { icon: Icon, className, size, strokeWidth } = props;

  // Forward only the curated visual props (omit undefineds — the project runs
  // exactOptionalPropertyTypes, so an explicit `undefined` is not assignable).
  const visual: { size?: number | string; strokeWidth?: number | string } = {};
  if (size !== undefined) visual.size = size;
  if (strokeWidth !== undefined) visual.strokeWidth = strokeWidth;

  const base = cn("inline-block shrink-0", className);

  if (props.decorative) {
    return <Icon aria-hidden="true" focusable={false} className={base} {...visual} />;
  }
  return <Icon role="img" aria-label={props.label} className={base} {...visual} />;
}
