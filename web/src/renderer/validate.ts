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

import {
  ACTION_VERBS,
  COMPUTE_FNS,
  INPUT_TYPES,
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
  type ValidateOptions,
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
  strictInputLabels: boolean;
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

/** Validate a LiveBinding `{ path, live: true }` (UIS-109). */
function validateLiveBinding(v: Record<string, unknown>, path: string, ctx: Ctx): void {
  if (v.live !== true) {
    fail(ctx, "BINDING_PATH_INVALID", path, "a LiveBinding's `live` must be literally true (UIS-109)");
    return;
  }
  if (!isValidBindingPath(v.path)) {
    fail(ctx, "BINDING_PATH_INVALID", `${path}.path`, "a LiveBinding's `path` must be a valid Binding (UIS-109)");
  }
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
  const segments = splitTopLevel(s, ".");
  if (segments[0] === "$context") {
    const name = segments[1] ? splitTopLevel(segments[1], "[")[0] : undefined;
    if (!name || !ctx.contextKeys.has(name)) {
      fail(ctx, "CONTEXT_REF_UNDEFINED", path, `$context.${name ?? ""} is not a declared context feed (UIS-105)`);
    }
  }
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
// msgRef, firstKey's literal key-array.
const COMPUTE_SKIP_GRAMMAR: Record<string, Set<number>> = {
  label: new Set([0]), // vocabRef
  msg: new Set([0]), // msgRef
  firstKey: new Set([1]), // literal candidate-key array
};

function validateComputed(v: Record<string, unknown>, path: string, ctx: Ctx): void {
  const fn = v.compute;
  if (typeof fn !== "string" || !(COMPUTE_FNS as readonly string[]).includes(fn)) {
    fail(ctx, "COMPUTE_FN_UNKNOWN", `${path}.compute`, `"${String(fn)}" is not a ui-schema/1 Computed function (UIS-140)`);
    return;
  }
  const args = v.args;
  if (!Array.isArray(args)) return;
  const skip = COMPUTE_SKIP_GRAMMAR[fn] ?? new Set<number>();
  args.forEach((arg, i) => {
    const argPath = `${path}.args[${i}]`;
    if (fn === "label" && i === 0) {
      // label(vocabRef, valueBinding): arg0 is a vocabRef, not a data path.
      if (typeof arg === "string" && !(arg in VOCAB_TABLE)) {
        fail(ctx, "VOCAB_REF_UNKNOWN", argPath, `"${arg}" is not a member of the UIS-120 vocabRef table`);
      }
      return;
    }
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
      break;
    case "submit":
      if (v.target !== undefined) validateBinding(v.target, `${path}.target`, ctx, true);
      break;
    case "create":
      needBinding("target");
      break;
    case "delete":
      needBinding("target");
      break;
    case "call-action":
      if (typeof v.action !== "string") {
        fail(ctx, "ACTION_FIELDS_INVALID", `${path}.action`, "call-action requires an `action` name (UIS-160)");
      }
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
      if (Array.isArray(value)) {
        value.forEach((c, i) => {
          if (isObject(c) && "render" in c) validateWidget(c.render, `${path}[${i}].render`, ctx);
        });
      }
      break;
    case "columns":
      if (Array.isArray(value)) {
        value.forEach((col, i) => {
          if (isObject(col) && "cell" in col) validateBindingExpr(col.cell, `${path}[${i}].cell`, ctx);
        });
      }
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
      const requiredLabel = def.kind === "msg" && key === "labelMsg" && INPUT_TYPES.has(type) && ctx.strictInputLabels;
      if (def.required || requiredLabel) {
        fail(ctx, "WIDGET_REQUIRED_FIELD_MISSING", `${path}.props.${key}`, `${type} requires prop "${key}" (UIS-062/075)`);
      }
      continue;
    }
    validatePropByKind(def, props[key], `${path}.props.${key}`, ctx);
  }

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

function walkListDetail(doc: Record<string, unknown>, ctx: Ctx): void {
  const list = doc.list;
  if (isObject(list) && "display" in list) validateWidget(list.display, "list.display", ctx);
  const detail = doc.detail;
  if (isObject(detail) && "root" in detail) validateWidget(detail.root, "detail.root", ctx);
  if (doc.newAction !== undefined) validateActionRef(doc.newAction, "newAction", ctx);
  // list.source / detail.source are root Bindings against the page-level resource
  // namespace (UIS-005), where a collection may be named by a REST-ish path; they
  // are not subject to the Scope-relative UIS-100 grammar and are left opaque.
}

function walkSettingsForm(doc: Record<string, unknown>, ctx: Ctx): void {
  if (Array.isArray(doc.sections)) {
    doc.sections.forEach((section, si) => {
      if (isObject(section) && Array.isArray(section.fields)) {
        section.fields.forEach((field, fi) => validateWidget(field, `sections[${si}].fields[${fi}]`, ctx));
      }
    });
  }
  if (Array.isArray(doc.actions)) {
    doc.actions.forEach((action, ai) => validateWidget(action, `actions[${ai}]`, ctx));
  }
}

function walkDashboard(doc: Record<string, unknown>, ctx: Ctx): void {
  if (Array.isArray(doc.tiles)) {
    doc.tiles.forEach((tile, ti) => {
      if (isObject(tile) && "widget" in tile) validateWidget(tile.widget, `tiles[${ti}].widget`, ctx);
    });
  }
}

function walkWizard(doc: Record<string, unknown>, ctx: Ctx): void {
  const seen = new Set<string>();
  if (Array.isArray(doc.steps)) {
    doc.steps.forEach((step, si) => {
      if (!isObject(step)) return;
      if (typeof step.id === "string") {
        if (seen.has(step.id)) {
          fail(ctx, "WIZARD_STEP_ID_DUPLICATE", `steps[${si}].id`, `duplicate wizard step id "${step.id}" (UIS-050)`);
        }
        seen.add(step.id);
      }
      if ("root" in step) validateWidget(step.root, `steps[${si}].root`, ctx);
      if (step.canAdvanceIf !== undefined) validateBindingExpr(step.canAdvanceIf, `steps[${si}].canAdvanceIf`, ctx);
    });
  }
  if (doc.onFinish !== undefined) validateActionRef(doc.onFinish, "onFinish", ctx);
}

// ── Entry point ─────────────────────────────────────────────────────────────

/** Validate a ui-schema/1 page document against the contract's closed sets and
 * grammar (UIS-200). Returns `{ ok: true }` for a conformant document, or
 * `{ ok: false, errors }` with one typed rejection per violation. */
export function validatePage(doc: unknown, opts: ValidateOptions = {}): ValidationResult {
  const ctx: Ctx = {
    errors: [],
    fragmentKeys: new Set(),
    contextKeys: new Set(),
    slotNames: [],
    inWizard: false,
    strictInputLabels: opts.strictInputLabels ?? false,
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
