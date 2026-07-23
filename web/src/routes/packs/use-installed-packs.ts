// The Extensions nav model: the installed packs' pages, resolved for the shell.
//
// Lists installed packs over the api/1 client and, for each, resolves its display
// name and its ui.pages[] titles from the pack's own locale catalog (MAN-060/111).
// A pack with no pages contributes nothing. Any failure (packs unreachable, a
// catalog absent) degrades to an empty nav rather than breaking the shell — the
// Extensions section simply does not appear.

import { useEffect, useMemo, useState } from "react";
import { collectPages, createApi, type Pack, type WaiveoApi } from "@/api";
import { loadPackCatalog, resolveTitle } from "./catalog";

/** One installed page as a nav destination: its `/p/{pack}/{path}` route and its
 * catalog-resolved title. */
export interface PackNavPage {
  to: string;
  label: string;
}

/** One installed pack's nav group: the resolved pack title and its pages. */
export interface PackNavGroup {
  packId: string;
  title: string;
  pages: PackNavPage[];
}

/** Resolve the Extensions nav groups for the shell. Re-runs when the api client
 * identity changes (once, in practice). */
export function useInstalledPackNav(api?: WaiveoApi): PackNavGroup[] {
  const client = useMemo(() => api ?? createApi(), [api]);
  const [groups, setGroups] = useState<PackNavGroup[]>([]);

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const packs = await collectPages<Pack>((cursor) => client.packs.list({ cursor }));
        const built = await Promise.all(
          packs.map(async (pack): Promise<PackNavGroup> => {
            const messages = await loadPackCatalog(client.packs, pack.id);
            const pages = (pack.manifest.ui?.pages ?? []).map((p) => ({
              to: `/p/${pack.id}/${p.path}`,
              label: resolveTitle(messages, p.titleMsg),
            }));
            return {
              packId: pack.id,
              title: resolveTitle(messages, pack.manifest.displayName),
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
  }, [client]);

  return groups;
}
