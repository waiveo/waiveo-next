// ui-schema/1 — renderer runtime types.
//
// Loose structural shapes the renderer reads AFTER validation (validate.ts) has
// already proven the document conformant. The validator is the closed-set gate;
// these types describe the shape the interpreter walks, not a second schema.

/** A widget node envelope (UIS-060): `{type, id?, bind?, props?, on?, visibleIf?,
 * children?}`. Post-validation, `type` is a Widget-catalog member. */
export interface WidgetNode {
  type: string;
  id?: string;
  bind?: unknown;
  props?: Record<string, unknown>;
  on?: Record<string, unknown>;
  visibleIf?: unknown;
  children?: WidgetNode[];
}

/** An ActionRef (UIS-160): a closed verb plus verb-specific fields, and the two
 * verb-independent envelope fields `confirm` (UIS-165) / `outcomeTo` (UIS-166). */
export interface ActionRef {
  verb: string;
  [field: string]: unknown;
}

/** A ConfirmSpec (UIS-165): the operator acknowledgement an ActionRef's `confirm`
 * field interposes between a widget event and that ActionRef's own dispatch. Every
 * label is a `msg:` reference (MAN-003/111); `titleMsg` is required because it is
 * the dialog's accessible name and there is nowhere else for one to come from
 * (the same argument UIS-075 makes for an input's `labelMsg`). */
export interface ConfirmSpec {
  titleMsg: string;
  bodyMsg?: string;
  confirmLabelMsg?: string;
  cancelLabelMsg?: string;
  destructive?: boolean;
}

/** The ActionOutcome an `outcomeTo` ActionRef publishes into `$ui` (UIS-166).
 *
 * `pending` at dispatch, then `ok` with the invocation's `result` or `error` with
 * the refusal's own `code`/`detail`/`traceId`. This is the ONLY place a
 * schema-authored page can read a published error code from: before it existed
 * the seam's result was discarded, so a refusal could not be rendered at all. */
export interface ActionOutcome {
  status: "pending" | "ok" | "error";
  code?: string | null;
  detail?: string | null;
  traceId?: string | null;
  result?: unknown;
}

/** The interim-progress channel a host MAY use while an invocation is still in
 * flight (UIS-166). The renderer merges the reported fields into the pending
 * record and leaves `status` at `pending`; a report arriving after the outcome
 * has settled is ignored. It is a property of the dispatch seam, not of the
 * document grammar — a page binds the SHAPE, never the reporting mechanism. */
export type OutcomeReporter = (partial: Pick<ActionOutcome, "detail" | "result">) => void;

/** The api-client seam (UIS-161): the host injects one handler and the closed
 * action verbs dispatch onto it. Every method is optional so a demo or a test
 * can wire only what it asserts; an unwired verb is a no-op the renderer swallows
 * rather than throwing — EXCEPT where the ActionRef declared an `outcomeTo`, which
 * settles as `ACTION_DISPATCH_UNWIRED` instead of lying or hanging (UIS-167).
 * `submit`/`create`/`delete`/`call-action` resolve to the host's ordinary
 * management-API paths (this contract does not name the routes).
 *
 * The four seam methods return `unknown`: whatever a method resolves with becomes
 * the ActionOutcome's `result`, and whatever it rejects with is read for a
 * `code`/`detail`/`traceId` (an api/1 Problem's own fields, duck-typed — the
 * renderer never imports a host's error class). Each also receives an
 * OutcomeReporter it MAY call while still in flight — but ONLY when the invoking
 * ActionRef declared an `outcomeTo`, since there is nowhere to report progress
 * into otherwise, and passing it unconditionally would change the argument list
 * every already-written host sees. A host with nothing interim to say ignores it. */
export interface ActionHandler {
  navigate?(to: string, params: Record<string, unknown>): void;
  submit?(target: string | undefined, resource: unknown, report?: OutcomeReporter): unknown;
  create?(target: string, itemDefault: Record<string, unknown>, report?: OutcomeReporter): unknown;
  remove?(target: string, resource: unknown, report?: OutcomeReporter): unknown;
  callAction?(action: string, params: Record<string, unknown>, report?: OutcomeReporter): unknown;
  /** Fetch the next keyset page of a paginated `list.source` (UIS-023/024). */
  fetchPage?(
    path: string,
    cursor: string | null,
    limit: number | undefined,
  ): Promise<{ items: unknown[]; cursor: string | null }>;
}

/** The wizard step controller the built-in wizard verbs invoke (UIS-050/160). */
export interface WizardController {
  next(): void;
  back(): void;
  finish(): void;
}
