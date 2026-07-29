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
