import * as React from "react";
import * as SwitchPrimitive from "@radix-ui/react-switch";
import { cn } from "@/lib/utils";

// Switch — the "on" state carries `--grad-accent` (the primary toggle "on" fill,
// a sanctioned gradient surface). Off is a flat neutral track.
function Switch({ className, ...props }: React.ComponentProps<typeof SwitchPrimitive.Root>) {
  return (
    <SwitchPrimitive.Root
      data-slot="switch"
      className={cn(
        "peer inline-flex h-5 w-9 shrink-0 items-center rounded-pill border border-transparent shadow-xs outline-none transition-[background-color,background-image,box-shadow]",
        "focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-background",
        "disabled:cursor-not-allowed disabled:opacity-50",
        "data-[state=unchecked]:bg-[color:var(--wv-off-bg)] data-[state=checked]:bg-[image:var(--grad-accent)]",
        className,
      )}
      {...props}
    >
      <SwitchPrimitive.Thumb
        data-slot="switch-thumb"
        className={cn(
          "pointer-events-none block size-4 rounded-pill bg-white shadow-sm ring-0 transition-transform data-[state=unchecked]:translate-x-0.5 data-[state=checked]:translate-x-[18px]",
        )}
      />
    </SwitchPrimitive.Root>
  );
}

export { Switch };
