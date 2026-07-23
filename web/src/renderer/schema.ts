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

export const ERROR_CODES = [
  "UNKNOWN_PAGE_TYPE",
  "UNKNOWN_WIDGET_TYPE",
  "WIDGET_PROP_UNKNOWN",
  "WIDGET_REQUIRED_FIELD_MISSING",
  "WIDGET_EVENT_UNKNOWN",
  "BINDING_PATH_INVALID",
  "BINDING_TYPE_MISMATCH",
  "FRAGMENT_REF_UNDEFINED",
  "FRAGMENT_RECURSION_DEPTH_EXCEEDED",
  "SLOT_NAME_DUPLICATE",
  "VOCAB_REF_UNKNOWN",
  "OPTION_SOURCE_INVALID",
  "COMPUTE_FN_UNKNOWN",
  "CURRENCY_CODE_INVALID",
  "CONTEXT_REF_UNDEFINED",
  "ACTION_VERB_UNKNOWN",
  "ACTION_FIELDS_INVALID",
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

// ── OptionSource kinds (UIS-130) ────────────────────────────────────────────

export const OPTION_SOURCE_KINDS = ["literal", "vocab", "data"] as const;

// ── Binding-grammar reserved roots (UIS-100/103–107) ────────────────────────
// `$index` is the read-only iteration-position token (UIS-107); it is a valid
// segment name for a read but never a write destination.

export const RESERVED_ROOTS = ["$root", "$ui", "$context", "$params", "item", "$index"] as const;

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
    props: { value: req("bindingExpr") },
    events: {},
  },
  badge: {
    category: "display",
    children: false,
    bind: "none",
    props: { value: req("bindingExpr"), tone: p("enum") },
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
    props: { labelMsg: req("msg"), value: req("bindingExpr"), tone: p("enum") },
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
    props: { labelMsg: req("msg"), modes: p("modes") },
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
    props: { labelMsg: req("msg"), style: p("enum") },
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
