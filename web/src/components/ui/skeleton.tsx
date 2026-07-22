import * as React from "react";
import { cn } from "@/lib/utils";

// Skeleton — a flat, animated placeholder on the content layer (never a
// gradient shimmer; loading is not a decorative moment).
function Skeleton({ className, ...props }: React.ComponentProps<"div">) {
  return (
    <div
      data-slot="skeleton"
      className={cn("animate-pulse rounded-input bg-[color:var(--wv-surface-2)]", className)}
      {...props}
    />
  );
}

export { Skeleton };
