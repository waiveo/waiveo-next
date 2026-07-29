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

function parseTaxonomy(path) {
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
    if (/^##\s+Error taxonomy\s*$/.test(line)) {
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
  const seen = new Set();
  for (const code of codes) {
    if (seen.has(code)) failures.push(`${path}: Error taxonomy lists ${code} twice`);
    seen.add(code);
  }
}

const publishedCodes = new Set([...publishedByContract.values()].flat());

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
function insideArrayLiteral(lines, index) {
  for (let i = index - 1; i >= 0; i--) {
    const line = lines[i].trim();
    if (line === "") continue;
    if (BARE_ELEMENT_RE.test(line)) continue;
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
  sourceFiles.push({ rel, lines: text.split("\n"), brs: rel.endsWith(".brs") });
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
  for (const code of codes) {
    const key = `${contract}\u0000${code}`;
    if (allowed.has(key)) {
      failures.push(`${at}: ${contract} lists ${code} twice — one entry per (contract, code) pair`);
      continue;
    }
    allowed.set(key, reason);
    // #2 the pair is really published.
    if (!published.includes(code)) {
      failures.push(
        `${at}: ${contract} does not publish ${code} in its Error taxonomy — the code was renamed or removed; delete this entry`
      );
      continue;
    }
    // #3 the pair is still unimplemented.
    if (isImplemented(code)) {
      const at1 = usesByCode.get(code)?.[0] ?? "an implementation source";
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

// ---------------------------------------------------------------------------
const publishedPairs = [...publishedByContract.values()].reduce((n, c) => n + c.length, 0);
if (failures.length) {
  console.error(failures.join("\n"));
  console.log(`SUMMARY: validate-error-codes: FAILED — ${failures.length} issue(s); first: ${failures[0]}`);
  process.exitCode = 1;
} else {
  console.log(
    `validate-error-codes: OK (${publishedPairs} published (contract, code) pair(s) over ${publishedByContract.size} contract(s), ` +
      `${publishedCodes.size} distinct code(s); ${allowed.size} allowlisted as unimplemented)`
  );
  console.log(
    `SUMMARY: validate-error-codes: OK (${publishedPairs} published pair(s), ${allowed.size} allowlisted unimplemented)`
  );
}
