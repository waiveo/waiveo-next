import type { ComponentProps } from "react";
import { Checkbox as UICheckbox } from "@/components/ui/checkbox";

/**
 * Checkbox — the kit's binary control, wrapping the vendored shadcn/Radix base.
 *
 * It exists because `DataTable` row selection needs one and a ROUTE may not
 * import `@/components/ui` (the ESLint `no-restricted-imports` boundary, proven
 * by `import-boundary.test.ts`). The answer to "the primitive is unreachable" is
 * a kit wrapper, never an eslint-disable at the call site: the boundary is what
 * keeps there being one checkbox in the console instead of four.
 *
 * A checkbox MUST have an accessible name, and the type refuses one without it —
 * either `aria-label` (a bare box in a table cell, which is the common case) or
 * `aria-labelledby` (a box named by visible text elsewhere). A tri-state box
 * passes `checked="indeterminate"`, which Radix renders as `aria-checked="mixed"`
 * — the state a "some of this page is selected" header checkbox is actually in.
 */
type CheckboxBase = Omit<
  ComponentProps<typeof UICheckbox>,
  "aria-label" | "aria-labelledby"
>;

export type CheckboxProps = CheckboxBase &
  ({ "aria-label": string; "aria-labelledby"?: string } | { "aria-labelledby": string; "aria-label"?: string });

export function Checkbox(props: CheckboxProps) {
  return <UICheckbox {...props} />;
}
