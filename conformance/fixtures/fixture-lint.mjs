#!/usr/bin/env node
// conformance/fixtures/fixture-lint.mjs — asserts every widget type, page type,
// vocabRef, OptionSource kind, ActionRef verb, Computed function, fragment
// reference, and Binding path a fixture page document uses is DEFINED in
// contracts/ui-schema-1.md (or, for fragment refs, in the fixture's own
// `fragments` map) — zero undefined references. Dependency-free Node
// (fs/path only), mirroring scripts/validate-contracts.mjs's house style.
//
// Usage: node conformance/fixtures/fixture-lint.mjs [fixtureDir]
//   fixtureDir defaults to conformance/fixtures/automation-builder, and must
//   contain a page document named <dirname-without-path>.json — for
//   automation-builder that is automation-builder.json. sample-data.json (or
//   any file not matching that name) is illustrative-only and never scanned.

import { readFileSync, existsSync } from "node:fs";
import { basename, join } from "node:path";

const REPO_ROOT = join(import.meta.dirname, "..", "..");
const CONTRACT_PATH = join(REPO_ROOT, "contracts", "ui-schema-1.md");
const fixtureDirArg = process.argv[2] ?? join(import.meta.dirname, "automation-builder");
const fixtureName = basename(fixtureDirArg);
const DOC_PATH = join(fixtureDirArg, `${fixtureName}.json`);

const failures = [];
const stats = { widgetTypes: new Set(), bindings: 0, vocabRefs: new Set(), verbs: new Set(), computeFns: new Set(), optionKinds: new Set(), fragmentRefs: new Set() };

// ---------------------------------------------------------------------------
// 1. Extract the closed sets this contract defines, straight from the prose.
// ---------------------------------------------------------------------------

function extractTable(text, headerLine) {
  const idx = text.indexOf(headerLine);
  if (idx === -1) throw new Error(`fixture-lint: table header not found in contract: ${JSON.stringify(headerLine)}`);
  const lines = text.slice(idx).split("\n");
  const values = [];
  for (const line of lines) {
    if (!line.trimStart().startsWith("|")) {
      if (values.length > 0) break; // ran past the table
      continue;
    }
    const firstCell = line.split("|")[1]?.trim() ?? "";
    const m = firstCell.match(/^`([^`]+)`$/);
    if (m) values.push(m[1]);
  }
  return values;
}

function extractPageTypes(text) {
  const out = [];
  for (const m of text.matchAll(/^### Page types: (.+)$/gm)) out.push(m[1].trim());
  return out;
}

function extractOptionKinds(text) {
  const out = new Set();
  for (const m of text.matchAll(/\{kind:\s*"(\w+)"/g)) out.add(m[1]);
  return [...out];
}

if (!existsSync(CONTRACT_PATH)) {
  console.error(`fixture-lint: contract not found: ${CONTRACT_PATH}`);
  console.log("SUMMARY: fixture-lint: FAILED — contract file missing");
  process.exit(1);
}
const contractText = readFileSync(CONTRACT_PATH, "utf8");

const CLOSED = {
  pageTypes: new Set(extractPageTypes(contractText)),
  widgetTypes: new Set(extractTable(contractText, "| type | category | children? | props (closed) | `bind` shape | events |")),
  vocabRefs: new Set(extractTable(contractText, "| vocabRef | source | members |")),
  actionVerbs: new Set(extractTable(contractText, "| verb | fields | effect |")),
  computeFns: new Set(extractTable(contractText, "| compute | signature | behavior |")),
  optionKinds: new Set(extractOptionKinds(contractText)),
};

for (const [name, set] of Object.entries(CLOSED)) {
  if (set.size === 0) throw new Error(`fixture-lint: extracted zero members for ${name} — extraction is broken, not the contract`);
}

// ---------------------------------------------------------------------------
// 2. A hand-rolled validator for the UIS-100 Binding data-path grammar:
//      path := segment ("." segment)*
//      segment := name | name "[" index "]" | name "[" predicate "]"
//      name := reserved token | Kebab identifier | [A-Za-z_][A-Za-z0-9_]*
//      index := <non-negative integer> | "$index"
//      predicate := name "=" (JSON literal | path)
// ---------------------------------------------------------------------------

const NAME_RE = /^[A-Za-z_$][A-Za-z0-9_-]*$/;

function isValidBindingPath(str) {
  if (typeof str !== "string" || str.length === 0) return false;
  let i = 0;
  const n = str.length;

  function parseName() {
    const start = i;
    while (i < n && /[A-Za-z0-9_$-]/.test(str[i])) i++;
    if (i === start) return null;
    return str.slice(start, i);
  }

  function parseBracket() {
    // str[i] === '[' on entry
    i++;
    const start = i;
    let depth = 1;
    while (i < n && depth > 0) {
      if (str[i] === "[") depth++;
      else if (str[i] === "]") depth--;
      if (depth > 0) i++;
    }
    if (depth !== 0) return null; // unmatched
    const inner = str.slice(start, i);
    i++; // consume ']'
    return inner;
  }

  function isValidIndexOrPredicate(inner) {
    if (inner === "$index") return true;
    if (/^\d+$/.test(inner)) return true;
    const eq = inner.indexOf("=");
    if (eq <= 0) return false;
    const field = inner.slice(0, eq);
    const value = inner.slice(eq + 1);
    if (!NAME_RE.test(field) || value === "") return false;
    if (/^".*"$/.test(value) || /^-?\d+(\.\d+)?$/.test(value) || value === "true" || value === "false" || value === "null") return true;
    return isValidBindingPath(value);
  }

  function parseSegment() {
    const name = parseName();
    if (name === null || !NAME_RE.test(name)) return false;
    if (str[i] === "[") {
      const inner = parseBracket();
      if (inner === null || !isValidIndexOrPredicate(inner)) return false;
    }
    return true;
  }

  if (!parseSegment()) return false;
  while (i < n) {
    if (str[i] !== ".") return false;
    i++;
    if (!parseSegment()) return false;
  }
  return i === n;
}

function checkBinding(path, value) {
  stats.bindings++;
  if (!isValidBindingPath(value)) failures.push(`${path}: BINDING_PATH_INVALID — ${JSON.stringify(value)} does not match the UIS-100 data-path grammar`);
}

// A `{lit: ...}` object is an explicit literal (UIS-108) — its own value is
// taken verbatim, never validated as a Binding.
function isLit(value) {
  return value && typeof value === "object" && !Array.isArray(value) && "lit" in value && !("compute" in value);
}

// A BindingExpr is a Binding string, a Computed object, a {lit} literal, or a
// JSON literal (UIS-108: a bare string is a Binding; a {lit} forces a literal).
function checkBindingExpr(path, value) {
  if (isLit(value)) return; // explicit literal — nothing to check
  if (typeof value === "string") checkBinding(path, value);
  else if (value && typeof value === "object" && "compute" in value) checkComputed(path, value);
  // number/boolean/null/plain object/array outside the Computed/lit shapes — literal, valid as-is.
}

// Per UIS-140, a generic Computed arg (a bare string) is a Binding; only
// `label`'s first argument is pinned to a narrower type (`vocabRef`, not a
// Binding) — checked against UIS-120 instead of the data-path grammar. A
// `{lit}`-wrapped string arg (UIS-108) is an explicit literal, not a Binding.
const COMPUTE_ARG0_VOCABREF = new Set(["label"]);

function checkComputed(path, node) {
  stats.computeFns.add(node.compute);
  if (!CLOSED.computeFns.has(node.compute)) failures.push(`${path}.compute: COMPUTE_FN_UNKNOWN — "${node.compute}" is not in ui-schema-1.md's UIS-140 table`);
  if (!Array.isArray(node.args)) {
    failures.push(`${path}.args: ACTION_FIELDS_INVALID — Computed.args must be an array`);
    return;
  }
  node.args.forEach((arg, idx) => {
    if (idx === 0 && COMPUTE_ARG0_VOCABREF.has(node.compute)) {
      stats.vocabRefs.add(arg);
      if (!CLOSED.vocabRefs.has(arg)) failures.push(`${path}.args[0]: VOCAB_REF_UNKNOWN — "${arg}" is not in ui-schema-1.md's UIS-120 table`);
      return;
    }
    checkBindingExprOrLiteral(`${path}.args[${idx}]`, arg);
  });
}

function checkBindingExprOrLiteral(path, value) {
  // Computed args may be a Binding string, a {lit} literal, a plain literal, or
  // a nested Computed (UIS-108: a bare string is a Binding, {lit} forces a literal).
  if (isLit(value)) return;
  if (typeof value === "string") checkBinding(path, value);
  else if (value && typeof value === "object" && !Array.isArray(value) && "compute" in value) checkComputed(path, value);
  // else: plain JSON literal, nothing to check.
}

// ---------------------------------------------------------------------------
// 3. Generic recursive scan for shape-unambiguous closed-vocabulary uses:
//    ActionRef ({verb,...}), OptionSource ({kind,...}), Computed ({compute,...}).
//    These three shapes do not collide with anything else in a page document.
// ---------------------------------------------------------------------------

function scanGeneric(path, node) {
  if (Array.isArray(node)) {
    node.forEach((el, idx) => scanGeneric(`${path}[${idx}]`, el));
    return;
  }
  if (!node || typeof node !== "object") return;

  if (typeof node.verb === "string") {
    stats.verbs.add(node.verb);
    if (!CLOSED.actionVerbs.has(node.verb)) failures.push(`${path}.verb: ACTION_VERB_UNKNOWN — "${node.verb}" is not in ui-schema-1.md's UIS-160 table`);
    // `target` is always a Binding (a write destination, or a repeat-remove item ref).
    if (typeof node.target === "string") checkBinding(`${path}.target`, node.target);
    // `set.value` is a literal-or-Binding position (UIS-108): a bare string is a
    // Binding, a {lit}/number/etc. is a literal.
    if ("value" in node) checkBindingExprOrLiteral(`${path}.value`, node.value);
  }
  if (typeof node.kind === "string" && ("items" in node || "ref" in node || "source" in node)) {
    stats.optionKinds.add(node.kind);
    if (!CLOSED.optionKinds.has(node.kind)) failures.push(`${path}.kind: OPTION_SOURCE_INVALID — "${node.kind}" is not a closed OptionSource kind`);
    if (node.kind === "vocab" && typeof node.ref === "string") {
      stats.vocabRefs.add(node.ref);
      if (!CLOSED.vocabRefs.has(node.ref)) failures.push(`${path}.ref: VOCAB_REF_UNKNOWN — "${node.ref}" is not in ui-schema-1.md's UIS-120 table`);
    }
    if (node.kind === "data") {
      if (typeof node.source === "string") checkBinding(`${path}.source`, node.source);
      // valuePath/labelPath are item-relative field names, not full Bindings — not path-checked here.
    }
  }
  if (typeof node.compute === "string" && "args" in node) checkComputed(path, node);

  for (const [key, val] of Object.entries(node)) scanGeneric(`${path}.${key}`, val);
}

// ---------------------------------------------------------------------------
// 4. Structural walk: widget-type closure + fragment-ref resolution + the
//    unambiguous Binding-typed fields (bind, discriminant, visibleIf, cell,
//    list/detail source).
// ---------------------------------------------------------------------------

let fragmentNames = new Set();

function checkWidgetNode(path, node) {
  if (!node || typeof node !== "object" || Array.isArray(node)) {
    failures.push(`${path}: WIDGET_REQUIRED_FIELD_MISSING — expected a widget node object`);
    return;
  }
  if (typeof node.type !== "string") {
    failures.push(`${path}.type: WIDGET_REQUIRED_FIELD_MISSING — widget node has no "type"`);
    return;
  }
  stats.widgetTypes.add(node.type);
  if (!CLOSED.widgetTypes.has(node.type)) {
    failures.push(`${path}.type: UNKNOWN_WIDGET_TYPE — "${node.type}" is not in ui-schema-1.md's UIS-070 catalog`);
    return;
  }

  if (typeof node.bind === "string") checkBinding(`${path}.bind`, node.bind);
  if ("visibleIf" in node) checkBindingExpr(`${path}.visibleIf`, node.visibleIf);

  if (Array.isArray(node.children)) node.children.forEach((child, idx) => checkWidgetNode(`${path}.children[${idx}]`, child));

  const props = node.props ?? {};

  if (node.type === "repeat" && props.itemTemplate) checkWidgetNode(`${path}.props.itemTemplate`, props.itemTemplate);

  if (node.type === "switch") {
    if ("discriminant" in props) checkBindingExpr(`${path}.props.discriminant`, props.discriminant);
    (props.cases ?? []).forEach((c, idx) => {
      if (c.render) checkWidgetNode(`${path}.props.cases[${idx}].render`, c.render);
    });
    if (props.default) checkWidgetNode(`${path}.props.default`, props.default);
  }

  if (node.type === "fragment") {
    // fragment's rescoping "bind" is the ordinary top-level widget-node field
    // (same field `repeat` uses for its array, UIS-070) — already checked
    // generically above via `if (typeof node.bind === "string") ...`.
    if (typeof props.ref === "string") {
      stats.fragmentRefs.add(props.ref);
      if (!fragmentNames.has(props.ref)) failures.push(`${path}.props.ref: FRAGMENT_REF_UNDEFINED — "${props.ref}" is not a key of this document's own "fragments" map`);
    }
  }

  if (node.type === "table") {
    if (typeof props.source === "string") checkBinding(`${path}.props.source`, props.source);
    (props.columns ?? []).forEach((col, idx) => {
      if ("cell" in col) checkBindingExpr(`${path}.props.columns[${idx}].cell`, col.cell);
    });
  }

  if ((node.type === "text" || node.type === "badge" || node.type === "stat-tile") && "value" in props) {
    checkBindingExpr(`${path}.props.value`, props.value);
  }
}

// ---------------------------------------------------------------------------
// 5. Load + walk the page document.
// ---------------------------------------------------------------------------

if (!existsSync(DOC_PATH)) {
  console.error(`fixture-lint: page document not found: ${DOC_PATH}`);
  console.log("SUMMARY: fixture-lint: FAILED — page document missing");
  process.exit(1);
}
const doc = JSON.parse(readFileSync(DOC_PATH, "utf8"));

if (!CLOSED.pageTypes.has(doc.pageType)) {
  failures.push(`pageType: UNKNOWN_PAGE_TYPE — "${doc.pageType}" is not one of ui-schema-1.md's Page types`);
}

fragmentNames = new Set(Object.keys(doc.fragments ?? {}));
for (const [name, node] of Object.entries(doc.fragments ?? {})) checkWidgetNode(`fragments.${name}`, node);

if (doc.list) {
  if (typeof doc.list.source === "string") checkBinding("list.source", doc.list.source);
  if (doc.list.display) checkWidgetNode("list.display", doc.list.display);
}
if (doc.detail) {
  if (typeof doc.detail.source === "string") checkBinding("detail.source", doc.detail.source);
  if (doc.detail.root) checkWidgetNode("detail.root", doc.detail.root);
}
if (Array.isArray(doc.tiles)) doc.tiles.forEach((t, idx) => t.widget && checkWidgetNode(`tiles[${idx}].widget`, t.widget));
if (Array.isArray(doc.sections)) doc.sections.forEach((s, idx) => (s.fields ?? []).forEach((f, j) => checkWidgetNode(`sections[${idx}].fields[${j}]`, f)));
if (Array.isArray(doc.steps)) doc.steps.forEach((s, idx) => s.root && checkWidgetNode(`steps[${idx}].root`, s.root));

// Generic pass over the WHOLE document for ActionRef / OptionSource / Computed —
// these three shapes are unambiguous regardless of where they appear (on.*,
// newAction, nested inside props), so a second, non-structural pass catches
// every occurrence without needing an exhaustive per-widget prop model.
scanGeneric("$", doc);

// ---------------------------------------------------------------------------
// 6. Report.
// ---------------------------------------------------------------------------

if (failures.length) {
  console.error(failures.join("\n"));
  console.log(`SUMMARY: fixture-lint: FAILED — ${failures.length} issue(s); first: ${failures[0]}`);
  process.exitCode = 1;
} else {
  console.log(
    `fixture-lint: OK (${fixtureName}) — ` +
      `${stats.widgetTypes.size} widget type(s) used [${[...stats.widgetTypes].sort().join(", ")}], ` +
      `${stats.bindings} binding(s) checked, ` +
      `${stats.vocabRefs.size} vocabRef(s) used [${[...stats.vocabRefs].sort().join(", ")}], ` +
      `${stats.verbs.size} action verb(s) used [${[...stats.verbs].sort().join(", ")}], ` +
      `${stats.computeFns.size} compute fn(s) used [${[...stats.computeFns].sort().join(", ")}], ` +
      `${stats.optionKinds.size} option kind(s) used [${[...stats.optionKinds].sort().join(", ")}], ` +
      `${stats.fragmentRefs.size} fragment ref(s) used [${[...stats.fragmentRefs].sort().join(", ")}] — zero undefined references`
  );
  console.log("SUMMARY: fixture-lint: OK (0 undefined references)");
}
