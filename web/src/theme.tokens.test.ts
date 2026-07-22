import { describe, expect, it } from "vitest";
// The raw text of the stylesheet (Vite `?raw` import) — asserted against the
// verbatim brand values below.
import themeCss from "./theme.css?raw";

/**
 * Token-equality guard: `src/theme.css` must carry the HORIZON brand values
 * VERBATIM (the canonical source is brand/HORIZON.md's TOKENS block). Every
 * expected value below is transcribed from that file; a mismatch — or a drift
 * back to stock shadcn neutrals — is a brand defect and fails here.
 */

// The dark set lives on `:root, [data-theme="dark"]` (before the light block);
// the light set on `[data-theme="light"]` (before the @supports upgrade). Split
// so a value can never satisfy an assertion from the wrong theme. Anchor on the
// real selector/at-rule syntax (space-brace / `(background:`) so prose mentions
// of the tokens in comments never move the boundary.
const lightIdx = themeCss.indexOf('[data-theme="light"] {');
const supportsIdx = themeCss.indexOf("@supports (background:");
const darkRegion = themeCss.slice(0, lightIdx);
const lightRegion = themeCss.slice(lightIdx, supportsIdx);

// brand/HORIZON.md TOKENS.dark (content layer) — CSS var name -> verbatim value.
const DARK: Record<string, string> = {
  "--wv-bg": "#0C0A18",
  "--wv-surface": "#14111F",
  "--wv-surface-2": "#1E1A30",
  "--wv-border": "#2E2945",
  "--wv-row-border": "#221E36",
  "--wv-text": "#F1ECFA",
  "--wv-muted": "#9C95B8",
  "--wv-accent": "#F368C4",
  "--wv-accent-fg": "#1A0A16",
  "--wv-accent-text": "#F368C4",
  "--wv-nav-active-bg": "#2A1428",
  "--wv-nav-active-fg": "#F79AD8",
  "--wv-ok": "#43C98A",
  "--wv-ok-bg": "#10241C",
  "--wv-warn": "#F1B54B",
  "--wv-warn-bg": "#2A2010",
  "--wv-err": "#FB6A6A",
  "--wv-err-bg": "#2C1417",
  "--wv-off-bg": "#211E30",
  "--grad-hero-fg": "#FFF6EC",
  "--grad-accent-fg": "#1A0A16",
};

// brand/HORIZON.md TOKENS.light (content layer).
const LIGHT: Record<string, string> = {
  "--wv-bg": "#FCF6F0",
  "--wv-surface": "#FFFFFF",
  "--wv-surface-2": "#F5EEE7",
  "--wv-border": "#E7DCD2",
  "--wv-row-border": "#EFE7DE",
  "--wv-text": "#221826",
  "--wv-muted": "#6B5F6D",
  "--wv-accent": "#BC2A88",
  "--wv-accent-fg": "#FFFFFF",
  "--wv-accent-text": "#BC2A88",
  "--wv-nav-active-bg": "#FBE6F3",
  "--wv-nav-active-fg": "#BC2A88",
  "--wv-ok": "#0F7D46",
  "--wv-ok-bg": "#E8F6EE",
  "--wv-warn": "#9A5A08",
  "--wv-warn-bg": "#FCF1E0",
  "--wv-err": "#BE312B",
  "--wv-err-bg": "#FCEAE7",
  "--wv-off-bg": "#EFEAF0",
  "--grad-hero-fg": "#2A1420",
  "--grad-accent-fg": "#FFFFFF",
};

// brand/HORIZON.md TOKENS gradient set (the exact `in oklch` CSS strings).
const DARK_GRADIENTS = [
  "linear-gradient(150deg in oklch, #FF7A3D 0%, #EA4BA4 46%, #4A34B8 100%)",
  "linear-gradient(120deg in oklch, #FF7A33 0%, #FB3D93 100%)",
  "linear-gradient(160deg in oklch, #FFB25A 0%, #FF6F91 55%, #EA4BA4 100%)",
  "linear-gradient(160deg in oklch, #FF6FB0 0%, #C13AD0 55%, #5B34C4 100%)",
  "linear-gradient(160deg in oklch, #FF9A3D 0%, #F0533E 55%, #B4265E 100%)",
  "linear-gradient(160deg in oklch, #7A5CF0 0%, #4A34B8 55%, #1E1A5A 100%)",
];
const LIGHT_GRADIENTS = [
  "linear-gradient(150deg in oklch, #FFD9A8 0%, #FBAFC8 46%, #A9C7F5 100%)",
  "linear-gradient(120deg in oklch, #C42E8F 0%, #93267E 100%)",
  "linear-gradient(160deg in oklch, #FFE1A6 0%, #FFB27A 55%, #FF8FA0 100%)",
  "linear-gradient(160deg in oklch, #FFC7DE 0%, #F79AC8 55%, #C77BE0 100%)",
  "linear-gradient(160deg in oklch, #FFD9A8 0%, #FFA9B0 55%, #E59AD8 100%)",
  "linear-gradient(160deg in oklch, #BFD4FA 0%, #A9C7F5 55%, #C9B8F2 100%)",
];

describe("HORIZON tokens (theme.css == brand/HORIZON.md, verbatim)", () => {
  it("Dusk (dark) is the default — declared on :root and [data-theme=dark]", () => {
    expect(themeCss).toMatch(/:root\s*,\s*\[data-theme="dark"\]/);
    expect(themeCss).toContain('[data-theme="light"]');
  });

  it.each(Object.entries(DARK))("dark %s == %s", (name, value) => {
    expect(darkRegion).toContain(`${name}: ${value};`);
  });

  it.each(Object.entries(LIGHT))("light %s == %s", (name, value) => {
    expect(lightRegion).toContain(`${name}: ${value};`);
  });

  it.each(DARK_GRADIENTS)("dark gradient present: %s", (grad) => {
    expect(themeCss).toContain(grad);
  });

  it.each(LIGHT_GRADIENTS)("light gradient present: %s", (grad) => {
    expect(themeCss).toContain(grad);
  });

  it("declares a plain-CSS gradient fallback BEFORE the oklch upgrade", () => {
    // The plain (no `in oklch`) form is declared in the base blocks; the
    // @supports block below upgrades it. Engines without oklch interpolation
    // keep the plain gradient rather than failing to a blank fill.
    const plainDarkHero = "--grad-hero: linear-gradient(150deg, #FF7A3D 0%, #EA4BA4 46%, #4A34B8 100%);";
    const plainLightHero = "--grad-hero: linear-gradient(150deg, #FFD9A8 0%, #FBAFC8 46%, #A9C7F5 100%);";
    expect(darkRegion).toContain(plainDarkHero);
    expect(lightRegion).toContain(plainLightHero);
    expect(themeCss).toMatch(/@supports \(background: linear-gradient\(in oklch/);
    expect(themeCss.indexOf(plainDarkHero)).toBeLessThan(supportsIdx);
  });

  it("radii are 14px card / 10px button (brand/HORIZON.md TOKENS)", () => {
    expect(themeCss).toContain("--radius-card: 14px;");
    expect(themeCss).toContain("--radius-btn: 10px;");
  });

  it("self-hosts the three OFL brand faces (no external font families)", () => {
    expect(themeCss).toContain("Bricolage Grotesque");
    expect(themeCss).toContain("Inter");
    expect(themeCss).toContain("JetBrains Mono");
  });

  it("carries a grain-texture utility as an inline SVG feTurbulence data-URI", () => {
    expect(themeCss).toContain("feTurbulence");
    expect(themeCss).toContain("data:image/svg+xml");
  });

  it("retains NO stock shadcn neutral palette (no drift from HORIZON)", () => {
    // shadcn's default light/dark neutrals — must not appear anywhere.
    expect(themeCss).not.toContain("oklch(1 0 0)");
    expect(themeCss).not.toContain("oklch(0.205 0 0)");
    expect(themeCss).not.toContain("222.2");
  });
});
