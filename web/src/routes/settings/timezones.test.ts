import { describe, it, expect, afterEach } from "vitest";
import { FALLBACK_ZONES, timeZoneOptions } from "./timezones";

// The zone list is the whole reason the tz control can be a closed select
// instead of a text box, so each of its three guards is tested by BREAKING the
// condition it guards against — a runtime that enumerates nothing, a runtime
// that throws, and a record already carrying a zone this runtime never lists.

type IntlWithEnumeration = { supportedValuesOf?: unknown };
const intl = Intl as unknown as IntlWithEnumeration;
const real = Object.getOwnPropertyDescriptor(Intl, "supportedValuesOf");

function restore() {
  if (real) Object.defineProperty(Intl, "supportedValuesOf", real);
  else delete intl.supportedValuesOf;
}
afterEach(restore);

describe("timeZoneOptions", () => {
  it("offers what the runtime enumerates", () => {
    intl.supportedValuesOf = (key: string) =>
      key === "timeZone" ? ["Europe/London", "America/Chicago"] : [];
    const zones = timeZoneOptions();
    expect(zones).toContain("Europe/London");
    expect(zones).toContain("America/Chicago");
  });

  it("falls back to a usable list when the runtime enumerates nothing", () => {
    delete intl.supportedValuesOf;
    const zones = timeZoneOptions();
    // Not merely non-empty: the control has to stay USABLE, so the fallback must
    // actually be the fallback set rather than a bare ["UTC"].
    expect(zones).toEqual(expect.arrayContaining([...FALLBACK_ZONES]));
    expect(zones.length).toBeGreaterThan(10);
  });

  it("falls back when the runtime throws rather than propagating", () => {
    intl.supportedValuesOf = () => {
      throw new Error("locked down");
    };
    expect(() => timeZoneOptions()).not.toThrow();
    expect(timeZoneOptions()).toContain("Europe/London");
  });

  it("always offers UTC even when the runtime omits it", () => {
    intl.supportedValuesOf = () => ["Europe/London"];
    expect(timeZoneOptions()).toContain("UTC");
  });

  // THE guard. A stored zone the runtime does not enumerate must still be an
  // option, or the select renders with nothing selected and the next save
  // silently rewrites a site's time zone to whatever was picked instead.
  it("keeps a STORED zone the runtime does not enumerate", () => {
    intl.supportedValuesOf = () => ["Europe/London"];
    const zones = timeZoneOptions(["Antarctica/Troll"]);
    expect(zones).toContain("Antarctica/Troll");
  });

  it("ignores null, undefined and blank stored values rather than offering them", () => {
    intl.supportedValuesOf = () => ["Europe/London"];
    const zones = timeZoneOptions([null, undefined, "   ", "Asia/Tokyo"]);
    expect(zones).toContain("Asia/Tokyo");
    expect(zones).not.toContain("");
    expect(zones).not.toContain("   ");
  });

  it("de-duplicates and sorts, so the select has no repeated option", () => {
    intl.supportedValuesOf = () => ["Europe/London", "Asia/Tokyo"];
    const zones = timeZoneOptions(["Europe/London", "Europe/London"]);
    expect(zones.filter((z) => z === "Europe/London")).toHaveLength(1);
    expect([...zones].sort((a, b) => a.localeCompare(b))).toEqual(zones);
  });
});
