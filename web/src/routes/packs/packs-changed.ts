// The console's "the installed-pack set changed" signal.
//
// This module is what SURVIVED the pack-contributed rail section (owner,
// 2026-08-19): the section is gone, but the signal it was invented for is still
// load-bearing, and it now serves the rail's updates badge
// (./use-updates-waiting.ts). It lives on its own rather than inside a nav model
// that no longer exists — a shared event that only one half of the console
// raises and the other half hears is not a detail of either.

/** The DOM event the Extensions console fires after an install, an update or an
 * uninstall lands.
 *
 * It exists so that a pack lifecycle action is reflected in the console the
 * operator is looking at, not just on the box. The badge counting packs that
 * need attention is resolved once per client identity; without a signal, taking
 * an update from /extensions would leave the rail claiming that update is still
 * waiting until the page was reloaded — and a management console that requires a
 * reload to show its own work has, from where the operator sits, required a
 * restart.
 *
 * A window event rather than a shared store or a prop chain because the producer
 * (a route) and the consumer (the shell around it) are on opposite sides of the
 * router outlet, and threading a refresh callback through AppShell would put
 * console-page state in the shell's props for one page's benefit. */
export const PACKS_CHANGED_EVENT = "waiveo:packs-changed";

/** Announce that the installed-pack set has changed, so every subscriber
 * re-resolves. A no-op outside a browser. */
export function notifyPacksChanged(): void {
  if (typeof window === "undefined") return;
  window.dispatchEvent(new Event(PACKS_CHANGED_EVENT));
}
