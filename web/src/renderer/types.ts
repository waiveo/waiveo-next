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

/** An ActionRef (UIS-160): a closed verb plus verb-specific fields. */
export interface ActionRef {
  verb: string;
  [field: string]: unknown;
}

/** The api-client seam (UIS-161): the host injects one handler and the closed
 * action verbs dispatch onto it. Every method is optional so a demo or a test
 * can wire only what it asserts; an unwired verb is a no-op the renderer swallows
 * rather than throwing. `submit`/`create`/`delete`/`call-action` resolve to the
 * host's ordinary management-API paths (this contract does not name the routes). */
export interface ActionHandler {
  navigate?(to: string, params: Record<string, unknown>): void;
  submit?(target: string | undefined, resource: unknown): void | Promise<void>;
  create?(target: string, itemDefault: Record<string, unknown>): void | Promise<void>;
  remove?(target: string, resource: unknown): void | Promise<void>;
  callAction?(action: string, params: Record<string, unknown>): void | Promise<void>;
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
