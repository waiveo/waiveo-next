/**
 * The Waiveo widget kit — the ONLY sanctioned component surface.
 *
 * Pages, routes and (later) the ui-schema renderer import from here and nowhere
 * else: never from `@/components/ui` directly (the ESLint `no-restricted-imports`
 * boundary enforces it). One PageHeader, one DataTable, one FormField, one Modal
 * — so the console and every extension page rendered through it are structurally
 * uniform and on-brand. Every interactive primitive requires an accessible label
 * at the type level.
 */

export { KitIcon } from "./kit-icon";
export type { KitIconProps } from "./kit-icon";

export { Button } from "./button";
export type { ButtonProps } from "./button";

export { StatusBadge } from "./status-badge";
export type { StatusBadgeProps, Status } from "./status-badge";

export { PageHeader } from "./page-header";
export type { PageHeaderProps } from "./page-header";

export { StatCard } from "./stat-card";
export type { StatCardProps } from "./stat-card";

export { FormField } from "./form-field";
export type { FormFieldProps, FormFieldControl } from "./form-field";

export { Modal, ConfirmModal } from "./modal";
export type { ModalProps, ModalSize, ConfirmModalProps } from "./modal";

export { DataTable } from "./data-table";
export type { DataTableProps } from "./data-table";

export { EmptyState } from "./empty-state";
export type { EmptyStateProps } from "./empty-state";

export { Toaster, toast } from "./toaster";

// Re-export the table typing surface pages need to declare columns, so callers
// stay within the kit import boundary rather than reaching for the vendor.
export type { ColumnDef } from "@tanstack/react-table";
