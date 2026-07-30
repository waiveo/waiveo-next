#!/usr/bin/env node
// scripts/validate-deadcode.mjs — no NEW unreachable Go code. Today's set is
// frozen, with a written reason per package; anything not in that set fails.
//
// Same defect class as validate-error-codes.mjs, seen from the other side: a
// capability is declared and nothing is wired to it. There, the declaration is a
// published error code with no emit site. Here, it is a function — an exported
// reader nobody reads with, a revocation check nobody calls, a clock-floor
// `Advance` with no caller. Both are invisible to `go build` and to `go vet`,
// and both survive review indefinitely because the code LOOKS finished.
//
// ── Roots ───────────────────────────────────────────────────────────────────
// A function is reachable if some root reaches it. The root set is:
//
//     go tool deadcode -test ./cmd/... ./conformance/... ./scripts/...
//
//   ./cmd/...          the two shipped binaries — waiveo-feeder, waiveo-relay.
//   ./conformance/...  the executable contract drivers (and driven-manifest).
//                      A driver is how a `covered` row is EARNED, so code that
//                      exists to be driven is not dead. This is why the flag is
//                      `-test`: the drivers are test binaries, and without it
//                      they are not roots at all.
//   ./scripts/...      the in-repo dev programs `make dev` runs. `packsig.Sign`
//                      has exactly one caller, scripts/examplepack; a root set
//                      that omitted it would report eight pack-signing
//                      functions dead while a program in this repo calls them.
//
// The tempting fourth option — `-test ./...`, which makes EVERY package's own
// unit tests roots — is deliberately not used. It collapses the report by more
// than an order of magnitude, because a function called only by a unit test in
// its own package then counts as live. That is the wrong bar for this gate twice
// over: a constructor option only a test ever passes is precisely an unwired
// capability, and, concretely, `-test ./...` did not report eventsse's
// `Hub.after` — the real defect a review found by hand, since deleted — because a
// unit test exercised it while nothing in the delivery path did. The gate has to
// be able to see that shape.
//
// ── GOOS/GOARCH ─────────────────────────────────────────────────────────────
// RTA is valid for one build configuration at a time, and this module has
// GOOS-gated files (internal/app/auth/peercred_{linux,darwin,unsupported}.go,
// consolestat_{unix,other}.go). The analysis is therefore pinned to
// linux/amd64 — what CI runs and what the container ships — so the baseline
// means the same thing on a contributor's macOS laptop as in CI. The two agree
// today; pinning is what keeps that from being luck. Pinning also forces the
// two-step below: `go tool deadcode` under a cross GOOS would cross-COMPILE the
// tool and then fail to exec it, so the tool is built for the host and only its
// package loading is pointed at linux/amd64.
//
// ── Checks ──────────────────────────────────────────────────────────────────
//   1. Every unreachable function deadcode reports is in the baseline.
//   2. Every baseline entry is still reported. Wiring a function up (or
//      deleting it) means deleting its baseline line — the file shrinks as the
//      platform is built and can never claim a gap that has been closed.
//   3. Every group carries a real reason: no "TODO"/"TBD" placeholders, minimum
//      length enforced. 104 unexplained lines would teach nobody anything.
//
// ── What this gate cannot see ───────────────────────────────────────────────
// RTA treats a type whose values reach the reflection machinery as having ALL
// its exported methods potentially callable, so a new EXPORTED METHOD on such a
// type is never reported. Measured by mutation, not assumed: adding
// `func (h *Hub) PendingSubscriberIDs() []string` to internal/app/eventsse
// passes this gate (`deadcode -whylive` answers "reachable only through
// reflection"), and so does an exported method on internal/relay/identity.Store.
// Three shapes ARE caught on the same types — an unexported method, a
// package-level function, and an exported method on a type reflection does not
// touch (internal/relay/playerserver.Server, internal/relay/clocktrust.Controller
// were both reported when probed the same way). So this catches most new dead
// code and not all of it; it is a ratchet, not a proof.
//
// Not caught here, on purpose: an unused package-level CONSTANT or struct field.
// deadcode analyses the call graph, so a declared-but-never-constructed error
// code is validate-error-codes.mjs's job, and a decoded-but-never-applied wire
// field is nobody's yet — it needs go/types to resolve each selector's receiver
// type, and a name-matching approximation of it produces confident nonsense
// (three unrelated `Revoked` fields mask the one that mattered).
import { readFileSync, existsSync, mkdtempSync, rmSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";

// The tool is a `tool` dependency of THIS module (go.mod), so it is always built
// from the module that owns this script — never from whatever tree the analysis
// happens to be pointed at. That keeps the version pinned by go.sum, and it is
// what lets the unit tests run the real analysis against a throwaway fixture
// module without that fixture needing golang.org/x/tools in its own graph.
const TOOL_MODULE_DIR = dirname(import.meta.dirname);

const BASELINE_PATH = join("scripts", "deadcode-baseline.json");
const PACKAGE_PATTERNS = ["./cmd/...", "./conformance/...", "./scripts/..."];
const ANALYSIS_GOOS = "linux";
const ANALYSIS_GOARCH = "amd64";
const PLACEHOLDER_REASON_RE = /^\s*(todo|tbd|n\/?a|\?+|-+)\b/i;
const MIN_REASON_LENGTH = 24;

const failures = [];

// ---------------------------------------------------------------------------
// Step 1: run the analysis.
// ---------------------------------------------------------------------------
function runDeadcode() {
  const scratch = mkdtempSync(join(tmpdir(), "deadcode-"));
  const bin = join(scratch, "deadcode");
  try {
    const build = spawnSync("go", ["build", "-o", bin, "golang.org/x/tools/cmd/deadcode"], {
      encoding: "utf8",
      cwd: TOOL_MODULE_DIR,
    });
    if (build.status !== 0) {
      throw new Error(
        `go build of golang.org/x/tools/cmd/deadcode failed (it is a tool dependency in go.mod; run "go mod download"):\n${build.stderr ?? ""}`
      );
    }
    const run = spawnSync(bin, ["-json", "-test", ...PACKAGE_PATTERNS], {
      encoding: "utf8",
      maxBuffer: 64 * 1024 * 1024,
      env: { ...process.env, GOOS: ANALYSIS_GOOS, GOARCH: ANALYSIS_GOARCH },
    });
    if (run.status !== 0) throw new Error(`deadcode exited ${run.status}:\n${run.stderr ?? ""}`);
    // deadcode emits JSON `null`, not `[]`, when nothing is unreachable.
    return JSON.parse(run.stdout) ?? [];
  } finally {
    rmSync(scratch, { recursive: true, force: true });
  }
}

// The module path of the tree being analysed, so baseline entries read as
// repo-relative package directories rather than repeating the module prefix on
// every line.
function modulePath() {
  const out = spawnSync("go", ["list", "-m"], { encoding: "utf8" });
  return out.status === 0 ? `${out.stdout.trim()}/` : "";
}

// pkg (module-relative) -> Map(funcName -> "file:line")
const reported = new Map();
try {
  const prefix = modulePath();
  for (const pkg of runDeadcode()) {
    const rel = prefix && pkg.Path.startsWith(prefix) ? pkg.Path.slice(prefix.length) : pkg.Path;
    const funcs = reported.get(rel) ?? new Map();
    for (const fn of pkg.Funcs ?? []) {
      funcs.set(fn.Name, `${fn.Position.File}:${fn.Position.Line}`);
    }
    reported.set(rel, funcs);
  }
} catch (err) {
  console.error(String(err.message ?? err));
  console.log(`SUMMARY: validate-deadcode: FAILED — analysis did not run: ${String(err.message ?? err).split("\n")[0]}`);
  process.exit(1);
}

// ---------------------------------------------------------------------------
// Step 2: the baseline.
// ---------------------------------------------------------------------------
let baseline = { groups: [] };
if (!existsSync(BASELINE_PATH)) {
  failures.push(`${BASELINE_PATH}: file does not exist — the frozen set of already-unreachable functions lives here`);
} else {
  try {
    baseline = JSON.parse(readFileSync(BASELINE_PATH, "utf8"));
  } catch (err) {
    failures.push(`${BASELINE_PATH}: invalid JSON: ${err.message}`);
  }
}

// "pkg func" -> group index
const frozen = new Map();

for (const [gi, group] of (baseline.groups ?? []).entries()) {
  const at = `${BASELINE_PATH}: groups[${gi}]`;
  const pkg = group.package;
  const reason = typeof group.reason === "string" ? group.reason.trim() : "";
  const funcs = Array.isArray(group.funcs) ? group.funcs : [];

  if (typeof pkg !== "string" || pkg === "") {
    failures.push(`${at}: missing "package"`);
    continue;
  }
  if (reason.length < MIN_REASON_LENGTH || PLACEHOLDER_REASON_RE.test(reason)) {
    failures.push(
      `${at} (${pkg}): reason is a placeholder or too short — say what the package is and why nothing calls this yet; ` +
        `at least ${MIN_REASON_LENGTH} characters, and never "TODO"/"TBD"`
    );
  }
  if (funcs.length === 0) failures.push(`${at} (${pkg}): funcs is empty — delete the group`);

  for (const fn of funcs) {
    const key = `${pkg} ${fn}`;
    if (frozen.has(key)) {
      failures.push(`${at}: ${pkg}.${fn} is listed twice`);
      continue;
    }
    frozen.set(key, gi);
    // #2 still unreachable?
    if (!reported.get(pkg)?.has(fn)) {
      failures.push(
        `${at}: ${pkg}.${fn} is no longer reported unreachable — it gained a caller, or it was deleted or renamed. ` +
          `Delete it from ${BASELINE_PATH}: the baseline is an inventory of what is NOT wired, and an entry that ` +
          `outlives the gap makes the inventory lie.`
      );
    }
  }
}

// #1 nothing new.
let reportedCount = 0;
for (const [pkg, funcs] of reported) {
  for (const [fn, where] of funcs) {
    reportedCount++;
    if (frozen.has(`${pkg} ${fn}`)) continue;
    failures.push(
      `${where}: ${fn} is unreachable from every root (the two binaries, the conformance drivers, the dev scripts) — ` +
        `wire it up, delete it, or add it to ${BASELINE_PATH} under a group for "${pkg}" with a reason that says why it ` +
        `exists with no caller`
    );
  }
}

// ---------------------------------------------------------------------------
if (failures.length) {
  console.error(failures.join("\n"));
  console.log(`SUMMARY: validate-deadcode: FAILED — ${failures.length} issue(s); first: ${failures[0]}`);
  process.exitCode = 1;
} else {
  console.log(
    `validate-deadcode: OK (${reportedCount} unreachable func(s) over ${reported.size} package(s), all baselined; ` +
      `analysis pinned to ${ANALYSIS_GOOS}/${ANALYSIS_GOARCH})`
  );
  console.log(`SUMMARY: validate-deadcode: OK (${reportedCount} baselined unreachable func(s))`);
}
