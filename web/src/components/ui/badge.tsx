import * as React from "react";
import { Slot } from "@radix-ui/react-slot";
import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "@/lib/utils";

// Badge — a flat pill/chip on the content layer. Status meaning (ok/warn/err/
// off/pending) is carried by the kit's StatusBadge with the flat status tokens;
// this base keeps the neutral shadcn variants, themed. A gradient never fills a
// badge (status is flat + icon + label, never a wash).
const badgeVariants = cva(
  "inline-flex w-fit shrink-0 items-center justify-center gap-1 overflow-hidden rounded-pill border px-2 py-0.5 text-[11px] font-semibold tracking-[0.04em] whitespace-nowrap [&>svg]:size-3 [&>svg]:pointer-events-none focus-visible:ring-2 focus-visible:ring-ring transition-[color,background-color,border-color]",
  {
    variants: {
      variant: {
        default: "border-transparent bg-secondary text-secondary-foreground",
        accent: "border-transparent bg-[color:var(--wv-accent)] text-[color:var(--wv-accent-fg)]",
        outline: "border-border text-foreground",
        warning: "border-transparent bg-[color:var(--wv-warn-bg)] text-[color:var(--wv-warn)]",
        destructive:
          "border-transparent bg-[color:var(--wv-err-bg)] text-[color:var(--wv-err)]",
      },
      // A chip whose content is an IDENTIFIER — an event schema, a pack id, a
      // duration — renders in the mono face at normal weight and tracking, the
      // same treatment identifiers get everywhere else in this console. The
      // display face's semibold + wide tracking is a callout gesture and reads
      // as shouting on a value that is meant to be read literally.
      face: {
        sans: "",
        mono: "font-mono font-normal tracking-normal",
      },
    },
    defaultVariants: { variant: "default", face: "sans" },
  },
);

function Badge({
  className,
  variant,
  face,
  asChild = false,
  ...props
}: React.ComponentProps<"span"> &
  VariantProps<typeof badgeVariants> & { asChild?: boolean }) {
  const Comp = asChild ? Slot : "span";
  return (
    <Comp
      data-slot="badge"
      className={cn(badgeVariants({ variant, face }), className)}
      {...props}
    />
  );
}

export { Badge, badgeVariants };
