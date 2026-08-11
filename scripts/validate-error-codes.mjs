#!/usr/bin/env node
// scripts/validate-error-codes.mjs — every error code a contract PUBLISHES is
// either wired into an implementation or carries a written reason why it is not.
//
// The defect this exists to catch is a shape this repo has produced repeatedly by
// hand: a capability is DECLARED and nothing is wired to it. A contract's Error
// taxonomy table is a promise that a specific, distinguishable refusal exists —
// callers branch on the `code`, so a code no implementation can ever emit is a
// promise that silently cannot be kept. Worse, the traceability row for the
// requirement that names it can still read `covered`, because a driver can
// observe the ACCEPT side of a rule and never reach the refusal.
//
// This is deliberately a separate script from validate-contracts.mjs (markdown
// grammar) and validate-coverage.mjs (traceability contents): those two never
// read implementation source at all. This one crosses that line, so it gets its
// own file, its own failure taxonomy, and its own `SUMMARY:` marker — the same
// reasoning the header of validate-coverage.mjs sets out.
//
// ── What "published" means ──────────────────────────────────────────────────
// A code is published if it appears in the first cell of a row of a contract's
// `## Error taxonomy` table, spelled as `` `SCREAMING_SNAKE` ``. Nine codes are
// published by more than one contract (`INTERNAL` by four); the unit this script
// decides on is therefore the (contract, code) PAIR, so the inventory reads
// per-contract and a reason can name the work item that owns it.
//
// ── What "implemented" means ───────────────────────────────────────────────
// The code's literal appears, at least once, in an implementation source in a
// position that is a USE rather than a re-declaration of the taxonomy:
//
//   Implementation sources: non-test Go (`*.go`, not `*_test.go`), non-test
//   TypeScript under web/src, and BrightScript under player-v3. All three ship;
//   ui-schema/1's validator is TypeScript and player/1's is BrightScript, so a
//   Go-only scan would report 17 ui-schema/1 codes as unimplemented that are in
//   fact implemented and driven.
//
//   NOT implementation sources, each for a reason:
//     - `*_test.go` / `*.test.ts(x)` — a test asserting a code proves the test
//       spells it, not that anything emits it.
//     - `conformance/**` — a driver is the MEASURING apparatus. Two codes
//       (CLASS_IDENTIFIER_COLLISION, GROUP_NAME_COLLISION) are today computed
//       inside the device-class/1 driver and nowhere else, which is precisely
//       the defect this gate is for; counting the driver would hide it.
//     - generated files (`Code generated ... DO NOT EDIT.`, api/gen/**) — code
//       generated FROM the contract's own OpenAPI enum. Finding a code there
//       proves only that the contract says so, which we already knew.
//     - shell. One real client is a POSIX shell script —
//       scripts/install-from-channel-index.sh, channel-index/1's relay-release
//       installer — and it spells its two refusals inside a longer message
//       string rather than as a standalone literal, which no rule this script
//       could apply would separate from prose. Both of those codes are
//       implemented in Go as well, so nothing is misjudged today; if a refusal
//       is ever implemented ONLY in shell, this scan will not see it and the
//       allowlist entry must say so rather than claim the code is unbuilt.
//
// A "use" is any occurrence except these two, which re-state the taxonomy
// rather than act on it:
//
//   binding      `CodeDecryptFailed = "DECRYPT_FAILED"` — a named constant.
//                Counts as implemented only if the IDENTIFIER is referenced
//                somewhere else in implementation source. internal/archive
//                declares five codes its own doc comment calls "Declared, not
//                raised here"; a bare literal scan calls those implemented.
//   mirror       a line that is nothing but the literal and a comma, inside an
//                array literal — `web/src/renderer/schema.ts`'s ERROR_CODES is
//                the whole ui-schema/1 taxonomy copied into TypeScript. A
//                wrapped call argument (`fail(\n ctx,\n "CURRENCY_CODE_INVALID",`)
//                is NOT a mirror: the enclosing-array test is what separates
//                them, and it is deliberately conservative — when in doubt a
//                line counts as a use, because a false "unimplemented" is a
//                broken build and a false "implemented" is only a missed catch.
//
// ── Known limits (stated, not papered over) ────────────────────────────────
//   - Two contracts publishing the same code share one implementation verdict:
//     the check is textual, so `UNKNOWN_PAGE_TYPE` implemented for manifest/1
//     also reads as implemented for ui-schema/1. No published pair is affected
//     today (all nine shared codes are implemented on both sides).
//   - A code emitted only on a branch nothing can reach still counts as
//     implemented. Reachability is scripts/validate-deadcode.mjs's job.
//
// ── Checks ──────────────────────────────────────────────────────────────────
//   1. Every published (contract, code) pair is implemented, or listed in the
//      allowlist.
//   2. Every allowlist entry names a contract that really publishes that code —
//      a renamed or deleted code cannot rot in the list unnoticed.
//   3. Every allowlist entry is still unimplemented. Wiring a code up means
//      deleting its line: the list shrinks as the platform is built, and it can
//      never claim a refusal is missing that in fact exists.
//   4. No pair is listed twice, and every group carries a real reason — no
//      "TODO"/"TBD"/"n/a" placeholders, minimum length enforced. The inventory
//      is the deliverable; an unexplained baseline teaches nobody anything.
import { readdirSync, readFileSync, statSync, existsSync } from "node:fs";
import { join, basename, relative, sep } from "node:path";

const CONTRACTS_ROOT = "contracts";
const ALLOWLIST_PATH = join("conformance", "unimplemented-error-codes.json");
const EXEMPT_CONTRACT_NAMES = new Set(["README.md", "TEMPLATE.md"]);

// The first cell of an Error taxonomy row: a backticked SCREAMING_SNAKE code.
const TAXONOMY_CELL_RE = /^`([A-Z][A-Z0-9_]*)`$/;
// Any SCREAMING_SNAKE token inside quotes, in source. Three chars minimum so
// short all-caps strings ("OK", "ID") never enter the candidate set.
const QUOTED_TOKEN_RE = /["'`]([A-Z][A-Z0-9_]{2,})["'`]/g;
const GENERATED_MARKER_RE = /^\/\/ Code generated .* DO NOT EDIT\.$/m;
const PLACEHOLDER_REASON_RE = /^\s*(todo|tbd|n\/?a|\?+|-+)\b/i;
const MIN_REASON_LENGTH = 24;

const failures = [];

function walk(dir, out = []) {
  if (!existsSync(dir)) return out;
  for (const name of readdirSync(dir)) {
    if (name === "node_modules" || name === ".git" || name === "dist" || name.startsWith(".")) continue;
    const p = join(dir, name);
    if (statSync(p).isDirectory()) walk(p, out);
    else out.push(p);
  }
  return out;
}

// ---------------------------------------------------------------------------
// Step 1: what each contract publishes.
// ---------------------------------------------------------------------------
// contract filename -> string[] of codes, in table order.
const publishedByContract = new Map();
const publishedFieldByContract = new Map();

function parseTaxonomy(path, heading = "Error taxonomy") {
  const lines = readFileSync(path, "utf8").split("\n");
  const codes = [];
  let inSection = false;
  let inFence = false;
  for (const raw of lines) {
    if (/^\s*(```|~~~)/.test(raw)) {
      inFence = !inFence;
      continue;
    }
    if (inFence) continue;
    const line = raw.trim();
    if (new RegExp(`^##\\s+${heading}\\s*$`).test(line)) {
      inSection = true;
      continue;
    }
    // Any subsequent `## ` heading ends the section — never a `### ` one, so a
    // taxonomy split into subsections stays whole.
    if (inSection && /^##\s+\S/.test(line)) {
      inSection = false;
      continue;
    }
    if (!inSection || !line.startsWith("|")) continue;
    const first = line.split("|")[1]?.trim();
    const m = first && TAXONOMY_CELL_RE.exec(first);
    if (m) codes.push(m[1]);
  }
  return codes;
}

for (const path of walk(CONTRACTS_ROOT)) {
  if (!path.endsWith(".md")) continue;
  const name = basename(path);
  if (EXEMPT_CONTRACT_NAMES.has(name)) continue;
  const codes = parseTaxonomy(path);
  publishedByContract.set(name, codes);
  publishedFieldByContract.set(name, parseTaxonomy(path, "Field-level error register"));
  const seen = new Set();
  for (const code of codes) {
    if (seen.has(code)) failures.push(`${path}: Error taxonomy lists ${code} twice`);
    seen.add(code);
  }
}

const publishedCodes = new Set([...publishedByContract.values()].flat());
// Field-level codes are a SEPARATE register (api/1 API-013): they appear as
// `errors[].code`, never as a Problem's top-level `code`. Kept apart so a
// field code cannot satisfy a top-level publication requirement or vice versa.
const publishedFieldCodes = new Set([...publishedFieldByContract.values()].flat());

// ---------------------------------------------------------------------------
// Step 2: classify every occurrence of a published code in implementation source.
// ---------------------------------------------------------------------------
// A path is implementation source if it ships and is hand-written.
function implementationSource(rel) {
  const parts = rel.split(sep);
  if (rel.endsWith(".go")) {
    if (rel.endsWith("_test.go")) return false;
    if (parts[0] === "conformance") return false;
    return true;
  }
  if (/\.(ts|tsx)$/.test(rel)) {
    if (parts[0] !== "web" || parts[1] !== "src") return false;
    if (/\.test\.tsx?$/.test(rel)) return false;
    if (parts[2] === "test") return false;
    return true;
  }
  return rel.endsWith(".brs");
}

// Strip line comments so a doc comment naming a code (or naming the constant
// that holds it) can never stand in for wiring. BrightScript's comment marker
// is `'`, which is also not a string delimiter there — Go and TypeScript use
// `//`, and only TypeScript uses `'` for strings, so the two never collide.
function stripComments(line, isBrightScript) {
  if (isBrightScript) {
    const i = line.indexOf("'");
    return i === -1 ? line : line.slice(0, i);
  }
  const i = line.indexOf("//");
  if (i === -1) return line;
  // Do not truncate at a `//` that sits inside a string (a URL, most often).
  const before = line.slice(0, i);
  const quotes = (before.match(/["'`]/g) ?? []).length;
  return quotes % 2 === 0 ? before : line;
}

// Blank out `/* … */` block comments, preserving the line COUNT so every line
// number this scan reports (and insideArrayLiteral's own look-back) stays exact.
//
// stripComments above handles only `//`, which is the entire comment syntax Go
// and BrightScript have — but TypeScript's doc comments are `/** … */`, and every
// TypeScript implementation source in this tree is documented that way. A code
// named inside one was reaching BOTH halves of the implemented test: a backticked
// mention matched QUOTED_TOKEN_RE and counted as a USE outright, and a bare
// mention satisfied the "the identifier is referenced somewhere else" rule that
// makes a named constant count. That is exactly the substitution this file's own
// header says must never happen — "a doc comment naming a code (or naming the
// constant that holds it) can never stand in for wiring" — and it held for Go
// while silently not holding for TypeScript, which is where ui-schema/1's whole
// taxonomy lives. Found by mutation: stripping a renderer refusal's only emit
// site left the gate green, because two JSDoc paragraphs still named it.
function stripBlockComments(lines) {
  let inBlock = false;
  return lines.map((raw) => {
    let out = "";
    let i = 0;
    while (i < raw.length) {
      if (inBlock) {
        const end = raw.indexOf("*/", i);
        if (end === -1) return out;
        inBlock = false;
        i = end + 2;
        continue;
      }
      const lineComment = raw.indexOf("//", i);
      let start = raw.indexOf("/*", i);
      // A `/*` inside a string literal (a glob, a URL) is text, not a comment —
      // the same odd-quote-count heuristic stripComments applies to `//`.
      while (start !== -1 && (raw.slice(0, start).match(/["'`]/g) ?? []).length % 2 === 1) {
        start = raw.indexOf("/*", start + 2);
      }
      if (start === -1 || (lineComment !== -1 && lineComment < start)) return out + raw.slice(i);
      out += raw.slice(i, start);
      inBlock = true;
      i = start + 2;
    }
    return out;
  });
}

// `NAME = "CODE"`, with an optional declaration keyword and an optional Go or
// TypeScript type between the two. Captures NAME.
function bindingIdentifier(line, code) {
  const re = new RegExp(
    `^\\s*(?:(?:export\\s+)?(?:const|let|var)\\s+)?([A-Za-z_]\\w*)` +
      `(?:\\s+[\\w.\\[\\]*]+)?(?:\\s*:\\s*[^=]+?)?\\s*=\\s*["'\`]${code}["'\`]\\s*[,;]?\\s*$`
  );
  return re.exec(line)?.[1] ?? null;
}

// A line holding nothing but a quoted SCREAMING_SNAKE token and an optional
// comma — an array element, or a wrapped call argument. Which one it is depends
// on the enclosing construct, resolved by the caller.
const BARE_ELEMENT_RE = /^\s*["'`][A-Z][A-Z0-9_]{2,}["'`]\s*,?\s*$/;

// True when `index` sits inside an array literal: walk back over blank lines and
// sibling bare elements to an opening `[`. Anything else — a call's `(`, a
// struct literal's `{` — is not an enumeration, so the line is a use.
// The same shape spelled as an IDENTIFIER rather than a string literal — the
// form a taxonomy mirror takes when one of its members is a named constant
// (`ERROR_CODES = [ "A", "B", FRAGMENT_RECURSION_DEPTH_EXCEEDED, "C" ]`, which is
// how ui-schema/1 declares the two codes its renderer raises rather than its
// validator). Enumerating a constant in the mirror is not reading it.
const BARE_IDENT_ELEMENT_RE = /^\s*[A-Za-z_]\w*\s*,?\s*$/;

function insideArrayLiteral(lines, index) {
  for (let i = index - 1; i >= 0; i--) {
    const line = lines[i].trim();
    if (line === "") continue;
    if (BARE_ELEMENT_RE.test(line)) continue;
    if (BARE_IDENT_ELEMENT_RE.test(line)) continue;
    return /\[\s*$/.test(line);
  }
  return false;
}

const sourceFiles = [];
for (const path of walk(".")) {
  const rel = relative(".", path);
  if (!implementationSource(rel)) continue;
  let text;
  try {
    text = readFileSync(path, "utf8");
  } catch {
    continue;
  }
  if (GENERATED_MARKER_RE.test(text) || text.includes("auto-generated by openapi-typescript")) continue;
  const brs = rel.endsWith(".brs");
  const rawLines = text.split("\n");
  sourceFiles.push({ rel, lines: brs ? rawLines : stripBlockComments(rawLines), brs });
}

// code -> [ "file:line", ... ] where the literal is USED.
const usesByCode = new Map();
// code -> Set of identifiers bound to it.
const bindingsByCode = new Map();
// identifier -> [ "file:line", ... ] where it is bound (so a binding line never
// counts as a reference to itself).
const bindingSites = new Map();

for (const { rel, lines, brs } of sourceFiles) {
  lines.forEach((raw, i) => {
    const line = stripComments(raw, brs);
    QUOTED_TOKEN_RE.lastIndex = 0;
    const tokens = new Set();
    let m;
    while ((m = QUOTED_TOKEN_RE.exec(line)) !== null) {
      if (publishedCodes.has(m[1])) tokens.add(m[1]);
    }
    if (tokens.size === 0) return;
    const where = `${rel}:${i + 1}`;
    for (const code of tokens) {
      const ident = bindingIdentifier(line, code);
      if (ident) {
        if (!bindingsByCode.has(code)) bindingsByCode.set(code, new Set());
        bindingsByCode.get(code).add(ident);
        if (!bindingSites.has(ident)) bindingSites.set(ident, []);
        bindingSites.get(ident).push(where);
        continue;
      }
      if (BARE_ELEMENT_RE.test(line) && insideArrayLiteral(lines, i)) continue;
      if (!usesByCode.has(code)) usesByCode.set(code, []);
      usesByCode.get(code).push(where);
    }
  });
}

// A bound identifier counts only if something else names it.
const referencedIdentifiers = new Set();
if (bindingSites.size > 0) {
  const identifiers = [...bindingSites.keys()];
  const identRe = new RegExp(`\\b(${identifiers.map((s) => s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")).join("|")})\\b`, "g");
  for (const { rel, lines, brs } of sourceFiles) {
    lines.forEach((raw, i) => {
      const line = stripComments(raw, brs);
      // A constant ENUMERATED in a taxonomy mirror is not a constant anything
      // reads — the same judgement the string-literal scan above already makes
      // about an array element, applied to the identifier spelling of one. Without
      // it, declaring a code and listing it in ERROR_CODES is enough to satisfy
      // the "referenced somewhere else" rule with no emit site anywhere.
      if (BARE_IDENT_ELEMENT_RE.test(line) && insideArrayLiteral(lines, i)) return;
      identRe.lastIndex = 0;
      let m;
      while ((m = identRe.exec(line)) !== null) {
        if (bindingSites.get(m[1])?.includes(`${rel}:${i + 1}`)) continue;
        referencedIdentifiers.add(m[1]);
      }
    });
  }
}

function isImplemented(code) {
  if (usesByCode.has(code)) return true;
  for (const ident of bindingsByCode.get(code) ?? []) {
    if (referencedIdentifiers.has(ident)) return true;
  }
  return false;
}

// ---------------------------------------------------------------------------
// Step 3: the allowlist.
// ---------------------------------------------------------------------------
let allowlist = { groups: [] };
if (!existsSync(ALLOWLIST_PATH)) {
  failures.push(`${ALLOWLIST_PATH}: file does not exist — every published error code needs a verdict, and this file records the "not implemented, because …" half`);
} else {
  try {
    allowlist = JSON.parse(readFileSync(ALLOWLIST_PATH, "utf8"));
  } catch (err) {
    failures.push(`${ALLOWLIST_PATH}: invalid JSON: ${err.message}`);
  }
}

const CODE_FIELD_RE = /\b(?:Err)?Code:\s*"([A-Z][A-Z0-9_]{2,})"/g;
const FIELD_THEN_CODE_RE = /"[a-z][a-z0-9_.]*"\s*,\s*"([A-Z][A-Z0-9_]{2,})"\s*,/g;
const CODE_FIRST_CALL_RE = /\b[A-Za-z_]\w*\(\s*"([A-Z][A-Z0-9_]{2,})"\s*,\s*[`"]/g;

const emittedAt = new Map();
for (const path of walk(".")) {
  const rel = relative(".", path);
  if (!implementationSource(rel)) continue;
  const src = readFileSync(path, "utf8");
  if (GENERATED_MARKER_RE.test(src)) continue;
  for (const re of [CODE_FIELD_RE, FIELD_THEN_CODE_RE, CODE_FIRST_CALL_RE]) {
    re.lastIndex = 0;
    let m;
    while ((m = re.exec(src)) !== null) {
      const code = m[1];
      const line = src.slice(0, m.index).split("\n").length;
      if (!emittedAt.has(code)) emittedAt.set(code, `${rel}:${line}`);
    }
  }
}

// "contract\u0000code" -> the group's reason.
const allowed = new Map();

for (const [gi, group] of (allowlist.groups ?? []).entries()) {
  const at = `${ALLOWLIST_PATH}: groups[${gi}]`;
  const contract = group.contract;
  const reason = typeof group.reason === "string" ? group.reason.trim() : "";
  const codes = Array.isArray(group.codes) ? group.codes : [];

  if (!contract || !publishedByContract.has(contract)) {
    failures.push(`${at}: contract "${contract}" is not a file under ${CONTRACTS_ROOT}/`);
    continue;
  }
  // #4 a real reason, not a placeholder.
  if (reason.length < MIN_REASON_LENGTH || PLACEHOLDER_REASON_RE.test(reason)) {
    failures.push(
      `${at} (${contract}): reason is a placeholder or too short — say plainly why the code is published with nothing to emit it ` +
        `(what exists, what does not, and what would change it); at least ${MIN_REASON_LENGTH} characters, and never "TODO"/"TBD"`
    );
  }
  if (codes.length === 0) failures.push(`${at} (${contract}): codes is empty — delete the group`);

  const published = publishedByContract.get(contract);
  const publishedField = publishedFieldByContract.get(contract) ?? [];
  for (const code of codes) {
    const key = `${contract}\u0000${code}`;
    if (allowed.has(key)) {
      failures.push(`${at}: ${contract} lists ${code} twice — one entry per (contract, code) pair`);
      continue;
    }
    allowed.set(key, reason);
    // #2 the pair is really published — in EITHER register. A field-level code
    // is published in a different table and is unimplemented in exactly the same
    // way, so it earns an entry here on the same terms.
    const isField = publishedField.includes(code);
    if (!published.includes(code) && !isField) {
      failures.push(
        `${at}: ${contract} does not publish ${code} in its Error taxonomy or its Field-level error register — ` +
          `the code was renamed or removed; delete this entry`
      );
      continue;
    }
    // #3 the pair is still unimplemented.
    //
    // "Implemented" means something different for the two registers, and using
    // one test for both would be wrong in both directions. A top-level code is
    // implemented when an occurrence of it is classified as a USE (isImplemented,
    // which knows about taxonomy mirrors and array literals). A field-level code
    // only ever appears as an EMISSION — it is written into an errors[] entry and
    // nowhere else — so the emission scan is its whole notion of implemented.
    if (isField ? emittedAt.has(code) : isImplemented(code)) {
      const at1 = (isField ? emittedAt.get(code) : usesByCode.get(code)?.[0]) ?? "an implementation source";
      failures.push(
        `${at}: ${contract}'s ${code} IS implemented now (${at1}) — delete it from ${ALLOWLIST_PATH}; ` +
          `the list is an inventory of what is missing, and an entry that outlives the gap makes it lie`
      );
    }
  }
}

// #1 every published pair has a verdict.
for (const [contract, codes] of publishedByContract) {
  for (const code of codes) {
    if (isImplemented(code)) continue;
    if (allowed.has(`${contract}\u0000${code}`)) continue;
    failures.push(
      `contracts/${contract}: ${code} is published in the Error taxonomy but no implementation source emits it — ` +
        `wire the refusal, or add ${code} to a group in ${ALLOWLIST_PATH} whose reason says why the surface exists and the refusal does not`
    );
  }
}

// #7 every published FIELD-LEVEL pair has a verdict too.
//
// #1 asks this of the Error taxonomy and always has. The Field-level error
// register arrived later and was not covered by it, so a per-field code could be
// published with nothing emitting it, no reason recorded anywhere, and this gate
// reporting OK — which is the "capability declared, caller never written" shape
// the allowlist exists to make visible, reintroduced one table over from where it
// was closed.
//
// It is not hypothetical: three VARIABLE_* codes were published for a surface
// that does not exist yet and passed green. That is a legitimate thing to do —
// contract text goes up for review before it is built — but it has to be SAID,
// and saying it is what an allowlist entry is for.
for (const [contract, codes] of publishedFieldByContract) {
  for (const code of codes) {
    if (emittedAt.has(code)) continue;
    if (allowed.has(`${contract}\u0000${code}`)) continue;
    failures.push(
      `contracts/${contract}: ${code} is published in the Field-level error register but no implementation source emits it — ` +
        `wire the refusal, or add ${code} to a group in ${ALLOWLIST_PATH} whose reason says why the field exists and the ` +
        `refusal does not. A per-field code nothing raises is a promise to a client that cannot be kept, and it reaches no ` +
        `generated client either.`
    );
  }
}

// #5 a code the Go api layer can actually RETURN must be expressible in the
// OpenAPI ErrorCode enum, or no generated client can name it.
//
// This is a different question from #1-#4, and the gate did not ask it: those walk
// published -> implemented, so a code that IS implemented and IS published can
// still be missing from the schema. That happened — three scope-node delete
// refusals shipped, were published, passed every check here, and were absent from
// the enum, so the console had to compare a raw string and cast past the type
// system. API-011 says a server MUST NOT emit a code outside the registry; a
// registry the clients cannot see is only half a registry.
//
// Scope is deliberately narrow: only codes this repo's own api layer emits, since
// those are the ones an /api/v1 response can carry. A code emitted by the relay's
// player surface rides a different binding and belongs to no enum here.
const OPENAPI_PATH = "api/openapi.yaml";
const API_LAYER_PREFIX = "internal/app/api/";
if (existsSync(OPENAPI_PATH)) {
  const spec = readFileSync(OPENAPI_PATH, "utf8");
  const enumBlock = /\n    ErrorCode:\n(?:.*\n)*?      enum:\n((?:        - \w+\n)+)/.exec(spec);
  if (!enumBlock) {
    failures.push(`${OPENAPI_PATH}: could not locate the ErrorCode enum — this check cannot run, and a check that silently does not run is worse than none`);
  } else {
    const enumerated = new Set(enumBlock[1].split("\n").map((l) => l.trim().replace(/^- /, "")).filter(Boolean));
    for (const code of publishedCodes) {
      if (!usesByCode.get(code)?.some((at) => at.startsWith(API_LAYER_PREFIX))) continue;
      if (enumerated.has(code)) continue;
      const at1 = usesByCode.get(code).find((at) => at.startsWith(API_LAYER_PREFIX));
      failures.push(
        `${OPENAPI_PATH}: ${code} is emitted by the api layer (${at1}) and published in a contract taxonomy, but is not a member of the ErrorCode enum — ` +
          `add it and regenerate both clients, or no generated client can branch on a code this binding returns`
      );
    }
  }
}

// #6 THE REVERSE DIRECTION: a code an implementation EMITS must be published.
//
// Checks #1-#5 all walk published -> implemented. That direction cannot see a
// code that is emitted and published nowhere, in either of its two forms — and
// that is not hypothetical: twenty-two distinct per-field codes were emitted
// across this tree while appearing in no taxonomy any client could read, which
// is exactly what API-011 forbids ("a server MUST NOT emit a `code` value
// outside the registry"). The gate was structurally silent about all of them.
//
// The cost of that silence is concrete. An unpublished code cannot reach a
// generated client, so a console or CLI author cannot branch on it in a typed
// way; they compare a raw string, which is the thing a published registry
// exists to prevent.
//
// WHAT THIS RECOGNISES AS AN EMISSION, and what it therefore does not:
//
//   (a) a struct field named `Code`/`ErrCode` assigned a quoted
//       SCREAMING_SNAKE token — `Error{Field: "x", Code: "Y"}`;
//   (b) a quoted SCREAMING_SNAKE token immediately following a quoted
//       lowercase field name AND followed by a comma — `RowError{"scope_node",
//       "REQUIRED", "..."}` and `add("url", "VALUE_INVALID", "...")`.
//
// The trailing comma in (b) is load-bearing rather than incidental. Without it
// the pattern also matched `q.Set("algorithm", "SHA1")` — a URL query
// parameter, not an error at all. Every real emission carries a human message
// as its third argument, and a two-argument call does not; requiring the comma
// separates them without a list of names to keep up to date.
//
//   (c) a call taking a quoted SCREAMING_SNAKE token as its FIRST argument and
//       a string as its second — `artifactErr("PACK_ARTIFACT_INVALID", "...")`.
//
// (c) was added after check #7 below caught what (a) and (b) missed: the pack
// surface builds its refusals through a helper that takes the code first, so
// twenty-nine codes were emitted through a shape this scan did not recognise.
// The two checks catch each other's blind spots, which is the argument for
// having both rather than trusting either.
//
// (c) would also match a two-string call that is not an error at all —
// `os.Setenv("SOME_VAR", "x")` has the same shape. None exists in this tree
// today (every one of its twenty-nine matches is an error constructor), and if
// one appears it fails loudly asking for the name to be published or changed,
// rather than passing silently. That is the right direction for this check to
// be wrong in.
//
// A call whose field argument is an EXPRESSION rather than a literal — the
// `add(schemaField(i), "VALUE_INVALID", ...)` spelling — is still not matched,
// so a code emitted ONLY that way remains invisible here. That limit is written
// down rather than papered over: it is the same class of blind spot this check
// exists to close, one level in, and closing it properly wants a Go AST pass
// rather than a regex.
for (const [code, at] of [...emittedAt].sort()) {
  if (publishedCodes.has(code) || publishedFieldCodes.has(code)) continue;
  failures.push(
    `${at}: ${code} is emitted but published in no contract — api/1 API-011 forbids emitting a code outside the registry, ` +
      `and an unpublished code cannot reach a generated client, so a caller must compare a raw string. ` +
      `Publish it in the owning contract's "Field-level error register" (per-field, an errors[].code) or its "Error taxonomy" ` +
      `(a top-level code). Ownership follows the FIELD's rule, not the emitting package (api/1 API-013).`
  );
}

// ---------------------------------------------------------------------------
const publishedPairs = [...publishedByContract.values()].reduce((n, c) => n + c.length, 0);
if (failures.length) {
  console.error(failures.join("\n"));
  console.log(`SUMMARY: validate-error-codes: FAILED — ${failures.length} issue(s); first: ${failures[0]}`);
  process.exitCode = 1;
} else {
  // The field-register counts are reported, not merely computed. A reverse
  // check that silently matched nothing — a regex that stopped firing, a walk
  // that skipped the tree — looks exactly like a clean tree, which is the
  // failure this whole check exists to end.
  //
  // It is REPORTED rather than asserted non-zero. A tree with no emissions at
  // all is a legitimate tree — the gate's own fixture trees are exactly that —
  // so "zero" is not evidence of a broken scan in general. What guards the
  // scan instead is scripts/validate-error-codes.test.mjs, which drives an
  // emitted-but-unpublished code through it and requires the failure.
  const fieldPairs = [...publishedFieldByContract.values()].reduce((n, c) => n + c.length, 0);
  console.log(
    `validate-error-codes: OK (${publishedPairs} published (contract, code) pair(s) over ${publishedByContract.size} contract(s), ` +
      `${publishedCodes.size} distinct code(s); ${allowed.size} allowlisted as unimplemented; ` +
      `${fieldPairs} field-level pair(s), ${publishedFieldCodes.size} distinct; ${emittedAt.size} emitted code(s) checked back)`
  );
  console.log(
    `SUMMARY: validate-error-codes: OK (${publishedPairs} published pair(s), ${fieldPairs} field-level pair(s), ` +
      `${emittedAt.size} emitted checked back, ${allowed.size} allowlisted unimplemented)`
  );
}
