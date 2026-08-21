import { describe, it, expect } from "vitest";
import { deviceRecognizers } from "./recognizers";
import type { Pack } from "@/api";

// Discovery finds every host and, by design, knows what none of them is.
// Recognising a KIND is an extension's contribution — core declares no pattern
// of its own on purpose — so "nothing installed recognises anything" is a real,
// consequential state and not an error. Fifty of sixty-three rows on the dev box
// read `unclassified` for exactly this reason.

function pack(over: Partial<Pack> = {}, manifest: Record<string, unknown> = {}): Pack {
  return {
    id: "waiveo/roku",
    revision: 1,
    version: "1.0.0",
    data_model_version: 1,
    created_at: 0,
    updated_at: 0,
    enabled: true,
    manifest: {
      id: "waiveo/roku",
      version: "1.0.0",
      displayName: "Roku",
      ui: { pages: [] },
      dataModel: { version: 1, collections: [] },
      devices: [{ deviceClass: "media-player", match: [{ ssdp: "roku:ecp" }] }],
      ...manifest,
    },
    ...over,
  } as Pack;
}

describe("deviceRecognizers", () => {
  it("names the pack and the classes it claims", () => {
    const [r] = deviceRecognizers([pack()]);
    expect(r.id).toBe("waiveo/roku");
    expect(r.classes).toEqual(["media-player"]);
    expect(r.disabled).toBe(false);
  });

  it("returns nothing for a pack that declares no device patterns", () => {
    // waiveo/discovery is exactly this: it owns scan POLICY and teaches no
    // recognition. It must not be counted, or the page would claim the fleet
    // can be identified when nothing can identify it.
    const policyOnly = pack({ id: "waiveo/discovery" }, { id: "waiveo/discovery", devices: [] });
    expect(deviceRecognizers([policyOnly])).toEqual([]);
  });

  it("tolerates a manifest with no devices member at all", () => {
    // The COMMON case — most packs declare none, and an older box omits it.
    // Throwing here would take down the Devices page over a manifest shape.
    const p = pack({ id: "acme/thing" }, { id: "acme/thing" });
    delete (p.manifest as { devices?: unknown }).devices;
    expect(() => deviceRecognizers([p])).not.toThrow();
    expect(deviceRecognizers([p])).toEqual([]);
  });

  it("reports a DISABLED recognizer as present-but-inert, not as absent", () => {
    // "Nothing can classify" and "the thing that classifies is switched off"
    // have different remedies, and only one of them is an install.
    const [r] = deviceRecognizers([pack({ enabled: false })]);
    expect(r.disabled).toBe(true);
    expect(r.classes).toEqual(["media-player"]);
  });

  it("does not read a MISSING enabled member as disabled", () => {
    // absence is not a state — the api/1 note on Pack.enabled.
    const p = pack();
    delete (p as { enabled?: boolean }).enabled;
    expect(deviceRecognizers([p])[0].disabled).toBe(false);
  });

  it("de-duplicates and sorts the classes a pack claims", () => {
    const multi = pack({}, {
      devices: [
        { deviceClass: "speaker", match: [] },
        { deviceClass: "media-player", match: [] },
        { deviceClass: "media-player", match: [] },
      ],
    });
    expect(deviceRecognizers([multi])[0].classes).toEqual(["media-player", "speaker"]);
  });
});
