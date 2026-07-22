import type { ReactNode } from "react";
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

/**
 * Modal — THE dialog idiom. One layout for the whole console: title / body /
 * footer, a handful of sizes, and a confirm variant. Built on Radix Dialog, so
 * focus is trapped, Escape closes, and the overlay is inert-safe out of the box.
 *
 * The `title` prop is REQUIRED at the type level — a modal without an accessible
 * name is a defect the compiler refuses (Radix also warns at runtime). Body copy
 * renders on the flat content layer; a modal is never a gradient surface.
 */
export type ModalSize = "sm" | "md" | "lg" | "xl";

const SIZE: Record<ModalSize, string> = {
  sm: "sm:max-w-sm",
  md: "sm:max-w-lg",
  lg: "sm:max-w-2xl",
  xl: "sm:max-w-4xl",
};

export interface ModalProps {
  /** Required accessible name (rendered as the dialog title). */
  title: string;
  description?: string;
  open?: boolean;
  defaultOpen?: boolean;
  onOpenChange?: (open: boolean) => void;
  size?: ModalSize;
  /** Footer actions (e.g. Cancel + a primary Button). */
  footer?: ReactNode;
  /** An element that opens the modal (uncontrolled usage). */
  trigger?: ReactNode;
  children?: ReactNode;
  className?: string;
  showCloseButton?: boolean;
}

export function Modal({
  title,
  description,
  open,
  defaultOpen,
  onOpenChange,
  size = "md",
  footer,
  trigger,
  children,
  className,
  showCloseButton = true,
}: ModalProps) {
  // Only forward the open-state props that are actually set (exactOptionalPropertyTypes).
  const rootProps: Partial<Pick<ModalProps, "open" | "defaultOpen" | "onOpenChange">> = {};
  if (open !== undefined) rootProps.open = open;
  if (defaultOpen !== undefined) rootProps.defaultOpen = defaultOpen;
  if (onOpenChange) rootProps.onOpenChange = onOpenChange;

  return (
    <Dialog {...rootProps}>
      {trigger ? <DialogTrigger asChild>{trigger}</DialogTrigger> : null}
      <DialogContent showCloseButton={showCloseButton} className={cn("wv-modal", SIZE[size], className)}>
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          {description ? <DialogDescription>{description}</DialogDescription> : null}
        </DialogHeader>
        {children}
        {footer ? <DialogFooter>{footer}</DialogFooter> : null}
      </DialogContent>
    </Dialog>
  );
}

/**
 * ConfirmModal — the confirm variant of the one modal idiom. A standard
 * Cancel / Confirm footer, with a `destructive` flag when the action removes
 * something. Both buttons are wrapped in DialogClose so the modal closes in
 * controlled and uncontrolled usage alike; Confirm fires `onConfirm` first.
 */
export interface ConfirmModalProps {
  title: string;
  description?: string;
  open?: boolean;
  defaultOpen?: boolean;
  onOpenChange?: (open: boolean) => void;
  size?: ModalSize;
  confirmLabel?: string;
  cancelLabel?: string;
  onConfirm: () => void;
  destructive?: boolean;
  trigger?: ReactNode;
  children?: ReactNode;
}

export function ConfirmModal({
  confirmLabel = "Confirm",
  cancelLabel = "Cancel",
  onConfirm,
  destructive = false,
  children,
  ...modalProps
}: ConfirmModalProps) {
  return (
    <Modal
      {...modalProps}
      footer={
        <>
          <DialogClose asChild>
            <Button variant="secondary">{cancelLabel}</Button>
          </DialogClose>
          <DialogClose asChild>
            <Button variant={destructive ? "destructive" : "default"} onClick={onConfirm}>
              {confirmLabel}
            </Button>
          </DialogClose>
        </>
      }
    >
      {children}
    </Modal>
  );
}
