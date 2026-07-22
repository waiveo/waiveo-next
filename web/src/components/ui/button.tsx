import * as React from "react";
import { Slot } from "@radix-ui/react-slot";
import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "@/lib/utils";

// Button — vendored shadcn base, restyled onto HORIZON. The `default` (primary)
// variant is the ONE primary action per view: it carries `--grad-accent` (the
// hotter Afterglow ramp) with `--grad-accent-fg` ink that holds AA across the
// whole gradient. Every other variant stays on the flat content layer.
const buttonVariants = cva(
  "inline-flex shrink-0 items-center justify-center gap-2 whitespace-nowrap rounded-btn text-[13px] font-medium tracking-[0.01em] outline-none transition-[background-color,background-image,border-color,box-shadow,color,filter] duration-150 disabled:pointer-events-none disabled:opacity-50 focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-background aria-invalid:ring-destructive/40 [&_svg]:pointer-events-none [&_svg]:shrink-0 [&_svg:not([class*='size-'])]:size-4",
  {
    variants: {
      variant: {
        default:
          "bg-[image:var(--grad-accent)] text-[color:var(--grad-accent-fg)] shadow-[0_1px_2px_rgba(12,10,24,0.34),0_6px_20px_rgba(188,42,136,0.20)] hover:brightness-[1.06] active:brightness-95",
        secondary:
          "border border-border bg-secondary text-secondary-foreground hover:bg-accent",
        outline:
          "border border-border bg-transparent text-foreground hover:bg-accent hover:text-accent-foreground",
        ghost:
          "bg-transparent text-foreground hover:bg-accent hover:text-accent-foreground",
        destructive:
          "bg-destructive text-destructive-foreground hover:brightness-95 focus-visible:ring-destructive",
        link:
          "bg-transparent text-[color:var(--wv-accent-text)] underline-offset-4 hover:underline",
      },
      size: {
        default: "h-9 px-4 py-2 has-[>svg]:px-3",
        sm: "h-8 gap-1.5 px-3 has-[>svg]:px-2.5",
        lg: "h-10 px-6 has-[>svg]:px-4",
        icon: "size-9",
      },
    },
    defaultVariants: { variant: "default", size: "default" },
  },
);

function Button({
  className,
  variant,
  size,
  asChild = false,
  ...props
}: React.ComponentProps<"button"> &
  VariantProps<typeof buttonVariants> & { asChild?: boolean }) {
  const Comp = asChild ? Slot : "button";
  return (
    <Comp
      data-slot="button"
      className={cn(buttonVariants({ variant, size }), className)}
      {...props}
    />
  );
}

export { Button, buttonVariants };
