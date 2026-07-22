import { describe, expect, it } from "vitest";
import responsiveCss from "./responsive.css?raw";

/**
 * Responsive is a CONTRACT (Matt, 2026-07-22): the console must work at 360px
 * with NO horizontal PAGE scroll, the nav is locked left (a permanent rail at
 * >=1024px, a drawer + hamburger below), modals go full-screen on small
 * viewports, and touch targets are >=44px. Those breakpoint rules are authored
 * explicitly in responsive.css (rather than left implicit in scattered Tailwind
 * utilities) precisely so this contract can assert they exist and never silently
 * regress. Whitespace is normalised so the assertions read against intent, not
 * formatting.
 */
const css = responsiveCss.replace(/\s+/g, " ");

describe("responsive CSS contract", () => {
  it("locks the nav left at the 1024px desktop breakpoint (rail visible, hamburger hidden)", () => {
    // The rail is hidden by default (drawer takes over below desktop)...
    expect(css).toMatch(/\.wv-shell__sidebar\s*\{[^}]*display:\s*none/);
    // ...and permanently visible from 1024px up.
    expect(responsiveCss).toMatch(/@media\s*\(min-width:\s*1024px\)/);
    expect(css).toMatch(/@media \(min-width: 1024px\) \{[^@]*\.wv-shell__sidebar\s*\{[^}]*display:\s*flex/);
    // The hamburger is the mirror image: shown below desktop, hidden at/above it.
    expect(css).toMatch(/@media \(min-width: 1024px\) \{[^@]*\.wv-shell__hamburger\s*\{[^}]*display:\s*none/);
  });

  it("takes a modal full-screen at the 640px small-viewport breakpoint", () => {
    expect(responsiveCss).toMatch(/@media\s*\(max-width:\s*640px\)/);
    // Full-bleed: pinned to the viewport edges with no rounded panel inset.
    expect(css).toMatch(/@media \(max-width: 640px\) \{[^@]*\.wv-modal[^{]*\{[^}]*inset:\s*0/);
    expect(css).toMatch(/\.wv-modal[^{]*\{[^}]*max-width:\s*100%/);
  });

  it("guarantees >=44px touch targets", () => {
    expect(css).toMatch(/\.wv-touch\s*\{[^}]*min-(height|block-size):\s*44px/);
    expect(css).toMatch(/\.wv-touch\s*\{[^}]*min-(width|inline-size):\s*44px/);
  });

  it("contains table overflow INSIDE the table region (never the page)", () => {
    expect(css).toMatch(/\.wv-table-region\s*\{[^}]*overflow-x:\s*auto/);
  });
});
