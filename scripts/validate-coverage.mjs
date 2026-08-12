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
//      that appears in driven-manifest.json's entry for THIS ROW'S OWN
//      contract (never any other contract's entry) — a "covered" row whose
//      cases are all unexecuted by its own contract's driver must downgrade.
//      This is deliberately scoped per-contract, not unioned across the
//      manifest: a flattened union would let any hand-maintained entry (e.g.
//      ui-schema/1's, which conformance/cmd/driven-manifest cannot regenerate
//      or verify) vouch for a completely unrelated contract's rows just by
//      naming its case IDs.
//   9. driven-manifest.json: every driven/pending case ID it lists must exist
//      in the corpus (catches the manifest itself drifting from reality on
//      the "phantom case" side; the runtime-vs-manifest drift on the driver
//      side is caught by Go tests, not this script).
//  10. INDEX.md: per contract, covered/TBD-wave1/Requirements/Seed-cases
//      match the actual counts; the Total row matches the column sums; and
//      every contract defined under contracts/ has exactly one row (a
//      deleted row is caught, not just a wrong sum over whatever rows
//      remain).
//  11. A "covered" row is affirmed against the requirement TEXT it was read
//      against, via a digest in conformance/coverage-digests.json. Checks
//      1-10 all key on requirement ID, and IDs here are paragraph-sized and
//      carry several MUSTs each — so a new obligation added INSIDE an
//      already-covered requirement satisfies every one of them while being
//      exercised by nothing. Refresh with `--affirm` after re-reading the
//      cases. See checkCoveredTextDigest for what this does not do.
import { readdirSync, readFileSync, statSync, existsSync, writeFileSync } from "node:fs";
import { join, basename, dirname } from "node:path";
import { createHash } from "node:crypto";

const CONTRACTS_ROOT = "contracts";
const TRACEABILITY_ROOT = join("conformance", "traceability");
const CORPORA_ROOT = join("conformance", "corpora");
const MANIFEST_PATH = join("conformance", "driven-manifest.json");
const DIGESTS_PATH = join("conformance", "coverage-digests.json");
const AFFIRM = process.argv.includes("--affirm");
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
  return { path, contractValue, requirementIds, requirementText: parseRequirementBlocks(lines) };
}

// A requirement's BLOCK — everything from its `**[ID]**` line up to the next
// requirement ID, the next heading, or end of file.
//
// Not just the ID's own line, and the difference is the whole point. Most
// requirements here are one (very long) source line, but six are not: DAT-119,
// EVT-081, MKT-043, MKT-060a and SUR-060 carry their normative clauses in a
// list underneath — sometimes after a blank line — and SEC-003f is followed
// immediately by SEC-004 with no blank line between them. A first-line-only
// digest would be blind to an edit to exactly the kind of enumerated `(a)/(b)`
// clause most likely to gain a new obligation, which is the failure this whole
// mechanism exists to catch.
//
// Trailing non-normative prose (the `*Note: …*` under DAT-075, say) lands in
// the block too, and that is deliberate rather than tolerated: such a note
// constrains what the requirement's cases may assert — DAT-075's says the
// contract intentionally leaves `playlist_id` unconstrained against
// `display_power` — so an edit to one is worth the glance at its cases that
// re-affirming costs.
//
// Fences are carried through verbatim: a fenced example inside a block is part
// of the requirement's meaning, and its lines must not be read as new
// requirements or headings while inside the fence.
function parseRequirementBlocks(lines) {
  const blocks = new Map();
  let currentId = null;
  let buf = [];
  let inFence = false;
  const flush = () => {
    if (currentId) blocks.set(currentId, buf.join("\n"));
    currentId = null;
    buf = [];
  };
  for (const line of lines) {
    if (/^\s*(```|~~~)/.test(line)) {
      inFence = !inFence;
      if (currentId) buf.push(line);
      continue;
    }
    if (!inFence) {
      const m = REQUIREMENT_ID_RE.exec(line);
      if (m) {
        flush();
        currentId = m[1];
        buf.push(line);
        continue;
      }
      if (/^#{1,6}\s/.test(line)) {
        flush();
        continue;
      }
    }
    if (currentId) buf.push(line);
  }
  flush();
  return blocks;
}

// The digest a coverage claim is affirmed against. Whitespace is collapsed
// before hashing so that re-wrapping a paragraph — which changes no
// obligation — does not re-open every row in the file and train everyone to
// re-affirm without reading. Any change to the WORDS still moves it.
function digestOf(text) {
  return createHash("sha256").update(text.replace(/\s+/g, " ").trim()).digest("hex").slice(0, 16);
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

// Step 2b: load the affirmed requirement-text digests (check #11). A missing
// file is not a failure on its own — every covered row reports its own missing
// digest, which says what to do about it in the place it matters.
let digests = {};
if (existsSync(DIGESTS_PATH)) {
  try {
    digests = JSON.parse(readFileSync(DIGESTS_PATH, "utf8")).contracts ?? {};
  } catch (err) {
    failures.push(`${DIGESTS_PATH}: invalid JSON: ${err.message}`);
  }
}
// "<stem>/<REQ-ID>" -> digest, filled as covered rows are checked. Doubles as
// the affirm-mode output and as the set the stale-entry sweep is taken against.
const affirmed = new Map();


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

// ---------------------------------------------------------------------------
// Check #11: a "covered" row is affirmed against the requirement TEXT it was
// read against.
//
// Every other check here answers "is this requirement ID cited by some case?"
// That is the citation half. The question that decides whether the corpus
// means anything is "is every OBLIGATION exercised?", and requirement IDs are
// far too coarse to stand in for it: they are paragraph-sized and routinely
// carry several MUSTs. DAT-075 alone carries a dozen.
//
// The hole that produced this check: a new normative MUST was added INSIDE
// DAT-075, which was already `covered` by two cases about fall-through and
// DST. Neither touches the new clause, and no gate could notice, because the
// row still cited cases and the ID was still real. A fresh obligation slid in
// under a satisfied row, and nothing in the pipeline was capable of seeing it.
//
// So: record the digest of the text a coverage claim was affirmed against.
// When the text changes, the claim is no longer known to hold and the row
// re-opens until a human looks at the cases again and re-affirms with
// `node scripts/validate-coverage.mjs --affirm`.
//
// WHAT THIS DOES NOT DO, stated plainly because the gate's name oversells it:
// it cannot find an obligation that is ALREADY unexercised. Bootstrapping
// records today's text as affirmed, DAT-075's uncovered clause included. This
// stops the corpus drifting further behind the contract; it does not measure
// how far behind it already is. Closing that needs a case-cites-a-clause
// grammar or a MUST-count rule, both of which re-tag the whole corpus.
function checkCoveredTextDigest(stem, reqId, loc, caseIdsCell) {
  const text = contractsByStem.get(stem)?.requirementText?.get(reqId);
  if (text === undefined) return; // validate-contracts owns "the ID is real"
  const actual = digestOf(text);
  affirmed.set(`${stem}/${reqId}`, actual);
  if (AFFIRM) return;

  const recorded = digests[stem]?.[reqId];
  if (recorded === undefined) {
    failures.push(
      `${loc}: ${reqId} is covered but ${DIGESTS_PATH} records no digest for it — a coverage claim ` +
        `nothing has affirmed against the requirement's text. Read ${caseIdsCell || "its cases"} against ` +
        `the requirement, then run \`node scripts/validate-coverage.mjs --affirm\``,
    );
    return;
  }
  if (recorded !== actual) {
    failures.push(
      `${loc}: ${reqId}'s normative text CHANGED since its coverage was affirmed ` +
        `(${recorded} -> ${actual}). Its cases were read against the old wording, so "covered" is no ` +
        `longer a claim anyone has checked — an added MUST inside an already-covered requirement is ` +
        `exactly the drift this catches. Re-read ${caseIdsCell || "its cases"} against the new text, add ` +
        `a case if the obligation is new, then \`node scripts/validate-coverage.mjs --affirm\``,
    );
  }
}

function checkTraceabilityFile(path) {
  const stem = basename(path).replace(/\.md$/, "");
  const lines = readFileSync(path, "utf8").split("\n");
  const existingIds = caseIdsByStem.get(stem) ?? new Set();
  // #8 is scoped to THIS contract's own manifest entry — never the whole
  // manifest — so one contract's hand-maintained driven list can never vouch
  // for another contract's coverage claim (see check #8's doc comment above).
  const contractValue = contractsByStem.get(stem)?.contractValue;
  const ownDriven = new Set(manifest[contractValue]?.driven ?? []);
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
      checkCoveredTextDigest(stem, reqId, loc, caseIdsCell);
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

    // #8 a "covered" row needs >=1 of its named cases in ITS OWN contract's
    // driven set — never some other contract's entry (see doc comment #8).
    if (status === "covered") {
      const anyDriven = ids.some((id) => ownDriven.has(id));
      if (!anyDriven) {
        failures.push(
          `${loc}: ${reqId} is covered but none of [${ids.join(", ")}] appears in ${contractValue ?? stem}'s own ` +
            `driven set (${MANIFEST_PATH}) — wire a case into a driver and rerun "go run ./conformance/cmd/driven-manifest -write", ` +
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
  const seenStems = new Set();

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

    seenStems.add(stem);

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

  // #10 completeness: every contract defined under contracts/ must have
  // exactly one row here — otherwise "Total matches the column sums" is
  // vacuous for whatever got dropped (a deleted row's counts simply never
  // enter the sums, so the Total still balances against the reduced set).
  for (const stem of contractsByStem.keys()) {
    if (!seenStems.has(stem)) {
      const contractValue = contractsByStem.get(stem)?.contractValue ?? stem;
      failures.push(
        `${indexPath}: no row for contract "${contractValue}" (contracts/${stem}.md) — every contract must roll up here`
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
// Check #11's other half: the digest file must not carry entries for rows that
// are no longer covered. A file that only ever grows rots into a list nobody
// can read, and a stale entry is worse than absent — it is an affirmation of a
// claim the traceability map has since withdrawn.
// ---------------------------------------------------------------------------
function sweepAndWriteDigests() {
  if (!AFFIRM) {
    for (const [stem, byId] of Object.entries(digests)) {
      for (const reqId of Object.keys(byId)) {
        if (!affirmed.has(`${stem}/${reqId}`)) {
          failures.push(
            `${DIGESTS_PATH}: carries a digest for ${stem}/${reqId}, which is no longer a covered row — ` +
              `run \`node scripts/validate-coverage.mjs --affirm\` to prune it`,
          );
        }
      }
    }
    return;
  }

  const next = {};
  for (const [key, digest] of [...affirmed.entries()].sort(([a], [b]) => a.localeCompare(b))) {
    const idx = key.lastIndexOf("/");
    const stem = key.slice(0, idx);
    const reqId = key.slice(idx + 1);
    (next[stem] ??= {})[reqId] = digest;
  }
  let added = 0;
  let changed = 0;
  for (const [stem, byId] of Object.entries(next)) {
    for (const [reqId, digest] of Object.entries(byId)) {
      const before = digests[stem]?.[reqId];
      if (before === undefined) added++;
      else if (before !== digest) changed++;
    }
  }
  let removed = 0;
  for (const [stem, byId] of Object.entries(digests)) {
    for (const reqId of Object.keys(byId)) if (next[stem]?.[reqId] === undefined) removed++;
  }

  const body = {
    _README: [
      "Machine-maintained. Do not hand-edit: run `node scripts/validate-coverage.mjs --affirm`.",
      "One digest per `covered` traceability row: the hash of the requirement's normative text",
      "(its whole block, whitespace-collapsed) AT THE MOMENT ITS COVERAGE WAS AFFIRMED.",
      "",
      "It exists because every other coverage check keys on requirement ID, and IDs here are",
      "paragraph-sized and carry several MUSTs each. A new obligation added inside an already",
      "covered requirement is invisible to an ID-keyed gate -- it happened to DAT-075 -- so the",
      "corpus can drift arbitrarily far behind the contract while reporting full coverage.",
      "",
      "A changed digest is not an error in itself. It means the requirement's wording moved and",
      "nobody has since checked that its cases still exercise it. Re-read the cases against the",
      "new text, add one if the obligation is new, and affirm.",
      "",
      "It cannot find an obligation that was ALREADY unexercised when a digest was first taken.",
      "This stops further drift; it does not measure the drift already there.",
    ],
    contracts: next,
  };
  writeFileSync(DIGESTS_PATH, `${JSON.stringify(body, null, 2)}\n`);
  console.log(
    `validate-coverage: --affirm wrote ${DIGESTS_PATH} — ${affirmed.size} covered row(s): ` +
      `${added} added, ${changed} updated, ${removed} pruned`,
  );
}

sweepAndWriteDigests();

if (failures.length) {
  console.error(failures.join("\n"));
  console.log(`SUMMARY: validate-coverage: FAILED — ${failures.length} issue(s); first: ${failures[0]}`);
  process.exitCode = 1;
} else {
  console.log(`validate-coverage: OK (${contractsByStem.size} contract(s), ${caseLocations.size} corpus case(s))`);
  console.log(`SUMMARY: validate-coverage: OK (${contractsByStem.size} contract(s), ${caseLocations.size} corpus case(s))`);
}
