// ui-schema/1 — the closed sets, the document types, and the error taxonomy.
//
// This module encodes, as data, exactly the closed vocabularies contract
// `contracts/ui-schema-1.md` defines: the four page types (UIS-003), the widget
// catalog (UIS-070), the binding grammar's reserved roots (UIS-100/103–107), the
// vocabulary-reference table (UIS-120), the OptionSource kinds (UIS-130), the
// Computed function list (UIS-140), and the action-verb grammar (UIS-160). Growth
// of any of these is a ui-schema/1 minor version — never a silent host extension
// (Negotiation) — so they live here as frozen literals the validator reads.

// ── Page types (UIS-002/003) ────────────────────────────────────────────────

export const PAGE_TYPES = ["list-detail", "settings-form", "dashboard", "wizard"] as const;
export type PageType = (typeof PAGE_TYPES)[number];

// ── Error taxonomy (§Error taxonomy) ────────────────────────────────────────

/** UIS-182's render-time recursion refusal, named once.
 *
 * Exported as its own constant because it is the only code in this taxonomy
 * raised by the RENDERER rather than the validator — the depth a fragment chain
 * reaches is a property of the bound data, which no static check can know — so
 * the one place that raises it cannot reach it through the validator's `fail()`.
 * A duplicated string literal there is how a published code drifts from the
 * refusal that is supposed to carry it. */
export const FRAGMENT_RECURSION_DEPTH_EXCEEDED = "FRAGMENT_RECURSION_DEPTH_EXCEEDED";

/** UIS-167's unwired-seam refusal, named once, for the same reason
 * FRAGMENT_RECURSION_DEPTH_EXCEEDED is: it is raised by the ACTION DISPATCHER
 * rather than the validator — whether a host wired a seam is not a property of
 * the document — so the one place that raises it cannot reach it through the
 * validator's `fail()`. An ActionRef that asked for an outcome and got silence
 * cannot tell "the host is not wired" from "it worked", so the refusal is named
 * rather than left as a bare `status: "error"`. */
export const ACTION_DISPATCH_UNWIRED = "ACTION_DISPATCH_UNWIRED";

export const ERROR_CODES = [
  "UNKNOWN_PAGE_TYPE",
  "UNKNOWN_WIDGET_TYPE",
  "WIDGET_PROP_UNKNOWN",
  "WIDGET_REQUIRED_FIELD_MISSING",
  "WIDGET_EVENT_UNKNOWN",
  "BINDING_PATH_INVALID",
  "BINDING_TYPE_MISMATCH",
  "FRAGMENT_REF_UNDEFINED",
  FRAGMENT_RECURSION_DEPTH_EXCEEDED,
  "SLOT_NAME_DUPLICATE",
  "VOCAB_REF_UNKNOWN",
  "OPTION_SOURCE_INVALID",
  "COMPUTE_FN_UNKNOWN",
  "CURRENCY_CODE_INVALID",
  "CONTEXT_REF_UNDEFINED",
  "ACTION_VERB_UNKNOWN",
  "ACTION_FIELDS_INVALID",
  ACTION_DISPATCH_UNWIRED,
  "WIZARD_STEP_ID_DUPLICATE",
] as const;
export type ErrorCode = (typeof ERROR_CODES)[number];

/** One rejection: a taxonomy `code` plus the offending field's own dotted/bracket
 * `path` (UIS-200 — a driver asserts on `code`, not on message text). `field` is
 * an alias of `path` matching the corpus's own key naming. */
export interface ValidationError {
  code: ErrorCode;
  path: string;
  field: string;
  message: string;
}

/** The result of validating a page document: either clean, or a non-empty list of
 * typed rejections. A non-conformant document NEVER renders (Scope), so a caller
 * that gets `{ ok: false }` must refuse to paint the page. */
export type ValidationResult = { ok: true } | { ok: false; errors: ValidationError[] };

// ── Vocabulary references (UIS-120) ─────────────────────────────────────────
// The complete closed set, each row's members in the order the contract's table
// lists them (UIS-132's candidate ordering depends on it).

export const VOCAB_TABLE: Record<string, readonly string[]> = {
  "rules/1:trigger-kind": [
    "state",
    "numeric",
    "time",
    "time_pattern",
    "sun",
    "template",
    "event",
    "webhook",
  ],
  "rules/1:condition-kind": [
    "and",
    "or",
    "not",
    "state",
    "numeric",
    "time",
    "sun",
    "variable",
    "template",
  ],
  "rules/1:condition-leaf-kind": ["state", "numeric", "time", "sun", "variable", "template"],
  "rules/1:action-kind": [
    "device_command",
    "preset_batch",
    "choose",
    "delay",
    "log",
    "notify",
    "variable_write",
    "workflow_start",
    "pack_action",
  ],
  "rules/1:mode": ["single", "restart", "queued", "parallel"],
  "rules/1:misfire": ["catch_up_once", "skip", "fire_each"],
  "rules/1:filter": [
    "state",
    "attr",
    "default",
    "upper",
    "lower",
    "trim",
    "round",
    "abs",
    "int",
    "float",
    "now",
    "elapsed",
    "duration",
    "convert",
  ],
};

// ── Computed functions (UIS-140) ────────────────────────────────────────────

export const COMPUTE_FNS = [
  "eq",
  "not",
  "and",
  "or",
  "count",
  "isEmpty",
  "join",
  "label",
  "msg",
  "formatDuration",
  "formatDate",
  "formatNumber",
  "formatCurrency",
  "firstKey",
] as const;
export type ComputeFn = (typeof COMPUTE_FNS)[number];

// `formatCurrency`'s pinned `currencyCode` arg (UIS-140/143) is a literal, not a
// Binding — but unlike a Binding it gets NO grammar check elsewhere, so its own
// shape must be enforced here (UIS-144): a bare 3-letter alpha code, matched
// case-insensitively (e.g. "USD", "eur"). validate.ts enforces this at
// validate-time (CURRENCY_CODE_INVALID); bindings.ts re-uses the SAME pattern for
// its own graceful-degradation fallback so the two can never diverge.
export const ISO_4217_PATTERN = /^[A-Za-z]{3}$/;

// ── Action verbs (UIS-160) ──────────────────────────────────────────────────

export const ACTION_VERBS = [
  "navigate",
  "submit",
  "create",
  "delete",
  "call-action",
  "set",
  "repeat-add",
  "repeat-remove",
  "wizard-next",
  "wizard-back",
  "wizard-finish",
] as const;
export type ActionVerb = (typeof ACTION_VERBS)[number];

export const WIZARD_ONLY_VERBS: readonly ActionVerb[] = ["wizard-next", "wizard-back", "wizard-finish"];

// ── ActionRef envelope fields (UIS-160/165/166/167) ─────────────────────────
// Two OPTIONAL fields that belong to no single verb: they govern HOW the named
// verb is invoked, not what it does. Enumerated here (rather than inline in the
// validator) because UIS-164's closed `create` field set has to know them — a
// field valid on every verb is never an "unrecognized create field".

/** The **seam** verbs (UIS-167): the four that dispatch through the host's own
 * write/dispatch path (UIS-161) and can therefore produce a result or a refusal.
 * `outcomeTo` is valid on exactly these; on a local/wizard/navigate verb it is a
 * declaration that could never settle, so it is a static rejection. */
export const OUTCOME_VERBS: readonly ActionVerb[] = ["submit", "create", "delete", "call-action"];

/** A ConfirmSpec's closed field set (UIS-165). */
export const CONFIRM_FIELDS = ["titleMsg", "bodyMsg", "confirmLabelMsg", "cancelLabelMsg", "destructive"] as const;

/** The ActionOutcome `status` values (UIS-166). */
export const OUTCOME_STATUSES = ["pending", "ok", "error"] as const;
export type OutcomeStatus = (typeof OUTCOME_STATUSES)[number];

/** `text`'s live-region politeness (UIS-077). An unrecognized value resolves to
 * `polite` rather than to a non-live node — an over-announcement is recoverable,
 * a missing one is invisible to everyone who can see the screen. */
export const ANNOUNCE_MODES = ["polite", "assertive"] as const;
export type AnnounceMode = (typeof ANNOUNCE_MODES)[number];

// ── OptionSource kinds (UIS-130) ────────────────────────────────────────────

export const OPTION_SOURCE_KINDS = ["literal", "vocab", "data"] as const;

// ── Binding-grammar reserved roots (UIS-100/103–107) ────────────────────────
// `$index` is the read-only iteration-position token (UIS-107); it is a valid
// segment name for a read but never a write destination.

export const RESERVED_ROOTS = ["$root", "$ui", "$context", "$params", "item", "$index"] as const;

// ── entity-picker binding shapes (UIS-073a) ─────────────────────────────────
// Which shape an `entity-picker`'s `bind` addresses: `entityRef` — UIS-073's
// EntityRef object, the default the picker has always had — or `entityId`, the
// scalar entity-id string `rules/1` inlines into a trigger/condition/action
// (RUL-010). A closed two-member set, enforced rather than presence-checked
// (UIS-073a): a typo would silently revert the picker to the object shape and
// paint an empty control, the very failure the scalar shape removes.

export const ENTITY_PICKER_BIND_SHAPES = ["entityRef", "entityId"] as const;
export type EntityPickerBindShape = (typeof ENTITY_PICKER_BIND_SHAPES)[number];

/** The scalar shape's own name — the one a node opts into. */
export const ENTITY_PICKER_SCALAR_SHAPE: EntityPickerBindShape = "entityId";

/** The only UIS-073 form a scalar bind can express: a bare entity id carries no
 * discriminant, so `selector`/`deviceClass` have nowhere to live (UIS-073a). */
export const ENTITY_PICKER_SCALAR_MODES: readonly string[] = ["entity"];

// ── Widget catalog (UIS-070) ────────────────────────────────────────────────

export type WidgetCategory = "structural" | "display" | "input" | "action";
export type BindRequirement = "none" | "optional" | "required";

/** How a prop's value is validated. Kinds that name a sub-structure the validator
 * must recurse into (a nested widget node, a BindingExpr, an OptionSource, …) drive
 * the walk; the scalar/msg kinds are presence-checked only (there is no taxonomy
 * code for a wrong scalar type, so the validator never invents one). */
export type PropKind =
  | "msg" // a msg: reference — opaque string, never a Binding
  | "bool"
  | "int"
  | "number"
  | "enum"
  | "modes" // entity-picker mode subset
  | "keyArray" // firstKey candidate keys — literal, never grammar-checked
  | "bindingExpr" // Binding | Computed | JSON literal (UIS-108/141)
  | "binding" // a Binding (bare string) or LiveBinding (UIS-066/109)
  | "widget" // a single nested widget node (repeat.itemTemplate, switch.default)
  | "cases" // switch cases: [{ when, render }]
  | "columns" // table columns: [{ headerMsg, cell }]
  | "options" // an OptionSource
  | "fragmentRef" // a key of the document's fragments map
  | "slotName" // a slot name (collected for the duplicate check)
  | "params"; // fragment params: object of literal/Binding

export interface PropDef {
  required: boolean;
  kind: PropKind;
}

export interface EventDef {
  required: boolean;
}

export interface WidgetSpec {
  category: WidgetCategory;
  children: boolean;
  bind: BindRequirement;
  props: Record<string, PropDef>;
  events: Record<string, EventDef>;
}

const p = (kind: PropKind, required = false): PropDef => ({ kind, required });
const req = (kind: PropKind): PropDef => ({ kind, required: true });

export const WIDGET_CATALOG: Record<string, WidgetSpec> = {
  section: {
    category: "structural",
    children: true,
    bind: "none",
    props: { titleMsg: p("msg"), collapsible: p("bool"), defaultCollapsed: p("bool") },
    events: {},
  },
  repeat: {
    category: "structural",
    children: false,
    // UIS-070 bind shape `array` / UIS-107: `bind` names the array `itemTemplate`
    // iterates — a repeat with no bound array has nothing to render, so bind is
    // required (contrast `fragment`, whose bind is explicitly optional and rescopes).
    bind: "required",
    props: {
      itemTemplate: req("widget"),
      itemScope: p("msg"), // a kebab identifier — presence-checked only
      minItems: p("int"),
      maxItems: p("int"),
      emptyMsg: p("msg"),
    },
    events: {},
  },
  switch: {
    category: "structural",
    children: false,
    bind: "none",
    props: { discriminant: req("bindingExpr"), cases: req("cases"), default: p("widget") },
    events: {},
  },
  fragment: {
    category: "structural",
    children: false,
    bind: "optional",
    props: { ref: req("fragmentRef"), params: p("params") },
    events: {},
  },
  slot: {
    category: "structural",
    children: false,
    bind: "none",
    props: { name: req("slotName") },
    events: {},
  },
  text: {
    category: "display",
    children: false,
    bind: "none",
    // `announce` (UIS-077) makes the node an ARIA live region so text that lands
    // after the page has been read — an ActionOutcome's refusal sentence, most of
    // all — is announced rather than silently painted.
    // `titleMsg` (UIS-079) is the explanatory hover text a three-state cell needs
    // to be distinguishable rather than decorative.
    props: { value: req("bindingExpr"), announce: p("enum"), titleMsg: p("msg") },
    events: {},
  },
  badge: {
    category: "display",
    children: false,
    bind: "none",
    // `tone` stays the STATIC enum it has always been and `toneFrom` (UIS-078) is
    // its dynamic sibling, rather than `tone` itself widening to a BindingExpr: a
    // bare string is a Binding PATH in that grammar (UIS-108), so widening would
    // silently reinterpret every existing `tone: "warning"` as a lookup of a field
    // named `warning` and paint the fallback tone on every badge already shipped.
    props: {
      value: req("bindingExpr"),
      tone: p("enum"),
      toneFrom: p("bindingExpr"),
      titleMsg: p("msg"),
    },
    events: {},
  },
  table: {
    category: "display",
    children: false,
    bind: "none",
    props: { source: req("binding"), columns: req("columns") },
    events: { rowPress: { required: false } },
  },
  "stat-tile": {
    category: "display",
    children: false,
    bind: "none",
    props: { labelMsg: req("msg"), value: req("bindingExpr"), tone: p("enum"), titleMsg: p("msg") },
    events: {},
  },
  "text-input": {
    category: "input",
    children: false,
    bind: "required",
    props: {
      labelMsg: req("msg"), // required accessible label (UIS-075)
      placeholderMsg: p("msg"),
      multiline: p("bool"),
      maxLength: p("int"),
    },
    events: { change: { required: false } },
  },
  "number-input": {
    category: "input",
    children: false,
    bind: "required",
    props: { labelMsg: req("msg"), min: p("number"), max: p("number"), step: p("number") },
    events: { change: { required: false } },
  },
  "duration-input": {
    category: "input",
    children: false,
    bind: "required",
    props: { labelMsg: req("msg"), displayUnit: p("enum"), min: p("number") },
    events: { change: { required: false } },
  },
  toggle: {
    category: "input",
    children: false,
    bind: "required",
    props: { labelMsg: req("msg"), onLabelMsg: p("msg"), offLabelMsg: p("msg") },
    events: { change: { required: false } },
  },
  select: {
    category: "input",
    children: false,
    bind: "required",
    props: { labelMsg: req("msg"), options: req("options"), placeholderMsg: p("msg") },
    events: { change: { required: false } },
  },
  "multi-select": {
    category: "input",
    children: false,
    bind: "required",
    props: { labelMsg: req("msg"), options: req("options") },
    events: { change: { required: false } },
  },
  "entity-picker": {
    category: "input",
    children: false,
    bind: "required",
    // `bindShape` (UIS-073a) selects which shape `bind` addresses: the EntityRef
    // object (`entityRef`, the default) or the scalar entity id rules/1 inlines
    // (`entityId`). Declared, never inferred from the bound value.
    props: { labelMsg: req("msg"), bindShape: p("enum"), modes: p("modes") },
    events: { change: { required: false } },
  },
  "time-of-day": {
    category: "input",
    children: false,
    bind: "required",
    props: { labelMsg: req("msg") },
    events: { change: { required: false } },
  },
  button: {
    category: "action",
    children: false,
    bind: "none",
    // `disabledIf` (UIS-076) keeps the control present but un-pressable — the only
    // construct here that can make a widget-triggered invocation non-re-entrant
    // (bind it to an ActionOutcome's `pending`, UIS-166).
    props: { labelMsg: req("msg"), style: p("enum"), disabledIf: p("bindingExpr") },
    events: { press: { required: true } },
  },
};

/** The eight input-category widget types UIS-075 requires an accessible `labelMsg`
 * on. Each carries `labelMsg` as a required prop in the catalog above, so a
 * missing label fails validation as WIDGET_REQUIRED_FIELD_MISSING (UIS-062/075)
 * through the ordinary required-prop check — no separate gate. */
export const INPUT_TYPES = new Set(
  Object.entries(WIDGET_CATALOG)
    .filter(([, spec]) => spec.category === "input")
    .map(([type]) => type),
);
