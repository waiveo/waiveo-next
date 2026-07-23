// api/1 — the uniform per-resource CRUD factory.
//
// Extracted so BOTH the core resource families (resources.ts) and the pack-data
// collections (packs.ts) build on the exact same surface — the api/1 conventions
// (Problem, ETag/If-Match, Idempotency-Key on create, cursor pagination) are the
// ApiClient's law (client.ts) and this factory hangs one typed module off a base
// path, never re-deriving a convention. The shared shape types live in
// resources.ts; this module imports them type-only, so there is no runtime import
// cycle (resources.ts and packs.ts both depend on this leaf).

import { ApiClient } from "./client";
import { paginate, type PageFetcher } from "./pagination";
import type { ListParams, ResourceModule } from "./resources";

/** Build the uniform api/1 CRUD module for a resource family rooted at `path`.
 * `update`/`remove` require an ETag by type (no unconditional-overwrite path,
 * API-022); `create` carries a fresh Idempotency-Key; `list` threads the keyset
 * cursor; the item path percent-encodes the id. */
export function crud<T, TCreate, TUpdate>(
  client: ApiClient,
  path: string,
): ResourceModule<T, TCreate, TUpdate> {
  const itemPath = (id: string): string => `${path}/${encodeURIComponent(id)}`;
  const mod: ResourceModule<T, TCreate, TUpdate> = {
    path,
    list(params = {}) {
      const query: Record<string, string | number | undefined> = {};
      if (params.selector) query.selector = params.selector;
      if (params.limit !== undefined) query.limit = params.limit;
      // Omit the cursor on the first page — an empty cursor is not a keyset
      // position and the server rejects it (API-035).
      if (params.cursor) query.cursor = params.cursor;
      return client.list<T>(path, query);
    },
    pages(params: Omit<ListParams, "cursor"> = {}) {
      const fetchPage: PageFetcher<T> = (cursor) => mod.list({ ...params, cursor });
      return paginate(fetchPage);
    },
    get(id) {
      return client.read<T>(itemPath(id));
    },
    create(body) {
      return client.create<T>(path, body);
    },
    update(id, patch, etag) {
      return client.update<T>(itemPath(id), patch, etag);
    },
    remove(id, etag) {
      return client.remove(itemPath(id), etag);
    },
    etagFor(id) {
      return client.etagFor(itemPath(id));
    },
  };
  return mod;
}
