import { describe, it, expect } from "vitest";
import { ALLOWED_PACK_ICONS, DEFAULT_PACK_ICON, resolvePackIcon } from "./pack-icon";

// resolvePackIcon reads UNTRUSTED manifest data — install validates the manifest
// as JSON and the icon name's grammar, never its runtime type or its membership
// of the host's set. The one property that matters is therefore total: it always
// returns a real component, so the console can never render a broken or missing
// glyph.
//
// This became a unit test when the rail section it used to be exercised through
// was removed (owner, 2026-08-19). Its wiring — that the resolved glyph actually
// reaches the DOM on a pack card — is asserted in ./extensions-route.test.tsx;
// this covers the mapping itself, including the inputs a DOM test cannot easily
// enumerate.

describe("resolvePackIcon", () => {
  it("maps every host-allowed name to its own component", () => {
    for (const [name, icon] of Object.entries(ALLOWED_PACK_ICONS)) {
      expect(resolvePackIcon(name)).toBe(icon);
    }
    // …and the set is not empty, or the loop above asserts nothing.
    expect(Object.keys(ALLOWED_PACK_ICONS).length).toBeGreaterThan(5);
  });

  it("returns the DEFAULT glyph for anything else — never undefined, never a throw", () => {
    for (const bad of [
      undefined,
      null,
      "",
      "totally-not-a-real-glyph",
      "Puzzle", // the component name, not the kebab-case lucide id
      42,
      { evil: 1 },
      ["puzzle"],
      Object.create(null) as unknown,
    ]) {
      expect(resolvePackIcon(bad)).toBe(DEFAULT_PACK_ICON);
    }
  });

  it("is not fooled by a name inherited from Object.prototype", () => {
    // ALLOWED_PACK_ICONS is an object literal, so a lookup of "constructor" or
    // "toString" finds a truthy prototype member. Returning that as a component
    // would put a function React cannot render into the tree — the exact broken
    // icon this module exists to make impossible.
    for (const inherited of ["constructor", "toString", "hasOwnProperty", "__proto__"]) {
      expect(resolvePackIcon(inherited)).toBe(DEFAULT_PACK_ICON);
    }
  });
});
