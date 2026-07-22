import { useId, type ReactNode } from "react";
import { Label } from "@/components/ui/label";
import { cn } from "@/lib/utils";

/**
 * FormField — THE form idiom: label + control + help + error, wired for a11y in
 * one place so no page has to remember the plumbing. It generates a stable id,
 * points the `<label>` at the control, associates help/error text via
 * `aria-describedby`, flags `aria-invalid` when an error is present, and marks
 * the control `aria-required` when the field is required (the visible asterisk
 * is decorative and `aria-hidden`, so this is what assistive tech actually
 * announces).
 *
 * The control is a render function that receives those wired props; passing them
 * onto the control is the whole contract. This makes correct labelling the path
 * of least resistance — the control is always named, always described.
 */
export interface FormFieldControl {
  id: string;
  "aria-describedby"?: string;
  "aria-invalid"?: true;
  "aria-required"?: true;
}

export interface FormFieldProps {
  label: string;
  /** Override the generated control id (otherwise a stable `useId` is used). */
  htmlFor?: string;
  help?: string;
  error?: string;
  required?: boolean;
  className?: string;
  children: (control: FormFieldControl) => ReactNode;
}

export function FormField({
  label,
  htmlFor,
  help,
  error,
  required,
  className,
  children,
}: FormFieldProps) {
  const generatedId = useId();
  const id = htmlFor ?? generatedId;
  const helpId = help ? `${id}-help` : undefined;
  const errorId = error ? `${id}-error` : undefined;
  const describedBy = [errorId, helpId].filter(Boolean).join(" ") || undefined;

  // Build the control props with only the keys that are actually set, so nothing
  // spreads an explicit `undefined` onto the control (exactOptionalPropertyTypes).
  const control: FormFieldControl = { id };
  if (describedBy) control["aria-describedby"] = describedBy;
  if (error) control["aria-invalid"] = true;
  if (required) control["aria-required"] = true;

  return (
    <div data-slot="form-field" className={cn("flex flex-col gap-1.5", className)}>
      <Label htmlFor={id}>
        {label}
        {required ? (
          <span className="text-[color:var(--wv-accent-text)]" aria-hidden="true">
            *
          </span>
        ) : null}
      </Label>
      {children(control)}
      {help ? (
        <p id={helpId} className="text-[13px] text-muted-foreground">
          {help}
        </p>
      ) : null}
      {error ? (
        <p id={errorId} role="alert" className="text-[13px] text-[color:var(--wv-err)]">
          {error}
        </p>
      ) : null}
    </div>
  );
}
