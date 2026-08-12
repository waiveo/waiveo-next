// Installed-pack locale + data-source helpers, shared by the Extensions nav and
// the pack page route.
//
// A pack ships its display strings as a locale catalog per locale (MAN-110): a
// flat `{ key: text }` map whose keys are BARE (`page.menuItems.title`), while the
// grammar addresses them by a `msg:` reference (`msg:page.menuItems.title`). These
// helpers bridge the two — prefixing the catalog into the form the ui-schema
// renderer's message resolver reads, layering the en fallback under the active
// locale (MAN-111: every msg: ultimately resolves against en), and resolving a
// single title for the nav. They also read a page document's data source to name
// the pack collection its rows come from, so the route can load them.

import type { PacksModule } from "@/api";

/** The viewer's active locale (BCP-47), or `en` when none is exposed. */
export function activeLocale(): string {
  if (typeof navigator !== "undefined" && navigator.language) return navigator.language;
  return "en";
}

/** Prefix a bare pack locale catalog with the `msg:` the ui-schema grammar
 * addresses its entries by, layering the active-locale catalog OVER the en base so
 * a key present only in en still resolves (MAN-111). The result is the exact
 * `Record<string, string>` shape PageRenderer's `messages` prop expects. */
export function toRendererMessages(
  en: Record<string, string>,
  active?: Record<string, string> | null,
): Record<string, string> {
  const merged = { ...en, ...(active ?? {}) };
  const out: Record<string, string> = {};
  for (const [key, text] of Object.entries(merged)) out[`msg:${key}`] = text;
  return out;
}

/** Resolve a `msg:` reference to display text against an already-prefixed catalog,
 * humanizing the reference's last segment when the catalog has no string entry (the
 * same graceful fallback the renderer applies to unresolved refs).
 *
 * The catalog is UNTRUSTED pack data: install validates a locale catalog only as
 * well-formed JSON, never that its values are strings, so a pack can ship
 * messages/en.json with a NON-string value (an object/array/number) under a
 * manifest-referenced key. toRendererMessages carries that value through verbatim,
 * so a bare `messages[ref] ?? …` would return the non-string — which then flows to
 * `group.title`/a header and crashes the whole console shell on React's "Objects
 * are not valid as a React child" (there is no ErrorBoundary; AppShell wraps every
 * route). Take the entry ONLY when it is a string; anything else — a non-string OWN
 * value, or an inherited Object.prototype member reached by a `msg:`-prefix-dropped
 * ref — humanizes. This mirrors the renderer's makeMessageResolver guard exactly. */
export function resolveTitle(messages: Record<string, string>, ref: string): string {
  const value = messages[ref];
  return typeof value === "string" ? value : humanizeMsgRef(ref);
}

/** Humanize a `msg:` reference — its last dotted segment, de-camel/kebab/snaked
 * and capitalized — for when no catalog entry exists. */
function humanizeMsgRef(ref: string): string {
  const body = ref.startsWith("msg:") ? ref.slice(4) : ref;
  const last = body.split(".").pop() ?? body;
  const spaced = last
    .replace(/[-_]/g, " ")
    .replace(/([a-z0-9])([A-Z])/g, "$1 $2")
    .trim();
  return spaced ? spaced.charAt(0).toUpperCase() + spaced.slice(1) : ref;
}

/** Load a pack's locale catalog as renderer-ready messages: the guaranteed en
 * base (MAN-111) overlaid by the active locale when the pack ships one. en's
 * absence or a missing active locale never throws — resolution degrades to
 * whatever is present, then to humanized references. */
export async function loadPackCatalog(
  packs: PacksModule,
  packId: string,
): Promise<Record<string, string>> {
  const en = await packs.messages(packId, "en").catch(() => ({}) as Record<string, string>);
  const loc = activeLocale();
  let active: Record<string, string> | null = null;
  // en IS the active locale's base — only fetch a distinct one, and treat its
  // absence (a 404) as "no override", never a failure (the en base still resolves).
  if (loc && !/^en(-|$)/i.test(loc)) {
    active = await packs.messages(packId, loc).catch(() => null);
    if (!active && loc.includes("-")) {
      active = await packs.messages(packId, loc.split("-")[0]).catch(() => null);
    }
  }
  return toRendererMessages(en, active);
}

/** The root key of a page data source binding (`menu_items`, `menu_items[...]`,
 * `{paginated, path}`) — the first path segment before any predicate/index/dot. */
function sourceRootKey(source: unknown): string | null {
  const fromString = (s: string): string | null => {
    const seg = s.split(/[./[]/)[0]?.trim();
    return seg ? seg : null;
  };
  if (typeof source === "string") return fromString(source);
  if (source && typeof source === "object") {
    const path = (source as { path?: unknown }).path;
    if (typeof path === "string") return fromString(path);
  }
  return null;
}

/** The pack collection a page's rows come from, but only when it names a
 * manifest-declared collection (so a source that is not a collection loads
 * nothing rather than 404ing).
 *
 * Two page types bind one: a `list-detail` through `list.source` (the rows it
 * pages through) and a `settings-form` through its top-level `source` (the
 * single record it edits, UIS-005/UIS-030). The install gate guarantees a
 * settings-form's source names a declared SINGLETON collection (MAN-064), so
 * the collection this returns for one always holds at most one row.
 *
 * `dashboard` and `wizard` still return null — neither binds a page-wide
 * collection, and neither has a persistence path yet. */
export function primaryCollection(doc: unknown, collectionNames: Set<string>): string | null {
  if (!doc || typeof doc !== "object") return null;
  const d = doc as { pageType?: unknown; list?: { source?: unknown }; source?: unknown };
  const source = d.pageType === "list-detail" ? d.list?.source : d.pageType === "settings-form" ? d.source : null;
  if (source == null) return null;
  const key = sourceRootKey(source);
  return key && collectionNames.has(key) ? key : null;
}

/** Whether a page document is a settings-form — the page type that binds ONE
 * record rather than a list, so the route feeds the renderer a single object
 * and its save creates the record when it does not exist yet. */
export function isSettingsForm(doc: unknown): boolean {
  return !!doc && typeof doc === "object" && (doc as { pageType?: unknown }).pageType === "settings-form";
}
