/**
 * The HORIZON token scales, as data the gallery visualizes. Every value here is
 * transcribed from brand/HORIZON.md so the /design route is a faithful, living
 * reference of the brand system — the palettes (both skies), the gradient set,
 * the type scale, and the shape/space scales.
 *
 * The two palettes are rendered as STATIC swatches (literal hex) so the gallery
 * shows Dusk AND Daybreak side by side regardless of the active theme — the
 * token reference, not just the live-themed value. The gradient bands, by
 * contrast, use the live `--grad-*` variables so they re-tone with the theme,
 * demonstrating the dual-mode identity in motion.
 */

export interface Swatch {
  /** The `--wv-*` token name (without the `--wv-` prefix). */
  token: string;
  /** The literal value from brand/HORIZON.md, for the swatch + caption. */
  value: string;
  /** What the token is for. */
  role: string;
}

// brand/HORIZON.md → TOKENS.dark (the content layer).
export const duskPalette: Swatch[] = [
  { token: "bg", value: "#0C0A18", role: "App backdrop — indigo near-black" },
  { token: "surface", value: "#14111F", role: "Panels" },
  { token: "surface-2", value: "#1E1A30", role: "Cards / raised" },
  { token: "border", value: "#2E2945", role: "Hairline" },
  { token: "text", value: "#F1ECFA", role: "Primary text — 16.9:1" },
  { token: "muted", value: "#9C95B8", role: "Secondary text — 6.6:1" },
  { token: "accent", value: "#F368C4", role: "Dusk magenta — interactive" },
  { token: "nav-active-bg", value: "#2A1428", role: "Active nav chip" },
  { token: "ok", value: "#43C98A", role: "Live / ok — the reserved green" },
  { token: "warn", value: "#F1B54B", role: "Amber warn" },
  { token: "err", value: "#FB6A6A", role: "Error red" },
  { token: "off-bg", value: "#211E30", role: "Disabled / offline chip" },
];

// brand/HORIZON.md → TOKENS.light (the same ramp, re-toned to paper).
export const daybreakPalette: Swatch[] = [
  { token: "bg", value: "#FCF6F0", role: "App backdrop — warm dawn paper" },
  { token: "surface", value: "#FFFFFF", role: "Panels / cards" },
  { token: "surface-2", value: "#F5EEE7", role: "Inset" },
  { token: "border", value: "#E7DCD2", role: "Hairline" },
  { token: "text", value: "#221826", role: "Primary plum-ink — 16.0:1" },
  { token: "muted", value: "#6B5F6D", role: "Secondary text — 6.0:1" },
  { token: "accent", value: "#BC2A88", role: "Deep magenta — interactive" },
  { token: "nav-active-bg", value: "#FBE6F3", role: "Active nav chip" },
  { token: "ok", value: "#0F7D46", role: "Live / ok — the reserved green" },
  { token: "warn", value: "#9A5A08", role: "Amber warn" },
  { token: "err", value: "#BE312B", role: "Error red" },
  { token: "off-bg", value: "#EFEAF0", role: "Disabled / offline chip" },
];

export interface GradientSpec {
  /** The `--grad-*` CSS variable painting the band. */
  cssVar: string;
  /** Human name (the daypart, for the decks). */
  name: string;
  /** Where this gradient is sanctioned to live. */
  where: string;
}

// The gradient set (brand/HORIZON.md §4). Decks 1–4 bind to dayparts — the sky
// turning across the day IS the schedule category, not decoration on top of it.
export const gradients: GradientSpec[] = [
  { cssVar: "--grad-hero", name: "Hero", where: "Hero bands & page headers (behind a scrim)" },
  { cssVar: "--grad-accent", name: "Accent", where: "The one primary action per view" },
  { cssVar: "--grad-deck-1", name: "Sunrise", where: "Morning daypart — content thumbs" },
  { cssVar: "--grad-deck-2", name: "Midday", where: "Bright / promo daypart" },
  { cssVar: "--grad-deck-3", name: "Golden Hour", where: "Evening daypart" },
  { cssVar: "--grad-deck-4", name: "Nightfall", where: "Night daypart" },
];

export interface TypeSpec {
  role: string;
  /** The utility class carrying the family (or "" for the Inter UI base). */
  fontClass: string;
  family: string;
  /** px */
  size: number;
  weight: number;
  lineHeight: number;
  /** em */
  tracking: number;
  uppercase?: boolean;
  tabular?: boolean;
  sample: string;
}

// The type scale (brand/HORIZON.md §5). Bricolage Grotesque (display), Inter
// (UI), JetBrains Mono (telemetry) — all self-hosted OFL.
export const typeScale: TypeSpec[] = [
  { role: "Display XL", fontClass: "font-display", family: "Bricolage Grotesque", size: 46, weight: 700, lineHeight: 1.04, tracking: -0.02, sample: "One horizon, two skies" },
  { role: "Display L", fontClass: "font-display", family: "Bricolage Grotesque", size: 32, weight: 600, lineHeight: 1.1, tracking: -0.015, sample: "Screens that glow like a room" },
  { role: "H1", fontClass: "font-display", family: "Bricolage Grotesque", size: 26, weight: 600, lineHeight: 1.18, tracking: -0.01, sample: "Schedules" },
  { role: "H2", fontClass: "font-display", family: "Bricolage Grotesque", size: 21, weight: 600, lineHeight: 1.25, tracking: 0, sample: "On air across your screens" },
  { role: "H3", fontClass: "", family: "Inter", size: 17, weight: 600, lineHeight: 1.3, tracking: 0, sample: "Dinner rush daypart" },
  { role: "Body L", fontClass: "", family: "Inter", size: 16, weight: 400, lineHeight: 1.55, tracking: 0, sample: "The box that carries your space across the line." },
  { role: "Body (UI base)", fontClass: "", family: "Inter", size: 14, weight: 400, lineHeight: 1.55, tracking: 0, sample: "Nothing scheduled for this daypart yet. Add content to light it up." },
  { role: "Label / UI-strong", fontClass: "", family: "Inter", size: 13, weight: 500, lineHeight: 1.4, tracking: 0.01, sample: "Screen name" },
  { role: "Eyebrow / status", fontClass: "", family: "Inter", size: 11, weight: 600, lineHeight: 1.4, tracking: 0.1, uppercase: true, sample: "On air" },
  { role: "Telemetry", fontClass: "font-mono", family: "JetBrains Mono", size: 13, weight: 400, lineHeight: 1.45, tracking: 0, tabular: true, sample: "192.0.2.11 · 42ms · 99.98%" },
];

export interface RadiusSpec {
  role: string;
  /** The `rounded-*` utility. */
  radiusClass: string;
  /** The px label. */
  value: string;
}

// Radii (brand/HORIZON.md §6). Softer than round 1 (the daybreak warmth), still
// engineered. Pills are status-only.
export const radii: RadiusSpec[] = [
  { role: "Data-table edge", radiusClass: "rounded-none", value: "0" },
  { role: "Input / chip", radiusClass: "rounded-input", value: "8px" },
  { role: "Button", radiusClass: "rounded-btn", value: "10px" },
  { role: "Card", radiusClass: "rounded-card", value: "14px" },
  { role: "Panel / modal", radiusClass: "rounded-panel", value: "18px" },
  { role: "Pill (status only)", radiusClass: "rounded-pill", value: "999px" },
];

// Space scale — 4px base (brand/HORIZON.md §6).
export const spacing: number[] = [4, 8, 12, 16, 24, 32, 48, 64];
