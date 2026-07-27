// scripts/validate-coverage.test.mjs — exercises validate-coverage.mjs against
// disposable fixture trees built at test runtime, mirroring
// validate-contracts.test.mjs's pattern: a deliberately-bad tree living under
// the real repo would itself trip the validator it's meant to test.
import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { spawnSync } from "node:child_process";

const VALIDATOR = join(import.meta.dirname, "validate-coverage.mjs");

const GOOD_CONTRACT = `# Example Contract

**Contract:** example/1
**Version:** 1.0
**Status:** draft

## Normative requirements

**[XXX-001]** The example MUST do a thing.

**[XXX-002]** The example SHOULD do another thing.
`;

// caseJson builds an XXX-001-basic-shaped envelope with the given overrides —
// used (rather than string-replace on JSON.stringify's pretty-printed output)
// whenever a test needs to change an array-valued field like req_ids, since
// pretty-printing spreads a one-element array across lines and a naive
// string.replace on the inline form would silently no-op.
function caseJson(overrides = {}) {
  return JSON.stringify({
    case_id: "XXX-001-basic",
    contract: "example/1",
    req_ids: ["XXX-001"],
    description: "Exercises XXX-001's basic path.",
    input: { a: 1 },
    expected: { ok: true },
    ...overrides,
  });
}

const GOOD_CASE_001 = caseJson();

const GOOD_TRACEABILITY_MAP = `| req-id | contract §anchor | case-id(s) | status |
|---|---|---|---|
| XXX-001 | \`contracts/example-1.md#normative-requirements\` | \`XXX-001-basic\` | covered |
| XXX-002 | \`contracts/example-1.md#normative-requirements\` | - | TBD-wave1 |
`;

const GOOD_MANIFEST = JSON.stringify(
  {
    "example/1": {
      driver: "test fixture",
      driven: ["XXX-001-basic"],
      pending: [],
    },
  },
  null,
  2
);

const GOOD_INDEX = `# Conformance Traceability Index

| Contract | Requirements | Seed cases | covered | TBD-wave1 | Open draft-notes |
|---|---|---|---|---|---|
| example/1 | 2 | 1 | 1 | 1 | 0 |
| **Total** | **2** | **1** | **1** | **1** | **0** |
`;

function makeFixture(build) {
  const root = mkdtempSync(join(tmpdir(), "validate-coverage-"));
  const write = (relPath, content) => {
    const full = join(root, relPath);
    mkdirSync(dirname(full), { recursive: true });
    writeFileSync(full, content);
  };
  build(write);
  return root;
}

function writeGoodTree(write) {
  write("contracts/example-1.md", GOOD_CONTRACT);
  write("conformance/corpora/example-1/XXX-001-basic.json", GOOD_CASE_001);
  write("conformance/traceability/example-1.md", GOOD_TRACEABILITY_MAP);
  write("conformance/traceability/INDEX.md", GOOD_INDEX);
  write("conformance/driven-manifest.json", GOOD_MANIFEST);
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

test("good tree passes", () => {
  withFixture(writeGoodTree, (root) => {
    const res = runValidator(root);
    assert.equal(res.status, 0, `expected exit 0, got ${res.status}\n${res.stdout}${res.stderr}`);
    assert.match(res.stdout, /SUMMARY: validate-coverage: OK/);
  });
});

test("a status other than covered/TBD-wave1 fails", () => {
  withFixture(
    (write) => {
      writeGoodTree(write);
      write(
        "conformance/traceability/example-1.md",
        GOOD_TRACEABILITY_MAP.replace("| covered |", "| done |")
      );
    },
    (root) => {
      const res = runValidator(root);
      assert.notEqual(res.status, 0);
      const out = res.stdout + res.stderr;
      assert.match(out, /XXX-001/);
      assert.match(out, /status "done"/);
      assert.match(out, /must be exactly "covered" or "TBD-wave1"/);
      assert.match(res.stdout, /SUMMARY: validate-coverage: FAILED/);
    }
  );
});

test("a covered row naming a case with no matching corpus file fails", () => {
  withFixture(
    (write) => {
      writeGoodTree(write);
      write(
        "conformance/traceability/example-1.md",
        GOOD_TRACEABILITY_MAP.replace("XXX-001-basic", "XXX-001-typo")
      );
    },
    (root) => {
      const res = runValidator(root);
      assert.notEqual(res.status, 0);
      const out = res.stdout + res.stderr;
      assert.match(out, /XXX-001-typo/);
      assert.match(out, /no such file conformance\/corpora\/example-1\/XXX-001-typo\.json/);
      assert.match(out, /fix the typo or freeze the case/);
      assert.match(res.stdout, /SUMMARY: validate-coverage: FAILED/);
    }
  );
});

test("a covered row whose case is not in its own contract's driven set fails, naming the fix", () => {
  withFixture(
    (write) => {
      writeGoodTree(write);
      // The case is real, but no driver drives it.
      write("conformance/driven-manifest.json", JSON.stringify({ "example/1": { driver: "test", driven: [], pending: [] } }));
    },
    (root) => {
      const res = runValidator(root);
      assert.notEqual(res.status, 0);
      const out = res.stdout + res.stderr;
      assert.match(out, /XXX-001/);
      assert.match(out, /XXX-001-basic/);
      assert.match(out, /none of \[.*\] appears in example\/1's own driven set/);
      assert.match(out, /conformance\/driven-manifest\.json/);
      assert.match(out, /downgrade the row/);
      assert.match(res.stdout, /SUMMARY: validate-coverage: FAILED/);
    }
  );
});

test("a covered row's case must be in ITS OWN contract's driven set — another contract's driven list vouching for it still fails (closes the cross-contract poisoning exploit)", () => {
  withFixture(
    (write) => {
      // Two independent contracts. other/1's own driver has driven nothing,
      // but example/1's manifest entry (a completely different contract)
      // happens to list other/1's case id — a flattened cross-contract union
      // would wrongly let that vouch for other/1's covered row.
      writeGoodTree(write);
      write(
        "contracts/other-1.md",
        "# Other Contract\n\n**Contract:** other/1\n**Version:** 1.0\n**Status:** draft\n\n**[YYY-001]** placeholder.\n"
      );
      write(
        "conformance/corpora/other-1/YYY-001-basic.json",
        JSON.stringify({
          case_id: "YYY-001-basic",
          contract: "other/1",
          req_ids: ["YYY-001"],
          description: "d",
          input: {},
          expected: {},
        })
      );
      write(
        "conformance/traceability/other-1.md",
        "| req-id | contract §anchor | case-id(s) | status |\n|---|---|---|---|\n| YYY-001 | `contracts/other-1.md#normative-requirements` | `YYY-001-basic` | covered |\n"
      );
      write(
        "conformance/driven-manifest.json",
        JSON.stringify({
          "example/1": { driver: "test", driven: ["XXX-001-basic", "YYY-001-basic"], pending: [] },
          "other/1": { driver: "test", driven: [], pending: [] },
        })
      );
      write(
        "conformance/traceability/INDEX.md",
        GOOD_INDEX.replace(
          "| example/1 | 2 | 1 | 1 | 1 | 0 |\n",
          "| example/1 | 2 | 1 | 1 | 1 | 0 |\n| other/1 | 1 | 1 | 1 | 0 | 0 |\n"
        ).replace("| **Total** | **2** | **1** | **1** | **1** | **0** |", "| **Total** | **3** | **2** | **2** | **1** | **0** |")
      );
    },
    (root) => {
      const res = runValidator(root);
      assert.notEqual(res.status, 0);
      const out = res.stdout + res.stderr;
      assert.match(out, /YYY-001/);
      assert.match(out, /YYY-001-basic/);
      assert.match(out, /none of \[.*\] appears in other\/1's own driven set/);
      assert.match(res.stdout, /SUMMARY: validate-coverage: FAILED/);
    }
  );
});

test("a driven-but-FAILING case still counts as executed (execution is the bar, not passing)", () => {
  // The manifest records a case as driven with no notion of PASS/FAIL — proving
  // a covered row backed by such a case passes shows the checker never demands
  // the driver's outcome, only that it executed the case.
  withFixture(writeGoodTree, (root) => {
    const res = runValidator(root);
    assert.equal(res.status, 0, `expected exit 0, got ${res.status}\n${res.stdout}${res.stderr}`);
  });
});

test("driven-manifest.json listing a case absent from the corpus fails", () => {
  withFixture(
    (write) => {
      writeGoodTree(write);
      write(
        "conformance/driven-manifest.json",
        JSON.stringify({ "example/1": { driver: "test", driven: ["XXX-001-basic", "XXX-999-ghost"], pending: [] } })
      );
    },
    (root) => {
      const res = runValidator(root);
      assert.notEqual(res.status, 0);
      const out = res.stdout + res.stderr;
      assert.match(out, /driven-manifest\.json: example\/1 lists "XXX-999-ghost" not present in the corpus/);
      assert.match(out, /regenerate the manifest/);
      assert.match(res.stdout, /SUMMARY: validate-coverage: FAILED/);
    }
  );
});

test("envelope: case_id not matching its filename stem fails", () => {
  withFixture(
    (write) => {
      writeGoodTree(write);
      write(
        "conformance/corpora/example-1/XXX-001-basic.json",
        GOOD_CASE_001.replace('"XXX-001-basic"', '"XXX-001-wrong"')
      );
    },
    (root) => {
      const res = runValidator(root);
      assert.notEqual(res.status, 0);
      const out = res.stdout + res.stderr;
      assert.match(out, /example-1\/XXX-001-basic\.json/);
      assert.match(out, /XXX-001-wrong/);
      assert.match(res.stdout, /SUMMARY: validate-coverage: FAILED/);
    }
  );
});

test("envelope: duplicate case_id across two different corpus directories fails", () => {
  withFixture(
    (write) => {
      writeGoodTree(write);
      write(
        "contracts/other-1.md",
        "# Other Contract\n\n**Contract:** other/1\n**Version:** 1.0\n**Status:** draft\n\n**[YYY-001]** placeholder.\n"
      );
      write(
        "conformance/traceability/other-1.md",
        "| req-id | contract §anchor | case-id(s) | status |\n|---|---|---|---|\n| YYY-001 | `contracts/other-1.md#normative-requirements` | - | TBD-wave1 |\n"
      );
      // Same case_id as example-1's XXX-001-basic, filed under a different
      // contract's directory with a different filename.
      write(
        "conformance/corpora/other-1/XXX-001-dup.json",
        JSON.stringify({
          case_id: "XXX-001-basic",
          contract: "other/1",
          req_ids: ["YYY-001"],
          description: "Collides with example-1's case_id.",
          input: {},
          expected: {},
        })
      );
    },
    (root) => {
      const res = runValidator(root);
      assert.notEqual(res.status, 0);
      const out = res.stdout + res.stderr;
      assert.match(out, /XXX-001-basic/);
      assert.match(out, /not unique/);
      assert.match(res.stdout, /SUMMARY: validate-coverage: FAILED/);
    }
  );
});

test("envelope: contract field not matching the owning contract's header fails (no dir+major string surgery)", () => {
  withFixture(
    (write) => {
      // device-class-registry-style: the corpus dir carries no "-1" suffix,
      // but the contract file's header value does carry "/1" — the field must
      // be checked against the REAL header, not a dir-derived guess.
      write(
        "contracts/device-class-registry.md",
        "# Device Class Registry\n\n**Contract:** device-class-registry/1\n**Version:** 1.0\n**Status:** draft\n\n**[REG-001]** placeholder.\n"
      );
      write(
        "conformance/traceability/device-class-registry.md",
        "| req-id | contract §anchor | case-id(s) | status |\n|---|---|---|---|\n| REG-001 | `contracts/device-class-registry.md#x` | `REG-001-basic` | covered |\n"
      );
      write(
        "conformance/corpora/device-class-registry/REG-001-basic.json",
        JSON.stringify({
          case_id: "REG-001-basic",
          contract: "device-class-registry-1", // wrong: dir-derived guess, not the real header value
          req_ids: ["REG-001"],
          description: "d",
          input: {},
          expected: {},
        })
      );
      write(
        "conformance/driven-manifest.json",
        JSON.stringify({ "device-class-registry/1": { driver: "test", driven: ["REG-001-basic"], pending: [] } })
      );
      write(
        "conformance/traceability/INDEX.md",
        "| Contract | Requirements | Seed cases | covered | TBD-wave1 | Open draft-notes |\n|---|---|---|---|---|---|\n| device-class-registry/1 | 1 | 1 | 1 | 0 | 0 |\n| **Total** | **1** | **1** | **1** | **0** | **0** |\n"
      );
    },
    (root) => {
      const res = runValidator(root);
      assert.notEqual(res.status, 0);
      const out = res.stdout + res.stderr;
      assert.match(out, /device-class-registry\/REG-001-basic\.json/);
      assert.match(out, /device-class-registry-1/);
      assert.match(out, /device-class-registry\/1/);
      assert.match(res.stdout, /SUMMARY: validate-coverage: FAILED/);
    }
  );
});

test("envelope: a req_ids entry not defined in the owning contract fails", () => {
  withFixture(
    (write) => {
      writeGoodTree(write);
      write("conformance/corpora/example-1/XXX-001-basic.json", caseJson({ req_ids: ["XXX-404"] }));
    },
    (root) => {
      const res = runValidator(root);
      assert.notEqual(res.status, 0);
      const out = res.stdout + res.stderr;
      assert.match(out, /XXX-001-basic\.json/);
      assert.match(out, /XXX-404/);
      assert.match(res.stdout, /SUMMARY: validate-coverage: FAILED/);
    }
  );
});

test("envelope: a missing required field fails", () => {
  withFixture(
    (write) => {
      writeGoodTree(write);
      write(
        "conformance/corpora/example-1/XXX-001-basic.json",
        JSON.stringify({
          case_id: "XXX-001-basic",
          contract: "example/1",
          req_ids: ["XXX-001"],
          input: { a: 1 },
          expected: { ok: true },
          // description omitted
        })
      );
    },
    (root) => {
      const res = runValidator(root);
      assert.notEqual(res.status, 0);
      const out = res.stdout + res.stderr;
      assert.match(out, /XXX-001-basic\.json/);
      assert.match(out, /description/);
      assert.match(res.stdout, /SUMMARY: validate-coverage: FAILED/);
    }
  );
});

test("envelope: an empty req_ids array fails", () => {
  withFixture(
    (write) => {
      writeGoodTree(write);
      write("conformance/corpora/example-1/XXX-001-basic.json", caseJson({ req_ids: [] }));
    },
    (root) => {
      const res = runValidator(root);
      assert.notEqual(res.status, 0);
      const out = res.stdout + res.stderr;
      assert.match(out, /req_ids/);
      assert.match(res.stdout, /SUMMARY: validate-coverage: FAILED/);
    }
  );
});

test("INDEX.md row disagreeing with the actual per-contract counts fails, naming both sides", () => {
  withFixture(
    (write) => {
      writeGoodTree(write);
      write(
        "conformance/traceability/INDEX.md",
        GOOD_INDEX.replace("| example/1 | 2 | 1 | 1 | 1 | 0 |", "| example/1 | 2 | 1 | 2 | 0 | 0 |")
      );
    },
    (root) => {
      const res = runValidator(root);
      assert.notEqual(res.status, 0);
      const out = res.stdout + res.stderr;
      assert.match(out, /INDEX\.md: example\/1 row says/);
      assert.match(out, /covered=2/);
      assert.match(out, /actual/);
      assert.match(out, /regenerate the row/);
      assert.match(res.stdout, /SUMMARY: validate-coverage: FAILED/);
    }
  );
});

test("INDEX.md Total row disagreeing with the column sums fails", () => {
  withFixture(
    (write) => {
      writeGoodTree(write);
      write(
        "conformance/traceability/INDEX.md",
        GOOD_INDEX.replace("| **Total** | **2** | **1** | **1** | **1** | **0** |", "| **Total** | **2** | **1** | **9** | **1** | **0** |")
      );
    },
    (root) => {
      const res = runValidator(root);
      assert.notEqual(res.status, 0);
      const out = res.stdout + res.stderr;
      assert.match(out, /INDEX\.md: Total row says/);
      assert.match(res.stdout, /SUMMARY: validate-coverage: FAILED/);
    }
  );
});

test("a TBD-wave1 row citing a real (future) case id is not itself a failure", () => {
  withFixture(
    (write) => {
      write("contracts/example-1.md", GOOD_CONTRACT);
      write("conformance/corpora/example-1/XXX-001-basic.json", GOOD_CASE_001);
      write(
        "conformance/corpora/example-1/XXX-002-future.json",
        JSON.stringify({
          case_id: "XXX-002-future",
          contract: "example/1",
          req_ids: ["XXX-002"],
          description: "Frozen ahead of its driver.",
          input: {},
          expected: {},
        })
      );
      write(
        "conformance/traceability/example-1.md",
        "| req-id | contract §anchor | case-id(s) | status |\n|---|---|---|---|\n" +
          "| XXX-001 | `contracts/example-1.md#normative-requirements` | `XXX-001-basic` | covered |\n" +
          "| XXX-002 | `contracts/example-1.md#normative-requirements` | `XXX-002-future` | TBD-wave1 |\n"
      );
      write("conformance/driven-manifest.json", GOOD_MANIFEST);
      write(
        "conformance/traceability/INDEX.md",
        "| Contract | Requirements | Seed cases | covered | TBD-wave1 | Open draft-notes |\n|---|---|---|---|---|---|\n| example/1 | 2 | 2 | 1 | 1 | 0 |\n| **Total** | **2** | **2** | **1** | **1** | **0** |\n"
      );
    },
    (root) => {
      const res = runValidator(root);
      assert.equal(res.status, 0, `expected exit 0, got ${res.status}\n${res.stdout}${res.stderr}`);
      assert.match(res.stdout, /SUMMARY: validate-coverage: OK/);
    }
  );
});

test("a TBD-wave1 row citing a case that does not exist still fails the existence check", () => {
  withFixture(
    (write) => {
      writeGoodTree(write);
      write(
        "conformance/traceability/example-1.md",
        GOOD_TRACEABILITY_MAP.replace("| - | TBD-wave1 |", "| `XXX-002-ghost` | TBD-wave1 |")
      );
    },
    (root) => {
      const res = runValidator(root);
      assert.notEqual(res.status, 0);
      const out = res.stdout + res.stderr;
      assert.match(out, /XXX-002-ghost/);
      assert.match(out, /no such file/);
      assert.match(res.stdout, /SUMMARY: validate-coverage: FAILED/);
    }
  );
});

test("INDEX.md resolves a row spelled as the bare corpus-dir name (no /1), matching the real repo's device-class-registry/security-model/channel-index rows", () => {
  withFixture(
    (write) => {
      writeGoodTree(write);
      // "example-1" is both the contracts filename stem AND, in the real repo,
      // a dir that never carries a "-1" suffix — using it bare here proves the
      // fallback resolves by stem, not just by the "slug/major" header value.
      write(
        "conformance/traceability/INDEX.md",
        GOOD_INDEX.replace("| example/1 | 2 | 1 | 1 | 1 | 0 |", "| example-1 | 2 | 1 | 1 | 1 | 0 |")
      );
    },
    (root) => {
      const res = runValidator(root);
      assert.equal(res.status, 0, `expected exit 0, got ${res.status}\n${res.stdout}${res.stderr}`);
      assert.match(res.stdout, /SUMMARY: validate-coverage: OK/);
    }
  );
});

test("INDEX.md missing a row for a contract that exists fails, even though the Total still balances against the reduced set", () => {
  withFixture(
    (write) => {
      writeGoodTree(write);
      // A second real contract with a row deliberately omitted from INDEX.md;
      // the Total is kept internally consistent with the ONE remaining row,
      // proving the sum check alone cannot catch a dropped row.
      write(
        "contracts/other-1.md",
        "# Other Contract\n\n**Contract:** other/1\n**Version:** 1.0\n**Status:** draft\n\n**[YYY-001]** placeholder.\n"
      );
      write(
        "conformance/traceability/other-1.md",
        "| req-id | contract §anchor | case-id(s) | status |\n|---|---|---|---|\n| YYY-001 | `contracts/other-1.md#normative-requirements` | - | TBD-wave1 |\n"
      );
      // INDEX.md is untouched — still only the example/1 row + a Total that
      // balances against just that row, as if other/1 never existed.
    },
    (root) => {
      const res = runValidator(root);
      assert.notEqual(res.status, 0);
      const out = res.stdout + res.stderr;
      assert.match(out, /no row for contract "other\/1" \(contracts\/other-1\.md\)/);
      assert.match(res.stdout, /SUMMARY: validate-coverage: FAILED/);
    }
  );
});

test("INDEX.md row for a contract slug nobody defines fails loudly rather than being silently skipped", () => {
  withFixture(
    (write) => {
      writeGoodTree(write);
      write(
        "conformance/traceability/INDEX.md",
        GOOD_INDEX.replace("| example/1 | 2 | 1 | 1 | 1 | 0 |\n", "| example/1 | 2 | 1 | 1 | 1 | 0 |\n| nonexistent/1 | 1 | 1 | 1 | 0 | 0 |\n")
      );
    },
    (root) => {
      const res = runValidator(root);
      assert.notEqual(res.status, 0);
      const out = res.stdout + res.stderr;
      assert.match(out, /unrecognized contract "nonexistent\/1"/);
      assert.match(res.stdout, /SUMMARY: validate-coverage: FAILED/);
    }
  );
});
