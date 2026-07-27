#!/usr/bin/env node
// scripts/validate-mcp-tags.mjs — the `mcp:` operation-tag curation rules from
// contracts/api-1.md (API-070/071/072), enforced against api/openapi.yaml.
//
// Those three requirements make the OpenAPI document the SOLE input to MCP tool
// generation: whichever operations carry `mcp:read` or `mcp:act` become tools,
// with no allowlist anywhere else to consult or forget. That is a good design
// and a fragile one — it means a one-word edit to a `tags:` line silently
// publishes or retires an MCP tool, and, for a mutating one, silently decides
// whether an MCP client's own retry-on-timeout can double-apply it. Until now
// nothing checked any of it.
//
// Checks:
//   1. (API-070) No operation carries BOTH `mcp:read` and `mcp:act`. The rule is
//      "exactly one of the two" for an operation intended as a tool; carrying
//      NEITHER is not a violation — that is simply an operation nobody curated,
//      which API-070's own second sentence provides for — so the enforceable
//      half is that the two are mutually exclusive. An operation claiming both
//      would be simultaneously "safe to retry" and "mutates state".
//   2. (API-071) The only `mcp:`-prefixed tags in the document are those two,
//      and both are declared in the document-level `tags` list. A third
//      `mcp:something` tag would be a second curation channel invented beside
//      the one the contract says is sole; an undeclared one would steer tool
//      generation from a name the document never introduces.
//   3. (API-072) Every operation tagged `mcp:act` whose method is POST accepts
//      `Idempotency-Key`. Path-item-level parameters count: a parameter
//      declared once for the whole path applies to each operation on it, and an
//      operation that inherits the header does accept it. Non-POST `mcp:act`
//      operations (PATCH, DELETE) are deliberately out of scope — API-072 says
//      POST, because those verbs are already addressed by the If-Match
//      precondition rather than by a key.
//
// It reads the document with a deliberately narrow, strict reader rather than a
// YAML library: this repo's validators are dependency-free node scripts that run
// in every CI tier, including the node-only nightly lane. The reader accepts the
// shapes this document actually uses and FAILS on anything else it meets inside
// `paths:` — silence on an unrecognized line is how a parser-based check starts
// passing because it stopped seeing the operations, and a check that cannot see
// a violation reports the same "OK" as a document that has none.
import { readFileSync, existsSync } from "node:fs";

const DOC = "api/openapi.yaml";

const HTTP_METHODS = new Set(["get", "put", "post", "delete", "options", "head", "patch", "trace"]);
// Keys a path item may carry beside its operations (OpenAPI 3.1 Path Item
// Object). Anything else at that level is a shape this reader has not been
// taught, and is reported rather than skipped.
const PATH_ITEM_KEYS = new Set(["summary", "description", "servers", "parameters", "$ref"]);
const CURATION_TAGS = new Set(["mcp:read", "mcp:act"]);
const IDEMPOTENCY_KEY = "Idempotency-Key";

const failures = [];

// ---------------------------------------------------------------------------
// Reader
// ---------------------------------------------------------------------------

/** Every non-blank, non-comment line of the top-level `key:` block, as
 *  {lineNo, indent, text} — `lineNo` 1-based so a failure names a real line. */
function topLevelBlock(lines, key) {
  const start = lines.findIndex((l) => l === `${key}:`);
  if (start === -1) return null;
  const out = [];
  for (let i = start + 1; i < lines.length; i++) {
    const raw = lines[i];
    if (raw.trim() === "" || raw.trim().startsWith("#")) continue;
    if (!/^\s/.test(raw)) break; // the next top-level key ends the block
    out.push({ lineNo: i + 1, indent: raw.length - raw.trimStart().length, text: raw.trim() });
  }
  return out;
}

/** The document-level `tags:` names — a block sequence of `- name: <x>` items. */
function readDeclaredTags(lines) {
  const block = topLevelBlock(lines, "tags");
  if (!block) return new Set();
  const names = new Set();
  for (const { text } of block) {
    const m = /^- name: (.+)$/.exec(text);
    if (m) names.add(unquote(m[1]));
  }
  return names;
}

/** `components.parameters` as name -> {name, in} — what a `$ref` to one
 *  resolves to, so the Idempotency-Key check tests the referenced PARAMETER
 *  rather than merely counting that some ref was present. */
function readComponentParameters(lines) {
  const block = topLevelBlock(lines, "components");
  const out = new Map();
  if (!block) return out;
  const paramsAt = block.findIndex((e) => e.indent === 2 && e.text === "parameters:");
  if (paramsAt === -1) return out;
  let current = null;
  for (let i = paramsAt + 1; i < block.length; i++) {
    const e = block[i];
    if (e.indent <= 2) break; // the next components/* section
    if (e.indent === 4) {
      const m = /^([A-Za-z0-9_.-]+):$/.exec(e.text);
      current = m ? m[1] : null;
      if (current) out.set(current, { name: null, in: null });
      continue;
    }
    if (e.indent === 6 && current) {
      const m = /^(name|in): (.+)$/.exec(e.text);
      if (m) out.get(current)[m[1]] = unquote(m[2]);
    }
  }
  return out;
}

/** Every operation in `paths:`, as {path, method, lineNo, tags, params}. */
function readOperations(lines) {
  const block = topLevelBlock(lines, "paths");
  if (!block) {
    failures.push(`${DOC}: no top-level \`paths:\` block — the document this check reads is not the document it expected`);
    return [];
  }
  const operations = [];
  let path = null;
  let pathParams = [];
  let op = null;

  for (let i = 0; i < block.length; i++) {
    const e = block[i];

    if (e.indent === 2) {
      const m = /^(\/\S*):$/.exec(e.text);
      if (!m) {
        failures.push(`${DOC}:${e.lineNo}: expected a path key (\`/...:\`) at this indentation, found ${JSON.stringify(e.text)}`);
        continue;
      }
      path = m[1];
      pathParams = [];
      op = null;
      continue;
    }

    if (e.indent === 4) {
      const m = /^([A-Za-z0-9_$-]+):$/.exec(e.text);
      if (!m) {
        failures.push(`${DOC}:${e.lineNo}: expected an operation or path-item key at this indentation, found ${JSON.stringify(e.text)}`);
        op = null;
        continue;
      }
      const key = m[1].toLowerCase();
      if (HTTP_METHODS.has(key)) {
        op = { path, method: key, lineNo: e.lineNo, tags: [], params: [], get pathParams() { return pathParams; } };
        operations.push(op);
      } else if (key === "parameters") {
        op = null;
        pathParams = readParameterList(block, i, 6);
      } else if (PATH_ITEM_KEYS.has(key)) {
        op = null;
      } else {
        failures.push(`${DOC}:${e.lineNo}: ${JSON.stringify(m[1])} is neither an HTTP method nor a path-item key — this reader does not know what it means, and guessing is how a check stops seeing operations`);
        op = null;
      }
      continue;
    }

    if (e.indent === 6 && op) {
      if (e.text.startsWith("tags:")) {
        op.tags = readTagList(block, i);
      } else if (e.text === "parameters:") {
        op.params = readParameterList(block, i, 8);
      }
    }
  }
  return operations;
}

/** A `tags:` value, flow (`tags: [a, "b"]`) or block (`- a` items below). */
function readTagList(block, at) {
  const e = block[at];
  const inline = /^tags:\s*\[(.*)\]$/.exec(e.text);
  if (inline) {
    return inline[1]
      .split(",")
      .map((t) => unquote(t.trim()))
      .filter((t) => t !== "");
  }
  if (e.text !== "tags:") {
    failures.push(`${DOC}:${e.lineNo}: unreadable \`tags:\` value ${JSON.stringify(e.text)} — expected a flow sequence or a block sequence`);
    return [];
  }
  const out = [];
  for (let i = at + 1; i < block.length && block[i].indent > e.indent; i++) {
    const m = /^- (.+)$/.exec(block[i].text);
    if (m) out.push(unquote(m[1]));
  }
  return out;
}

/** A `parameters:` list as [{ref}|{name, in}] entries at `itemIndent`. */
function readParameterList(block, at, itemIndent) {
  const out = [];
  let current = null;
  for (let i = at + 1; i < block.length && block[i].indent >= itemIndent; i++) {
    const e = block[i];
    if (e.indent === itemIndent && e.text.startsWith("- ")) {
      const body = e.text.slice(2);
      const ref = /^\$ref: "#\/components\/parameters\/([A-Za-z0-9_.-]+)"$/.exec(body);
      if (ref) {
        out.push({ ref: ref[1] });
        current = null;
        continue;
      }
      current = { name: null, in: null };
      out.push(current);
      const kv = /^(name|in): (.+)$/.exec(body);
      if (kv) current[kv[1]] = unquote(kv[2]);
      continue;
    }
    if (e.indent === itemIndent + 2 && current) {
      const kv = /^(name|in): (.+)$/.exec(e.text);
      if (kv) current[kv[1]] = unquote(kv[2]);
    }
  }
  return out;
}

function unquote(s) {
  const t = s.trim();
  if ((t.startsWith('"') && t.endsWith('"')) || (t.startsWith("'") && t.endsWith("'"))) return t.slice(1, -1);
  return t;
}

// ---------------------------------------------------------------------------
// Rules
// ---------------------------------------------------------------------------

if (!existsSync(DOC)) {
  console.error(`${DOC}: not found`);
  console.log(`SUMMARY: validate-mcp-tags: FAILED — ${DOC} not found`);
  process.exit(1);
}

const lines = readFileSync(DOC, "utf8").split("\n");
const declaredTags = readDeclaredTags(lines);
const componentParameters = readComponentParameters(lines);
const operations = readOperations(lines);

let curatedCount = 0;

for (const op of operations) {
  const where = `${DOC}:${op.lineNo}: ${op.method.toUpperCase()} ${op.path}`;
  const curation = op.tags.filter((t) => CURATION_TAGS.has(t));
  const strayMcp = op.tags.filter((t) => t.startsWith("mcp:") && !CURATION_TAGS.has(t));

  // API-071 — no third curation channel.
  for (const tag of strayMcp) {
    failures.push(
      `${where}: carries \`${tag}\`, which is not one of api/1's two curation tags. ` +
        `API-071 makes \`mcp:read\`/\`mcp:act\` the SOLE input to MCP tool generation; a third \`mcp:\` tag is a second channel beside the sole one.`
    );
  }

  // API-070 — the two are mutually exclusive.
  if (curation.length > 1) {
    failures.push(
      `${where}: carries both \`mcp:read\` and \`mcp:act\`. API-070 requires exactly ONE: ` +
        `an operation cannot be both free of side effects a retry could double-apply and a mutation of state.`
    );
  }
  if (curation.length === 1) curatedCount++;

  // API-071 — a curation tag must be declared at document level.
  for (const tag of curation) {
    if (!declaredTags.has(tag)) {
      failures.push(`${where}: carries \`${tag}\`, which the document-level \`tags\` list never declares — tool generation would be steered by a name this document does not introduce.`);
    }
  }

  // API-072 — a mutating MCP tool must be safe against an MCP client's retry.
  if (op.method === "post" && op.tags.includes("mcp:act") && !acceptsIdempotencyKey(op)) {
    failures.push(
      `${where}: is a POST tagged \`mcp:act\` but does not accept \`${IDEMPOTENCY_KEY}\` (API-072). ` +
        `Add \`- $ref: "#/components/parameters/IdempotencyKeyParam"\` to its parameters, or drop the \`mcp:act\` tag — ` +
        `an MCP client retries on timeout by itself, and without the header that retry re-applies the mutation.`
    );
  }
}

/** Whether op declares the Idempotency-Key header, on itself or on its path
 *  item — an inherited parameter is accepted by the operation just the same. */
function acceptsIdempotencyKey(op) {
  return [...op.params, ...op.pathParams].some((p) => {
    const resolved = p.ref ? componentParameters.get(p.ref) : p;
    return resolved && resolved.name === IDEMPOTENCY_KEY && resolved.in === "header";
  });
}

// Structural sanity. These do not restate a rule — they assert the READER saw a
// document, so that a reader broken by a future edit fails loudly instead of
// reporting the same "OK" a clean document does.
if (operations.length === 0) {
  failures.push(`${DOC}: no operations were read out of \`paths:\` — this check would pass on any document, so it proves nothing`);
}
if (curatedCount === 0) {
  failures.push(
    `${DOC}: not one operation carries \`mcp:read\` or \`mcp:act\`. Either the reader stopped seeing tags, or ` +
      `every MCP tool has been retired at once (API-071) — both are worth stopping for.`
  );
}
for (const tag of CURATION_TAGS) {
  if (!declaredTags.has(tag)) {
    failures.push(`${DOC}: the document-level \`tags\` list does not declare \`${tag}\` — api/1 names both curation tags, and a reader that cannot find them cannot check them.`);
  }
}

if (failures.length) {
  console.error(failures.join("\n"));
  console.log(`SUMMARY: validate-mcp-tags: FAILED — ${failures.length} issue(s); first: ${failures[0]}`);
  process.exitCode = 1;
} else {
  console.log(`validate-mcp-tags: OK (${operations.length} operation(s), ${curatedCount} curated as MCP tools)`);
  console.log(`SUMMARY: validate-mcp-tags: OK (${operations.length} operation(s), ${curatedCount} MCP-curated)`);
}
