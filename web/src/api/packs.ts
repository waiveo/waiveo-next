// api/1 — the declarative-packs client surface (manifest/1 + ui-schema/1).
//
// Two typed modules over the one ApiClient (client.ts), so the console consumes
// installed packs through the SAME conventions every core resource uses — never a
// second transport:
//
//   • `PacksModule` (the pack registry): list installed packs, read one, install
//     a zip artifact (a raw upload carrying an Idempotency-Key; the REAL manifest
//     engine gates it server-side and refuses with a 422 Problem), uninstall under
//     an If-Match, and fetch a pack's page documents (ui-schema/1) and locale
//     catalogs (MAN-110/111) verbatim for the renderer.
//   • `packData` (a pack's collections): the uniform api/1 CRUD ResourceModule for
//     one manifest-declared collection's universal-envelope rows (MAN-051/052) —
//     built from the shared `crud` factory, so a pack row honors If-Match/412,
//     Idempotency-Key on create, and the keyset cursor exactly as a scope-node
//     does. No pack code runs anywhere: a page doc, a catalog, and a row are all
//     DATA the console fetches and the renderer paints.

import { ApiClient, type Read } from "./client";
import { crud } from "./crud";
import type { ListParams, ResourceModule } from "./resources";
import type { Page } from "./pagination";

// ── Manifest shapes (the console-relevant subset) ────────────────────────────
//
// The install envelope carries the FULL manifest body; the console reads only the
// identity, the declared collections (to know which pack-data sources exist), and
// the ui.pages[] entries (to build the Extensions nav). Every other manifest
// section (capabilities, egress, devices, …) is validated server-side at install
// and irrelevant to rendering, so it is left untyped here.

/** One ui.pages[] entry (MAN-060): a pack-unique path, its page type, and a
 * `msg:` title reference resolved against the pack's locale catalog. */
export interface PackManifestPage {
  path: string;
  pageType: string;
  titleMsg: string;
  fragment?: string;
  sizeHint?: string;
}

/** One declared collection field (MAN-051). */
export interface PackManifestField {
  name: string;
  type: string;
  role?: string;
  searchable?: boolean;
  lifecycle?: string;
}

/** One manifest-declared collection (MAN-051) — the source of a pack-data family. */
export interface PackManifestCollection {
  name: string;
  fields: PackManifestField[];
}

/** The console-relevant slice of an installed pack's manifest. */
export interface PackManifest {
  id: string;
  version: string;
  /** A `msg:` reference resolved against the pack's locale catalog. */
  displayName: string;
  description?: string;
  ui: { pages: PackManifestPage[] };
  dataModel: { version: number; collections: PackManifestCollection[] };
}

/** An installed pack as the registry returns it: the identity + baseline plus the
 * full manifest body. `revision` doubles as the ETag validator (API-020). */
export interface Pack {
  id: string;
  revision: number;
  version: string;
  data_model_version: number;
  created_at: number;
  updated_at: number;
  manifest: PackManifest;
}

/** A successful install's summary (201 fresh | 200 reinstall) — the identity plus
 * what landed. */
export interface PackInstallResult {
  id: string;
  version: string;
  pages: string[];
  collections: string[];
  locales: string[];
}

// ── The pack registry module ─────────────────────────────────────────────────

export interface PacksModule {
  readonly path: string;
  /** List installed packs (one keyset page). */
  list(params?: ListParams): Promise<Page<Pack>>;
  /** Read one installed pack (captures its ETag for a later uninstall). */
  get(id: string): Promise<Read<Pack>>;
  /** Install a pack from a raw zip artifact. The manifest engine gates it
   * server-side (a refusal is a 422 Problem); the upload carries an
   * Idempotency-Key so a retry-on-timeout cannot double-install. */
  install(zip: BodyInit): Promise<PackInstallResult>;
  /** Uninstall a pack under its If-Match (removes files + rows atomically). */
  remove(id: string, etag: string): Promise<void>;
  /** Fetch one page document (ui-schema/1) verbatim, keyed by ui.pages[].path. */
  pageDoc(id: string, path: string): Promise<unknown>;
  /** Fetch one locale catalog verbatim (a flat `{ key: text }` map, keys WITHOUT
   * the `msg:` prefix — MAN-110/111). A missing locale is a 404 (ApiError). */
  messages(id: string, locale: string): Promise<Record<string, string>>;
}

/** Build the pack-registry module over one ApiClient. */
export function createPacksModule(client: ApiClient): PacksModule {
  const base = "/packs";
  return {
    path: base,
    list(params = {}) {
      const query: Record<string, string | number | undefined> = {};
      if (params.selector) query.selector = params.selector;
      if (params.limit !== undefined) query.limit = params.limit;
      if (params.cursor) query.cursor = params.cursor;
      return client.list<Pack>(base, query);
    },
    get(id) {
      // A pack id is `publisher/name` (two path segments the server route
      // rejoins) — its single slash is a real path separator, not an id byte, so
      // it must NOT be percent-encoded away.
      return client.read<Pack>(`${base}/${id}`);
    },
    install(zip) {
      return client.upload<PackInstallResult>(base, zip, "application/zip");
    },
    remove(id, etag) {
      return client.remove(`${base}/${id}`, etag);
    },
    pageDoc(id, path) {
      // The page path may itself be nested (a `{path...}` server wildcard); its
      // slashes are path separators, so it rides raw like the id.
      return client.read<unknown>(`${base}/${id}/pages/${path}`).then((r) => r.data);
    },
    messages(id, locale) {
      return client
        .read<Record<string, string>>(`${base}/${id}/messages/${encodeURIComponent(locale)}`)
        .then((r) => r.data);
    },
  };
}

// ── Pack-data rows (the universal entity envelope + declared fields) ──────────

/** A pack-data row as the api/1 surface returns it: the declared collection
 * fields flattened together with the universal entity envelope (MAN-051 — a
 * declared field can never collide with an envelope key, so the flatten is safe).
 * The declared fields ride under the index signature. */
export type PackRow = {
  entity_id: string;
  revision: number;
  lifecycle_state: string;
  scope_node: string;
  labels: string[];
  template_ref: string | null;
  params: Record<string, unknown> | null;
  external_id?: string;
  created_at: number;
  updated_at: number;
} & Record<string, unknown>;

/** A pack-data write body: the required `scope_node` envelope key plus the
 * optional envelope fields and the collection's declared fields (under the index
 * signature). Host-owned fields (entity_id/revision/timestamps) may be echoed on
 * a full-object PATCH round-trip and are ignored server-side (MAN-051). */
export type PackRowWrite = {
  scope_node: string;
  lifecycle_state?: string;
  labels?: string[];
  template_ref?: string | null;
  params?: Record<string, unknown> | null;
  external_id?: string | null;
} & Record<string, unknown>;

/** The uniform api/1 CRUD module for one pack collection's rows, rooted at
 * `/packs/{pack}/data/{collection}`. Identical in shape and conventions to every
 * core resource family — the row's `entity_id` is the id the item verbs address. */
export function packData(
  client: ApiClient,
  packId: string,
  collection: string,
): ResourceModule<PackRow, PackRowWrite, PackRowWrite> {
  return crud<PackRow, PackRowWrite, PackRowWrite>(
    client,
    `/packs/${packId}/data/${encodeURIComponent(collection)}`,
  );
}
