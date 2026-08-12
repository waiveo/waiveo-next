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
  /** An optional display hint: a lucide glyph name (kebab-case) the console
   * resolves to the pack's nav-group icon. Grammar-validated at install; an
   * absent or unrecognized name degrades to the default extension glyph, never a
   * broken icon (resolvePackIcon). Untrusted — the runtime value is not
   * guaranteed to be a string. */
  icon?: string;
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

// ── Marketplace shapes (marketplace/1) ───────────────────────────────────────

/** MKT-020's CLOSED four-member trust-channel enum. Mirrored here because a
 * marketplace reference REQUIRES one (MKT-060a(b)) and neither the host nor this
 * client may default or infer it: the trust channel IS the provenance tier the
 * install is accepted under, so choosing one on the operator's behalf would be
 * choosing how much review the pack has had. Growth is a marketplace/1 minor. */
export const TRUST_CHANNELS = ["first-party", "verified", "community", "dev"] as const;
export type TrustChannel = (typeof TRUST_CHANNELS)[number];

/** A marketplace reference (MKT-060a, openapi `MarketplaceRef`): the input an
 * install is RESOLVED from, as opposed to the artifact bytes an upload supplies.
 *
 *   • `pack_id` MUST be fully publisher-qualified (`<publisher>/<name>`, MKT-008).
 *   • `trust_channel` is REQUIRED — see TRUST_CHANNELS.
 *   • `source` is optional: absent, the deployment's ordered registry source list
 *     is walked (MKT-061).
 *   • `version` is optional: absent, the reference resolves through that source's
 *     channel pointer (MKT-046/047) under MKT-050's anti-rollback; present, it
 *     pins one exact version (MKT-060a(c)).
 *
 * The server decodes this body with unknown members REFUSED, so an empty optional
 * is OMITTED rather than sent as `""` (createPacksModule.installRef). */
export interface MarketplaceRef {
  pack_id: string;
  trust_channel: TrustChannel;
  source?: string;
  version?: string;
}

/** One append-only install record (MKT-094 + MKT-094a) — what was resolved, and
 * the provenance the install was accepted on.
 *
 * `trust_channel` and `artifact_digest` are NULLABLE because a direct,
 * out-of-band artifact install genuinely has neither: no channel pointer stands
 * behind it to auto-track (MKT-090) and no index entry named it. A consumer MUST
 * treat `trust_channel === null` as "not auto-tracked" rather than as an unknown
 * channel — it is the difference between a pack the box can re-resolve and one it
 * cannot (MKT-094a).
 *
 * `content_digest`/`key_id` name the MKT-009a signature envelope whose
 * verification admitted the bytes. That envelope is dropped at install and never
 * persisted, so these records are the ONLY answer to "which key vouched for what
 * is running". */
export interface PackInstallRecord {
  id: string;
  pack_id: string;
  resolved_version: string;
  trust_channel: string | null;
  source: string;
  stale_source: boolean;
  content_digest: string;
  key_id: string;
  artifact_digest: string | null;
  /** Epoch MILLISECONDS — the store's own clock (store.WallClockMs), same unit as
   * a pack envelope's created_at/updated_at. */
  installed_at: number;
}

/** What one update check actually did (MKT-090, MKT-093):
 *   • `unchanged` — the pinned channel still points at the installed version, and
 *     NOTHING was written.
 *   • `updated`   — the pointer named a different version and it installed.
 *   • `reverted`  — the applied version had been YANKED, and the most recent
 *     still-resolvable version this install had previously applied was
 *     reinstalled in its place (the sole backward move MKT-050 permits). */
export type PackUpdateAction = "unchanged" | "updated" | "reverted";

/** One update check's outcome. `from_version` and `to_version` are equal exactly
 * when `action` is `unchanged`; the landed-artifact lists are absent then, because
 * nothing landed. */
export interface PackUpdateResult {
  action: PackUpdateAction;
  id: string;
  from_version: string;
  to_version: string;
  pages?: string[];
  collections?: string[];
  locales?: string[];
}

/** What an update check WOULD do, reported without doing it (MKT-095).
 *
 * `action` reuses PackUpdateAction deliberately: a client that has learned to
 * read one outcome vocabulary should not have to learn a second for the report
 * of it. `reverted` is the one to surface loudly — the running version has been
 * revoked, and a check would replace it. */
export interface PackUpdateAvailability {
  action: PackUpdateAction;
  id: string;
  from_version: string;
  to_version: string;
  /** The pin the answer was resolved through. "An update is waiting" is not
   * actionable without knowing which channel is offering it. */
  trust_channel: string;
  source: string;
}

/** One artifact a registry source offers (MKT-096).
 *
 * What a listing is NOT: an endorsement. Until channel-index/1's signature
 * chain is enforced an index is untrusted transport, so this says "the source
 * named below claims to have this" and nothing more. Installing it runs the
 * full resolution and install-time verification, which is what makes acting on
 * an untrusted listing safe — so the UI must offer the INSTALL, never present
 * the listing itself as a guarantee.
 *
 * There is deliberately no digest, size or download url: those are resolution
 * inputs, and a client that had them might act on the listing directly. */
export interface CatalogEntry {
  id: string;
  version: string;
  kind: string;
  source: string;
  trust_channel: string;
  status: string;
}

/** One source's offerings, or the reason it could not be read.
 *
 * `unavailable` non-empty and `entries` empty are DIFFERENT from `entries`
 * empty alone: the first is a registry this box could not reach, the second is
 * a registry that answered and offers nothing. */
export interface CatalogSource {
  source: string;
  trust_channel: string;
  entries: CatalogEntry[];
  unavailable?: string;
}

// ── Pack page-path confinement ───────────────────────────────────────────────

/**
 * Encode a pack-declared page path for SAFE splicing into a route href or a fetch
 * URL. Unlike a pack id (idSegmentRe-constrained server-side), the manifest engine
 * does not yet constrain `ui.pages[].path` to a segment grammar — so the console
 * must not trust it. A path like `../../design` or `%2e%2e/%2e%2e/scope-nodes`,
 * concatenated raw, escapes the pack's own `/p/{pack}/{path}` confinement:
 * react-router's `resolvePath` and the WHATWG URL parser `fetch()` uses BOTH
 * collapse dot-segments before computing the destination.
 *
 * Each `/`-separated segment is percent-encoded individually — a genuinely nested
 * path keeps its separators, but a literal `%2e%2e` becomes an inert `%252e%252e`
 * the parser will not collapse. A `.`/`..`/empty segment is REJECTED outright:
 * encodeURIComponent leaves `.`/`..` untouched (dots are unreserved), so it would
 * otherwise remain a live dot-segment the router/parser still folds away. Returns
 * the safe path, or `null` when the path carries a traversal or empty segment.
 */
export function encodePackPagePath(path: string): string | null {
  if (path === "") return null;
  const out: string[] = [];
  for (const segment of path.split("/")) {
    if (segment === "" || segment === "." || segment === "..") return null;
    out.push(encodeURIComponent(segment));
  }
  return out.join("/");
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
  /** Install a pack by MARKETPLACE REFERENCE (MKT-060a) — the same endpoint, the
   * same pipeline, the same trust anchors and the same manifest gate as an
   * uploaded artifact; only the way the bytes are LOCATED differs. Carries an
   * Idempotency-Key over the reference bytes. */
  installRef(ref: MarketplaceRef): Promise<PackInstallResult>;
  /** Run ONE update check against this pack's own pinned trust channel (MKT-090),
   * reverting to its last known good version if the applied one has been yanked
   * (MKT-093). Nothing about HOW the pack is re-resolved is sent — the source and
   * channel are read server-side off the pack's newest install record, which is
   * what stops a host silently choosing a provenance tier. A pack installed
   * directly (no channel pinned) is REFUSED rather than defaulted onto one. */
  update(id: string): Promise<PackUpdateResult>;
  /** Report whether an update is waiting, WITHOUT applying one (MKT-095). The
   * same resolution `update` performs, read rather than run: nothing is
   * installed, reverted, or recorded. This is the call that answers "is there
   * an update" — `update` is the one that takes it. */
  updateAvailability(id: string): Promise<PackUpdateAvailability>;
  /** What the configured registry sources currently OFFER (MKT-096) — the
   * discovery half. Grouped by source and never merged: source order is a
   * resolution preference, never a trust decision. */
  catalog(): Promise<CatalogSource[]>;
  /** List a pack's append-only install history, oldest first (MKT-094b). The last
   * entry is the current pin. A pack that is not installed is a 404. */
  installs(id: string, params?: ListParams): Promise<Page<PackInstallRecord>>;
  /** Uninstall a pack under its If-Match (removes files + rows atomically). A pack
   * on this deployment's required-pack roster is REFUSED — a 422 whose `errors[]`
   * carries `REQUIRED_PACK_FLOOR` (MKT-093b(i)). */
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
    installRef(ref) {
      // The server decodes this body with DisallowUnknownFields and treats an
      // empty `source`/`version` as "unspecified" — so an absent optional is
      // OMITTED rather than sent as "". Same endpoint as `install`; the JSON
      // content type is the discriminant that says "resolve this" rather than
      // "install these bytes". `action` carries the Idempotency-Key, so a
      // retry-on-timeout replays the original outcome instead of resolving twice.
      const body: Record<string, string> = {
        pack_id: ref.pack_id,
        trust_channel: ref.trust_channel,
      };
      if (ref.source) body.source = ref.source;
      if (ref.version) body.version = ref.version;
      return client.action<PackInstallResult>(base, body);
    },
    update(id) {
      // No body: the operation is fully determined by its path (the channel pin
      // lives in the install record server-side). The pack id's single slash is a
      // real path separator, exactly as in `get`.
      return client.action<PackUpdateResult>(`${base}/${id}/update`);
    },
    updateAvailability(id) {
      // A GET on the same path as `update`'s POST — one question, two verbs.
      // `.read` is the plain-GET seam here; the ETag it captures is not used
      // because this addresses no mutable resource, only a computed answer.
      return client.read<PackUpdateAvailability>(`${base}/${id}/update`).then((r) => r.data);
    },
    catalog() {
      // One literal segment, so it can never be read as a `publisher/name`.
      return client
        .read<{ sources: CatalogSource[] }>(`${base}/catalog`)
        .then((r) => r.data.sources ?? []);
    },
    installs(id, params = {}) {
      // No `selector`: install records are not label-selectable — the route
      // declares only cursor/limit.
      const query: Record<string, string | number | undefined> = {};
      if (params.limit !== undefined) query.limit = params.limit;
      if (params.cursor) query.cursor = params.cursor;
      return client.list<PackInstallRecord>(`${base}/${id}/installs`, query);
    },
    remove(id, etag) {
      return client.remove(`${base}/${id}`, etag);
    },
    pageDoc(id, path) {
      // The page path is UNTRUSTED manifest data (the engine does not constrain its
      // grammar) whose `/` are real separators (a `{path...}` server wildcard) — so
      // it is encoded per-segment and a dot-segment refused, NEVER concatenated raw,
      // or the URL parser would collapse `../` (or `%2e%2e`) out of this pack's
      // `/pages/` namespace onto another endpoint (a sibling pack, another api/1
      // resource). The pack id rides raw: it is idSegmentRe-safe server-side.
      const safe = encodePackPagePath(path);
      if (safe === null) {
        return Promise.reject(new Error(`refusing an unsafe pack page path: "${path}"`));
      }
      return client.read<unknown>(`${base}/${id}/pages/${safe}`).then((r) => r.data);
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
