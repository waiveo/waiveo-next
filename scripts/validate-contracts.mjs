#!/usr/bin/env node
// scripts/validate-contracts.mjs — contract corpus format + requirement-ID rules.
//
//   1. Every contracts/**/*.md except README.md/TEMPLATE.md opens with the header
//      block: # Title, then **Contract:**, **Version:**, **Status:** lines.
//   2. Every such file contains >=1 requirement-ID anchor: a line starting with
//      **[XXX-000]** (three uppercase letters, a hyphen, three digits).
//   3. Requirement IDs are unique across the whole contracts corpus.
//   4. Every requirement ID referenced from a conformance/traceability/*.md map
//      (other than its own README.md) exists somewhere in the contracts corpus.
import { readdirSync, readFileSync, statSync, existsSync } from "node:fs";
import { join } from "node:path";

const CONTRACTS_ROOT = "contracts";
const TRACEABILITY_ROOT = join("conformance", "traceability");
const EXEMPT_NAMES = new Set(["README.md", "TEMPLATE.md"]);
const REQUIREMENT_ID_RE = /^\*\*\[([A-Z]{3}-\d{3})\]\*\*/;
const TRACEABILITY_ID_RE = /^[A-Z]{3}-\d{3}$/;

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

function checkHeader(path, lines) {
  const head = lines.slice(0, 10);
  const need = [
    /^# .+/,
    /^\*\*Contract:\*\* [a-z0-9-]+\/\d+/,
    /^\*\*Version:\*\* \d+\.\d+/,
    /^\*\*Status:\*\* (draft|review|normative|superseded)$/,
  ];
  for (const re of need) if (!head.some((l) => re.test(l))) failures.push(`${path}: missing ${re}`);
}

// Requirement ID -> the file that first defined it (also doubles as the global set).
const idOwner = new Map();

function collectRequirementIds(path, lines) {
  let found = 0;
  lines.forEach((line, i) => {
    const m = REQUIREMENT_ID_RE.exec(line);
    if (!m) return;
    found++;
    const id = m[1];
    const owner = idOwner.get(id);
    if (owner) failures.push(`${path}:${i + 1}: duplicate requirement ID ${id} (already defined in ${owner})`);
    else idOwner.set(id, path);
  });
  if (found === 0) failures.push(`${path}: no requirement-ID anchor found (expected >=1 line matching ${REQUIREMENT_ID_RE})`);
}

let contractFileCount = 0;
for (const path of walkMarkdown(CONTRACTS_ROOT)) {
  const name = path.split("/").pop();
  if (EXEMPT_NAMES.has(name)) continue;
  contractFileCount++;
  const lines = readFileSync(path, "utf8").split("\n");
  checkHeader(path, lines);
  collectRequirementIds(path, lines);
}

// A traceability map row looks like: | XXX-001 | contract §anchor | case-id(s) | status |
// Only the first cell is inspected; anything not shaped like a requirement ID
// (the header row, the --- separator row, prose) is silently skipped.
function checkTraceabilityFile(path) {
  const lines = readFileSync(path, "utf8").split("\n");
  lines.forEach((raw, i) => {
    const line = raw.trim();
    if (!line.startsWith("|")) return;
    const first = line
      .split("|")[1]
      ?.trim()
      .replace(/`/g, "");
    if (!first || !TRACEABILITY_ID_RE.test(first)) return;
    if (!idOwner.has(first)) failures.push(`${path}:${i + 1}: traceability map references undefined requirement ID ${first}`);
  });
}

for (const path of walkMarkdown(TRACEABILITY_ROOT)) {
  if (path.split("/").pop() === "README.md") continue;
  checkTraceabilityFile(path);
}

if (failures.length) {
  console.error(failures.join("\n"));
  console.log(`SUMMARY: validate-contracts: FAILED — ${failures.length} issue(s); first: ${failures[0]}`);
  process.exitCode = 1;
} else {
  console.log(`validate-contracts: OK (${CONTRACTS_ROOT} scanned, ${contractFileCount} file(s), ${idOwner.size} requirement ID(s))`);
  console.log(`SUMMARY: validate-contracts: OK (${contractFileCount} contract file(s), ${idOwner.size} requirement ID(s))`);
}
