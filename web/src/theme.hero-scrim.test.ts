import { describe, expect, it } from "vitest";
import themeCss from "./theme.css?raw";

/**
 * Brand build-rule (HORIZON §4, §8.2): text NEVER sits on a raw gradient — a
 * scrim/panel always intervenes. `--grad-hero` is the loudest ramp, so this test
 * enforces the invariant structurally: the ONLY sanctioned way to paint the hero
 * gradient is the `.wv-hero` utility, which lays `--grad-hero-scrim` between the
 * ramp and its content. Any source file that applies `var(--grad-hero)` as a
 * background must also reference the scrim in the same file — so no page or
 * component can ever ship a headline directly on the raw hero ramp.
 */

// Every source file's raw text (excluding tests, which mention the token in
// assertions). Vite's import.meta.glob resolves relative to this file (src/).
const modules = import.meta.glob("./**/*.{ts,tsx,css}", {
  query: "?raw",
  import: "default",
  eager: true,
}) as Record<string, string>;

const sources = Object.entries(modules).filter(([path]) => !/\.test\.tsx?$/.test(path));

// Match `var(--grad-hero)` used as a background — but NOT `--grad-hero-fg` or
// `--grad-hero-scrim` (the `)` must immediately follow the token name).
const RAW_HERO = /var\(--grad-hero\)/;

describe("hero gradient never carries text without a scrim", () => {
  it("defines the scrim token and the scrim-laying .wv-hero utility", () => {
    expect(themeCss).toContain("--grad-hero-scrim");
    // The .wv-hero utility paints the ramp AND lays the scrim via ::before.
    expect(themeCss).toMatch(/\.wv-hero\s*\{[^}]*var\(--grad-hero\)/);
    expect(themeCss).toMatch(/\.wv-hero::before\s*\{[^}]*var\(--grad-hero-scrim\)/);
  });

  it("no source paints --grad-hero without also laying --grad-hero-scrim", () => {
    const offenders = sources
      .filter(([, text]) => RAW_HERO.test(text) && !text.includes("--grad-hero-scrim"))
      .map(([path]) => path);
    expect(offenders, `these files paint --grad-hero with no scrim: ${offenders.join(", ")}`).toEqual([]);
  });
});
