// Who can start a scan, and where an operator goes to do it.
//
// Discovery splits in two on purpose (discovery spec, and the Discovery pack's
// own manifest): the enumeration ENGINE is core on the relay, because only a
// relay can see a LAN, while the POLICY — which subnets may be actively
// scanned, on what schedule, how hard, and *whether to scan right now* — belongs
// to an extension. Passive discovery runs always and originates nothing;
// everything that probes waits for a scan an extension asks for.
//
// The consequence for the core Devices page is that it genuinely cannot start a
// sweep, and for a while it said exactly that: "there is no scan to start from
// here." That sentence was true about the page and false about the deployment.
// A scan CAN be started — `waiveo/discovery` declares a `scan-now` action gated
// on `discovery.scan`, and invoking it drives a real ARP sweep and port scan
// across the configured subnets. An operator reading the page concluded the
// platform could not scan on demand, and the one operational control in
// Discovery sat three clicks away under "Extensions" with nothing pointing at
// it.
//
// So this module answers the question the page should have been answering all
// along: not "can I scan" (core can't) but "who here can, and where is it".
//
// It matches on CAPABILITY, never on action name or pack id. `discovery.scan`
// is the platform's word for the privilege; `scan-now` is one pack's word for
// one action. Matching the capability means a different discovery extension —
// or a second one — is found without core learning its name, which is the whole
// point of the ownership split. Hardcoding `waiveo/discovery` here would put a
// specific pack's identity into core code that is supposed to outlive it.

import type { Pack } from "@/api";
import { packHealth } from "@/routes/extensions/pack-console";

/** The capability a manifest action declares when invoking it scans a network.
 * MAN-070's `capabilityScope`, matched exactly — a prefix match would let
 * `discovery.scanner-config` masquerade as the act of scanning. */
export const SCAN_CAPABILITY = "discovery.scan";

export interface ScanOwner {
  /** The pack id, `publisher/name`. */
  id: string;
  /** The pack's display name as the registry returned it. May be a `msg:` ref
   * the caller resolves; it is passed through untouched rather than guessed at. */
  displayName: string;
  /** Where to send an operator: the pack's first reachable page, already
   * confined to its own `/p/{publisher}/{name}/` namespace by packHealth.
   *
   * Null when the pack declares a scan action but no page this console can open
   * — a disabled pack (pages withdrawn), or one whose declared paths all escape
   * its namespace. That is a REAL state and must not be flattened into "no
   * owner": the deployment does have something that scans, and an operator whose
   * only lead is a card is better served by being told so than by being told
   * nothing can scan. */
  href: string | null;
  /** True when a page exists but is withdrawn because the pack is disabled —
   * the one case with an obvious operator remedy, and worth saying out loud
   * rather than rendering as a dead end. */
  disabled: boolean;
}

/** Which installed packs can start a scan.
 *
 * Pure: it reads manifests the registry already returned and issues nothing.
 * Returns them in registry order; a deployment with two discovery extensions
 * gets both rather than an arbitrary winner, because picking one silently would
 * hide a configuration the operator built on purpose.
 */
export function scanOwners(packs: Pack[]): ScanOwner[] {
  const owners: ScanOwner[] = [];
  for (const pack of packs) {
    // `?? []` and not `!`: the member is optional on the console's manifest
    // slice and an older box omits it entirely, which must read as "declares no
    // actions", never as a crash on the Devices page — a page that fails to
    // render because a pack's manifest is a shape older than this build is a far
    // worse outcome than a missing link.
    const actions = pack.manifest.actions ?? [];
    if (!actions.some((a) => a.capabilityScope === SCAN_CAPABILITY)) continue;
    const health = packHealth(pack);
    const first = health.pages[0];
    owners.push({
      id: pack.id,
      displayName: pack.manifest.displayName,
      // A withdrawn page is not a destination. packHealth already refuses to
      // link a disabled pack's pages — the server answers 404 while a pack is
      // off — so offering the link anyway would be a click that fails rather
      // than one that was never offered.
      href: health.pagesWithdrawn || first === undefined ? null : first.href,
      disabled: health.pagesWithdrawn,
    });
  }
  return owners;
}
