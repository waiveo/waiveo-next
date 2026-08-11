// ui-schema/1 — the renderer's public surface.
//
// A host renders a pack-declared page by validating then painting it: import
// `PageRenderer` and hand it the page document plus the page-level data, an
// api-client handler (the action seam), an optional message catalog, and slot
// content. `validatePage` is exposed for a build/lint-time check ahead of any
// render (UIS-201).

export { PageRenderer, isCreateDraftUi, type PageRendererProps } from "./PageRenderer";
export { validatePage, isValidBindingPath } from "./validate";
export type {
  ActionHandler,
  ActionOutcome,
  ActionRef,
  ConfirmSpec,
  OutcomeReporter,
  WidgetNode,
  WizardController,
} from "./types";
export type { ValidationError, ValidationResult, ErrorCode } from "./schema";
