import { describe, it, expect } from "vitest";
import { dashboardCollections, primaryCollection, shapeCollection } from "./catalog";

// The data-source half of catalog.ts: which pack collections a page document
// reads. The defect these close is silent by construction — a collection the
// route fails to name is a tile that renders EMPTY, with no error anywhere, so
// every case here asserts the SET rather than "something was found".

const declared = new Set(["menu_items", "settings", "specials"]);

function dash(...tiles: unknown[]) {
  return { pageType: "dashboard", tiles };
}

describe("primaryCollection — the ONE page-wide bound collection", () => {
  it("resolves a list-detail's list.source and a settings-form's source", () => {
    expect(primaryCollection({ pageType: "list-detail", list: { source: "menu_items" } }, declared)).toBe(
      "menu_items",
    );
    expect(primaryCollection({ pageType: "settings-form", source: "settings" }, declared)).toBe("settings");
  });

  // A dashboard has no page-wide resource BY DESIGN (UIS-040). Returning null is
  // correct here and was never the bug — the bug was the route treating that null
  // as "this page reads nothing".
  it("returns null for a dashboard, which binds no page-wide collection", () => {
    expect(primaryCollection(dash({ size: "small", widget: { type: "table", props: { source: "menu_items" } } }), declared)).toBeNull();
  });

  it("returns null for a source naming no declared collection", () => {
    expect(primaryCollection({ pageType: "list-detail", list: { source: "not_declared" } }, declared)).toBeNull();
  });
});

describe("dashboardCollections — every collection a dashboard's tiles read (UIS-040)", () => {
  // The headline. Before this existed the route had no way to name these at all.
  it("collects a collection from each tile, deduplicated and sorted", () => {
    const doc = dash(
      { size: "large", widget: { type: "table", props: { source: "menu_items", columns: [{ cell: "item.name" }] } } },
      { size: "small", widget: { type: "stat-tile", props: { labelMsg: "msg:x", value: "settings.greeting" } } },
      // A second tile on the SAME collection must not duplicate it.
      { size: "small", widget: { type: "text", props: { value: "menu_items[0].name" } } },
    );
    expect(dashboardCollections(doc, declared)).toEqual(["menu_items", "settings"]);
  });

  // Nested descendants count: UIS-040 says a tile's widget AND its descendants
  // resolve their bindings as root Bindings, so a collection reached only from
  // inside a section is exactly as bound as one at the top.
  it("descends into nested widget children", () => {
    const doc = dash({
      size: "large",
      widget: {
        type: "section",
        props: { titleMsg: "msg:s" },
        children: [
          { type: "text", props: { value: "msg:static" } },
          { type: "table", props: { source: "specials", columns: [{ cell: "item.name" }] } },
        ],
      },
    });
    expect(dashboardCollections(doc, declared)).toEqual(["specials"]);
  });

  it("reads a paginated source object's path (UIS-023/024)", () => {
    const doc = dash({
      size: "large",
      widget: { type: "table", props: { source: { paginated: true, path: "menu_items" } } },
    });
    expect(dashboardCollections(doc, declared)).toEqual(["menu_items"]);
  });

  it("takes the ROOT key off a predicated or indexed binding", () => {
    const doc = dash(
      { size: "small", widget: { type: "text", props: { value: "menu_items[entity_id=$ui.selected].name" } } },
      { size: "small", widget: { type: "text", props: { value: "specials.0.title" } } },
    );
    expect(dashboardCollections(doc, declared)).toEqual(["menu_items", "specials"]);
  });

  it("ignores a name the pack never declared", () => {
    const doc = dash({ size: "small", widget: { type: "table", props: { source: "menu_items_typo" } } });
    expect(dashboardCollections(doc, declared)).toEqual([]);
  });

  // The reserved-root and msg: guards are LOAD-BEARING, not decoration, and this
  // case is written to prove it rather than to pass by accident.
  //
  // MAN-051 puts no grammar on a collection name — only that it is non-empty and
  // unique within the pack (verified in internal/manifest/datamodel.go, which
  // checks exactly those two things). So a pack may legally declare a collection
  // called `$ui` or `msg:dashboard`. With such a manifest installed, an
  // intersection-only filter would read every `$ui.…` binding on the page as a
  // collection reference and fetch it — a request for renderer-local state that
  // the renderer would then ignore, since `$ui` addresses ephemeral UI state
  // (UIS-104) and no pack row can ever appear there.
  //
  // The first draft of this test declared only ordinary names, so the intersection
  // alone excluded these and removing the guard changed nothing. A mutation caught
  // that.
  it("ignores reserved roots and msg: refs EVEN WHEN a pack declares collections by those names", () => {
    const hostile = new Set(["$ui", "$root", "msg:dashboard", "menu_items"]);
    const doc = dash(
      { size: "small", widget: { type: "text", props: { value: "$ui.draft.name" } } },
      { size: "small", widget: { type: "text", props: { value: "$root.total" } } },
      { size: "small", widget: { type: "text", props: { value: "msg:dashboard.title" } } },
      { size: "small", widget: { type: "table", props: { source: "menu_items" } } },
    );
    // Only the real one, however the manifest names its collections.
    expect(dashboardCollections(doc, hostile)).toEqual(["menu_items"]);
  });

  // UIS-108's literal escape. `{lit}` is how an author says "this string is DATA,
  // not a Binding" — descending into it would read a string they went out of
  // their way to mark as not-a-path.
  it("does not read inside a {lit} literal escape (UIS-108)", () => {
    const doc = dash({ size: "small", widget: { type: "text", props: { value: { lit: "menu_items" } } } });
    expect(dashboardCollections(doc, declared)).toEqual([]);
  });

  it("returns nothing for a dashboard with no tiles, or a non-document", () => {
    expect(dashboardCollections({ pageType: "dashboard" }, declared)).toEqual([]);
    expect(dashboardCollections(dash(), declared)).toEqual([]);
    expect(dashboardCollections(null, declared)).toEqual([]);
  });

  // The pageType gate, tested against a document that would otherwise slip past
  // it: a `list-detail` carrying a stray `tiles` array. Keying only on `tiles`
  // would walk it, and the two page types answer different questions — a
  // list-detail binds ONE page-wide collection as an array of rows, a dashboard
  // binds several per tile. Conflating them is how one collection ends up
  // requested twice under two different shapes.
  it("walks tiles only on a dashboard, never on a page type that binds page-wide", () => {
    const strayTiles = {
      pageType: "list-detail",
      list: { source: "menu_items" },
      tiles: [{ size: "small", widget: { type: "table", props: { source: "specials" } } }],
    };
    expect(dashboardCollections(strayTiles, declared)).toEqual([]);
    // …and the page-wide reading is the one that applies to it.
    expect(primaryCollection(strayTiles, declared)).toBe("menu_items");
  });

  // A pack that declares no collections can bind none, however its tiles read.
  it("finds nothing when the pack declares no collections", () => {
    const doc = dash({ size: "large", widget: { type: "table", props: { source: "menu_items" } } });
    expect(dashboardCollections(doc, new Set())).toEqual([]);
  });
});

describe("shapeCollection — a singleton enters the namespace as the RECORD", () => {
  // MAN-056. A pack declaring `singleton: true` and binding `settings.greeting`
  // is the natural authoring, and it reads the row's field — handing it a
  // one-element array would make that binding resolve to nothing, which is the
  // silent-empty failure this whole change is about.
  it("unwraps a singleton's single row, and an empty one to {} rather than undefined", () => {
    expect(shapeCollection([{ greeting: "hi" }], true)).toEqual({ greeting: "hi" });
    expect(shapeCollection([], true)).toEqual({});
  });

  it("leaves an ordinary collection as its rows array", () => {
    const rows = [{ name: "a" }, { name: "b" }];
    expect(shapeCollection(rows, false)).toBe(rows);
    expect(shapeCollection([], false)).toEqual([]);
  });
});
