// scripts/validate-contracts.test.mjs — exercises validate-contracts.mjs against
// disposable fixture trees built at test runtime. Fixtures are never committed:
// a deliberately-bad contract living under the real contracts/ would itself
// trip the validator it's meant to test.
import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { spawnSync } from "node:child_process";

const VALIDATOR = join(import.meta.dirname, "validate-contracts.mjs");

// A minimal but real contract, used as the baseline every fixture starts from.
const GOOD_CONTRACT = `# Example Contract

**Contract:** example/1
**Version:** 1.0
**Status:** draft

## Normative requirements

**[XXX-001]** The example MUST do a thing.

**[XXX-002]** The example SHOULD do another thing.
`;

// TEMPLATE.md's placeholder header deliberately does NOT match the header
// regex (angle brackets aren't [a-z0-9-]) — proves the exemption is real
// rather than incidental.
const TEMPLATE_FIXTURE = `# <Contract Title>

**Contract:** <slug>/1
**Version:** 1.0
**Status:** draft

**[XXX-000]** placeholder — not a real requirement.
`;

// The traceability README's own worked example references an ID that is not
// defined anywhere in the fixture's contracts — proves it's exempt from the
// "must exist in its contract" rule, same as contracts/README.md.
const TRACEABILITY_README_FIXTURE = `# Traceability maps

Format example only — this ID need not exist:

| req-id | contract §anchor | case-id(s) | status |
|---|---|---|---|
| ZZZ-999 | contracts/example-1.md#normative-requirements | ZZZ-999-example | covered |
`;

const GOOD_TRACEABILITY_MAP = `| req-id | contract §anchor | case-id(s) | status |
|---|---|---|---|
| XXX-001 | contracts/example-1.md#normative-requirements | XXX-001-basic | covered |
| XXX-002 | contracts/example-1.md#normative-requirements | - | TBD-wave1 |
`;

function makeFixture(build) {
  const root = mkdtempSync(join(tmpdir(), "validate-contracts-"));
  const write = (relPath, content) => {
    const full = join(root, relPath);
    mkdirSync(dirname(full), { recursive: true });
    writeFileSync(full, content);
  };
  build(write);
  return root;
}

function writeGoodCorpus(write) {
  write("contracts/README.md", "# Contracts\n\nHeader/ID rules do not apply to this file.\n");
  write("contracts/TEMPLATE.md", TEMPLATE_FIXTURE);
  write("contracts/example-1.md", GOOD_CONTRACT);
  write("conformance/traceability/README.md", TRACEABILITY_README_FIXTURE);
  write("conformance/traceability/example-1.md", GOOD_TRACEABILITY_MAP);
}

function runValidator(cwd) {
  return spawnSync(process.execPath, [VALIDATOR], { cwd, encoding: "utf8" });
}

function withFixture(build, fn) {
  const root = makeFixture(build);
  try {
    fn(root);
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
}

test("good corpus passes", () => {
  withFixture(writeGoodCorpus, (root) => {
    const res = runValidator(root);
    assert.equal(res.status, 0, `expected exit 0, got ${res.status}\n${res.stdout}${res.stderr}`);
    assert.match(res.stdout, /SUMMARY: validate-contracts: OK/);
  });
});

test("duplicate requirement ID across two files fails", () => {
  withFixture(
    (write) => {
      writeGoodCorpus(write);
      write(
        "contracts/example-2.md",
        "# Example Contract Two\n\n**Contract:** example/2\n**Version:** 1.0\n**Status:** draft\n\n**[XXX-001]** Reuses example-1's ID by mistake.\n"
      );
    },
    (root) => {
      const res = runValidator(root);
      assert.notEqual(res.status, 0);
      assert.match(res.stdout + res.stderr, /duplicate requirement ID XXX-001/);
      assert.match(res.stdout, /SUMMARY: validate-contracts: FAILED/);
    }
  );
});

test("traceability referencing a nonexistent ID fails", () => {
  withFixture(
    (write) => {
      writeGoodCorpus(write);
      write(
        "conformance/traceability/example-1.md",
        "| req-id | contract §anchor | case-id(s) | status |\n|---|---|---|---|\n| XXX-404 | contracts/example-1.md#normative-requirements | XXX-404-missing | TBD-wave1 |\n"
      );
    },
    (root) => {
      const res = runValidator(root);
      assert.notEqual(res.status, 0);
      assert.match(res.stdout + res.stderr, /references undefined requirement ID XXX-404/);
      assert.match(res.stdout, /SUMMARY: validate-contracts: FAILED/);
    }
  );
});

test("contract doc with zero requirement IDs fails", () => {
  withFixture(
    (write) => {
      writeGoodCorpus(write);
      write(
        "contracts/example-3.md",
        "# Example Contract Three\n\n**Contract:** example/3\n**Version:** 1.0\n**Status:** draft\n\nNo requirement anchors in this file.\n"
      );
    },
    (root) => {
      const res = runValidator(root);
      assert.notEqual(res.status, 0);
      assert.match(res.stdout + res.stderr, /no requirement-ID anchor found/);
      assert.match(res.stdout, /SUMMARY: validate-contracts: FAILED/);
    }
  );
});
