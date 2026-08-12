// The Extensions nav model: the installed packs' pages, resolved for the shell.
//
// Lists installed packs over the api/1 client and, for each, resolves its display
// name and its ui.pages[] titles from the pack's own locale catalog (MAN-060/111).
// A pack with no pages contributes nothing. Any failure (packs unreachable, a
// catalog absent) degrades to an empty nav rather than breaking the shell — the
// Extensions section simply does not appear.

import { useEffect, useMemo, useState } from "react";
import { collectPages, createApi, encodePackPagePath, type Pack, type WaiveoApi } from "@/api";
import { loadPackCatalog, resolveTitle } from "./catalog";

/** One installed page as a nav destination: its `/p/{pack}/{path}` route and its
 * catalog-resolved title. */
export interface PackNavPage {
  to: string;
  label: string;
}

/** One installed pack's nav group: the resolved pack title, its optional
 * manifest-declared icon name (resolved to a glyph by the shell — undefined when
 * absent or not a string; an unknown name degrades to the default, never broken),
 * and its pages. */
export interface PackNavGroup {
  packId: string;
  title: string;
  icon?: string;
  pages: PackNavPage[];
}

/** The DOM event the Extensions console fires after an install, an update or an
 * uninstall lands.
 *
 * It exists so that "a pack installs LIVE" is true of the console the operator is
 * looking at, not just of the box. The nav is resolved once per client identity;
 * without a signal, a pack installed from /extensions would not appear in the
 * rail until the page was reloaded — and a management console that requires a
 * reload to show its own work has, from where the operator sits, required a
 * restart.
 *
 * A window event rather than a shared store or a prop chain because the producer
 * (a route) and the consumer (the shell around it) are on opposite sides of the
 * router outlet, and threading a refresh callback through AppShell would put
 * console-page state in the shell's props for one page's benefit. */
export const PACKS_CHANGED_EVENT = "waiveo:packs-changed";

/** Announce that the installed-pack set has changed, so every subscriber
 * (the Extensions nav) re-resolves. A no-op outside a browser. */
export function notifyPacksChanged(): void {
  if (typeof window === "undefined") return;
  window.dispatchEvent(new Event(PACKS_CHANGED_EVENT));
}

/** Resolve the Extensions nav groups for the shell. Re-runs when the api client
 * identity changes (once, in practice) and whenever PACKS_CHANGED_EVENT fires. */
export function useInstalledPackNav(api?: WaiveoApi): PackNavGroup[] {
  const client = useMemo(() => api ?? createApi(), [api]);
  const [groups, setGroups] = useState<PackNavGroup[]>([]);
  // Bumped by PACKS_CHANGED_EVENT; re-runs the resolve effect below.
  const [changeToken, setChangeToken] = useState(0);

  useEffect(() => {
    if (typeof window === "undefined") return;
    const onChanged = () => setChangeToken((n) => n + 1);
    window.addEventListener(PACKS_CHANGED_EVENT, onChanged);
    return () => window.removeEventListener(PACKS_CHANGED_EVENT, onChanged);
  }, []);

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const packs = await collectPages<Pack>((cursor) => client.packs.list({ cursor }));
        const built = await Promise.all(
          // A DISABLED pack contributes no destinations (MKT-097). The server
          // refuses its pages too — the rail is the second half of "not a
          // navigable destination", never the whole of it, because a nav that
          // merely omits a pack whose route still answers has hidden it rather
          // than withdrawn it.
          packs.filter((p) => p.enabled).map(async (pack): Promise<PackNavGroup> => {
            const messages = await loadPackCatalog(client.packs, pack.id);
            const pages = (pack.manifest.ui?.pages ?? []).flatMap((p) => {
              // The manifest path is untrusted (the engine does not constrain its
              // grammar): a `..`/`.`/empty segment is dropped rather than emitted as a
              // nav landmark, because react-router's `resolvePath` would fold it into
              // an arbitrary OTHER console route — escaping the pack's own
              // `/p/{pack}/{path}` confinement and the Extensions demarcation. A safe
              // path is percent-encoded per-segment so it can never do so.
              const safe = encodePackPagePath(p.path);
              if (safe === null) return [];
              return [{ to: `/p/${pack.id}/${safe}`, label: resolveTitle(messages, p.titleMsg) }];
            });
            // The icon is untrusted manifest data — keep it only when it is a
            // string; the shell's resolvePackIcon maps a known name to a glyph and
            // an unknown (or absent) one to the default extension glyph.
            const icon = typeof pack.manifest.icon === "string" ? pack.manifest.icon : undefined;
            return {
              packId: pack.id,
              title: resolveTitle(messages, pack.manifest.displayName),
              ...(icon !== undefined ? { icon } : {}),
              pages,
            };
          }),
        );
        if (!cancelled) setGroups(built.filter((g) => g.pages.length > 0));
      } catch {
        // No packs / unreachable: keep the (already-empty) nav, returning the same
        // reference so React bails out of a redundant re-render.
        if (!cancelled) setGroups((prev) => (prev.length === 0 ? prev : []));
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [client, changeToken]);

  return groups;
}
