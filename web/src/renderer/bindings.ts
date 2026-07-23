// ui-schema/1 — the binding engine.
//
// The pure, React-free half of the renderer: it parses and evaluates the
// contract's binding grammar (UIS-100 data paths incl. the predicate-index form
// UIS-101), the Computed function list (UIS-140 incl. the locale-formatting
// functions UIS-143), and the OptionSource grammar (UIS-130–133 incl. the
// vocabulary references UIS-132). Nothing here touches the DOM or React state —
// a document that reached this module is already conformant (validate.ts ran
// first), so these functions evaluate the grammar rather than re-validate it.

import { VOCAB_TABLE } from "./schema";

// ── Scope model (UIS-005/103–107/183) ───────────────────────────────────────
//
// A widget node resolves its bindings against a RenderScope: the reserved roots
// ($root, $ui, $context, $params) plus the innermost narrowed scope (`current`,
// what an unprefixed binding reads) and, inside a `repeat`/`table` row, the
// active item scope (UIS-107). `current`/`currentPath`/`currentTree` also locate
// the scope inside the editable tree so an input write lands in the right place.

/** An absolute location inside one of the store's two trees (resource | ui). */
export type PathLoc = (string | number)[];

/** The active `repeat`/`table` item scope (UIS-107). `name` is the itemScope name
 * (default `item`); `arrayPath` locates the containing array so `repeat-remove`
 * (UIS-162) can splice it without the array being addressable by path. */
export interface ItemScope {
  name: string;
  value: unknown;
  index: number;
  arrayPath: PathLoc;
  arrayTree: "resource" | "ui";
}

export interface RenderScope {
  /** The page's outermost Scope value (`$root`, UIS-103). */
  root: unknown;
  /** The innermost narrowed scope — what an unprefixed Binding reads (UIS-103). */
  current: unknown;
  /** Absolute location of `current` in its tree, for write-back (UIS-065). */
  currentPath: PathLoc;
  /** Which store tree `current`/`currentPath` live in. */
  currentTree: "resource" | "ui";
  /** Ephemeral UI state, `$ui` (UIS-104). */
  ui: Record<string, unknown>;
  /** Context feeds, `$context.<name>` (UIS-105/150). */
  context: Record<string, unknown>;
  /** Fragment params, `$params.<name>` (UIS-106/184). */
  params?: Record<string, unknown>;
  /** The active item scope inside a `repeat`/`table` row (UIS-107). */
  item?: ItemScope;
  /** Set only inside a list-detail **create draft** (the New idiom, UIS-021): the
   * collection a `submit` here POSTs a NEW row to (through the create path,
   * UIS-160/161) instead of If-Match-updating a persisted record. Absent in every
   * ordinary edit/settings scope. */
  draftCreateTarget?: string;
}

// ── Path parsing (UIS-100) ──────────────────────────────────────────────────

type PredValue = { kind: "lit"; value: unknown } | { kind: "binding"; path: string };

interface Segment {
  name: string;
  index?: number | "$index";
  predicate?: { field: string; value: PredValue };
}

const NUMBER_LITERAL = /^-?\d+(\.\d+)?$/;

function parsePredValue(text: string): PredValue {
  if (text === "true") return { kind: "lit", value: true };
  if (text === "false") return { kind: "lit", value: false };
  if (text === "null") return { kind: "lit", value: null };
  if (NUMBER_LITERAL.test(text)) return { kind: "lit", value: Number(text) };
  // A bare string is a Binding resolved before the comparison (UIS-101/108).
  return { kind: "binding", path: text };
}

/** Split a path into segments on `.` AND `/` at bracket depth 0. `.` is the
 * Scope-relative UIS-100 separator; `/` joins the resource segments of a root
 * Binding (UIS-005, e.g. `automations/<id>`) — a form the validator explicitly
 * accepts (isValidRootBindingString) but that a Scope-relative path never bears,
 * so treating the two separators alike here resolves both without a second parser
 * and never mis-splits an ordinary Binding. */
function splitSegments(s: string): string[] {
  const out: string[] = [];
  let depth = 0;
  let start = 0;
  for (let i = 0; i < s.length; i++) {
    const ch = s[i];
    if (ch === "[") depth++;
    else if (ch === "]") depth = Math.max(0, depth - 1);
    else if ((ch === "." || ch === "/") && depth === 0) {
      out.push(s.slice(start, i));
      start = i + 1;
    }
  }
  out.push(s.slice(start));
  return out;
}

/** Parse a UIS-100 path (or a UIS-005 root Binding, which additionally joins
 * resource segments with `/`) into segments. Assumes the input already passed the
 * validator's grammar check (validate.ts), so parsing is forgiving of shapes the
 * grammar already excluded. */
export function parsePath(path: string): Segment[] {
  return splitSegments(path).map((raw) => {
    const br = raw.indexOf("[");
    if (br === -1) return { name: raw };
    const name = raw.slice(0, br);
    const inner = raw.slice(br + 1, raw.length - 1);
    if (inner === "$index") return { name, index: "$index" };
    if (/^\d+$/.test(inner)) return { name, index: Number(inner) };
    const eq = inner.indexOf("=");
    if (eq > 0) {
      return {
        name,
        predicate: { field: inner.slice(0, eq), value: parsePredValue(inner.slice(eq + 1)) },
      };
    }
    return { name };
  });
}

function asRecord(v: unknown): Record<string, unknown> | undefined {
  return v !== null && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : undefined;
}

/** Select the first array element whose `field` equals the predicate's value
 * (UIS-101): zero matches resolve the whole path to null; multiple resolve to the
 * first in array order. */
function selectPredicate(arr: unknown, pred: { field: string; value: PredValue }, scope: RenderScope): unknown {
  if (!Array.isArray(arr)) return undefined;
  const wanted = pred.value.kind === "lit" ? pred.value.value : resolvePath(pred.value.path, scope);
  const found = arr.find((el) => {
    const rec = asRecord(el);
    return rec !== undefined && rec[pred.field] === wanted;
  });
  return found ?? null;
}

/** Apply one segment's `.name` + optional index/predicate against a running value. */
function stepSegment(cursor: unknown, seg: Segment, scope: RenderScope): unknown {
  const rec = asRecord(cursor);
  const arr = Array.isArray(cursor) ? (cursor as unknown[]) : undefined;
  let v: unknown = rec ? rec[seg.name] : arr ? undefined : undefined;
  // A bare array with a named segment has no field to read; only bracketed
  // access (index/$index) reaches into it — but the grammar always names first.
  if (seg.index !== undefined) {
    const idx = seg.index === "$index" ? (scope.item ? scope.item.index : NaN) : seg.index;
    v = Array.isArray(v) ? (v as unknown[])[idx as number] : undefined;
  }
  if (seg.predicate) v = selectPredicate(v, seg.predicate, scope);
  return v;
}

/** Resolve a UIS-100 Binding path against a scope to its current value. A
 * predicate matching nothing resolves to null; any other unreachable step
 * resolves to undefined (UIS-101). */
export function resolvePath(path: string, scope: RenderScope): unknown {
  const segs = parsePath(path);
  if (segs.length === 0) return undefined;
  const head = segs[0].name;

  // Reserved roots (UIS-103–106).
  if (head === "$root") return walk(scope.root, segs, 1, scope);
  if (head === "$ui") return walk(scope.ui, segs, 1, scope);
  if (head === "$context") return walk(scope.context, segs, 1, scope);
  if (head === "$params") return walk(scope.params ?? {}, segs, 1, scope);

  // The active item scope (UIS-107): `<itemScope>` addresses the element,
  // `<itemScope>.$index` its zero-based position.
  if (scope.item && head === scope.item.name) {
    if (segs.length >= 2 && segs[1].name === "$index" && segs[1].index === undefined && !segs[1].predicate) {
      return scope.item.index;
    }
    return walk(scope.item.value, segs, 1, scope);
  }

  // Unprefixed: resolve against the innermost scope (UIS-103).
  return walk(scope.current, segs, 0, scope);
}

function walk(base: unknown, segs: Segment[], from: number, scope: RenderScope): unknown {
  let cursor = base;
  for (let i = from; i < segs.length; i++) {
    if (cursor == null) return undefined;
    cursor = stepSegment(cursor, segs[i], scope);
  }
  return cursor;
}

/** Resolve a path AND report its absolute location, for establishing an editable
 * root scope (UIS-005) or an add/remove target. Only name + numeric-index
 * segments are locatable; a predicate segment locates the matched element's
 * index. Returns null loc when the location cannot be pinned (a computed selector
 * that missed, an unlocatable form). */
export function resolvePathWithLoc(
  path: string,
  scope: RenderScope,
): { value: unknown; loc: PathLoc | null; tree: "resource" | "ui" } {
  const segs = parsePath(path);
  const head = segs[0]?.name;
  let tree: "resource" | "ui" = scope.currentTree;
  let base: unknown;
  let loc: PathLoc | null;
  let from: number;

  if (head === "$ui") {
    tree = "ui";
    base = scope.ui;
    loc = [];
    from = 1;
  } else if (head === "$root") {
    base = scope.root;
    loc = [];
    from = 1;
  } else if (head === "$context" || head === "$params") {
    // Read-only namespaces (UIS-152) — resolvable, never a write location.
    return { value: resolvePath(path, scope), loc: null, tree };
  } else if (scope.item && head === scope.item.name) {
    base = scope.item.value;
    loc = [...scope.item.arrayPath, scope.item.index];
    tree = scope.item.arrayTree;
    from = 1;
  } else {
    base = scope.current;
    loc = [...scope.currentPath];
    from = 0;
  }

  let cursor = base;
  for (let i = from; i < segs.length; i++) {
    const seg = segs[i];
    if (cursor == null) return { value: undefined, loc: null, tree };
    const rec = asRecord(cursor);
    cursor = rec ? rec[seg.name] : undefined;
    if (loc) loc.push(seg.name);
    if (seg.index !== undefined) {
      const idx = seg.index === "$index" ? (scope.item ? scope.item.index : NaN) : seg.index;
      cursor = Array.isArray(cursor) ? (cursor as unknown[])[idx as number] : undefined;
      if (loc) loc.push(idx as number);
    }
    if (seg.predicate) {
      const arr = cursor;
      cursor = selectPredicate(arr, seg.predicate, scope);
      if (Array.isArray(arr) && cursor != null) {
        const idx = (arr as unknown[]).indexOf(cursor);
        if (loc && idx >= 0) loc.push(idx);
        else loc = null; // matched by value but not locatable
      } else {
        loc = null;
      }
    }
  }
  return { value: cursor, loc, tree };
}

/** Resolve a Binding to a WRITE location — the tree + absolute path a value is
 * stored at (UIS-065 input write-back, UIS-160 `set`). Returns null for a
 * read-only namespace ($context/$params, UIS-152). */
export function resolveWriteLocation(
  path: string,
  scope: RenderScope,
): { tree: "resource" | "ui"; loc: PathLoc } | null {
  const r = resolvePathWithLoc(path, scope);
  if (r.loc === null) return null;
  return { tree: r.tree, loc: r.loc };
}

// ── Environment (msg resolution, locale, vocab labels) ──────────────────────

/** Resolves a `msg:` reference (MAN-003/111) to display text, optionally with
 * positional args interpolated. */
export type MessageResolver = (ref: string, args?: unknown[]) => string;

export interface RenderEnv {
  msg: MessageResolver;
  /** Viewer-active locale for `formatDate`/`formatNumber` (UIS-143). */
  locale: string;
  /** vocabRef → member → msg reference, collected from the document's own vocab
   * OptionSources so `label` (UIS-140) can resolve without a parallel registry. */
  vocabLabels: Record<string, Record<string, string>>;
  /** Server field-validation errors for the currently-bound resource: a 422
   * VALIDATION_FAILED Problem's `errors[]` mapped to bind-path → message (the
   * api/1 convention that per-field errors surface on the offending FormField,
   * not only a toast). Keyed by the input's `bind` path; absent when the last
   * write succeeded or none has been attempted. */
  fieldErrors?: Record<string, string>;
}

/** Humanize a `msg:` reference when no catalog entry exists — the last dotted
 * segment, de-camelCased. Keeps the renderer legible with no message catalog. */
export function humanizeMsgRef(ref: string): string {
  const body = ref.startsWith("msg:") ? ref.slice(4) : ref;
  const last = body.split(".").pop() ?? body;
  const spaced = last
    .replace(/[_-]+/g, " ")
    .replace(/([a-z0-9])([A-Z])/g, "$1 $2")
    .trim();
  return spaced ? spaced.charAt(0).toUpperCase() + spaced.slice(1) : ref;
}

/** Build a MessageResolver over an optional catalog, falling back to humanizing. */
export function makeMessageResolver(catalog?: Record<string, string>): MessageResolver {
  return (ref, args = []) => {
    // A page doc's Msg-kind props are presence-only validated (there is no
    // scalar-shape taxonomy code), so a pack — maliciously, or by a typo that drops
    // the `msg:` prefix — can pass a bare name that collides with an
    // Object.prototype member (`__proto__`, `constructor`, `hasOwnProperty`,
    // `toString`, …). A plain `catalog?.[ref]` would then return the INHERITED
    // member (a truthy non-string), slip past the humanize fallback, and crash the
    // whole renderer on `.replace`. Take the entry only when it is the catalog's OWN
    // string; anything else humanizes, degrading to the standard fallback.
    const template =
      catalog && Object.prototype.hasOwnProperty.call(catalog, ref) ? catalog[ref] : undefined;
    const text = typeof template === "string" ? template : humanizeMsgRef(ref);
    // Positional {0}/{1}/… interpolation (msg's own arg convention).
    return text.replace(/\{(\d+)\}/g, (_, i: string) => String(args[Number(i)] ?? ""));
  };
}

/** Scan a page document for every `vocab`-kind OptionSource, collecting its
 * ref→labels map so a `label` Computed elsewhere on the page can resolve
 * (UIS-140). */
export function collectVocabLabels(doc: unknown): Record<string, Record<string, string>> {
  const out: Record<string, Record<string, string>> = {};
  const visit = (v: unknown): void => {
    if (Array.isArray(v)) {
      v.forEach(visit);
      return;
    }
    const rec = asRecord(v);
    if (!rec) return;
    if (rec.kind === "vocab" && typeof rec.ref === "string" && asRecord(rec.labels)) {
      out[rec.ref] = { ...(out[rec.ref] ?? {}), ...(rec.labels as Record<string, string>) };
    }
    for (const val of Object.values(rec)) visit(val);
  };
  visit(doc);
  return out;
}

// ── Computed values + BindingExpr (UIS-108/140/141) ─────────────────────────

function isComputed(v: unknown): v is { compute: string; args?: unknown[] } {
  const r = asRecord(v);
  return r !== undefined && typeof r.compute === "string";
}

function isLiveBinding(v: unknown): v is { path: string; live: true } {
  const r = asRecord(v);
  return r !== undefined && "path" in r && !("compute" in r);
}

function isLit(v: unknown): v is { lit: unknown } {
  const r = asRecord(v);
  return r !== undefined && "lit" in r && !("compute" in r) && !("path" in r);
}

function truthy(v: unknown): boolean {
  return Array.isArray(v) ? v.length > 0 : Boolean(v);
}

/** Evaluate a Computed `arg` (or any BindingExpr): a bare string is a Binding
 * (UIS-108), never a string literal; `{lit}` is the string-literal escape;
 * `{compute}` a nested Computed; `{path,live}` a LiveBinding (read its current
 * value here — the push subscription is a widget-level concern, UIS-110);
 * number/bool/null/array/plain-object are literals as-is. */
export function evalBindingExpr(expr: unknown, scope: RenderScope, env: RenderEnv): unknown {
  if (typeof expr === "string") return resolvePath(expr, scope);
  if (isComputed(expr)) return applyCompute(expr, scope, env);
  if (isLiveBinding(expr)) return resolvePath(expr.path, scope);
  if (isLit(expr)) return expr.lit;
  return expr;
}

function applyCompute(node: { compute: string; args?: unknown[] }, scope: RenderScope, env: RenderEnv): unknown {
  const args = Array.isArray(node.args) ? node.args : [];
  const ev = (i: number): unknown => evalBindingExpr(args[i], scope, env);
  switch (node.compute) {
    case "eq":
      return ev(0) === ev(1);
    case "not":
      return !truthy(ev(0));
    case "and":
      return args.every((_, i) => truthy(ev(i)));
    case "or":
      return args.some((_, i) => truthy(ev(i)));
    case "count": {
      const a = ev(0);
      return Array.isArray(a) ? a.length : 0;
    }
    case "isEmpty": {
      const a = ev(0);
      return !Array.isArray(a) || a.length === 0;
    }
    case "join": {
      const a = ev(0);
      const sep = String(ev(1) ?? "");
      return Array.isArray(a) ? a.join(sep) : "";
    }
    case "label": {
      // arg0 is a pinned vocabRef (never a Binding, UIS-140); arg1 the value.
      const ref = typeof args[0] === "string" ? args[0] : "";
      const value = ev(1);
      const msgRef = env.vocabLabels[ref]?.[String(value)];
      return msgRef ? env.msg(msgRef) : String(value ?? "");
    }
    case "msg": {
      // arg0 is a pinned msgRef; the rest are interpolation args.
      const ref = typeof args[0] === "string" ? args[0] : "";
      const rest = args.slice(1).map((_, i) => ev(i + 1));
      return env.msg(ref, rest);
    }
    case "formatDuration":
      return formatDuration(Number(ev(0)));
    case "formatDate":
      return formatDate(ev(0), env.locale);
    case "formatNumber":
      return formatNumber(ev(0), env.locale);
    case "formatCurrency":
      // arg1 is a pinned ISO 4217 currency code (never a Binding, UIS-140/143) —
      // read directly, the same treatment label's vocabRef and msg's msgRef get.
      return formatCurrency(ev(0), env.locale, args[1]);
    case "firstKey": {
      const obj = asRecord(ev(0));
      const keys = Array.isArray(args[1]) ? (args[1] as string[]) : [];
      if (obj) for (const k of keys) if (k in obj) return k;
      return null;
    }
    default:
      return undefined;
  }
}

// ── Locale-formatted display (UIS-143) ──────────────────────────────────────

export function formatDate(v: unknown, locale: string): string {
  if (typeof v !== "string" && typeof v !== "number") return v == null ? "" : String(v);
  const d = new Date(v);
  if (Number.isNaN(d.getTime())) return String(v);
  const hasTime = typeof v === "string" && v.includes("T");
  const opts: Intl.DateTimeFormatOptions = hasTime
    ? { dateStyle: "medium", timeStyle: "short" }
    : { dateStyle: "medium" };
  return new Intl.DateTimeFormat(locale, opts).format(d);
}

export function formatNumber(v: unknown, locale: string): string {
  const n = typeof v === "number" ? v : Number(v);
  if (!Number.isFinite(n)) return v == null ? "" : String(v);
  return new Intl.NumberFormat(locale).format(n);
}

const ISO_4217 = /^[A-Za-z]{3}$/;

/** Locale-formatted currency rendering (UIS-143): the viewer's active locale
 * drives punctuation/symbol placement (the same locale `formatNumber`/`formatDate`
 * use), while `currency` (an ISO 4217 code, e.g. `"USD"`) pins WHICH currency the
 * bound number is — a business fact of the priced value, not a viewer preference,
 * exactly as `Intl.NumberFormat`'s `{style: "currency"}` option separates the two.
 * This is the fix for a raw price (`4.5`) rendering as bare `"4.5"` instead of a
 * currency string (`"$4.50"`): a page author now wraps the bound value in
 * `{compute: "formatCurrency", args: [priceBinding, "USD"]}` instead of binding it
 * directly. A malformed/absent currency code degrades to `"USD"` rather than
 * crashing the renderer — the same graceful-degradation discipline every other
 * Computed function here follows. */
export function formatCurrency(v: unknown, locale: string, currency: unknown): string {
  const n = typeof v === "number" ? v : Number(v);
  if (!Number.isFinite(n)) return v == null ? "" : String(v);
  const code = typeof currency === "string" && ISO_4217.test(currency) ? currency.toUpperCase() : "USD";
  return new Intl.NumberFormat(locale, { style: "currency", currency: code }).format(n);
}

export function formatDuration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return "0s";
  const s = Math.floor(seconds);
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const sec = s % 60;
  const parts: string[] = [];
  if (h) parts.push(`${h}h`);
  if (m) parts.push(`${m}m`);
  if (sec || parts.length === 0) parts.push(`${sec}s`);
  return parts.join(" ");
}

/** Coerce an evaluated value to display text for a `text`/`badge`/`stat-tile`
 * value or a table cell — primitives verbatim, structures as compact JSON. */
export function toDisplay(v: unknown): string {
  if (v == null) return "";
  if (typeof v === "string") return v;
  if (typeof v === "number" || typeof v === "boolean") return String(v);
  try {
    return JSON.stringify(v);
  } catch {
    return String(v);
  }
}

// ── OptionSource resolution (UIS-130–133) ───────────────────────────────────

export interface ResolvedOption {
  value: unknown;
  label: string;
}

/** Resolve an `options` OptionSource to its candidate `{value, label}` list in
 * the contract's declared order (UIS-131/132/133). */
export function resolveOptions(source: unknown, scope: RenderScope, env: RenderEnv): ResolvedOption[] {
  const src = asRecord(source);
  if (!src) return [];
  if (src.kind === "literal") {
    const items = Array.isArray(src.items) ? src.items : [];
    return items.map((it) => {
      const r = asRecord(it) ?? {};
      return { value: r.value, label: env.msg(String(r.labelMsg ?? "")) };
    });
  }
  if (src.kind === "vocab") {
    const ref = String(src.ref);
    const labels = asRecord(src.labels) ?? {};
    const members = VOCAB_TABLE[ref] ?? [];
    return members.map((m) => ({ value: m, label: env.msg(String(labels[m] ?? "")) }));
  }
  if (src.kind === "data") {
    const arr = evalBindingExpr(src.source, scope, env);
    const rows = Array.isArray(arr) ? arr : [];
    const valuePath = String(src.valuePath);
    const labelPath = String(src.labelPath);
    return rows.map((row) => ({
      value: fieldOf(row, valuePath),
      label: toDisplay(fieldOf(row, labelPath)),
    }));
  }
  return [];
}

/** Read a field path relative to an array element (UIS-133 valuePath/labelPath),
 * using the same itemScope-relative grammar as an ordinary Binding. */
function fieldOf(element: unknown, path: string): unknown {
  const elementScope: RenderScope = {
    root: element,
    current: element,
    currentPath: [],
    currentTree: "resource",
    ui: {},
    context: {},
  };
  return resolvePath(path, elementScope);
}
