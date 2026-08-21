// ui-schema/1 — the validating half of the renderer.
//
// `validatePage(doc)` enforces the contract's validation story (UIS-200): the
// closed-set memberships (page type UIS-003, widget type UIS-060, action verb
// UIS-160, compute fn UIS-140, vocabRef UIS-121, OptionSource kind UIS-130), the
// grammar-well-formedness checks (binding paths UIS-100, option/label completeness
// UIS-132, fragment/slot references UIS-180/186, wizard step ids UIS-050), and the
// required-field checks (UIS-062/074/075). Every rejection carries a taxonomy code
// and the offending field's own dotted/bracket path so a driver asserts on `code`,
// never on message text. A document that fails ANY of these is non-conformant and
// MUST NOT render — closed sets are closed, so an unknown member is a typed error,
// never a silent skip or a best-effort container.

import { parseOutcomeTarget } from "./bindings";
import {
  ACTION_VERBS,
  CONFIRM_FIELDS,
  COMPUTE_FNS,
  OUTCOME_VERBS,
  ENTITY_PICKER_BIND_SHAPES,
  ENTITY_PICKER_SCALAR_MODES,
  ENTITY_PICKER_SCALAR_SHAPE,
  ISO_4217_PATTERN,
  OPTION_SOURCE_KINDS,
  PAGE_TYPES,
  RESERVED_ROOTS,
  VOCAB_TABLE,
  WIDGET_CATALOG,
  WIZARD_ONLY_VERBS,
  type ActionVerb,
  type ErrorCode,
  type PageType,
  type PropDef,
  type ValidationError,
  type ValidationResult,
  type WidgetSpec,
} from "./schema";

// ── Small JSON helpers ──────────────────────────────────────────────────────

function isObject(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

// ── Binding grammar (UIS-100) ───────────────────────────────────────────────

const RESERVED = new Set<string>(RESERVED_ROOTS);
const KEBAB = /^[a-z][a-z0-9-]*$/;
const FIELD_NAME = /^[A-Za-z_][A-Za-z0-9_]*$/;

/** Split a path on `.` at bracket depth 0 only, so a predicate value that is
 * itself a dotted path (`arr[id=$ui.selected]`) is not torn apart. */
function splitTopLevel(s: string, sep: string): string[] {
  const out: string[] = [];
  let depth = 0;
  let start = 0;
  for (let i = 0; i < s.length; i++) {
    const ch = s[i];
    if (ch === "[") depth++;
    else if (ch === "]") depth = Math.max(0, depth - 1);
    else if (ch === sep && depth === 0) {
      out.push(s.slice(start, i));
      start = i + 1;
    }
  }
  out.push(s.slice(start));
  return out;
}

function isValidName(name: string): boolean {
  if (RESERVED.has(name)) return true;
  if (name.startsWith("$")) return false; // $ is reserved for the roots above (UIS-102)
  return KEBAB.test(name) || FIELD_NAME.test(name);
}

const NUMBER_LITERAL = /^-?\d+(\.\d+)?$/;

/** A predicate value (UIS-101) is a JSON number/boolean/null literal, or a bare
 * string that is itself a Binding resolved before the comparison. */
function isValidPredicateValue(v: string): boolean {
  if (v === "true" || v === "false" || v === "null") return true;
  if (NUMBER_LITERAL.test(v)) return true;
  return isValidBindingPath(v);
}

function isValidSegment(seg: string): boolean {
  if (seg.length === 0) return false;
  const br = seg.indexOf("[");
  if (br === -1) return isValidName(seg);
  if (!seg.endsWith("]")) return false;
  const name = seg.slice(0, br);
  const inner = seg.slice(br + 1, seg.length - 1);
  if (!isValidName(name) || inner.length === 0) return false;
  // index := non-negative integer | "$index"
  if (/^\d+$/.test(inner) || inner === "$index") return true;
  // predicate := name "=" (literal | path)
  const eq = inner.indexOf("=");
  if (eq <= 0 || eq === inner.length - 1) return false;
  const left = inner.slice(0, eq);
  const right = inner.slice(eq + 1);
  return isValidName(left) && isValidPredicateValue(right);
}

/** True iff `input` matches the UIS-100 data-path grammar
 * `path := segment ("." segment)*`. Empty segments (a double dot), leading/
 * trailing dots, `$`-prefixed field names, and malformed predicates all fail. */
export function isValidBindingPath(input: unknown): boolean {
  if (typeof input !== "string" || input.length === 0) return false;
  const segments = splitTopLevel(input, ".");
  return segments.every(isValidSegment);
}

/** UIS-107: `$index` addresses a read-only iteration position; a Binding MUST NOT
 * target it as a write destination. True iff the path's final segment is `$index`. */
function targetsIndex(path: string): boolean {
  const segments = splitTopLevel(path, ".");
  return segments[segments.length - 1] === "$index";
}

// ── Validation context + error sink ─────────────────────────────────────────

interface Ctx {
  errors: ValidationError[];
  fragmentKeys: Set<string>;
  contextKeys: Set<string>;
  slotNames: { name: string; path: string }[];
  inWizard: boolean;
}

function fail(ctx: Ctx, code: ErrorCode, path: string, message: string): void {
  ctx.errors.push({ code, path, field: path, message });
}

// ── Binding / BindingExpr / LiveBinding validation ──────────────────────────

function looksLiveBinding(v: unknown): v is Record<string, unknown> {
  return isObject(v) && "path" in v && !("compute" in v);
}

function looksComputed(v: unknown): v is Record<string, unknown> {
  return isObject(v) && "compute" in v;
}

/** For a grammar-valid Binding path that begins `$context.<name>`, verify `<name>`
 * is a declared context feed (UIS-105); an undeclared feed fails
 * CONTEXT_REF_UNDEFINED. A no-op for any path with no `$context` root. */
function checkContextRef(s: string, path: string, ctx: Ctx): void {
  const segments = splitTopLevel(s, ".");
  if (segments[0] !== "$context") return;
  // The feed name is the second segment up to any index/predicate bracket, e.g.
  // `$context.ghosts[id=$ui.selected]` → `ghosts`. (A plain split on `[`, since
  // splitTopLevel treats `[` as a depth marker, never a separator.)
  const name = segments[1] ? segments[1].split("[")[0] : undefined;
  if (!name || !ctx.contextKeys.has(name)) {
    fail(ctx, "CONTEXT_REF_UNDEFINED", path, `$context.${name ?? ""} is not a declared context feed (UIS-105)`);
  }
}

/** Validate a LiveBinding `{ path, live: true }` (UIS-109). */
function validateLiveBinding(v: Record<string, unknown>, path: string, ctx: Ctx): void {
  if (v.live !== true) {
    fail(ctx, "BINDING_PATH_INVALID", path, "a LiveBinding's `live` must be literally true (UIS-109)");
    return;
  }
  if (!isValidBindingPath(v.path)) {
    fail(ctx, "BINDING_PATH_INVALID", `${path}.path`, "a LiveBinding's `path` must be a valid Binding (UIS-109)");
    return;
  }
  // A LiveBinding's `path` is a Binding (UIS-100); a `$context.<name>` in it is
  // subject to the same existence check a bare-string Binding gets (UIS-105).
  checkContextRef(v.path as string, `${path}.path`, ctx);
}

/** Validate a bare-string Binding, checking grammar (UIS-100) and, for a
 * `$context.<name>` reference, that `<name>` is a declared context feed (UIS-105).
 * When `writeTarget`, also enforce the no-write-to-`$index` rule (UIS-107). */
function validateBindingString(s: string, path: string, ctx: Ctx, writeTarget = false): void {
  if (!isValidBindingPath(s)) {
    fail(ctx, "BINDING_PATH_INVALID", path, `"${s}" does not match the UIS-100 data-path grammar`);
    return;
  }
  if (writeTarget && targetsIndex(s)) {
    fail(ctx, "BINDING_PATH_INVALID", path, "cannot write to the read-only $index segment (UIS-107)");
    return;
  }
  checkContextRef(s, path, ctx);
}

/** A Binding-typed position (UIS-066): a bare-string Binding or a LiveBinding. */
function validateBinding(v: unknown, path: string, ctx: Ctx, writeTarget = false): void {
  if (typeof v === "string") {
    validateBindingString(v, path, ctx, writeTarget);
    return;
  }
  if (looksLiveBinding(v)) {
    validateLiveBinding(v, path, ctx);
    return;
  }
  fail(ctx, "BINDING_PATH_INVALID", path, "expected a Binding string or LiveBinding (UIS-066/109)");
}

// ── Root Bindings (UIS-005/UIS-023) ─────────────────────────────────────────
// A root Binding (list.source, detail.source, settings-form source, wizard
// draftSource) resolves against the page-level resource namespace rather than an
// enclosing Scope (UIS-005), where a collection or single record may be named by
// a REST-ish path — one or more resource segments joined by "/" (e.g.
// "automations/<id>"). That is a superset of the Scope-relative UIS-100 grammar:
// each "/"-delimited part is itself a UIS-100 path (a collection, optionally
// index-/predicate-selected) OR a bare resource id (a ULID/opaque key). It is
// still a Binding-typed position (UIS-066), so a part matching neither form —
// e.g. an empty segment (`a..b`) or a malformed predicate (`rows[id=]`) — MUST
// fail as BINDING_PATH_INVALID.

const RESOURCE_ID = /^[A-Za-z0-9][A-Za-z0-9_-]*$/;

function isValidRootBindingString(s: string): boolean {
  if (typeof s !== "string" || s.length === 0) return false;
  const parts = s.split("/");
  return parts.every((part) => part.length > 0 && (isValidBindingPath(part) || RESOURCE_ID.test(part)));
}

/** Validate a `list.source` paginated source object (UIS-023): `{ path, paginated:
 * true, limit? }`. `path` is a Binding (UIS-100/root-Binding), so a source missing
 * `path`, or whose `paginated` is not literally `true`, MUST fail as
 * BINDING_PATH_INVALID. */
function validatePaginatedSource(v: Record<string, unknown>, path: string, ctx: Ctx): void {
  if (v.path === undefined) {
    fail(ctx, "BINDING_PATH_INVALID", `${path}.path`, "a paginated list.source must declare a `path` Binding (UIS-023)");
  } else if (typeof v.path !== "string" || !isValidRootBindingString(v.path)) {
    fail(ctx, "BINDING_PATH_INVALID", `${path}.path`, `"${String(v.path)}" is not a valid Binding for a paginated list.source (UIS-023)`);
  }
  if (v.paginated !== true) {
    fail(ctx, "BINDING_PATH_INVALID", `${path}.paginated`, "a paginated list.source must set `paginated` literally true (UIS-023)");
  }
}

/** A root Binding (UIS-005): a REST-ish resource-path string, a LiveBinding
 * (UIS-109), or — for `list.source` only (`allowPaginated`) — a paginated source
 * object (UIS-023). Any other shape is a malformed Binding (BINDING_PATH_INVALID). */
function validateRootBinding(v: unknown, path: string, ctx: Ctx, allowPaginated = false): void {
  if (typeof v === "string") {
    if (!isValidRootBindingString(v)) {
      fail(ctx, "BINDING_PATH_INVALID", path, `"${v}" does not match the UIS-005/UIS-100 root-Binding grammar`);
      return;
    }
    // A root Binding endorses `$context.<name>` as a source (UIS-023); UIS-105's
    // existence check is unconditional, so each `/`-delimited part that is a
    // `$context` reference must resolve to a declared feed — grammar alone is not
    // enough, exactly as it is not for a Scope-relative Binding.
    for (const part of v.split("/")) checkContextRef(part, path, ctx);
    return;
  }
  if (isObject(v)) {
    if ("live" in v) {
      validateLiveBinding(v, path, ctx);
      return;
    }
    if (allowPaginated) {
      validatePaginatedSource(v, path, ctx);
      return;
    }
  }
  fail(ctx, "BINDING_PATH_INVALID", path, "expected a Binding string or LiveBinding root source (UIS-005/109)");
}

/** A BindingExpr position (UIS-108/141): a Binding, a Computed, or a JSON literal.
 * A bare string is a Binding (never a string literal); an object with `compute`
 * is a Computed; an object with `path` is a LiveBinding; `{lit}`/other objects/
 * arrays/numbers/booleans/null are literals and need no validation. */
function validateBindingExpr(v: unknown, path: string, ctx: Ctx): void {
  if (typeof v === "string") {
    validateBindingString(v, path, ctx);
    return;
  }
  if (looksComputed(v)) {
    validateComputed(v, path, ctx);
    return;
  }
  if (looksLiveBinding(v)) {
    validateLiveBinding(v, path, ctx);
    return;
  }
  // literal (number, boolean, null, {lit}, other object, array) — nothing to check
}

// ── Computed values (UIS-140) ───────────────────────────────────────────────

// Arg positions a compute function pins to a non-Binding type, so the validator
// must NOT grammar-check them as data paths (UIS-140): label's vocabRef, msg's
// msgRef, firstKey's literal key-array, formatCurrency's pinned currency code.
const COMPUTE_SKIP_GRAMMAR: Record<string, Set<number>> = {
  label: new Set([0]), // vocabRef
  msg: new Set([0]), // msgRef
  firstKey: new Set([1]), // literal candidate-key array
  formatCurrency: new Set([1]), // pinned ISO 4217 currency code (UIS-143)
};

function validateComputed(v: Record<string, unknown>, path: string, ctx: Ctx): void {
  const fn = v.compute;
  if (typeof fn !== "string" || !(COMPUTE_FNS as readonly string[]).includes(fn)) {
    fail(ctx, "COMPUTE_FN_UNKNOWN", `${path}.compute`, `"${String(fn)}" is not a ui-schema/1 Computed function (UIS-140)`);
    return;
  }
  const args = Array.isArray(v.args) ? v.args : [];
  // label(vocabRef, valueBinding): arg0 is a vocabRef pinned to the UIS-120 closed
  // set (UIS-140), enforced as a closed set (UIS-121). It MUST be present and a
  // member of the table, so an absent (missing/empty args), non-string, or
  // out-of-set vocabRef fails VOCAB_REF_UNKNOWN — never a silent skip.
  if (fn === "label") {
    const ref = args[0];
    if (typeof ref !== "string" || !(ref in VOCAB_TABLE)) {
      fail(ctx, "VOCAB_REF_UNKNOWN", `${path}.args[0]`, `"${String(ref)}" is not a member of the UIS-120 vocabRef table`);
    }
  }
  // formatCurrency(numberBinding, currencyCode): arg1 is a pinned ISO 4217
  // currency code (UIS-140/143), enforced the same way label's vocabRef (above)
  // is — a business fact of the priced row, not a viewer preference, so it MUST
  // be present and well-formed. An absent (missing/empty args), non-string, or
  // malformed code fails CURRENCY_CODE_INVALID (UIS-144) — never a silent
  // runtime fallback to USD, which would render the wrong currency with no
  // error surfaced anywhere in the pipeline.
  if (fn === "formatCurrency") {
    const code = args[1];
    if (typeof code !== "string" || !ISO_4217_PATTERN.test(code)) {
      fail(
        ctx,
        "CURRENCY_CODE_INVALID",
        `${path}.args[1]`,
        `"${String(code)}" is not a well-formed ISO 4217 currency code (UIS-144)`,
      );
    }
  }
  const skip = COMPUTE_SKIP_GRAMMAR[fn] ?? new Set<number>();
  args.forEach((arg, i) => {
    const argPath = `${path}.args[${i}]`;
    if (fn === "label" && i === 0) return; // vocabRef — enforced above, never grammar-checked
    if (skip.has(i)) return; // pinned non-Binding literal (msgRef, key-array)
    validateBindingExpr(arg, argPath, ctx);
  });
}

// ── OptionSource (UIS-130–132) ──────────────────────────────────────────────

function validateOptionSource(v: unknown, path: string, ctx: Ctx): void {
  if (!isObject(v) || typeof v.kind !== "string" || !(OPTION_SOURCE_KINDS as readonly string[]).includes(v.kind)) {
    fail(ctx, "OPTION_SOURCE_INVALID", path, "options.kind must be one of literal|vocab|data (UIS-130)");
    return;
  }
  if (v.kind === "literal") {
    if (!Array.isArray(v.items) || v.items.length === 0) {
      fail(ctx, "OPTION_SOURCE_INVALID", `${path}.items`, "a literal OptionSource needs a non-empty items array (UIS-131)");
    }
    return;
  }
  if (v.kind === "vocab") {
    const ref = v.ref;
    if (typeof ref !== "string" || !(ref in VOCAB_TABLE)) {
      fail(ctx, "VOCAB_REF_UNKNOWN", `${path}.ref`, `"${String(ref)}" is not a member of the UIS-120 vocabRef table`);
      return;
    }
    const labels = v.labels;
    if (!isObject(labels)) {
      fail(ctx, "OPTION_SOURCE_INVALID", `${path}.labels`, "a vocab OptionSource needs a labels map (UIS-132)");
      return;
    }
    for (const member of VOCAB_TABLE[ref]) {
      if (!(member in labels)) {
        fail(
          ctx,
          "OPTION_SOURCE_INVALID",
          `${path}.labels`,
          `labels map is missing an entry for vocabRef ${ref} member "${member}"`,
        );
        return; // one error is enough to reject; the corpus expects a single error
      }
    }
    return;
  }
  // kind === "data"
  if (typeof v.source !== "string" && !looksLiveBinding(v.source)) {
    fail(ctx, "OPTION_SOURCE_INVALID", `${path}.source`, "a data OptionSource needs a source Binding (UIS-133)");
  } else {
    validateBinding(v.source, `${path}.source`, ctx);
  }
  if (typeof v.valuePath !== "string" || typeof v.labelPath !== "string") {
    fail(ctx, "OPTION_SOURCE_INVALID", path, "a data OptionSource needs valuePath and labelPath (UIS-133)");
  }
}

// ── Actions (UIS-160) ───────────────────────────────────────────────────────

/** An ActionRef `params` field (UIS-160): an object of literal/Binding values.
 * A wrongly-shaped `params` fails as ACTION_FIELDS_INVALID; each string entry is a
 * Binding (UIS-108/UIS-184) and is grammar-checked (BINDING_PATH_INVALID). */
function validateActionParams(value: unknown, path: string, ctx: Ctx): void {
  if (value === undefined) return;
  if (!isObject(value)) {
    fail(ctx, "ACTION_FIELDS_INVALID", path, "`params` must be an object of literal/Binding values (UIS-160)");
    return;
  }
  for (const [k, pv] of Object.entries(value)) {
    // A JSON string is a Binding (UIS-108); non-strings are literals, not checked.
    if (typeof pv === "string") validateBindingString(pv, `${path}.${k}`, ctx);
  }
}

// UIS-164: a `create` ActionRef's field set is closed to exactly these keys. Its
// optional envelope-seed fields (`scopeFrom`/`lifecycle`) degrade SILENTLY to the
// host default when absent, so an unrecognized field (a misspelling) is a typed
// rejection, never accepted unvalidated on the theory the renderer defaults later.
// `confirm`/`outcomeTo` (UIS-165/166) are ActionRef ENVELOPE fields, valid on every
// verb, so they are never an "unrecognized create field" — omitting them here
// would make the create idiom the one verb that cannot be confirmed or observed.
const CREATE_FIELDS = new Set(["verb", "target", "itemDefault", "scopeFrom", "lifecycle", "confirm", "outcomeTo"]);

const CONFIRM_FIELD_SET = new Set<string>(CONFIRM_FIELDS);

/** Validate an ActionRef's `confirm` ConfirmSpec (UIS-165): a closed field set, a
 * REQUIRED `msg:` `titleMsg` (the dialog's accessible name — the same argument
 * UIS-075 makes for an input's `labelMsg`), and a boolean `destructive`. Every
 * violation is ACTION_FIELDS_INVALID at the offending field's own path: a confirm
 * that silently failed to render would hand the operator the verb with no warning,
 * which is worse than not offering the confirm at all. */
function validateConfirmSpec(value: unknown, path: string, ctx: Ctx): void {
  if (!isObject(value)) {
    fail(ctx, "ACTION_FIELDS_INVALID", path, "`confirm` must be a ConfirmSpec object (UIS-165)");
    return;
  }
  for (const key of Object.keys(value)) {
    if (!CONFIRM_FIELD_SET.has(key)) {
      fail(ctx, "ACTION_FIELDS_INVALID", `${path}.${key}`, `a ConfirmSpec has no field "${key}" (UIS-165)`);
    }
  }
  if (typeof value.titleMsg !== "string") {
    fail(
      ctx,
      "ACTION_FIELDS_INVALID",
      `${path}.titleMsg`,
      "a ConfirmSpec requires a `titleMsg` msg reference — it is the dialog's accessible name (UIS-165)",
    );
  }
  for (const key of ["bodyMsg", "confirmLabelMsg", "cancelLabelMsg"] as const) {
    if (value[key] !== undefined && typeof value[key] !== "string") {
      fail(ctx, "ACTION_FIELDS_INVALID", `${path}.${key}`, `a ConfirmSpec's \`${key}\` must be a msg reference (UIS-165)`);
    }
  }
  if (value.destructive !== undefined && typeof value.destructive !== "boolean") {
    fail(ctx, "ACTION_FIELDS_INVALID", `${path}.destructive`, "a ConfirmSpec's `destructive` must be a boolean (UIS-165)");
  }
}

/** Validate an ActionRef's `outcomeTo` (UIS-166/167): valid only on a seam verb,
 * and only as a `$ui.<name>[.<name>…]` destination.
 *
 * The two rules fail as different codes on purpose. A verb that cannot produce an
 * outcome is a wrongly-shaped ActionRef (ACTION_FIELDS_INVALID); a destination
 * outside `$ui` is a Binding naming a place this field forbids, which is the same
 * shape as writing to `$index` (BINDING_PATH_INVALID, UIS-107). */
function validateOutcomeTo(value: unknown, verb: string, path: string, ctx: Ctx): void {
  if (!(OUTCOME_VERBS as readonly string[]).includes(verb)) {
    fail(
      ctx,
      "ACTION_FIELDS_INVALID",
      path,
      `"${verb}" completes locally and produces no outcome — \`outcomeTo\` is valid only on ${OUTCOME_VERBS.join("/")} (UIS-167)`,
    );
    return;
  }
  if (typeof value !== "string") {
    fail(ctx, "BINDING_PATH_INVALID", path, "`outcomeTo` must be a Binding string rooted at $ui (UIS-166)");
    return;
  }
  if (!isValidBindingPath(value) || parseOutcomeTarget(value) === null) {
    fail(
      ctx,
      "BINDING_PATH_INVALID",
      path,
      `"${value}" is not a legal outcome destination — it must name $ui.<name>[.<name>…] (UIS-166)`,
    );
  }
}

function validateActionRef(v: unknown, path: string, ctx: Ctx): void {
  if (!isObject(v) || typeof v.verb !== "string") {
    fail(ctx, "ACTION_FIELDS_INVALID", path, "an ActionRef must be an object with a verb (UIS-160)");
    return;
  }
  const verb = v.verb;
  if (!(ACTION_VERBS as readonly string[]).includes(verb)) {
    fail(ctx, "ACTION_VERB_UNKNOWN", `${path}.verb`, `"${verb}" is not a ui-schema/1 action verb (UIS-160)`);
    return;
  }
  const av = verb as ActionVerb;
  if (WIZARD_ONLY_VERBS.includes(av) && !ctx.inWizard) {
    fail(ctx, "ACTION_FIELDS_INVALID", `${path}.verb`, `"${verb}" is valid only inside a wizard page (UIS-163)`);
    return;
  }
  // The two verb-independent ENVELOPE fields (UIS-160), validated before the
  // per-verb switch so they are checked identically on every verb — including the
  // wizard verbs and `navigate`, which the switch below has nothing to say about.
  if (v.confirm !== undefined) validateConfirmSpec(v.confirm, `${path}.confirm`, ctx);
  if (v.outcomeTo !== undefined) validateOutcomeTo(v.outcomeTo, verb, `${path}.outcomeTo`, ctx);
  const needBinding = (key: string): void => {
    if (v[key] === undefined) {
      fail(ctx, "ACTION_FIELDS_INVALID", `${path}.${key}`, `${verb} requires a "${key}" field (UIS-160)`);
    } else {
      validateBinding(v[key], `${path}.${key}`, ctx, key === "target");
    }
  };
  switch (av) {
    case "navigate":
      if (typeof v.to !== "string") {
        fail(ctx, "ACTION_FIELDS_INVALID", `${path}.to`, "navigate requires a `to` path template (UIS-160)");
      }
      validateActionParams(v.params, `${path}.params`, ctx);
      break;
    case "submit":
      if (v.target !== undefined) validateBinding(v.target, `${path}.target`, ctx, true);
      break;
    case "create":
      needBinding("target");
      // The create idiom's closed field set (UIS-164): an unrecognized field —
      // typically a misspelled optional seed field, e.g. `scopeForm` — fails
      // rather than silently degrading the new row's scope/lifecycle to a default.
      for (const key of Object.keys(v)) {
        if (!CREATE_FIELDS.has(key)) {
          fail(ctx, "ACTION_FIELDS_INVALID", `${path}.${key}`, `create has no field "${key}" (UIS-164)`);
        }
      }
      // `scopeFrom` is a once-resolved Binding (UIS-164, resolved like a bare-string
      // Binding at scope establishment, UIS-110 — never a LiveBinding) sourcing the
      // new row's `scope_node`; a non-string or grammar-malformed value fails
      // BINDING_PATH_INVALID (UIS-066), the same as any other Binding-typed field.
      if (v.scopeFrom !== undefined) {
        if (typeof v.scopeFrom !== "string") {
          fail(ctx, "BINDING_PATH_INVALID", `${path}.scopeFrom`, "create `scopeFrom` must be a Binding string (UIS-164)");
        } else {
          validateBindingString(v.scopeFrom, `${path}.scopeFrom`, ctx);
        }
      }
      // `lifecycle` is a literal string (UIS-108, never the string-is-Binding rule)
      // seeding `lifecycle_state`; a non-string is a wrongly-shaped field (UIS-160).
      if (v.lifecycle !== undefined && typeof v.lifecycle !== "string") {
        fail(ctx, "ACTION_FIELDS_INVALID", `${path}.lifecycle`, "create `lifecycle` must be a literal string (UIS-164)");
      }
      break;
    case "delete":
      needBinding("target");
      break;
    case "call-action":
      if (typeof v.action !== "string") {
        fail(ctx, "ACTION_FIELDS_INVALID", `${path}.action`, "call-action requires an `action` name (UIS-160)");
      }
      validateActionParams(v.params, `${path}.params`, ctx);
      break;
    case "set":
      needBinding("target");
      if (v.value === undefined) {
        fail(ctx, "ACTION_FIELDS_INVALID", `${path}.value`, "set requires a `value` (UIS-160)");
      } else {
        validateBindingExpr(v.value, `${path}.value`, ctx);
      }
      break;
    case "repeat-add":
      needBinding("target");
      break;
    case "repeat-remove":
      // target is an itemScope reference to the item being removed (UIS-162); it
      // must be present, and its own path (typically `item`) is grammar-checked.
      if (typeof v.target !== "string") {
        fail(ctx, "ACTION_FIELDS_INVALID", `${path}.target`, "repeat-remove requires an itemScope target (UIS-162)");
      } else {
        validateBindingString(v.target, `${path}.target`, ctx);
      }
      break;
    case "wizard-next":
    case "wizard-back":
    case "wizard-finish":
      break;
  }
}

// ── Widget node walk (UIS-060–075) ──────────────────────────────────────────

function validatePropByKind(def: PropDef, value: unknown, path: string, ctx: Ctx): void {
  switch (def.kind) {
    case "bindingExpr":
      validateBindingExpr(value, path, ctx);
      break;
    case "binding":
      validateBinding(value, path, ctx);
      break;
    case "options":
      validateOptionSource(value, path, ctx);
      break;
    case "widget":
      validateWidget(value, path, ctx);
      break;
    case "cases":
      // UIS-070: a non-empty array of {when, render} — `when` a JSON literal (any
      // JSON value, so presence-checked, never truthiness-checked), `render` a widget.
      if (!Array.isArray(value) || value.length === 0) {
        fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", path, "switch.cases must be a non-empty array of {when, render} (UIS-070)");
        break;
      }
      value.forEach((c, i) => {
        if (!isObject(c) || !("when" in c)) {
          fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", `${path}[${i}].when`, "a switch case must declare `when` (UIS-070)");
        }
        if (!isObject(c) || !("render" in c)) {
          fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", `${path}[${i}].render`, "a switch case must declare `render` (UIS-070)");
        } else {
          validateWidget(c.render, `${path}[${i}].render`, ctx);
        }
      });
      break;
    case "columns":
      // UIS-070/071a: a non-empty array of columns. A column declares EXACTLY ONE
      // of `cell` (a BindingExpr rendered as a value) or `cellWidget` (a widget
      // subtree rendered per row, `item` in scope) — never both, never neither.
      // A `cellWidget` column MAY add `cellValue`, the scalar it contributes for
      // sorting and search, which a rendered subtree cannot supply on its own.
      if (!Array.isArray(value) || value.length === 0) {
        fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", path, "table.columns must be a non-empty array of {headerMsg, cell|cellWidget} (UIS-070/071a)");
        break;
      }
      value.forEach((col, i) => {
        if (!isObject(col) || !("headerMsg" in col)) {
          fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", `${path}[${i}].headerMsg`, "a table column must declare `headerMsg` (UIS-070)");
        }
        const hasCell = isObject(col) && "cell" in col && col.cell !== undefined;
        const hasWidget = isObject(col) && "cellWidget" in col && col.cellWidget !== undefined;
        if (!hasCell && !hasWidget) {
          fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", `${path}[${i}].cell`, "a table column must declare `cell` or `cellWidget` (UIS-070/071a)");
          return;
        }
        if (hasCell && hasWidget) {
          // The two keys select mutually exclusive column shapes, so in a `cell`
          // column `cellWidget` is a key the shape does not declare (UIS-071a).
          fail(ctx, "WIDGET_PROP_UNKNOWN", `${path}[${i}].cellWidget`, "a table column declaring `cell` may not also declare `cellWidget` (UIS-071a)");
          return;
        }
        if (hasCell) {
          validateBindingExpr((col as { cell: unknown }).cell, `${path}[${i}].cell`, ctx);
          if (isObject(col) && "cellValue" in col && col.cellValue !== undefined) {
            // `cellValue` exists to give a rendered SUBTREE a sort/search key; a
            // `cell` column's own BindingExpr already is one (UIS-071a).
            fail(ctx, "WIDGET_PROP_UNKNOWN", `${path}[${i}].cellValue`, "`cellValue` belongs to a `cellWidget` column (UIS-071a)");
          }
          return;
        }
        validateWidget((col as { cellWidget: unknown }).cellWidget, `${path}[${i}].cellWidget`, ctx);
        if (isObject(col) && "cellValue" in col && col.cellValue !== undefined) {
          validateBindingExpr(col.cellValue, `${path}[${i}].cellValue`, ctx);
        }
      });
      break;
    case "fragmentRef":
      if (typeof value !== "string" || !ctx.fragmentKeys.has(value)) {
        fail(ctx, "FRAGMENT_REF_UNDEFINED", path, `fragment ref "${String(value)}" does not resolve (UIS-180)`);
      }
      break;
    case "slotName":
      if (typeof value === "string") ctx.slotNames.push({ name: value, path });
      break;
    case "params":
      if (isObject(value)) {
        for (const [k, pv] of Object.entries(value)) {
          if (typeof pv === "string") validateBindingString(pv, `${path}.${k}`, ctx);
        }
      }
      break;
    // msg | bool | int | number | enum | modes | keyArray — presence-only; there is
    // no taxonomy code for a wrong scalar type, so the validator never invents one.
    default:
      break;
  }
}

/** UIS-073a: an `entity-picker`'s declared binding shape and the forms it can
 * carry. A `bindShape` outside the closed set would otherwise fall back to the
 * object shape and paint an empty control over a record that does carry an
 * entity; a `selector`/`deviceClass` mode has nowhere to live in a bare scalar.
 * Both fail as BINDING_TYPE_MISMATCH at the offending prop's own path. */
function validateEntityPickerShape(props: Record<string, unknown>, path: string, ctx: Ctx): void {
  const shape = props.bindShape;
  if (shape !== undefined && !(ENTITY_PICKER_BIND_SHAPES as readonly unknown[]).includes(shape)) {
    fail(
      ctx,
      "BINDING_TYPE_MISMATCH",
      `${path}.props.bindShape`,
      `"${String(shape)}" is not an entity-picker binding shape — expected one of ${ENTITY_PICKER_BIND_SHAPES.join(", ")} (UIS-073a)`,
    );
    return; // an unknown shape says nothing about which modes it can carry
  }
  if (shape !== ENTITY_PICKER_SCALAR_SHAPE || !Array.isArray(props.modes)) return;
  const unusable = props.modes.filter((m) => !ENTITY_PICKER_SCALAR_MODES.includes(String(m)));
  if (unusable.length > 0) {
    fail(
      ctx,
      "BINDING_TYPE_MISMATCH",
      `${path}.props.modes`,
      `bindShape "${ENTITY_PICKER_SCALAR_SHAPE}" binds a bare entity id, which cannot carry the ${unusable.map(String).join("/")} form (UIS-073a)`,
    );
  }
}

function validateWidget(node: unknown, path: string, ctx: Ctx): void {
  if (!isObject(node)) {
    fail(ctx, "UNKNOWN_WIDGET_TYPE", path, "a widget node must be an object with a type (UIS-060)");
    return;
  }
  const type = node.type;
  if (typeof type !== "string" || !(type in WIDGET_CATALOG)) {
    fail(ctx, "UNKNOWN_WIDGET_TYPE", `${path}.type`, `"${String(type)}" is not a member of the ui-schema/1 Widget catalog`);
    return; // do not descend into an unknown type — its shape is unknowable
  }
  const spec: WidgetSpec = WIDGET_CATALOG[type];

  // children (UIS-064)
  if ("children" in node && node.children !== undefined) {
    if (!spec.children) {
      fail(ctx, "WIDGET_PROP_UNKNOWN", `${path}.children`, `${type} is not a children-bearing widget (UIS-064)`);
    } else if (Array.isArray(node.children)) {
      node.children.forEach((child, i) => validateWidget(child, `${path}.children[${i}]`, ctx));
    }
  }

  // props (UIS-062): only declared keys, all required keys present
  const props = isObject(node.props) ? node.props : {};
  for (const key of Object.keys(props)) {
    if (!(key in spec.props)) {
      fail(ctx, "WIDGET_PROP_UNKNOWN", `${path}.props.${key}`, `${type} has no prop "${key}" (UIS-062)`);
    }
  }
  for (const [key, def] of Object.entries(spec.props)) {
    const present = key in props && props[key] !== undefined;
    if (!present) {
      if (def.required) {
        // Includes every input-category widget's `labelMsg` (UIS-075), marked
        // required in the catalog, so a missing accessible label fails here.
        fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", `${path}.props.${key}`, `${type} requires prop "${key}" (UIS-062/075)`);
      }
      continue;
    }
    validatePropByKind(def, props[key], `${path}.props.${key}`, ctx);
  }

  // entity-picker binding shape (UIS-073a). Two checks the generic prop kinds
  // cannot express, both enforced rather than left to render time (UIS-200):
  // `bindShape` is a closed two-member set, and the scalar shape can only carry
  // the `entity` form, so a `modes` naming `selector`/`deviceClass` beside it is
  // a static contradiction.
  if (type === "entity-picker") validateEntityPickerShape(props, path, ctx);

  // on (UIS-062): only declared events, required events present, each an ActionRef
  const on = isObject(node.on) ? node.on : {};
  for (const [ev, def] of Object.entries(spec.events)) {
    if (def.required && !(ev in on)) {
      fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", `${path}.on.${ev}`, `${type} requires the "${ev}" event (UIS-074)`);
    }
  }
  for (const ev of Object.keys(on)) {
    if (!(ev in spec.events)) {
      fail(ctx, "WIDGET_EVENT_UNKNOWN", `${path}.on.${ev}`, `${type} has no event "${ev}" (UIS-062)`);
    } else {
      validateActionRef(on[ev], `${path}.on.${ev}`, ctx);
    }
  }

  // bind (UIS-065/066/075)
  const bind = node.bind;
  if (bind !== undefined) {
    validateBinding(bind, `${path}.bind`, ctx, spec.category === "input");
  } else if (spec.bind === "required") {
    fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", `${path}.bind`, `${type} requires a bind (UIS-065)`);
  }

  // visibleIf (UIS-063) — a BindingExpr gating render
  if ("visibleIf" in node && node.visibleIf !== undefined) {
    validateBindingExpr(node.visibleIf, `${path}.visibleIf`, ctx);
  }
}

// ── Page-type structural walks ──────────────────────────────────────────────
//
// Each page type's required substructure (UIS-020/030/031/040/050) MUST be
// present, not merely well-formed-when-present: a document missing its whole
// required substructure is non-conformant and MUST be flagged (UIS-200 forbids
// accepting it unvalidated on the theory the renderer fails later). The taxonomy
// has no dedicated page-structural code, so an absent required structural field
// reuses WIDGET_REQUIRED_FIELD_MISSING — the contract's single "a required field
// is absent" code — at the field's own path.

function walkListDetail(doc: Record<string, unknown>, ctx: Ctx): void {
  const list = doc.list;
  if (!isObject(list)) {
    fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", "list", "a list-detail page must declare `list` as {source, display} (UIS-020)");
  } else {
    if (list.source === undefined) {
      fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", "list.source", "list-detail requires `list.source` (UIS-020)");
    } else {
      validateRootBinding(list.source, "list.source", ctx, true); // list.source MAY be a paginated source (UIS-023)
    }
    if (list.display === undefined) {
      fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", "list.display", "list-detail requires a `list.display` widget (UIS-020)");
    } else {
      validateWidget(list.display, "list.display", ctx);
    }
  }
  const detail = doc.detail;
  if (!isObject(detail)) {
    fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", "detail", "a list-detail page must declare `detail` as {source, root} (UIS-020)");
  } else {
    if (detail.source === undefined) {
      fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", "detail.source", "list-detail requires `detail.source` (UIS-020)");
    } else {
      validateRootBinding(detail.source, "detail.source", ctx);
    }
    if (detail.root === undefined) {
      fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", "detail.root", "list-detail requires a `detail.root` widget (UIS-020)");
    } else {
      validateWidget(detail.root, "detail.root", ctx);
    }
  }
  if (doc.newAction !== undefined) validateActionRef(doc.newAction, "newAction", ctx);
}

function walkSettingsForm(doc: Record<string, unknown>, ctx: Ctx): void {
  // UIS-030: source (root Binding), sections (non-empty, each with non-empty fields).
  if (doc.source === undefined) {
    fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", "source", "a settings-form page must declare `source` (UIS-030)");
  } else {
    validateRootBinding(doc.source, "source", ctx);
  }
  if (!Array.isArray(doc.sections) || doc.sections.length === 0) {
    fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", "sections", "a settings-form page must declare a non-empty `sections` array (UIS-030)");
  } else {
    doc.sections.forEach((section, si) => {
      if (!isObject(section) || !Array.isArray(section.fields)) {
        fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", `sections[${si}].fields`, "a settings-form section must declare a `fields` array (UIS-030)");
        return;
      }
      if (section.fields.length === 0) {
        fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", `sections[${si}].fields`, "a settings-form section's `fields` must be non-empty (UIS-030)");
      }
      section.fields.forEach((field, fi) => validateWidget(field, `sections[${si}].fields[${fi}]`, ctx));
    });
  }
  // UIS-031: actions non-empty, at least one wiring on.press to a submit ActionRef.
  if (!Array.isArray(doc.actions) || doc.actions.length === 0) {
    fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", "actions", "a settings-form page must declare a non-empty `actions` array (UIS-031)");
  } else {
    doc.actions.forEach((action, ai) => validateWidget(action, `actions[${ai}]`, ctx));
    const hasSubmit = doc.actions.some(
      (a) => isObject(a) && isObject(a.on) && isObject(a.on.press) && (a.on.press as Record<string, unknown>).verb === "submit",
    );
    if (!hasSubmit) {
      fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", "actions", "a settings-form must wire at least one action's on.press to a submit ActionRef (UIS-031)");
    }
  }
}

// The closed dashboard tile-size enum (UIS-040) — the same three-value set
// manifest/1 MAN-061's `sizeHint` uses.
const TILE_SIZES = new Set(["small", "medium", "large"]);

function walkDashboard(doc: Record<string, unknown>, ctx: Ctx): void {
  // UIS-040: a dashboard MUST declare `tiles` as an array of {size, widget}. The
  // array MAY be empty; each tile's `size` is the closed small|medium|large enum.
  if (!Array.isArray(doc.tiles)) {
    fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", "tiles", "a dashboard page must declare a `tiles` array (UIS-040)");
    return;
  }
  doc.tiles.forEach((tile, ti) => {
    if (!isObject(tile)) {
      fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", `tiles[${ti}]`, "a dashboard tile must be an object {size, widget} (UIS-040)");
      return;
    }
    if (typeof tile.size !== "string" || !TILE_SIZES.has(tile.size)) {
      // A required, closed-valued structural field absent or outside its set —
      // reported with the "required field" code at the tile's own `size` path.
      fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", `tiles[${ti}].size`, "a dashboard tile `size` must be one of small|medium|large (UIS-040)");
    }
    if (tile.widget === undefined) {
      fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", `tiles[${ti}].widget`, "a dashboard tile must declare a `widget` (UIS-040)");
    } else {
      validateWidget(tile.widget, `tiles[${ti}].widget`, ctx);
    }
  });
}

function walkWizard(doc: Record<string, unknown>, ctx: Ctx): void {
  // UIS-050: steps (non-empty; each {id, titleMsg, root, canAdvanceIf?}) + onFinish.
  if (!Array.isArray(doc.steps) || doc.steps.length === 0) {
    fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", "steps", "a wizard page must declare a non-empty `steps` array (UIS-050)");
  } else {
    const seen = new Set<string>();
    doc.steps.forEach((step, si) => {
      if (!isObject(step)) {
        fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", `steps[${si}]`, "a wizard step must be an object {id, titleMsg, root} (UIS-050)");
        return;
      }
      if (typeof step.id !== "string") {
        fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", `steps[${si}].id`, "a wizard step must declare an `id` (UIS-050)");
      } else {
        if (seen.has(step.id)) {
          fail(ctx, "WIZARD_STEP_ID_DUPLICATE", `steps[${si}].id`, `duplicate wizard step id "${step.id}" (UIS-050)`);
        }
        seen.add(step.id);
      }
      if (step.titleMsg === undefined) {
        fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", `steps[${si}].titleMsg`, "a wizard step must declare a `titleMsg` (UIS-050)");
      }
      if (step.root === undefined) {
        fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", `steps[${si}].root`, "a wizard step must declare a `root` widget (UIS-050)");
      } else {
        validateWidget(step.root, `steps[${si}].root`, ctx);
      }
      if (step.canAdvanceIf !== undefined) validateBindingExpr(step.canAdvanceIf, `steps[${si}].canAdvanceIf`, ctx);
    });
  }
  if (doc.onFinish === undefined) {
    fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", "onFinish", "a wizard page must declare an `onFinish` ActionRef (UIS-050)");
  } else {
    validateActionRef(doc.onFinish, "onFinish", ctx);
  }
  // draftSource is optional (UIS-051); when declared it is a root Binding.
  if (doc.draftSource !== undefined) validateRootBinding(doc.draftSource, "draftSource", ctx);
}

// ── Entry point ─────────────────────────────────────────────────────────────

/** Validate a ui-schema/1 page document against the contract's closed sets and
 * grammar (UIS-200). Returns `{ ok: true }` for a conformant document, or
 * `{ ok: false, errors }` with one typed rejection per violation. */
export function validatePage(doc: unknown): ValidationResult {
  const ctx: Ctx = {
    errors: [],
    fragmentKeys: new Set(),
    contextKeys: new Set(),
    slotNames: [],
    inWizard: false,
  };

  if (!isObject(doc)) {
    fail(ctx, "UNKNOWN_PAGE_TYPE", "", "a page document must be an object with a pageType (UIS-003)");
    return { ok: false, errors: ctx.errors };
  }

  const pageType = doc.pageType;
  if (typeof pageType !== "string" || !(PAGE_TYPES as readonly string[]).includes(pageType)) {
    fail(ctx, "UNKNOWN_PAGE_TYPE", "pageType", `"${String(pageType)}" is not a ui-schema/1 page type (UIS-003)`);
    return { ok: false, errors: ctx.errors };
  }

  if (isObject(doc.fragments)) ctx.fragmentKeys = new Set(Object.keys(doc.fragments));
  if (isObject(doc.context)) ctx.contextKeys = new Set(Object.keys(doc.context));
  ctx.inWizard = pageType === "wizard";

  // Fragments are reusable widget-node subtrees (UIS-004) — validate their own
  // shape too, so a malformed fragment is caught even before it is referenced.
  if (isObject(doc.fragments)) {
    for (const [name, root] of Object.entries(doc.fragments)) {
      validateWidget(root, `fragments.${name}`, ctx);
    }
  }

  switch (pageType as PageType) {
    case "list-detail":
      walkListDetail(doc, ctx);
      break;
    case "settings-form":
      walkSettingsForm(doc, ctx);
      break;
    case "dashboard":
      walkDashboard(doc, ctx);
      break;
    case "wizard":
      walkWizard(doc, ctx);
      break;
  }

  // Slot names must be unique within one page document (UIS-186).
  const byName = new Map<string, number>();
  for (const slot of ctx.slotNames) {
    byName.set(slot.name, (byName.get(slot.name) ?? 0) + 1);
  }
  for (const slot of ctx.slotNames) {
    if ((byName.get(slot.name) ?? 0) > 1) {
      fail(ctx, "SLOT_NAME_DUPLICATE", slot.path, `duplicate slot name "${slot.name}" (UIS-186)`);
      byName.set(slot.name, 0); // report each duplicated name once
    }
  }

  return ctx.errors.length === 0 ? { ok: true } : { ok: false, errors: ctx.errors };
}
