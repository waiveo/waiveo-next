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
 * humanizing the reference's last segment when the catalog has no entry (the same
 * graceful fallback the renderer applies to unresolved refs). */
export function resolveTitle(messages: Record<string, string>, ref: string): string {
  return messages[ref] ?? humanizeMsgRef(ref);
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

/** The pack collection a list-detail page's rows come from: the root key of its
 * `list.source`, but only when it names a manifest-declared collection (so a
 * source that is not a collection loads nothing rather than 404ing). Only
 * list-detail binds a collection this wave; other page types return null. */
export function primaryCollection(doc: unknown, collectionNames: Set<string>): string | null {
  if (!doc || typeof doc !== "object") return null;
  const d = doc as { pageType?: unknown; list?: { source?: unknown } };
  if (d.pageType !== "list-detail") return null;
  const key = sourceRootKey(d.list?.source);
  return key && collectionNames.has(key) ? key : null;
}
