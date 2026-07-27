// scripts/validate-mcp-tags.test.mjs — exercises validate-mcp-tags.mjs against
// disposable fixture documents built at test runtime. Fixtures are never
// committed: a deliberately-bad api/openapi.yaml living in the repo would
// itself trip the validator it is meant to test, and every other check that
// reads that document too.
//
// Each case is a whole small OpenAPI document rather than a patch to the real
// one, so what a case proves is readable in the case itself.
import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { spawnSync } from "node:child_process";

const VALIDATOR = join(import.meta.dirname, "validate-mcp-tags.mjs");

// The shared preamble: both curation tags declared at document level, plus the
// component parameter a compliant `mcp:act` POST refs.
const PREAMBLE = `openapi: 3.1.0
info:
  title: Fixture
  version: 1.0.0
tags:
  - name: things
    description: Fixture family.
  - name: "mcp:read"
    description: Curation tag.
  - name: "mcp:act"
    description: Curation tag.
`;

const COMPONENTS = `components:
  parameters:
    IdempotencyKeyParam:
      name: Idempotency-Key
      in: header
      required: false
      schema:
        type: string
    TraceIdParam:
      name: Trace-Id
      in: header
      required: false
      schema:
        type: string
`;

/** A document = preamble + the given `paths:` body + components. */
function doc(pathsBody, { preamble = PREAMBLE, components = COMPONENTS } = {}) {
  return `${preamble}paths:\n${pathsBody}\n${components}`;
}

/** Runs the validator in a throwaway tree holding only api/openapi.yaml. */
function run(contents) {
  const root = mkdtempSync(join(tmpdir(), "validate-mcp-tags-"));
  try {
    if (contents !== null) {
      const full = join(root, "api/openapi.yaml");
      mkdirSync(dirname(full), { recursive: true });
      writeFileSync(full, contents);
    }
    const r = spawnSync(process.execPath, [VALIDATOR], { cwd: root, encoding: "utf8" });
    return { status: r.status, out: `${r.stdout}${r.stderr}` };
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
}

// A compliant baseline: one read tool, one act tool that refs the header.
const GOOD_PATHS = `  /things:
    get:
      operationId: listThings
      summary: List things
      tags: [things, "mcp:read"]
      responses:
        "200":
          description: ok
    post:
      operationId: createThing
      summary: Create a thing
      tags: [things, "mcp:act"]
      parameters:
        - $ref: "#/components/parameters/IdempotencyKeyParam"
        - $ref: "#/components/parameters/TraceIdParam"
      responses:
        "201":
          description: created`;

test("a compliant document passes", () => {
  const r = run(doc(GOOD_PATHS));
  assert.equal(r.status, 0, r.out);
  assert.match(r.out, /SUMMARY: validate-mcp-tags: OK \(2 operation\(s\), 2 MCP-curated\)/);
});

test("API-072: an mcp:act POST with no Idempotency-Key fails", () => {
  const r = run(
    doc(`  /things:
    post:
      operationId: createThing
      summary: Create a thing
      tags: [things, "mcp:act"]
      parameters:
        - $ref: "#/components/parameters/TraceIdParam"
      responses:
        "201":
          description: created
    get:
      operationId: listThings
      summary: List things
      tags: [things, "mcp:read"]
      responses:
        "200":
          description: ok`)
  );
  assert.equal(r.status, 1, r.out);
  assert.match(r.out, /POST \/things: is a POST tagged `mcp:act` but does not accept `Idempotency-Key` \(API-072\)/);
});

test("API-072: an inline Idempotency-Key header parameter satisfies the rule", () => {
  const r = run(
    doc(`  /things:
    get:
      operationId: listThings
      summary: List things
      tags: [things, "mcp:read"]
      responses:
        "200":
          description: ok
    post:
      operationId: createThing
      summary: Create a thing
      tags: [things, "mcp:act"]
      parameters:
        - name: Idempotency-Key
          in: header
          required: false
          schema:
            type: string
      responses:
        "201":
          description: created`)
  );
  assert.equal(r.status, 0, r.out);
});

test("API-072: a path-item-level Idempotency-Key is inherited and satisfies the rule", () => {
  const r = run(
    doc(`  /things:
    parameters:
      - $ref: "#/components/parameters/IdempotencyKeyParam"
    get:
      operationId: listThings
      summary: List things
      tags: [things, "mcp:read"]
      responses:
        "200":
          description: ok
    post:
      operationId: createThing
      summary: Create a thing
      tags: [things, "mcp:act"]
      responses:
        "201":
          description: created`)
  );
  assert.equal(r.status, 0, r.out);
});

test("API-072: a $ref to some OTHER component parameter does not satisfy the rule", () => {
  // Proves the ref is RESOLVED rather than merely counted — a check that only
  // noticed "some $ref is present" would pass this.
  const r = run(
    doc(`  /things:
    get:
      operationId: listThings
      summary: List things
      tags: [things, "mcp:read"]
      responses:
        "200":
          description: ok
    post:
      operationId: createThing
      summary: Create a thing
      tags: [things, "mcp:act"]
      parameters:
        - $ref: "#/components/parameters/TraceIdParam"
      responses:
        "201":
          description: created`)
  );
  assert.equal(r.status, 1, r.out);
  assert.match(r.out, /does not accept `Idempotency-Key`/);
});

test("API-072 is POST-only: an mcp:act PATCH needs no Idempotency-Key", () => {
  const r = run(
    doc(`  /things/{id}:
    parameters:
      - name: id
        in: path
        required: true
        schema:
          type: string
    get:
      operationId: getThing
      summary: Read a thing
      tags: [things, "mcp:read"]
      responses:
        "200":
          description: ok
    patch:
      operationId: updateThing
      summary: Update a thing
      tags: [things, "mcp:act"]
      responses:
        "200":
          description: ok`)
  );
  assert.equal(r.status, 0, r.out);
});

test("API-070: an operation carrying both curation tags fails", () => {
  const r = run(
    doc(`  /things:
    get:
      operationId: listThings
      summary: List things
      tags: [things, "mcp:read", "mcp:act"]
      responses:
        "200":
          description: ok`)
  );
  assert.equal(r.status, 1, r.out);
  assert.match(r.out, /carries both `mcp:read` and `mcp:act`\. API-070 requires exactly ONE/);
});

test("API-071: a third mcp: tag is rejected as a second curation channel", () => {
  const r = run(
    doc(`  /things:
    get:
      operationId: listThings
      summary: List things
      tags: [things, "mcp:read", "mcp:internal"]
      responses:
        "200":
          description: ok`)
  );
  assert.equal(r.status, 1, r.out);
  assert.match(r.out, /carries `mcp:internal`, which is not one of api\/1's two curation tags/);
});

test("API-071: a curation tag not declared at document level fails", () => {
  const preamble = `openapi: 3.1.0
info:
  title: Fixture
  version: 1.0.0
tags:
  - name: things
    description: Fixture family.
  - name: "mcp:read"
    description: Curation tag.
`;
  const r = run(doc(GOOD_PATHS, { preamble }));
  assert.equal(r.status, 1, r.out);
  assert.match(r.out, /the document-level `tags` list does not declare `mcp:act`/);
  assert.match(r.out, /carries `mcp:act`, which the document-level `tags` list never declares/);
});

test("a document with no MCP-curated operation fails rather than reporting OK", () => {
  // The non-vacuity guard: a reader that stopped seeing tags would otherwise
  // report the same OK as a clean document.
  const r = run(
    doc(`  /things:
    get:
      operationId: listThings
      summary: List things
      tags: [things]
      responses:
        "200":
          description: ok`)
  );
  assert.equal(r.status, 1, r.out);
  assert.match(r.out, /not one operation carries `mcp:read` or `mcp:act`/);
});

test("a document with no operations at all fails rather than reporting OK", () => {
  const r = run(`${PREAMBLE}paths: {}\n${COMPONENTS}`);
  assert.equal(r.status, 1, r.out);
  assert.match(r.out, /no operations were read out of/);
});

test("an unrecognized key at path-item level is reported, never skipped", () => {
  // A reader that silently ignored what it did not understand would be one
  // future edit away from silently ignoring the operations too.
  const r = run(
    doc(`  /things:
    getx:
      operationId: listThings
      summary: List things
      tags: [things, "mcp:read"]
      responses:
        "200":
          description: ok`)
  );
  assert.equal(r.status, 1, r.out);
  assert.match(r.out, /"getx" is neither an HTTP method nor a path-item key/);
});

test("a missing document fails loudly", () => {
  const r = run(null);
  assert.equal(r.status, 1, r.out);
  assert.match(r.out, /SUMMARY: validate-mcp-tags: FAILED — api\/openapi\.yaml not found/);
});
