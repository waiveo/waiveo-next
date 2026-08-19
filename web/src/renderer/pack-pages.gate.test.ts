/// <reference types="node" />
import { execFileSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import { basename, dirname, resolve } from "node:path";
import { describe, expect, it } from "vitest";
import { validatePage } from "./validate";
import { WIDGET_CATALOG } from "./schema";
import type { ValidationError } from "./schema";

// THE SHIPPED-PAGE GATE — the authoring half of UIS-200.
//
// validate.corpus.test.ts proves the VALIDATOR matches the frozen corpus. This
// file proves the PAGES WE SHIP match the validator. Until it existed a pack
// could be built, signed, published, installed and enabled while carrying a page
// the host refuses at render time, and every gate stayed green: the only detector
// was an operator clicking the link (#205 — waiveo/discovery's settings page, the
// deployment's ONLY UI surface, dead on the box with WIDGET_PROP_UNKNOWN +
// ACTION_FIELDS_INVALID). The contract sanctions exactly this check: UIS-201 — "a
// host MAY validate a bundled page document at pack-build/lint time, ahead of any
// install" — and renderer/index.ts already advertises validatePage for it.
//
// AUTHORITY BY CONSTRUCTION, NOT BY RESEMBLANCE. This gate holds a direct module
// reference to `validatePage`, the SAME symbol PageRenderer.tsx and
// routes/packs/pack-page-route.tsx call at render time. One compilation unit: an
// edit to validate.ts changes this gate's verdict in the same commit, so there is
// no copy to drift. The gate asserts only on `result.ok` and prints
// `result.errors` verbatim — it never restates a rule, a taxonomy code, or a
// catalog entry. Adding a prop to schema.ts automatically loosens it; removing one
// automatically tightens it.
//
// A SECOND VALIDATOR THAT DIVERGES IS WORSE THAN NO GATE, and that is not
// hypothetical: conformance/fixtures/fixture-lint.mjs regexes the closed sets out
// of contract prose and hand-rolls the UIS-100 grammar. Pointed at the discovery
// page the host refuses, it exits 0 — it has no prop model and never checks
// ActionRef field sets. Nothing here may be written that way. If you find yourself
// adding a rule to this file, you are writing the second validator: stop, and put
// it in validate.ts where the renderer will enforce it too.
//
// WHAT THIS GATE CANNOT SEE, stated so nobody assumes otherwise.
//
// (a) Anything the page document alone cannot answer. A page may be flawless
//     ui-schema/1 and still be refused by the host for disagreeing with the
//     MANIFEST beside it — a settings-form `source` naming no singleton
//     collection (MAN-064, refused at install), a `bind` naming an undeclared
//     field (MAN-051, 422 UNKNOWN_FIELD at Save), a `call-action` naming an
//     undeclared action (MAN-100, 404 at the button). Those authorities are all
//     Go and a TypeScript test cannot reach them, so they are gated by the
//     sibling of this file: internal/app/packs/packpages_gate_test.go. The two
//     gates derive their scope the same way (git ls-files → every pack
//     manifest), so a page added tomorrow is swept by both.
//
// (b) A rule the contract states but assigns NO taxonomy code. UIS-061 is the
//     live example: it says a widget node's `id` is "a Kebab identifier, unique
//     within its page document", and three shipped documents ship prose there
//     (devices/adopted-devices.uis.json "Adopted devices", settings/page.uis.json
//     "Sites", menu-board/ui/menu-items.json "Menu items"). validate.ts does not
//     enforce it and MUST NOT start: UIS-200 requires that "Every rejection MUST
//     report a `code` from Error taxonomy", and the taxonomy has none for a
//     malformed `id` — while it deliberately assigns one to each NEIGHBOURING
//     identifier rule (SLOT_NAME_DUPLICATE for UIS-186, WIZARD_STEP_ID_DUPLICATE
//     for UIS-050). Enforcing UIS-061 means inventing a code, which is the same
//     divergence this header forbids; validate.ts already states that discipline
//     ("there is no taxonomy code for a wrong scalar type, so the validator never
//     invents one"). Note also that `id` is NOT inert prose: widgets/display.tsx
//     hands it to DataTable as the table's accessible name, and `table` has no
//     `labelMsg` in the catalog, so kebab-casing those ids would DEGRADE three
//     a11y names to buy an unenforceable rule. Giving `table` a real `labelMsg`
//     is a ui-schema/1 minor and its own piece of work — not a silent edit here.

const PACK_ID = /^[a-z0-9-]+\/[a-z0-9-]+$/;

// Read the repo off disk at RUNTIME with Node fs (never a `new URL(...,
// import.meta.url)` — Vite rewrites that into an asset import Vite's fs-allow then
// denies for a path outside the web project). Vitest runs with cwd = web/, so the
// repo root is its parent. Anchor on go.mod + contracts/ — repo-level facts —
// rather than on any one pack, so moving or deleting a pack cannot silently
// repoint the root.
function repoRoot(): string {
  const cwd = process.cwd();
  for (const candidate of [resolve(cwd, ".."), cwd]) {
    if (existsSync(resolve(candidate, "go.mod")) && existsSync(resolve(candidate, "contracts"))) {
      return candidate;
    }
  }
  throw new Error(`pack-page-gate: cannot locate the repo root (go.mod + contracts/) from cwd ${cwd}`);
}

// SCOPE IS DERIVED, NEVER LISTED. A hard-coded file list is the bug this gate
// exists to prevent — a page added tomorrow in a directory nobody has created yet
// must be caught. `git ls-files` is the correct source: tracked-only excludes
// node_modules, build output, and .claude/worktrees (90 stale *.uis.json copies a
// repo-wide fs glob would happily sweep in) with no maintained exclusion list.
// import.meta.glob is not usable here either — a static literal pattern is a list
// with extra steps, and it resolves at transform time against the same worktrees.
function trackedFiles(root: string): string[] {
  const out = execFileSync("git", ["-C", root, "ls-files", "-z"], {
    encoding: "utf8",
    maxBuffer: 64 * 1024 * 1024,
  });
  return out.split("\0").filter(Boolean);
}

interface DeclaredPage {
  /** repo-relative path of the page document (UIS-001) */
  file: string;
  packId: string;
  /** the manifest's ui.pages[].path */
  path: string;
  /** the manifest's declared pageType, for the cross-check against the document */
  pageType: string;
}

interface Pack {
  dir: string;
  id: string;
  pages: DeclaredPage[];
  messages: Record<string, string>;
}

const root = repoRoot();
const tracked = trackedFiles(root);
const readJson = (rel: string): unknown => JSON.parse(readFileSync(resolve(root, rel), "utf8"));

// A pack is a directory with a manifest.json declaring a publisher/name `id` and a
// `compat` object. That discriminator rejects conformance/driven-manifest.json
// (which is a contract-driver index, not a pack) without naming it.
const packs: Pack[] = [];
for (const file of tracked) {
  if (basename(file) !== "manifest.json") continue;
  let m: Record<string, unknown>;
  try {
    m = readJson(file) as Record<string, unknown>;
  } catch {
    continue;
  }
  if (typeof m.id !== "string" || !PACK_ID.test(m.id)) continue;
  if (typeof m.compat !== "object" || m.compat === null) continue;

  const dir = dirname(file);
  const ui = m.ui as { pages?: Array<{ path?: unknown; pageType?: unknown }> } | null | undefined;
  const pages: DeclaredPage[] = [];
  for (const p of ui?.pages ?? []) {
    if (typeof p?.path !== "string") continue;
    pages.push({
      // Mirrors pageDocPath() in internal/app/packs/packs.go (UIS-001): a
      // manifest's ui.pages[].path P resolves to the bundle entry ui/P.json.
      file: `${dir}/ui/${p.path}.json`,
      packId: m.id,
      path: p.path,
      pageType: typeof p.pageType === "string" ? p.pageType : "",
    });
  }
  let messages: Record<string, string> = {};
  const catalog = `${dir}/messages/en.json`;
  if (existsSync(resolve(root, catalog))) {
    try {
      messages = readJson(catalog) as Record<string, string>;
    } catch {
      messages = {};
    }
  }
  packs.push({ dir, id: m.id, pages, messages });
}

const declared = packs.flatMap((p) => p.pages);
const declaredFiles = new Set(declared.map((d) => d.file));
const packDirs = packs.map((p) => p.dir);

// Every tracked */ui/*.json sitting under a discovered pack root. The orphan check
// below compares this against what the manifests actually declare.
const onDisk = tracked.filter(
  (f) => f.endsWith(".json") && /\/ui\/[^/]+\.json$/.test(f) && packDirs.some((d) => f.startsWith(`${d}/`)),
);

// The console's own route-local page documents travel the same renderer, so they
// belong in the same sweep — today only one of the three carries an explicit
// validatePage assertion of its own.
const consoleDocs = tracked.filter((f) => f.startsWith("web/src/") && f.endsWith(".uis.json"));

/** One hint line per error, GENERATED from the catalog so it cannot go stale — no
 * hand-written prose about the contract anywhere in this gate. */
function hint(err: ValidationError, doc: unknown): string | null {
  const m = /^(.*)\.props\.([A-Za-z0-9_]+)$/.exec(err.path);
  if (!m) return null;
  const type = widgetTypeAt(doc, m[1]);
  const spec = type ? WIDGET_CATALOG[type] : undefined;
  if (!spec) return null;
  const accepts = Object.entries(spec.props)
    .map(([k, d]) => (d.required ? `${k} (required)` : k))
    .join(", ");
  return `${type} accepts: ${accepts || "(no props)"}`;
}

/** Walk a dotted/bracket ValidationError path to the node it names, to recover that
 * node's widget `type` for the hint line. Pure navigation — no validation. */
function widgetTypeAt(doc: unknown, path: string): string | null {
  let cur: unknown = doc;
  for (const seg of path.split(/\.|\[(\d+)\]/).filter(Boolean)) {
    if (cur === null || typeof cur !== "object") return null;
    cur = Array.isArray(cur) ? cur[Number(seg)] : (cur as Record<string, unknown>)[seg];
  }
  if (cur !== null && typeof cur === "object" && typeof (cur as { type?: unknown }).type === "string") {
    return (cur as { type: string }).type;
  }
  return null;
}

function label(file: string): string {
  const d = declared.find((x) => x.file === file);
  return d ? `${file}  (${d.packId}, page "${d.path}", ${d.pageType})` : file;
}

describe("ui-schema/1 — every shipped page document validates under the host's own validatePage", () => {
  // A gate that silently matches nothing is not a gate. If discovery ever breaks —
  // a moved directory, a renamed manifest, a git invocation that returns nothing —
  // these fail loudly rather than reporting a green zero.
  it("discovers packs and page documents (a zero-length sweep is a broken gate, not a clean repo)", () => {
    expect(packs.length).toBeGreaterThan(0);
    expect(declared.length).toBeGreaterThan(0);
    expect(consoleDocs.length).toBeGreaterThan(0);
  });

  it("every page a manifest declares exists at ui/<path>.json (UIS-001)", () => {
    const missing = declared.filter((d) => !existsSync(resolve(root, d.file)));
    expect(
      missing.map((d) => `${d.packId} declares page "${d.path}" but ${d.file} does not exist`),
    ).toEqual([]);
  });

  it("every shipped ui/*.json is declared by its own pack manifest (no unreachable page)", () => {
    const orphans = onDisk.filter((f) => !declaredFiles.has(f));
    expect(
      orphans.map((f) => `${f} looks like a pack page but no manifest declares it — nothing would ever open it`),
    ).toEqual([]);
  });

  it("each document's own pageType matches the pageType its manifest declares", () => {
    const mismatched: string[] = [];
    for (const d of declared) {
      if (!existsSync(resolve(root, d.file))) continue;
      const doc = readJson(d.file) as { pageType?: unknown };
      if (doc.pageType !== d.pageType) {
        mismatched.push(`${d.file}: document pageType ${JSON.stringify(doc.pageType)} != manifest ${JSON.stringify(d.pageType)}`);
      }
    }
    expect(mismatched).toEqual([]);
  });

  // ── THE GATE ──────────────────────────────────────────────────────────────
  it("validates every shipped page document with the renderer's own validatePage (UIS-200/201)", () => {
    const targets = [...new Set([...declared.map((d) => d.file), ...onDisk, ...consoleDocs])].sort();
    const report: string[] = [];
    let badDocs = 0;
    let badErrors = 0;
    let first = "";

    for (const file of targets) {
      if (!existsSync(resolve(root, file))) continue;
      const doc = readJson(file);
      // THE authority: the same function the pack page route calls at :209 and
      // PageRenderer calls at :89. Not a resemblance of it.
      const result = validatePage(doc);
      if (result.ok) continue;
      badDocs++;
      badErrors += result.errors.length;
      report.push(`${label(file)}`);
      for (const e of result.errors) {
        // Code, path and message come straight off the ValidationError the host
        // produced — never reworded here.
        report.push(`  ${e.code.padEnd(28)} ${e.path}`);
        report.push(`      ${e.message}`);
        const h = hint(e, doc);
        if (h) report.push(`      ${h}`);
        if (!first) first = `${file} ${e.code} ${e.path}`;
      }
      report.push("");
    }

    if (badDocs > 0) {
      throw new Error(
        `pack-page-gate: ${badDocs} shipped page document(s) are not valid ui-schema/1.\n` +
          `These are REFUSED at render time — the operator sees "This page could not be displayed".\n\n` +
          `${report.join("\n")}\n` +
          `SUMMARY: pack-page-gate: FAILED — ${badDocs} document(s), ${badErrors} error(s); first: ${first}`,
      );
    }

    expect(badDocs).toBe(0);
  });

  // Its own named assertion: a page that validates but references a message key
  // its pack does not ship is the same defect with a different detector (a silent
  // humanized fallback nobody sees fail), and it must never read as a ui-schema
  // failure. MAN-111.
  it("every msg: reference in a shipped page resolves in its pack's own catalog (MAN-111)", () => {
    const dangling: string[] = [];
    for (const pack of packs) {
      for (const d of pack.pages) {
        if (!existsSync(resolve(root, d.file))) continue;
        for (const ref of msgRefs(readJson(d.file))) {
          const key = ref.slice(4);
          if (!(key in pack.messages)) dangling.push(`${d.file}: "${ref}" is not in ${pack.dir}/messages/en.json`);
        }
      }
    }
    expect(dangling).toEqual([]);
  });
});

/** Collect every `msg:` reference anywhere in a document — a generic string walk,
 * so it picks up refs wherever they sit (a prop, a section title, a column header,
 * or a `msg` Computed's pinned arg0) without encoding where those places are. */
function msgRefs(node: unknown, out = new Set<string>()): Set<string> {
  if (typeof node === "string") {
    if (node.startsWith("msg:")) out.add(node);
  } else if (Array.isArray(node)) {
    for (const v of node) msgRefs(v, out);
  } else if (node !== null && typeof node === "object") {
    for (const v of Object.values(node)) msgRefs(v, out);
  }
  return out;
}
