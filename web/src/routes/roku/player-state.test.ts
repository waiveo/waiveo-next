import { describe, it, expect } from "vitest";
import {
  extraAttributes,
  MEDIA_PLAYER_ATTRIBUTES,
  nowShowing,
  playerFacts,
} from "./player-state";
import type { Entity } from "@/api";

const BASE: Entity = {
  id: "01J8ZENT1TY00000000000000A",
  external_id: null,
  device_id: "01J8ZDEV1CE00000000000000A",
  relay_id: "relay-0f1e2d3c4b5a69788796a5b4c3d2e1f0",
  device_class: "media-player",
  name: "The Hanger main",
  scope_node: "01J8Z0ROOT0000000000000000",
  labels: {},
  state: "on",
};

function entity(over: Partial<Entity> = {}): Entity {
  return { ...BASE, ...over };
}

describe("MEDIA_PLAYER_ATTRIBUTES", () => {
  it("is exactly REG-064's six, with app_version the only cosmetic one", () => {
    expect(MEDIA_PLAYER_ATTRIBUTES.map((a) => a.name)).toEqual([
      "active_app",
      "active_app_id",
      "app_type",
      "is_screensaver",
      "power_mode",
      "app_version",
    ]);
    expect(
      MEDIA_PLAYER_ATTRIBUTES.filter((a) => a.emission === "cosmetic").map((a) => a.name),
    ).toEqual(["app_version"]);
  });
});

describe("playerFacts", () => {
  it("returns all six even when the driver reported none", () => {
    // The absence IS the fact: an entity with no attributes at all is what an
    // unpolled or unreachable device looks like, and hiding the rows hides it.
    const facts = playerFacts(entity());
    expect(facts).toHaveLength(6);
    expect(facts.every((f) => f.value === null)).toBe(true);
  });

  it("distinguishes 'not reported' from 'reported as empty'", () => {
    const facts = playerFacts(entity({ attributes: { active_app: "" } }));
    const app = facts.find((f) => f.name === "active_app")!;
    const id = facts.find((f) => f.name === "active_app_id")!;
    expect(app.value).toBe("");
    expect(id.value).toBeNull();
  });

  it("carries the values the driver did report", () => {
    const facts = playerFacts(
      entity({ attributes: { power_mode: "PowerOn", is_screensaver: "false" } }),
    );
    expect(facts.find((f) => f.name === "power_mode")!.value).toBe("PowerOn");
    // Booleans stay strings — the wire type is a string map on purpose, and
    // coercing would mean inventing an answer for "yes"/"1"/"".
    expect(facts.find((f) => f.name === "is_screensaver")!.value).toBe("false");
  });
});

describe("extraAttributes", () => {
  it("is empty when the driver reported only declared attributes", () => {
    expect(extraAttributes(entity({ attributes: { active_app: "Waiveo" } }))).toEqual([]);
  });

  it("surfaces what the class does not declare rather than dropping it", () => {
    // The one fact that explains a failure must never be the one the console
    // chose not to render.
    const extra = extraAttributes(
      entity({ attributes: { active_app: "Waiveo", zz_probe: "1", aa_probe: "2" } }),
    );
    expect(extra).toEqual([
      { name: "aa_probe", value: "2" },
      { name: "zz_probe", value: "1" },
    ]);
  });
});

describe("nowShowing — the one line an operator reads first", () => {
  it("says nothing has been polled rather than calling it off", () => {
    const e = entity();
    delete e.state;
    expect(nowShowing(e)).toMatch(/No state reported yet/);
    expect(nowShowing(e)).not.toMatch(/Not powered on/);
  });

  it("says the relay could not reach it, which is not the same as off", () => {
    expect(nowShowing(entity({ state: "unavailable" }))).toMatch(/could not reach this device/);
  });

  it("carries the raw power mode when the device is off, because standby is not off", () => {
    expect(nowShowing(entity({ state: "standby", attributes: { power_mode: "DisplayOff" } }))).toMatch(
      /power mode "DisplayOff"/,
    );
  });

  it("reports a screensaver as showing NO content, ahead of the app name", () => {
    // A screensaver is the single most common reason a wall looks wrong while
    // every status reads healthy — the screen is genuinely on and genuinely not
    // showing what was scheduled.
    const e = entity({
      state: "idle",
      attributes: { is_screensaver: "true", active_app: "Roku" },
    });
    expect(nowShowing(e)).toMatch(/showing its screensaver/);
    expect(nowShowing(e)).toMatch(/no content is on the screen/);
  });

  it("names the foregrounded app and its id", () => {
    const e = entity({
      state: "playing",
      attributes: { active_app: "Waiveo", active_app_id: "dev", is_screensaver: "false" },
    });
    expect(nowShowing(e)).toBe("Powered on, showing Waiveo (dev).");
  });

  it("admits when the driver did not say what is foregrounded", () => {
    expect(nowShowing(entity({ state: "playing" }))).toMatch(/did not report what is foregrounded/);
  });
});
