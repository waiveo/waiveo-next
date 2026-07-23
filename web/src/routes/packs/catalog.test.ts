import { describe, it, expect } from "vitest";
import { resolveTitle, toRendererMessages } from "./catalog";

// resolveTitle is the nav/header sibling of the renderer's makeMessageResolver,
// and it carries the SAME hazard: a pack's locale catalog is untrusted data. The
// install pipeline validates a catalog only as well-formed JSON (never that its
// values are strings), so a pack can ship messages/en.json with a NON-string
// value — an object or array — under a manifest-referenced key (e.g.
// `"pack.displayName": {"x": 1}` behind `displayName: "msg:pack.displayName"`).
// toRendererMessages copies that value through verbatim, so resolveTitle must not
// hand a non-string back: it would flow to `group.title` and crash the whole
// console shell on React's "Objects are not valid as a React child". Only an OWN
// string entry resolves; anything else humanizes, exactly like makeMessageResolver.
describe("resolveTitle — untrusted-catalog value guard", () => {
  it("resolves a genuine own string entry", () => {
    const messages = toRendererMessages({ "pack.displayName": "Menu Board" });
    expect(resolveTitle(messages, "msg:pack.displayName")).toBe("Menu Board");
  });

  it("humanizes the reference's last segment when the catalog has no entry", () => {
    expect(resolveTitle({}, "msg:page.menuItems.title")).toBe("Title");
    expect(resolveTitle({}, "msg:pack.displayName")).toBe("Display Name");
  });

  for (const [label, value] of [
    ["an object", { x: 1 }],
    ["an array", [1, 2, 3]],
    ["a number", 42],
    ["a boolean", true],
    ["null", null],
  ] as const) {
    it(`humanizes (never returns) a non-string catalog value: ${label}`, () => {
      // A catalog whose msg:pack.displayName value is a non-string — exactly what
      // toRendererMessages would carry through from an untrusted pack's en.json.
      const messages = { "msg:pack.displayName": value } as unknown as Record<string, string>;
      let title = "";
      expect(() => {
        title = resolveTitle(messages, "msg:pack.displayName");
      }).not.toThrow();
      expect(typeof title).toBe("string");
      // The humanized last segment, never the raw non-string value.
      expect(title).toBe("Display Name");
    });
  }

  it("resolves an inherited prototype member to the humanized fallback, not the member", () => {
    // A bare (msg:-prefix-dropped) reference colliding with an Object.prototype
    // member must not surface the inherited function — the OWN-value path guards it.
    expect(resolveTitle({}, "toString")).toBe("To String");
    expect(resolveTitle({}, "hasOwnProperty")).toBe("Has Own Property");
  });
});
