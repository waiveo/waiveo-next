/**
 * A type-level test (checked by `tsc --noEmit`, not run by vitest): it proves the
 * a11y contract is enforced by the compiler, not merely documented. A Modal MUST
 * carry a `title` (its accessible name). If `title` were ever made optional, the
 * `@ts-expect-error` below would become an unused directive and `tsc` would fail
 * here — so this file failing to type-check IS the guarantee.
 */
import { Modal } from "./modal";

// Happy path: a titled modal type-checks.
export function ValidModal() {
  return <Modal title="Rename screen">Body content</Modal>;
}

// Contract: a modal with no title must NOT type-check.
export function MissingTitleModal() {
  return (
    // @ts-expect-error — Modal requires an accessible `title` prop.
    <Modal>Body content</Modal>
  );
}
