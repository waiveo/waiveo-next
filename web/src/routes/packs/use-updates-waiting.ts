// How many installed packs have a newer version waiting — the rail's badge.
//
// # Why this is a second fetch rather than the Extensions page's
//
// The Extensions page already reads availability for every auto-tracked pack,
// in more detail than a badge needs. Reusing that would be less work over the
// wire and would produce a badge that is EMPTY until someone visits the page it
// is meant to send them to, which is the whole failure the register books: "an
// operator is never told a pack has a newer version; they must learn it out of
// band." A count that only appears once you have already looked is not a
// notification.
//
// So the rail asks for itself. The cost is bounded by the same fact the console
// relies on elsewhere — a box holds a handful of packs — and only auto-tracked
// ones are asked at all, because a direct install has no channel to resolve
// through (MKT-094a) and the endpoint refuses it.
//
// # It never blocks, and it never guesses
//
// The count starts at zero and stays there until answers arrive, so the shell
// renders immediately whatever the registry is doing. A pack whose lookup FAILS
// is not counted — a source that cannot be reached is not evidence that an
// update is waiting, and a badge is an assertion. That asymmetry is deliberate
// in this direction only: under-reporting sends nobody anywhere, while
// over-reporting sends an operator to a page that then says everything is fine.

import { useEffect, useMemo, useState } from "react";
import { collectPages, createApi, type Pack, type WaiveoApi } from "@/api";
import { PACKS_CHANGED_EVENT } from "./use-installed-packs";

/** What the rail needs to know about pending pack updates. */
export interface UpdatesWaiting {
  /** Packs whose pinned channel offers a newer version. */
  count: number;
  /** Packs whose RUNNING version has been withdrawn by its publisher.
   *
   * Counted apart from `count`, exactly as the Extensions page counts it apart:
   * a withdrawn version is a fault, not an upgrade, and summing the two would
   * file the more urgent state under the less urgent word. */
  withdrawn: number;
}

const NONE: UpdatesWaiting = { count: 0, withdrawn: 0 };

/** Count the packs with something waiting. Re-runs when the api client identity
 * changes (once, in practice) and whenever PACKS_CHANGED_EVENT fires — so
 * taking an update from the Extensions page clears the badge without a
 * reload. */
export function useUpdatesWaiting(api?: WaiveoApi): UpdatesWaiting {
  const client = useMemo(() => api ?? createApi(), [api]);
  const [waiting, setWaiting] = useState<UpdatesWaiting>(NONE);
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
        const reports = await Promise.all(
          packs.map(async (pack) => {
            try {
              return await client.packs.updateAvailability(pack.id);
            } catch {
              // Includes the ordinary refusal for a pack with no pinned channel
              // (MKT-094a), which is not a failure worth reporting anywhere —
              // it is what "not auto-tracked" looks like from here.
              return null;
            }
          }),
        );
        if (cancelled) return;
        setWaiting({
          count: reports.filter((r) => r?.action === "updated").length,
          withdrawn: reports.filter((r) => r?.action === "reverted").length,
        });
      } catch {
        // The pack list itself is unreachable. The rail keeps whatever it last
        // knew rather than asserting zero, for the same reason a failed
        // per-pack lookup is not counted: neither is evidence.
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [client, changeToken]);

  return waiting;
}
