#!/usr/bin/env node
// scripts/validate-coverage.mjs — traceability COVERAGE cross-check: does a
// "covered" row actually correspond to a case some driver executes, and does
// every artifact in the coverage chain (contract -> corpus envelope ->
// traceability row -> driven-manifest -> INDEX roll-up) agree with the
// others? scripts/validate-contracts.mjs already proves every requirement ID
// has >=1 traceability row and every row's ID is real; this script proves the
// CONTENTS of that row (case-id(s), status) are not merely well-formed but
// true.
//
// This is deliberately a separate script from validate-contracts.mjs rather
// than an extension of it: validate-contracts.mjs is a self-contained
// markdown linter whose `SUMMARY:` marker nightly.yml greps for by position;
// this script additionally reads corpus JSON and a driven-manifest, and has
// its own failure taxonomy. The two small parsers this script needs
// (requirement-ID-anchor extraction with fence-skipping, and traceability
// table-row splitting) are duplicated from validate-contracts.mjs rather than
// factored into a shared module — same reasoning: each script stays a single
// file a reader can audit standalone, and the two independently prove out the
// same underlying grammar.
//
// A "covered" row's bar is EXECUTION, not passing: a case a driver drives and
// FAILS still counts (api1's API-111 is driven-but-FAILING by design) because
// conformance/driven-manifest.json's `driven` list already reflects
// report.Report.Driven() (PASS ∪ FAIL, never PENDING) — this script only
// checks list membership, so it inherits that bar for free.
//
// Checks:
//   1. Envelope: case_id == filename stem.
//   2. Envelope: case_id unique across the WHOLE corpus (not just its dir).
//   3. Envelope: contract field == the owning contract's real `**Contract:**`
//      header value (never guessed from the directory name — several corpus
//      directories, e.g. device-class-registry/security-model/channel-index,
//      carry no "-<major>" suffix even though their contract field does).
//   4. Envelope: every req_ids entry is a requirement ID DEFINED IN THAT
//      CONTRACT (the same ownership rule validate-contracts.mjs check #4
//      applies to traceability rows).
//   5. Envelope: input/expected/description present, req_ids non-empty.
//   6. Traceability row: status is exactly "covered" or "TBD-wave1".
//   7. Traceability row: every named case-id resolves to a real corpus file
//      (in that map's own contract directory) — applies regardless of the
//      row's status, so a TBD-wave1 row citing a future case is still
//      typo-checked.
//   8. Traceability row: a "covered" row's named cases must include >=1 ID
//      that appears in SOME driver's driven set (driven-manifest.json) — a
//      "covered" row whose cases are all unexecuted must downgrade.
//   9. driven-manifest.json: every driven/pending case ID it lists must exist
//      in the corpus (catches the manifest itself drifting from reality on
//      the "phantom case" side; the runtime-vs-manifest drift on the driver
//      side is caught by Go tests, not this script).
//  10. INDEX.md: per contract, covered/TBD-wave1/Requirements/Seed-cases
//      match the actual counts; the Total row matches the column sums.
import { readdirSync, readFileSync, statSync, existsSync } from "node:fs";
import { join, basename, dirname } from "node:path";

const CONTRACTS_ROOT = "contracts";
const TRACEABILITY_ROOT = join("conformance", "traceability");
const CORPORA_ROOT = join("conformance", "corpora");
const MANIFEST_PATH = join("conformance", "driven-manifest.json");
const EXEMPT_CONTRACT_NAMES = new Set(["README.md", "TEMPLATE.md"]);
const EXEMPT_TRACEABILITY_NAMES = new Set(["README.md", "INDEX.md"]);

// Duplicated from validate-contracts.mjs on purpose — see the file doc above.
const REQUIREMENT_ID_RE = /^\s*\*\*\[([A-Z]{3}-\d{3}[a-z]?)\]\*\*/;
const TRACEABILITY_ID_RE = /^[A-Z]{3}-\d{3}[a-z]?$/;
const CONTRACT_HEADER_RE = /^\*\*Contract:\*\*\s*([a-z0-9-]+\/\d+)\s*$/;

const failures = [];

function walkMarkdown(dir) {
  const out = [];
  if (!existsSync(dir)) return out;
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    if (statSync(p).isDirectory()) out.push(...walkMarkdown(p));
    else if (p.endsWith(".md")) out.push(p);
  }
  return out;
}

// ---------------------------------------------------------------------------
// Step 1: parse every contracts/*.md into { stem -> { path, contractValue,
// requirementIds: Set } }. `stem` is the filename minus ".md" — by the
// repo's own naming convention this is identical to the matching corpus
// directory name AND the matching traceability map's filename.
// ---------------------------------------------------------------------------
const contractsByStem = new Map();

function parseContractFile(path) {
  const lines = readFileSync(path, "utf8").split("\n");
  let contractValue = null;
  for (const line of lines.slice(0, 10)) {
    const m = CONTRACT_HEADER_RE.exec(line.trim());
    if (m) {
      contractValue = m[1];
      break;
    }
  }
  const requirementIds = new Set();
  let inFence = false;
  for (const line of lines) {
    if (/^\s*(```|~~~)/.test(line)) {
      inFence = !inFence;
      continue;
    }
    if (inFence) continue;
    const m = REQUIREMENT_ID_RE.exec(line);
    if (m) requirementIds.add(m[1]);
  }
  return { path, contractValue, requirementIds };
}

for (const path of walkMarkdown(CONTRACTS_ROOT)) {
  const name = basename(path);
  if (EXEMPT_CONTRACT_NAMES.has(name)) continue;
  const stem = name.replace(/\.md$/, "");
  contractsByStem.set(stem, parseContractFile(path));
}

// Reverse index: contract slug value (e.g. "device-class-registry/1") -> stem,
// used by the INDEX.md check, whose rows are keyed by contract value, not
// filename stem.
const stemByContractValue = new Map();
for (const [stem, info] of contractsByStem) {
  if (info.contractValue) stemByContractValue.set(info.contractValue, stem);
}

// ---------------------------------------------------------------------------
// Step 2: load conformance/driven-manifest.json.
// ---------------------------------------------------------------------------
let manifest = {};
if (!existsSync(MANIFEST_PATH)) {
  failures.push(`${MANIFEST_PATH}: file does not exist — run "go run ./conformance/cmd/driven-manifest -write"`);
} else {
  try {
    manifest = JSON.parse(readFileSync(MANIFEST_PATH, "utf8"));
  } catch (err) {
    failures.push(`${MANIFEST_PATH}: invalid JSON: ${err.message}`);
  }
}

// unionDriven is every case ID any contract's driver entry actually drove —
// flattened across the whole manifest, since a case ID's contract prefix
// already makes cross-contract collision practically impossible, and the
// check is phrased as "any driver's driven set" (driven-manifest.json is a
// single artifact, not looked up per contract for this particular check).
const unionDriven = new Set();
for (const entry of Object.values(manifest)) {
  for (const id of entry.driven ?? []) unionDriven.add(id);
}

// ---------------------------------------------------------------------------
// Step 3: load every corpus file, running the per-envelope checks (#1-#5) as
// we go, and building the registries later checks need: caseIdsByStem (this
// contract's own case_id -> file) and a corpus-wide uniqueness map.
// ---------------------------------------------------------------------------
const caseIdsByStem = new Map(); // stem -> Set(case_id) that really exist as files
const caseLocations = new Map(); // case_id -> [ "dir/file", ... ] corpus-wide

function loadCorpusDir(stem) {
  const dir = join(CORPORA_ROOT, stem);
  if (!existsSync(dir)) return;
  const contract = contractsByStem.get(stem);
  const ids = new Set();
  for (const name of readdirSync(dir).sort()) {
    if (!name.endsWith(".json")) continue;
    const rel = `${stem}/${name}`;
    const fullPath = join(dir, name);
    let data;
    try {
      data = JSON.parse(readFileSync(fullPath, "utf8"));
    } catch (err) {
      failures.push(`${rel}: invalid JSON: ${err.message}`);
      continue;
    }
    const expectedId = name.replace(/\.json$/, "");

    // #1 case_id == filename stem.
    if (data.case_id !== expectedId) {
      failures.push(`${rel}: case_id "${data.case_id}" does not match filename stem "${expectedId}"`);
    } else {
      ids.add(data.case_id);
    }

    // #2 case_id unique corpus-wide.
    const key = data.case_id;
    if (key) {
      const locs = caseLocations.get(key) ?? [];
      locs.push(rel);
      caseLocations.set(key, locs);
    }

    // #3 contract field == the real header value (never dir-derived).
    if (contract?.contractValue && data.contract !== contract.contractValue) {
      failures.push(
        `${rel}: contract "${data.contract}" does not match contracts/${stem}.md's header "${contract.contractValue}"`
      );
    }

    // #4 every req_ids entry is defined in THIS contract.
    if (Array.isArray(data.req_ids)) {
      for (const reqId of data.req_ids) {
        if (!contract?.requirementIds?.has(reqId)) {
          failures.push(`${rel}: req_ids entry "${reqId}" is not a requirement defined in contracts/${stem}.md`);
        }
      }
    }

    // #5 required fields present.
    for (const field of ["input", "expected", "description"]) {
      if (data[field] === undefined || data[field] === null) {
        failures.push(`${rel}: missing required field "${field}"`);
      }
    }
    if (!Array.isArray(data.req_ids) || data.req_ids.length === 0) {
      failures.push(`${rel}: req_ids must be a non-empty array`);
    }
  }
  caseIdsByStem.set(stem, ids);
}

if (existsSync(CORPORA_ROOT)) {
  for (const name of readdirSync(CORPORA_ROOT).sort()) {
    if (statSync(join(CORPORA_ROOT, name)).isDirectory()) loadCorpusDir(name);
  }
}

for (const [caseId, locs] of caseLocations) {
  if (locs.length > 1) {
    failures.push(`${locs.join(", ")}: case_id "${caseId}" is not unique — every case_id must be unique across the whole corpus`);
  }
}

// #9: every driven-manifest case ID exists somewhere in the corpus.
for (const [contract, entry] of Object.entries(manifest)) {
  for (const id of [...(entry.driven ?? []), ...(entry.pending ?? [])]) {
    if (!caseLocations.has(id)) {
      failures.push(`${MANIFEST_PATH}: ${contract} lists "${id}" not present in the corpus — regenerate the manifest`);
    }
  }
}

// ---------------------------------------------------------------------------
// Step 4: parse each traceability map's rows (#6-#8), and tally per-contract
// covered/TBD-wave1 counts for the INDEX check.
// ---------------------------------------------------------------------------
// stem -> { covered: number, tbd: number }
const rowCounts = new Map();

function parseRowCells(line) {
  const cells = line.split("|");
  // cells[0] and cells[last] are the empty strings outside the leading/
  // trailing "|" of a well-formed row; 1=req-id, 2=anchor, 3=case-id(s), 4=status.
  return {
    reqId: cells[1]?.trim(),
    caseIdsCell: cells[3]?.trim(),
    status: cells[4]?.trim(),
  };
}

function namedCaseIds(cell) {
  if (!cell || cell === "-") return [];
  return cell
    .split(",")
    .map((s) => s.trim().replace(/`/g, ""))
    .filter(Boolean);
}

function checkTraceabilityFile(path) {
  const stem = basename(path).replace(/\.md$/, "");
  const lines = readFileSync(path, "utf8").split("\n");
  const existingIds = caseIdsByStem.get(stem) ?? new Set();
  let covered = 0;
  let tbd = 0;

  lines.forEach((raw, i) => {
    const line = raw.trim();
    if (!line.startsWith("|")) return;
    const { reqId, caseIdsCell, status } = parseRowCells(line);
    if (!reqId || !TRACEABILITY_ID_RE.test(reqId)) return; // header/separator/prose row

    const loc = `${path}:${i + 1}`;

    // #6 status must be exactly one of the two sanctioned values.
    if (status !== "covered" && status !== "TBD-wave1") {
      failures.push(`${loc}: ${reqId} status "${status}" — must be exactly "covered" or "TBD-wave1"`);
    } else if (status === "covered") {
      covered++;
    } else {
      tbd++;
    }

    const ids = namedCaseIds(caseIdsCell);

    // #7 every named case id resolves to a real corpus file, regardless of status.
    for (const id of ids) {
      if (!existingIds.has(id)) {
        failures.push(
          `${loc}: ${reqId} names case "${id}" — no such file conformance/corpora/${stem}/${id}.json; fix the typo or freeze the case`
        );
      }
    }

    // #8 a "covered" row needs >=1 of its named cases in the driven union.
    if (status === "covered") {
      const anyDriven = ids.some((id) => unionDriven.has(id));
      if (!anyDriven) {
        failures.push(
          `${loc}: ${reqId} is covered but none of [${ids.join(", ")}] appears in any driver's driven set ` +
            `(${MANIFEST_PATH}) — wire a case into a driver and rerun "go run ./conformance/cmd/driven-manifest -write", ` +
            `or downgrade the row to "- | TBD-wave1"`
        );
      }
    }
  });

  rowCounts.set(stem, { covered, tbd });
}

for (const path of walkMarkdown(TRACEABILITY_ROOT)) {
  if (EXEMPT_TRACEABILITY_NAMES.has(basename(path))) continue;
  checkTraceabilityFile(path);
}

// ---------------------------------------------------------------------------
// Step 5: INDEX.md invariant — per contract and the Total row.
// ---------------------------------------------------------------------------
function stripBold(s) {
  return s.replace(/\*\*/g, "").trim();
}

function checkIndex() {
  const indexPath = join(TRACEABILITY_ROOT, "INDEX.md");
  if (!existsSync(indexPath)) {
    failures.push(`${indexPath}: file does not exist`);
    return;
  }
  const lines = readFileSync(indexPath, "utf8").split("\n");

  let sumReq = 0;
  let sumSeed = 0;
  let sumCovered = 0;
  let sumTbd = 0;
  let totalRow = null;

  for (const raw of lines) {
    const line = raw.trim();
    if (!line.startsWith("|")) continue;
    const cells = line.split("|").map((c) => c.trim());
    // cells[0]="", 1=Contract, 2=Requirements, 3=Seed cases, 4=covered, 5=TBD-wave1, 6=Open draft-notes, 7=""
    const contractCell = cells[1];
    if (!contractCell) continue;

    if (stripBold(contractCell) === "Total") {
      totalRow = {
        req: Number(stripBold(cells[2])),
        seed: Number(stripBold(cells[3])),
        covered: Number(stripBold(cells[4])),
        tbd: Number(stripBold(cells[5])),
      };
      continue;
    }

    if (contractCell === "Contract" || /^-+$/.test(contractCell)) continue; // header or separator row

    // Most rows spell the Contract cell as the real "slug/major" header value
    // (e.g. "manifest/1"), but three of INDEX.md's own rows (device-class-registry,
    // security-model, channel-index) instead spell it as the bare corpus-dir
    // name with no major suffix — resolve both rather than silently skipping a
    // row this build cannot otherwise place (the same "no silent caps"
    // discipline the corpus drivers themselves follow).
    const stem = stemByContractValue.get(contractCell) ?? (contractsByStem.has(contractCell) ? contractCell : undefined);
    if (!stem) {
      failures.push(`${indexPath}: row for unrecognized contract "${contractCell}" — no contracts/*.md defines this slug`);
      continue;
    }

    const req = Number(cells[2]);
    const seed = Number(cells[3]);
    const covered = Number(cells[4]);
    const tbd = Number(cells[5]);

    const actualReq = contractsByStem.get(stem)?.requirementIds.size ?? 0;
    const actualSeed = caseIdsByStem.get(stem)?.size ?? 0;
    const counts = rowCounts.get(stem) ?? { covered: 0, tbd: 0 };

    sumReq += actualReq;
    sumSeed += actualSeed;
    sumCovered += counts.covered;
    sumTbd += counts.tbd;

    if (covered !== counts.covered || tbd !== counts.tbd || req !== actualReq || seed !== actualSeed) {
      failures.push(
        `${indexPath}: ${contractCell} row says covered=${covered}/TBD=${tbd}/req=${req}/seed=${seed}; ` +
          `actual ${counts.covered}/${counts.tbd}/${actualReq}/${actualSeed} — regenerate the row`
      );
    }
  }

  if (!totalRow) {
    failures.push(`${indexPath}: no **Total** row found`);
  } else if (
    totalRow.req !== sumReq ||
    totalRow.seed !== sumSeed ||
    totalRow.covered !== sumCovered ||
    totalRow.tbd !== sumTbd
  ) {
    failures.push(
      `${indexPath}: Total row says covered=${totalRow.covered}/TBD=${totalRow.tbd}/req=${totalRow.req}/seed=${totalRow.seed}; ` +
        `actual ${sumCovered}/${sumTbd}/${sumReq}/${sumSeed} — regenerate the row`
    );
  }
}

checkIndex();

// ---------------------------------------------------------------------------
if (failures.length) {
  console.error(failures.join("\n"));
  console.log(`SUMMARY: validate-coverage: FAILED — ${failures.length} issue(s); first: ${failures[0]}`);
  process.exitCode = 1;
} else {
  console.log(`validate-coverage: OK (${contractsByStem.size} contract(s), ${caseLocations.size} corpus case(s))`);
  console.log(`SUMMARY: validate-coverage: OK (${contractsByStem.size} contract(s), ${caseLocations.size} corpus case(s))`);
}
