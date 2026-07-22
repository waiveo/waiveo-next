// @vitest-environment node
import { describe, expect, it } from "vitest";
import { paginate, collectPages, type Page, type PageFetcher } from "./pagination";

describe("api/1 cursor pagination (API-030–036)", () => {
  it("walks every page in keyset order until the cursor is null", async () => {
    // Three pages; the last carries cursor: null (API-032).
    const pages: Page<number>[] = [
      { items: [1, 2], cursor: "c1" },
      { items: [3, 4], cursor: "c2" },
      { items: [5], cursor: null },
    ];
    const seenCursors: (string | null)[] = [];
    const fetchPage: PageFetcher<number> = (cursor) => {
      seenCursors.push(cursor);
      // The iterator hands back exactly the opaque token the prior page named;
      // the first call gets null (start from the beginning).
      const idx = cursor === null ? 0 : cursor === "c1" ? 1 : 2;
      return Promise.resolve(pages[idx]);
    };

    const out: number[] = [];
    for await (const n of paginate(fetchPage)) out.push(n);

    expect(out).toEqual([1, 2, 3, 4, 5]);
    // First call starts at null, then each page's opaque cursor is passed back
    // verbatim — never constructed, never parsed.
    expect(seenCursors).toEqual([null, "c1", "c2"]);
  });

  it("collectPages returns every row eagerly", async () => {
    const pages: Page<string>[] = [
      { items: ["a"], cursor: "n1" },
      { items: ["b", "c"], cursor: null },
    ];
    let call = 0;
    const fetchPage: PageFetcher<string> = () => Promise.resolve(pages[call++]);
    const all = await collectPages(fetchPage);
    expect(all).toEqual(["a", "b", "c"]);
  });

  it("stops after a single page whose cursor is already null", async () => {
    let calls = 0;
    const fetchPage: PageFetcher<number> = () => {
      calls++;
      return Promise.resolve({ items: [42], cursor: null });
    };
    const all = await collectPages(fetchPage);
    expect(all).toEqual([42]);
    // A null cursor on the first page ends the walk — no needless extra fetch.
    expect(calls).toBe(1);
  });

  it("yields nothing for an empty final page", async () => {
    const fetchPage: PageFetcher<number> = () => Promise.resolve({ items: [], cursor: null });
    const all = await collectPages(fetchPage);
    expect(all).toEqual([]);
  });
});
