/**
 * A type-level test (checked by `tsc --noEmit`, not run by vitest): it proves the
 * button's a11y contract is enforced by the compiler, not merely documented. A
 * Button MUST carry an accessible name — visible text `children` or an explicit
 * `aria-label` (the icon-only case). If that union were ever loosened so a button
 * with neither type-checked, the `@ts-expect-error` below would become an unused
 * directive and `tsc` would fail here — so this file failing to type-check IS the
 * guarantee.
 */
import { Trash2 } from "lucide-react";
import { Button } from "./button";

// Happy path: text children name the button.
export function LabelledByText() {
  return <Button variant="default">Publish schedule</Button>;
}

// Happy path: an icon-only button names itself with aria-label.
export function LabelledByAria() {
  return <Button icon={Trash2} size="icon" aria-label="Remove screen" />;
}

// Contract: a button with neither children nor aria-label must NOT type-check.
export function NamelessButton() {
  return (
    // @ts-expect-error — a Button requires an accessible name (children or aria-label).
    <Button icon={Trash2} size="icon" />
  );
}
