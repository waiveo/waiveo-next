// scripts/validate-deadcode.test.mjs — exercises validate-deadcode.mjs against
// disposable fixture Go modules built at test runtime. The REAL analysis runs
// against them: the script builds golang.org/x/tools/cmd/deadcode from this
// module (it is a `tool` dependency in go.mod) and points it at the fixture, so
// these cases prove the whole path, not a mocked half of it.
//
// The fixture module is dependency-free, so nothing here needs the network.
//
// The first two cases are the gate's own mutation evidence, kept executable:
// adding an unreachable function fails, and a baseline entry that has gained a
// caller fails. Freezing them here means the gate cannot quietly stop catching
// either one.
import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { spawnSync } from "node:child_process";

const VALIDATOR = join(import.meta.dirname, "validate-deadcode.mjs");

// The fixture mirrors the three root patterns the validator analyses, because a
// pattern matching no package at all is an error from the tool, not an empty
// result. conformance/ carries a driver-shaped test binary so the "a driver is a
// root" rule is exercised rather than assumed.
const GO_MOD = "module deadcodefixture\n\ngo 1.26\n";

const MAIN_GO = `package main

func main() { live() }

func live() {}
`;

const LIB_GO = `package lib

// CalledByTheBinary is reached from cmd/app.
func CalledByTheBinary() {}

// CalledByTheDriver is reached only from the conformance driver's test binary.
func CalledByTheDriver() {}

// NoCaller is reached from nowhere.
func NoCaller() {}
`;

const DRIVER_TEST_GO = `package driver

import (
	"testing"

	"deadcodefixture/internal/lib"
)

func TestDrives(t *testing.T) { lib.CalledByTheDriver() }
`;

const SCRIPT_GO = `package main

import "deadcodefixture/internal/lib"

func main() { lib.CalledByTheBinary() }
`;

function makeFixture(build) {
  const root = mkdtempSync(join(tmpdir(), "validate-deadcode-"));
  const write = (relPath, content) => {
    const full = join(root, relPath);
    mkdirSync(dirname(full), { recursive: true });
    writeFileSync(full, typeof content === "string" ? content : JSON.stringify(content, null, 2));
  };
  write("go.mod", GO_MOD);
  write("cmd/app/main.go", MAIN_GO);
  write("internal/lib/lib.go", LIB_GO);
  write("conformance/drivers/driver/driver_test.go", DRIVER_TEST_GO);
  write("scripts/tool/main.go", SCRIPT_GO);
  build(write);
  return root;
}

function run(cwd) {
  return spawnSync(process.execPath, [VALIDATOR], { cwd, encoding: "utf8" });
}

function withFixture(build, assertions) {
  const root = makeFixture(build);
  try {
    assertions(run(root));
  } finally {
    rmSync(root, { recursive: true, force: true });
  }
}

const BASELINE_WITH_NOCALLER = {
  groups: [
    {
      package: "internal/lib",
      reason: "NoCaller is the fixture's stand-in for a helper written ahead of the code that would use it.",
      funcs: ["NoCaller"],
    },
  ],
};

test("a fixture whose only unreachable function is baselined passes", () => {
  withFixture(
    (w) => w("scripts/deadcode-baseline.json", BASELINE_WITH_NOCALLER),
    (r) => {
      assert.equal(r.status, 0, r.stderr);
      assert.match(r.stdout, /SUMMARY: validate-deadcode: OK \(1 baselined unreachable func\(s\)\)/);
    }
  );
});

test("adding an unreachable function fails", () => {
  withFixture(
    (w) => {
      w("scripts/deadcode-baseline.json", BASELINE_WITH_NOCALLER);
      w("internal/lib/added.go", "package lib\n\n// FreshlyDead is the mutation: new code with no caller.\nfunc FreshlyDead() {}\n");
    },
    (r) => {
      assert.equal(r.status, 1);
      assert.match(r.stderr, /internal\/lib\/added\.go:4: FreshlyDead is unreachable from every root/);
      assert.match(r.stdout, /SUMMARY: validate-deadcode: FAILED — 1 issue/);
    }
  );
});

test("a baseline entry that has gained a caller fails, so the inventory shrinks", () => {
  withFixture(
    (w) => {
      w("scripts/deadcode-baseline.json", BASELINE_WITH_NOCALLER);
      w("cmd/app/main.go", `package main\n\nimport "deadcodefixture/internal/lib"\n\nfunc main() { live(); lib.NoCaller() }\n\nfunc live() {}\n`);
    },
    (r) => {
      assert.equal(r.status, 1);
      assert.match(r.stderr, /internal\/lib\.NoCaller is no longer reported unreachable/);
    }
  );
});

test("a function reachable only from a conformance driver counts as reachable", () => {
  // The judgement call the root set encodes: a driver is how a contract row is
  // earned. Dropping the driver from the fixture must be what makes it dead.
  withFixture(
    (w) => w("scripts/deadcode-baseline.json", BASELINE_WITH_NOCALLER),
    (r) => assert.equal(r.status, 0, "driver-only code must not be reported: " + r.stderr)
  );
  withFixture(
    (w) => {
      w("scripts/deadcode-baseline.json", BASELINE_WITH_NOCALLER);
      w("conformance/drivers/driver/driver_test.go", "package driver\n");
    },
    (r) => {
      assert.equal(r.status, 1);
      assert.match(r.stderr, /CalledByTheDriver is unreachable/);
    }
  );
});

test("a function reachable only from a dev script counts as reachable", () => {
  withFixture(
    (w) => {
      w("scripts/deadcode-baseline.json", BASELINE_WITH_NOCALLER);
      w("scripts/tool/main.go", "package main\n\nfunc main() {}\n");
    },
    (r) => {
      assert.equal(r.status, 1);
      assert.match(r.stderr, /CalledByTheBinary is unreachable/);
    }
  );
});

test("a placeholder reason fails, so the baseline cannot become 104 TODOs", () => {
  for (const reason of ["TODO", "TBD", "n/a", "short"]) {
    withFixture(
      (w) => w("scripts/deadcode-baseline.json", { groups: [{ package: "internal/lib", reason, funcs: ["NoCaller"] }] }),
      (r) => {
        assert.equal(r.status, 1, `reason ${JSON.stringify(reason)} must be rejected`);
        assert.match(r.stderr, /reason is a placeholder or too short/);
      }
    );
  }
});

test("a missing baseline file fails rather than passing vacuously", () => {
  withFixture(
    () => {},
    (r) => {
      assert.equal(r.status, 1);
      assert.match(r.stderr, /deadcode-baseline\.json: file does not exist/);
    }
  );
});

// ---------------------------------------------------------------------------
// The unimported-package check.
//
// deadcode only loads what the roots transitively import, so a package no root
// imports contributes no findings at all — an entire unwired package used to pass
// green. These pin the separate question, and the two exemptions that keep
// legitimate shapes out of the report without a baseline entry to maintain.

const UNIMPORTED_PKG_GO = `package orphan

// Real production code that nothing imports.
func NeverCalled() string { return "nobody imports this package" }
`;

test("a package no root imports fails, even though deadcode never loads it", () => {
  withFixture(
    (w) => {
      w("scripts/deadcode-baseline.json", BASELINE_WITH_NOCALLER);
      w("internal/orphan/orphan.go", UNIMPORTED_PKG_GO);
    },
    (r) => {
      assert.equal(r.status, 1, r.stdout);
      assert.match(r.stderr, /internal\/orphan: no root imports this package/);
    }
  );
});

test("an unimported package with a reason passes", () => {
  withFixture(
    (w) => {
      w("scripts/deadcode-baseline.json", {
        ...BASELINE_WITH_NOCALLER,
        unimportedPackages: [
          {
            package: "deadcodefixture/internal/orphan",
            reason: "the fixture's stand-in for a package written ahead of the surface that will import it.",
          },
        ],
      });
      w("internal/orphan/orphan.go", UNIMPORTED_PKG_GO);
    },
    (r) => assert.equal(r.status, 0, r.stderr)
  );
});

// A test-only package is not an unwired capability — there is no production code
// in it to be dead. The exemption is DERIVED (zero non-test files) rather than
// listed, so nobody has to keep an entry true forever.
test("a test-only package is not reported", () => {
  withFixture(
    (w) => {
      w("scripts/deadcode-baseline.json", BASELINE_WITH_NOCALLER);
      w("internal/testonly/only_test.go", "package testonly\n\nimport \"testing\"\n\nfunc TestNothing(t *testing.T) {}\n");
    },
    (r) => assert.equal(r.status, 0, r.stderr)
  );
});

// Same reasoning for a package whose only file is a package comment: it declares
// nothing, so there is nothing unwired.
test("a package whose only file declares nothing is not reported", () => {
  withFixture(
    (w) => {
      w("scripts/deadcode-baseline.json", BASELINE_WITH_NOCALLER);
      w("internal/docsonly/doc.go", "// Package docsonly documents a shell script that has nothing to import.\npackage docsonly\n");
    },
    (r) => assert.equal(r.status, 0, r.stderr)
  );
});

test("an unimportedPackages entry that gained an importer fails, so the inventory shrinks", () => {
  withFixture(
    (w) => {
      w("scripts/deadcode-baseline.json", {
        ...BASELINE_WITH_NOCALLER,
        unimportedPackages: [
          {
            package: "deadcodefixture/internal/lib",
            reason: "internal/lib is imported by the fixture's own binary, so this entry is a lie the gate must catch.",
          },
        ],
      });
    },
    (r) => {
      assert.equal(r.status, 1, r.stdout);
      assert.match(r.stderr, /which now HAS an importer/);
    }
  );
});

test("an unimportedPackages entry with a placeholder reason fails", () => {
  withFixture(
    (w) => {
      w("scripts/deadcode-baseline.json", {
        ...BASELINE_WITH_NOCALLER,
        unimportedPackages: [{ package: "deadcodefixture/internal/orphan", reason: "TODO" }],
      });
      w("internal/orphan/orphan.go", UNIMPORTED_PKG_GO);
    },
    (r) => {
      assert.equal(r.status, 1, r.stdout);
      assert.match(r.stderr, /has no usable reason/);
    }
  );
});
