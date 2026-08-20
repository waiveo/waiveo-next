import { describe, it, expect } from "vitest";
import { scanOwners, SCAN_CAPABILITY } from "./scan-owner";
import type { Pack } from "@/api";

// The question this module answers is "who can scan, and where do I go" — and
// every test below is really about a way of getting that WRONG that would ship
// green: naming a pack core should not know, linking a page the server will
// refuse, or reporting "nothing can scan" on a manifest shape this build did
// not expect.

function pack(over: Partial<Pack> = {}, manifest: Partial<Pack["manifest"]> = {}): Pack {
  return {
    id: "waiveo/discovery",
    revision: 1,
    version: "1.0.0",
    data_model_version: 1,
    created_at: 0,
    updated_at: 0,
    enabled: true,
    manifest: {
      id: "waiveo/discovery",
      version: "1.0.0",
      displayName: "Discovery",
      ui: { pages: [{ path: "settings", pageType: "settings-form", titleMsg: "msg:page.settings.title" }] },
      dataModel: { version: 1, collections: [] },
      actions: [{ name: "scan-now", capabilityScope: SCAN_CAPABILITY }],
      ...manifest,
    },
    ...over,
  } as Pack;
}

describe("scanOwners — who can start a scan", () => {
  it("finds the pack that declares a scan action, and links its page", () => {
    const [owner, ...rest] = scanOwners([pack()]);
    expect(rest).toHaveLength(0);
    expect(owner.id).toBe("waiveo/discovery");
    expect(owner.displayName).toBe("Discovery");
    expect(owner.href).toBe("/p/waiveo/discovery/settings");
    expect(owner.disabled).toBe(false);
  });

  it("matches on the CAPABILITY, not on the pack id or the action name", () => {
    // The whole point of the ownership split: a different discovery extension
    // must be found without core learning its name. A test that used the real
    // pack id would pass against an implementation that hardcoded it.
    const other = pack(
      { id: "acme/sweeper" },
      {
        id: "acme/sweeper",
        displayName: "Acme Sweeper",
        actions: [{ name: "look-around-now", capabilityScope: SCAN_CAPABILITY }],
      },
    );
    const owners = scanOwners([other]);
    expect(owners).toHaveLength(1);
    expect(owners[0].id).toBe("acme/sweeper");
    expect(owners[0].href).toBe("/p/acme/sweeper/settings");
  });

  it("ignores a pack whose actions scope something else", () => {
    const owners = scanOwners([
      pack({ id: "acme/other" }, { id: "acme/other", actions: [{ name: "run", capabilityScope: "storage.write" }] }),
    ]);
    expect(owners).toEqual([]);
  });

  it("does not let a longer capability masquerade as the act of scanning", () => {
    // A prefix match would accept this. `discovery.scanner-config` is a
    // privilege to CONFIGURE scanning, not to perform it.
    const owners = scanOwners([
      pack({ id: "acme/cfg" }, { id: "acme/cfg", actions: [{ name: "x", capabilityScope: "discovery.scanner-config" }] }),
    ]);
    expect(owners).toEqual([]);
  });

  it("tolerates a manifest with no actions member at all", () => {
    // An older box omits it. Reading it as a crash would take down the whole
    // Devices page over a pack manifest shape — far worse than a missing link.
    const legacy = pack({ id: "acme/legacy" }, { id: "acme/legacy" });
    delete (legacy.manifest as { actions?: unknown }).actions;
    expect(() => scanOwners([legacy])).not.toThrow();
    expect(scanOwners([legacy])).toEqual([]);
  });

  it("reports a DISABLED scanner as an owner with no link, not as absent", () => {
    // The server answers 404 for a disabled pack's pages, so a link here would
    // be a click that fails. But dropping the pack entirely would tell an
    // operator nothing can scan when the remedy is one toggle away.
    const owners = scanOwners([pack({ enabled: false })]);
    expect(owners).toHaveLength(1);
    expect(owners[0].disabled).toBe(true);
    expect(owners[0].href).toBeNull();
  });

  it("reports a scanner whose pages all escape its namespace as unlinkable", () => {
    const owners = scanOwners([
      pack({}, { ui: { pages: [{ path: "../escape", pageType: "settings-form", titleMsg: "m" }] } }),
    ]);
    expect(owners).toHaveLength(1);
    expect(owners[0].href).toBeNull();
    expect(owners[0].disabled).toBe(false);
  });

  it("returns every scanner, not an arbitrary winner", () => {
    const owners = scanOwners([
      pack(),
      pack({ id: "acme/sweeper" }, { id: "acme/sweeper", displayName: "Acme Sweeper" }),
    ]);
    expect(owners.map((o) => o.id)).toEqual(["waiveo/discovery", "acme/sweeper"]);
  });
});
