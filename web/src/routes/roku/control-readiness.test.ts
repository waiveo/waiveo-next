import { describe, it, expect } from "vitest";
import { assessReadiness, findAdoption } from "./control-readiness";
import type { AdoptedDevice, Device, Entity } from "@/api";

// The relay's rule is one sentence: an entity is controllable iff it is adopted
// AND enabled AND the relay can locate it. All three failures come back as one
// undifferentiated COMMAND_UNRESOLVED, after the operator has pressed a button
// and watched the TV not move — so these tests pin that the page can tell them
// apart BEFORE the press.

const RELAY = "relay-0f1e2d3c4b5a69788796a5b4c3d2e1f0";
const DEVICE_ID = "01J8ZDEV1CE00000000000000A";
const ENTITY_ID = "01J8ZENT1TY00000000000000A";
const SITE = "01J8Z0ROOT0000000000000000";

function device(over: Partial<Device> = {}): Device {
  return {
    id: DEVICE_ID,
    external_id: null,
    relay_id: RELAY,
    device_class: "media-player",
    name: "The Hanger",
    scope_node: SITE,
    labels: {},
    address: "192.0.2.31:8060",
    model: "Roku Ultra",
    adopted: true,
    ignored: false,
    ...over,
  };
}

function entity(over: Partial<Entity> = {}): Entity {
  return {
    id: ENTITY_ID,
    external_id: null,
    device_id: DEVICE_ID,
    relay_id: RELAY,
    device_class: "media-player",
    name: "The Hanger main",
    scope_node: SITE,
    labels: {},
    state: "idle",
    ...over,
  };
}

function record(over: Partial<AdoptedDevice> = {}): AdoptedDevice {
  return {
    id: DEVICE_ID,
    external_id: null,
    name: "The Hanger",
    scope_node: SITE,
    driver: "roku-ecp",
    native_id: "uuid:roku:ecp:AA11",
    poll_cadence_seconds: null,
    entities: [
      {
        entity_id: ENTITY_ID,
        device_class: "media-player",
        enabled: true,
        hidden: false,
        display_name: "The Hanger main",
        category: "primary",
      },
    ],
    labels: {},
    revision: 1,
    created_at: 1_753_142_400_000,
    updated_at: 1_753_142_400_000,
    ...over,
  };
}

describe("findAdoption — the join is by entity id", () => {
  it("finds the record and the entity's own policy row", () => {
    const found = findAdoption(ENTITY_ID, [record()])!;
    expect(found.record.driver).toBe("roku-ecp");
    expect(found.policy.entity_id).toBe(ENTITY_ID);
  });

  it("does not match a record that merely shares the device id", () => {
    // Device id and record id happen to coincide today (both derive from the
    // same tuple), and joining on it would be a coincidence this code must not
    // depend on. The join is on the identifier the two families genuinely
    // share — the one the relay itself matches a command against.
    const other = record({ entities: [] });
    expect(findAdoption(ENTITY_ID, [other])).toBeNull();
  });

  it("is null for an entity no record carries", () => {
    expect(findAdoption("01J8ZENT1TY0000000000000ZZ", [record()])).toBeNull();
  });
});

describe("assessReadiness — the three preconditions, separately", () => {
  it("is ready when adopted, enabled and addressed", () => {
    const r = assessReadiness(device(), entity(), [record()]);
    expect(r.controllable).toBe(true);
    expect(r.problems).toEqual([]);
    expect(r.record?.driver).toBe("roku-ecp");
    expect(r.policy?.enabled).toBe(true);
  });

  it("names NOT ADOPTED, and does not also complain about the record", () => {
    // An unadopted device has no record by definition; reporting both would be
    // two problems for one cause and would bury the actionable one.
    const r = assessReadiness(device({ adopted: false }), entity(), []);
    expect(r.problems.map((p) => p.code)).toEqual(["not-adopted"]);
    expect(r.problems[0]!.detail).toMatch(/COMMAND_UNRESOLVED/);
  });

  it("names an entity the adoption record does not carry", () => {
    // A record's entity list is fixed at adoption. An entity the device only
    // started reporting afterwards is absent from the inventory the relay was
    // sent, and every command to it is refused — with nothing on the device row
    // to hint at why.
    const r = assessReadiness(device(), entity(), [record({ entities: [] })]);
    expect(r.problems.map((p) => p.code)).toEqual(["entity-not-in-record"]);
  });

  it("names a DISABLED entity distinctly from a missing one", () => {
    const disabled = record();
    disabled.entities[0]!.enabled = false;
    const r = assessReadiness(device(), entity(), [disabled]);
    expect(r.problems.map((p) => p.code)).toEqual(["entity-disabled"]);
    expect(r.policy?.enabled).toBe(false);
  });

  it("names a missing address on its own, without impugning the adoption", () => {
    const noAddress = device();
    delete noAddress.address;
    const r = assessReadiness(noAddress, entity(), [record()]);
    expect(r.problems.map((p) => p.code)).toEqual(["no-address"]);
    // The relay-side override is real and invisible here, so the wording must
    // not assert the command WILL fail.
    expect(r.problems[0]!.detail).toMatch(/Unless the deployment pins one out of band/);
  });

  it("treats a blank address as no address, because a blank one cannot be dialled", () => {
    const r = assessReadiness(device({ address: "   " }), entity(), [record()]);
    expect(r.problems.map((p) => p.code)).toEqual(["no-address"]);
  });

  it("reports BOTH when adoption and address are missing together", () => {
    const bare = device({ adopted: false });
    delete bare.address;
    const r = assessReadiness(bare, entity(), []);
    expect(r.problems.map((p) => p.code)).toEqual(["not-adopted", "no-address"]);
    expect(r.controllable).toBe(false);
  });
});
