#!/usr/bin/env node
// scripts/check-public-hygiene.mjs — no tracked file may reference the private tracker.
import { execSync } from "node:child_process";
const files = execSync("git ls-files", { encoding: "utf8" }).trim().split("\n");
const banned = [/waiveo\/program/i];
const hits = [];
for (const f of files) {
  const text = execSync(`git show :${JSON.stringify(f).slice(1, -1)}`, { encoding: "utf8", maxBuffer: 1 << 24 });
  for (const re of banned) if (re.test(text)) hits.push(`${f}: matches ${re}`);
}
if (hits.length) { console.error(hits.join("\n")); process.exit(1); }
console.log(`check-public-hygiene: OK (${files.length} files)`);
