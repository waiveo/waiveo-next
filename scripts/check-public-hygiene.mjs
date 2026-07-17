#!/usr/bin/env node
// scripts/check-public-hygiene.mjs — no tracked file may reference the private tracker.
// Uses execFileSync with argv arrays (no shell), so filenames with spaces or shell
// metacharacters ($(), ;, backticks) are inert data, never interpreted.
import { execFileSync } from "node:child_process";
const files = execFileSync("git", ["ls-files"], { encoding: "utf8" }).trim().split("\n");
// NOTE: the pattern is written escaped (waiveo\/program) so this file itself does not
// contain the contiguous banned string and therefore never trips its own check.
const banned = [/waiveo\/program/i];
const hits = [];
for (const f of files) {
  if (!f) continue;
  const text = execFileSync("git", ["show", `:${f}`], { encoding: "utf8", maxBuffer: 1 << 24 });
  for (const re of banned) if (re.test(text)) hits.push(`${f}: matches ${re}`);
}
if (hits.length) {
  console.error(hits.join("\n"));
  console.log(`SUMMARY: check-public-hygiene: FAILED — ${hits.length} hit(s); first: ${hits[0]}`);
  process.exitCode = 1;
} else {
  console.log(`check-public-hygiene: OK (${files.length} files)`);
  console.log(`SUMMARY: check-public-hygiene: OK (${files.length} files)`);
}
