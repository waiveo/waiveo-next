// What teaches this deployment to RECOGNIZE a device.
//
// Discovery enumerates the network pattern-blind: it finds every host on the
// LAN and, by design, knows what none of them is. Recognising a kind of device
// is an extension's contribution — core deliberately declares no pattern of its
// own (cmd/waiveo-relay's empty builtinSSDP, guarded by coreisdeviceblind_test,
// owner 2026-08-17) — so a deployment with no such pack installed finds
// everything and classifies almost nothing.
//
// That is the state of the dev box: one installed pack, `waiveo/discovery`,
// which owns scan POLICY and declares no device patterns at all. Fifty of
// sixty-three devices read `unclassified`, and the page offered no explanation —
// which invites the reading that classification is broken, when it is absent by
// configuration and one install away from working.
//
// So this answers the question the device table provokes: not "what is this
// device" but "is anything here in a position to say?"

import type { Pack } from "@/api";

export interface Recognizer {
  id: string;
  displayName: string;
  /** The device classes this pack claims to recognize, de-duplicated and
   * sorted. Empty is impossible — a pack with no declarations is not a
   * recognizer and never reaches this list. */
  classes: string[];
  /** True when the pack is installed but DISABLED: its declarations reach no
   * relay, so it recognizes nothing today. Named rather than dropped, because
   * "nothing can classify" and "the thing that classifies is switched off" have
   * different remedies and only one of them is an install. */
  disabled: boolean;
}

/** Which installed packs teach device recognition.
 *
 * Pure: reads manifests the registry already returned and issues nothing. A
 * pack declaring a `devices[]` entry with no `match` is still counted — it has
 * claimed a class, and whether its patterns are well-formed is the relay's
 * judgement to make against signed state, not this console's to pre-empt. */
export function deviceRecognizers(packs: Pack[]): Recognizer[] {
  const out: Recognizer[] = [];
  for (const pack of packs) {
    // `?? []` because the member is optional and absent on most packs — the
    // common case, not an error, and never a reason to fail the page.
    const declared = pack.manifest.devices ?? [];
    if (declared.length === 0) continue;
    const classes = [...new Set(declared.map((d) => d.deviceClass).filter(Boolean))].sort();
    out.push({
      id: pack.id,
      displayName: pack.manifest.displayName,
      classes,
      // `=== false` only, never `!pack.enabled`: an older box that omits the
      // member entirely would otherwise read as disabled, which is the failure
      // the api/1 note on `Pack.enabled` warns about — absence is not a state.
      disabled: pack.enabled === false,
    });
  }
  return out;
}
