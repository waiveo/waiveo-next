// The pack icon resolver: a manifest-declared glyph name → a real lucide
// component for an installed pack.
//
// It sits beside the Extensions console because that is now its only consumer:
// the pack cards on /extensions. It used to live in the shell, resolving the
// glyph for a pack's own rail section — the section the owner killed on
// 2026-08-19 — and there is no longer any shell code that reads a pack's
// manifest at all.
//
// A pack MAY name an icon in its manifest (`icon`, an optional display hint —
// a lowercase kebab-case lucide glyph name, grammar-validated at install in
// internal/manifest). This module is the host-allowed SET: the names a pack may
// name, each mapped to the kit's imported component. Resolution NEVER fails —
// an absent, non-string, or unrecognized name (a typo, or a glyph a newer host
// added that this build does not carry) degrades to the default extension glyph.
// The console therefore never renders a broken/missing extension icon.

import {
  Activity,
  Blocks,
  Boxes,
  CalendarClock,
  ClipboardList,
  FileText,
  Images,
  LayoutDashboard,
  LayoutTemplate,
  Menu,
  MonitorPlay,
  Package,
  Palette,
  Puzzle,
  Settings,
  Store,
  Utensils,
  Workflow,
  type LucideIcon,
} from "lucide-react";

/** The glyph a pack wears when its manifest declares no icon — or declares one
 * the host does not recognize. A real, always-available extension glyph, so the
 * console never shows a broken/missing icon. */
export const DEFAULT_PACK_ICON: LucideIcon = Puzzle;

/**
 * The host-allowed extension-icon set: the lucide glyph names (kebab-case — the
 * canonical lucide id) a pack MAY name in its manifest `icon` field, mapped to the
 * kit's imported component. This is the single host-allowed set the render layer
 * resolves against; the manifest engine (internal/manifest) validates only that a
 * declared name is a well-formed lucide identifier, leaving membership (and its
 * graceful default) to resolve here — so an unknown-but-well-formed name is never
 * a broken icon and never a failed install.
 */
export const ALLOWED_PACK_ICONS: Record<string, LucideIcon> = {
  puzzle: Puzzle,
  blocks: Blocks,
  boxes: Boxes,
  package: Package,
  store: Store,
  utensils: Utensils,
  menu: Menu,
  "clipboard-list": ClipboardList,
  "layout-dashboard": LayoutDashboard,
  "layout-template": LayoutTemplate,
  "monitor-play": MonitorPlay,
  "calendar-clock": CalendarClock,
  images: Images,
  workflow: Workflow,
  activity: Activity,
  palette: Palette,
  "file-text": FileText,
  settings: Settings,
};

/**
 * Resolve a manifest-declared icon NAME to a lucide component.
 *
 * The value is UNTRUSTED manifest data (install validates the manifest as JSON,
 * not the icon's runtime type), so anything that is not an allowed string name —
 * `undefined`, a non-string, or an unrecognized name — returns DEFAULT_PACK_ICON.
 * The return is ALWAYS a real component: the console never renders a broken icon.
 */
export function resolvePackIcon(name?: unknown): LucideIcon {
  // OWN properties only. A plain-object lookup answers "constructor",
  // "toString" and "__proto__" out of Object.prototype with something truthy but
  // un-renderable, and the manifest grammar (a lowercase identifier) admits every
  // one of those names — so an unguarded index turns a pack's display hint into a
  // crash on the surface that renders it. `hasOwn` is the whole fix.
  if (typeof name === "string" && Object.hasOwn(ALLOWED_PACK_ICONS, name)) {
    const icon = ALLOWED_PACK_ICONS[name];
    if (icon) return icon;
  }
  return DEFAULT_PACK_ICON;
}
