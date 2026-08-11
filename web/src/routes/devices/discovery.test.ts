import { describe, it, expect } from "vitest";
import { describeDiscovery, MISSING_DEVICE_REASONS } from "./discovery";
import type { Device, RelayHealth } from "@/api";

// The classifier, without a DOM. Its whole value is that seven situations stay
// SEVEN — so most of these tests assert a `kind` and then assert that the
// sentence belonging to a NEIGHBOURING kind is absent. A classifier that
// returned one hedged paragraph covering every case would satisfy any
// positive-only assertion while restoring the exact defect it replaced.

const RELAY = "relay-0f1e2d3c4b5a69788796a5b4c3d2e1f0";

function device(over: Partial<Device> = {}): Device {
  return {
    id: "01J8ZDEV1CE00000000000000A",
    external_id: null,
    relay_id: RELAY,
    device_class: "media-player",
    name: "Hanger TV",
    scope_node: "01J8Z0ROOT0000000000000000",
    labels: {},
    adopted: false,
    ...over,
  };
}

const relays: RelayHealth[] = [{ relay_id: RELAY, address: "192.0.2.9:7421", screen_count: 1 }];

describe("describeDiscovery — the seven states stay apart", () => {
  it("reports the read failure ahead of everything, because nothing below it is known", () => {
    const d = describeDiscovery({
      devices: [],
      devicesError: "No relay is connected.",
      relays: [],
      blind: null,
    });
    expect(d.kind).toBe("unreadable");
    expect(d.detail).toBe("No relay is connected.");
  });

  it("is loading only while devices is null", () => {
    const d = describeDiscovery({ devices: null, devicesError: null, relays, blind: null });
    expect(d.kind).toBe("loading");
  });

  it("blames the missing relay, not the network, when nothing is connected", () => {
    const d = describeDiscovery({ devices: [], devicesError: null, relays: [], blind: null });
    expect(d.kind).toBe("no-relay");
    expect(d.relayCount).toBe(0);
    expect(d.headline).toMatch(/no relay is connected/);
    expect(d.detail).not.toMatch(/not on a network/);
  });

  it("blames the network, not the relay, when a relay is connected and found nothing", () => {
    const d = describeDiscovery({ devices: [], devicesError: null, relays, blind: null });
    expect(d.kind).toBe("searching");
    expect(d.headline).toMatch(/Discovery is running on 1 relay —/);
    expect(d.detail).toMatch(/only ever sees its own LAN/);
    expect(d.headline).not.toMatch(/not running/);
  });

  it("pluralizes the relay count rather than saying '2 relay'", () => {
    const two = [...relays, { relay_id: "relay-b", address: "192.0.2.10:7421", screen_count: 0 }];
    const d = describeDiscovery({ devices: [], devicesError: null, relays: two, blind: null });
    expect(d.headline).toMatch(/on 2 relays/);
  });

  it("says it does not know when health was refused, and claims neither running nor not", () => {
    const d = describeDiscovery({
      devices: [],
      devicesError: null,
      relays: null,
      blind: "forbidden",
    });
    expect(d.kind).toBe("blind");
    // relayCount stays NULL — never 0. Zero is a claim; this is an absence.
    expect(d.relayCount).toBeNull();
    expect(d.detail).toMatch(/Only the workspace owner can read relay health/);
    expect(d.headline).not.toMatch(/Discovery is running/);
    expect(d.headline).not.toMatch(/no relay is connected/);
    expect(d.caveat).not.toBeNull();
  });

  it("distinguishes a refusal from an unreachable health endpoint", () => {
    const d = describeDiscovery({
      devices: [],
      devicesError: null,
      relays: null,
      blind: "unreachable",
    });
    expect(d.kind).toBe("blind");
    expect(d.detail).toMatch(/could not be read/);
    expect(d.detail).not.toMatch(/workspace owner/);
  });

  it("calls a fully-adopted fleet a steady state, not an empty result", () => {
    const d = describeDiscovery({
      devices: [device({ adopted: true }), device({ id: "b", adopted: true })],
      devicesError: null,
      relays,
      blind: null,
    });
    expect(d.kind).toBe("all-adopted");
    expect(d.found).toBe(2);
    expect(d.adopted).toBe(2);
    expect(d.pending).toBe(0);
    expect(d.detail).toMatch(/steady state, not an empty result/);
  });

  it("counts the decisions waiting when something is unadopted", () => {
    const d = describeDiscovery({
      devices: [device({ adopted: true }), device({ id: "b" })],
      devicesError: null,
      relays,
      blind: null,
    });
    expect(d.kind).toBe("candidates");
    expect(d.pending).toBe(1);
    expect(d.headline).toMatch(/^1 device found and waiting/);
    expect(d.caveat).toBeNull();
  });

  it("still classifies found devices when health is unreadable, but caveats the tense", () => {
    // A device on the page was reported by SOME relay, so discovery
    // demonstrably ran. Whether it is running now is a different claim, and the
    // caveat is where that distinction lives.
    const d = describeDiscovery({
      devices: [device()],
      devicesError: null,
      relays: null,
      blind: "forbidden",
    });
    expect(d.kind).toBe("candidates");
    expect(d.relayCount).toBeNull();
    expect(d.caveat).toMatch(/whether it is running NOW is not known here/);
  });
});

describe("MISSING_DEVICE_REASONS", () => {
  it("names the five real paths a device goes missing by", () => {
    const titles = MISSING_DEVICE_REASONS.map((r) => r.title);
    expect(titles).toHaveLength(5);
    // Each of these is a real code path, not general networking advice — the
    // list is worth pinning so a future edit does not quietly replace one with
    // "check the cables".
    expect(titles.join(" ")).toMatch(/relay is not connected/);
    expect(titles.join(" ")).toMatch(/not on a relay's own network/);
    expect(titles.join(" ")).toMatch(/ignored/);
    expect(titles.join(" ")).toMatch(/Another relay already owns it/);
    expect(titles.join(" ")).toMatch(/whole report was refused/);
  });
});

describe("describeDiscovery — no relay DOMINATES, whatever the list length (HV-19)", () => {
  // The regression this pins shipped and was found on the lab box. `no-relay`
  // used to be nested inside the `found === 0` branch, so it was unreachable on
  // any box that had ever discovered anything — every real deployment. A total
  // relay outage fell through to `all-adopted` and headlined "Everything found
  // is adopted… This is the steady state" while the counter beside it read
  // RELAYS REPORTING 0.
  //
  // Every pre-existing case varied relayCount against an EMPTY list, which is why
  // a tested state could still be unreachable. When a classifier has an ordering,
  // the cross product is the thing to cover, not one axis.
  it("reports no-relay even when devices were previously discovered and adopted", () => {
    const d = describeDiscovery({
      devices: [device({ adopted: true }), device({ id: "b", adopted: true })],
      devicesError: null,
      relays: [],
      blind: null,
    });
    expect(d.kind).toBe("no-relay");
    expect(d.relayCount).toBe(0);
    // Emphatically NOT the reassuring one.
    expect(d.kind).not.toBe("all-adopted");
    expect(d.headline).not.toMatch(/steady state|no decisions waiting/i);
    expect(d.headline).toBe("No relay is connected — the 2 devices below are a past report");
  });

  it("says the listed devices are a PAST report, and that the screens are not evidence", () => {
    const d = describeDiscovery({
      devices: [device({ adopted: true })],
      devicesError: null,
      relays: [],
      blind: null,
    });
    // Singular AND plural, because the first version read "the 1 device below
    // ARE a past report" — the noun was pluralised and the verb was not.
    expect(d.headline).toBe("No relay is connected — the device below is a past report");
    // Never-wipe keeps a wall rendering for hours after its relay dies, so "the
    // screens look fine" must not be left available as a health signal.
    expect(d.detail).toMatch(/Screens keep playing/);
  });

  it("still reports all-adopted when a relay IS connected — the fix must not swallow the steady state", () => {
    const d = describeDiscovery({
      devices: [device({ adopted: true })],
      devicesError: null,
      relays,
      blind: null,
    });
    expect(d.kind).toBe("all-adopted");
  });

  it("leaves the unknown-relay-count case alone — 0 is a claim, null is an absence", () => {
    const d = describeDiscovery({
      devices: [device({ adopted: true })],
      devicesError: null,
      relays: null,
      blind: null,
    });
    expect(d.kind).toBe("all-adopted");
    expect(d.caveat).toMatch(/whether it is running NOW is not known/);
  });
});
