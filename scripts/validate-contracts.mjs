#!/usr/bin/env node
// scripts/validate-contracts.mjs — every contracts/**/*.md except README.md
// must open with: # Title, then **Contract:** name/major, **Version:**, **Status:** lines.
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";

const root = "contracts";
const failures = [];
function walk(dir) {
  for (const name of readdirSync(dir)) {
    const p = join(dir, name);
    if (statSync(p).isDirectory()) walk(p);
    else if (p.endsWith(".md") && name !== "README.md") check(p);
  }
}
function check(path) {
  const lines = readFileSync(path, "utf8").split("\n").slice(0, 10);
  const need = [/^# .+/, /^\*\*Contract:\*\* [a-z0-9-]+\/\d+/, /^\*\*Version:\*\* \d+\.\d+/, /^\*\*Status:\*\* (draft|review|normative|superseded)$/];
  for (const re of need) if (!lines.some((l) => re.test(l))) failures.push(`${path}: missing ${re}`);
}
walk(root);
if (failures.length) { console.error(failures.join("\n")); process.exit(1); }
console.log(`validate-contracts: OK (${root} scanned)`);
