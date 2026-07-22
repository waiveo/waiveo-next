import type { ReactNode } from "react";
import * as DialogPrimitive from "@radix-ui/react-dialog";
import { X } from "lucide-react";
import { cn } from "@/lib/utils";
import { KitIcon } from "./kit-icon";

/**
 * NavDrawer — the kit's left slide-in navigation drawer, the mobile counterpart
 * to the app shell's locked-left rail. It is built on Radix Dialog, so it comes
 * with a modal overlay, a focus trap, Escape-to-close, and a `trigger` that
 * carries `aria-expanded`/`aria-controls` reflecting the open state — the whole
 * a11y contract a nav drawer owes, for free and consistent with the kit Modal.
 *
 * It differs from `Modal` only in geometry: the panel is pinned to the LEFT edge
 * full-height (not centered), because it stands in for the sidebar rail. It is
 * hidden at the desktop breakpoint (`wv-shell__drawer`) so it can never shadow
 * the permanent rail. `title` is a required accessible name (rendered as a
 * visually-hidden dialog title); pass the nav itself as `children`.
 */
export interface NavDrawerProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** The control that opens the drawer (e.g. a hamburger button). */
  trigger: ReactNode;
  /** Required accessible name for the drawer dialog. */
  title: string;
  children: ReactNode;
  className?: string;
}

export function NavDrawer({
  open,
  onOpenChange,
  trigger,
  title,
  children,
  className,
}: NavDrawerProps) {
  return (
    <DialogPrimitive.Root open={open} onOpenChange={onOpenChange}>
      <DialogPrimitive.Trigger asChild>{trigger}</DialogPrimitive.Trigger>
      <DialogPrimitive.Portal>
        <DialogPrimitive.Overlay
          data-slot="nav-drawer-overlay"
          className={cn(
            "wv-shell__drawer fixed inset-0 z-50 bg-[color:var(--wv-bg)]/70 backdrop-blur-sm",
            "animate-in fade-in-0 data-[state=closed]:animate-out data-[state=closed]:fade-out-0",
          )}
        />
        <DialogPrimitive.Content
          data-slot="nav-drawer"
          className={cn(
            "wv-shell__drawer fixed inset-y-0 left-0 z-50 flex w-[17rem] max-w-[85vw] flex-col gap-2",
            "border-r border-border bg-card p-4 text-card-foreground wv-elevation",
            "animate-in slide-in-from-left duration-200",
            "data-[state=closed]:animate-out data-[state=closed]:slide-out-to-left",
            className,
          )}
        >
          <DialogPrimitive.Title className="sr-only">{title}</DialogPrimitive.Title>
          {children}
          <DialogPrimitive.Close
            data-slot="nav-drawer-close"
            aria-label="Close navigation menu"
            className={cn(
              "wv-touch absolute top-3 right-3 inline-flex items-center justify-center rounded-input",
              "text-muted-foreground outline-none transition-colors hover:bg-accent hover:text-foreground",
              "focus-visible:ring-2 focus-visible:ring-ring",
            )}
          >
            <KitIcon icon={X} decorative className="size-4" />
          </DialogPrimitive.Close>
        </DialogPrimitive.Content>
      </DialogPrimitive.Portal>
    </DialogPrimitive.Root>
  );
}
