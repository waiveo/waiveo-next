// api/1 keyset pagination — the cursor iterator every list surface pages by.
//
// api/1 lists are keyset-paginated (contracts/api-1.md, API-030–036): a page is
// the `{items, cursor}` envelope, where `cursor` is an OPAQUE continuation token
// (`null` on the final page) that a client never constructs, parses, or compares
// for meaning — it only passes it back verbatim to fetch the next page. There is
// no offset/limit/total; a consumer that wants "everything" walks the cursors.
//
// This module is the client half of that contract: a fetch-page callback plus an
// async iterator that drives it to exhaustion, so a page and a route both express
// "for each row" without re-implementing the walk.

/** One keyset page: the rows, plus the opaque continuation cursor. `cursor` is
 * `null` on the final page (API-032). The token is never interpreted here. */
export interface Page<T> {
  items: T[];
  cursor: string | null;
}

/** Fetches the page that FOLLOWS `cursor`; `null` requests the first page. The
 * caller (a resource module) turns a non-null cursor into the request's opaque
 * `cursor` query parameter and omits it entirely for the first page — an empty
 * cursor is not a keyset position and the server rejects it (API-035). */
export type PageFetcher<T> = (cursor: string | null) => Promise<Page<T>>;

/** Walk every row across every page, in keyset order, until the server returns a
 * `null` cursor. Because the cursor is opaque and the walk stops ONLY on `null`
 * (never on an empty page), it never skips or repeats a row across the round trip
 * (API-034) and never invents a "start from the beginning" from a bad token. */
export async function* paginate<T>(fetchPage: PageFetcher<T>): AsyncGenerator<T, void, void> {
  let cursor: string | null = null;
  for (;;) {
    const page = await fetchPage(cursor);
    for (const item of page.items) yield item;
    if (page.cursor === null) return;
    cursor = page.cursor;
  }
}

/** Collect every row across every page into one array — the eager form of
 * `paginate`, for the (small, bounded) collections a console page renders whole. */
export async function collectPages<T>(fetchPage: PageFetcher<T>): Promise<T[]> {
  const items: T[] = [];
  for await (const item of paginate(fetchPage)) items.push(item);
  return items;
}
