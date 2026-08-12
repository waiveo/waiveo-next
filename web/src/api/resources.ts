// api/1 typed resource modules — one narrow, typed surface per resource family
// over the shared ApiClient (client.ts). A page never hand-builds a path, a
// header, or a query string: it calls `api.schedules.update(id, patch, etag)` and
// the conventions (Problem, ETag/If-Match, Idempotency-Key, cursor pagination)
// are already applied.
//
// Types come from the generated OpenAPI (api/gen/ts/api.d.ts) where the contract
// documents a resource fully — scope-nodes and automations. The scheduling-core
// families (schedules/dayparts/playlists) and the content upload are served by
// the same generic api/1 CRUD surface but are not YET in the OpenAPI (it stubs
// every tag beyond those two, api/openapi.yaml); their Wire shapes are declared
// here to match the reference data-model/1 rows (internal/datamodel/rows.go) and
// should be regenerated once the contract adds those paths.

import type { components } from "../../../api/gen/ts/api";
import { ApiClient, RevisionConflictError, type ApiClientOptions, type Read } from "./client";
import { type Page } from "./pagination";
import { crud } from "./crud";
import { createAuthModule, type AuthModule } from "./auth";
import {
  createDevicePlaneModules,
  type AdoptedDevice,
  type AdoptedDeviceCreate,
  type AdoptedDeviceUpdate,
  type DevicesModule,
  type EntitiesModule,
} from "./devices";
import {
  createPacksModule,
  packData,
  type PackRow,
  type PackRowWrite,
  type PacksModule,
} from "./packs";
import { createCastsModule, type CastsModule, type SlideLayer } from "./casts";
import { createDiagnosticsModule, type DiagnosticsModule } from "./diagnostics";
import { createBackupModule, createJobsModule, type BackupModule, type JobsModule } from "./backup";

// ── Generated (contract-canonical) types ────────────────────────────────────

export type ScopeNode = components["schemas"]["ScopeNode"];
export type ScopeNodeCreate = components["schemas"]["ScopeNodeCreate"];
export type ScopeNodeUpdate = components["schemas"]["ScopeNodeUpdate"];

export type Automation = components["schemas"]["Automation"];
export type AutomationCreate = components["schemas"]["AutomationCreate"];
export type AutomationUpdate = components["schemas"]["AutomationUpdate"];
export type AutomationRunResult = components["schemas"]["AutomationRunResult"];

export type Screen = components["schemas"]["Screen"];
/** A screen's active push-now override — what an operator put on it out of band. */
export type ScreenNow = components["schemas"]["ScreenNow"];
/** The body naming what to push: exactly one of cast_id / playlist_id. */
export type ScreenNowRequest = components["schemas"]["ScreenNowRequest"];
/** One screen's authored identity joined to what the relays have observed of it. */
export type ScreenStatus = components["schemas"]["ScreenStatus"];
export type ScreenCreate = components["schemas"]["ScreenCreate"];
export type ScreenUpdate = components["schemas"]["ScreenUpdate"];
export type PairingCodeResult = components["schemas"]["PairingCodeResult"];

/** A named, scope-placed scalar (`data-model/1` DAT-130): the shared state a
 * `rules/1` `variable` condition reads (RUL-150) and a `variable_write` action
 * writes (RUL-220). Its `name` — not its `id` — is how a rule refers to it. */
export type Variable = components["schemas"]["Variable"];
export type VariableCreate = components["schemas"]["VariableCreate"];
export type VariableUpdate = components["schemas"]["VariableUpdate"];
/** A variable's value: a JSON scalar. `null` is NOT settable (DAT-133) — the way
 * to unset a variable is to delete the row. */
export type VariableValue = components["schemas"]["VariableValue"];

// ── Scheduling-core Wire shapes (data-model/1; not yet in the OpenAPI) ───────

/** Which content shape one playlist entry carries (DAT-041). `asset` names
 * content-addressed bytes, `playable` names a pack's content, `slide` carries
 * ONE authored layer stack inline on the item, and `cast` names an AUTHORED cast
 * row by id — the source that makes the Studio reach a TV.
 *
 * A `cast` entry is the one source that is not one-to-one with a played item:
 * at projection time it expands into one slide content item per slide of the
 * referenced cast, in authored order (internal/feeder/snapshot and
 * internal/relay/schedulehost both do this), which is exactly why the reference
 * is by id and not an inlined copy — editing the cast changes every screen
 * playing it.
 *
 * This is the CLOSED wire vocabulary, not the set of sources this console can
 * AUTHOR. `slide` was missing here while the server had shipped, stored and
 * served it for two releases, and the omission was not cosmetic: it made
 * `normalizePlaylistItem` below rebuild every inline-slide item from an empty
 * field list, so merely opening a playlist that contained one and pressing Save
 * DELETED the slide — a screen quietly one item shorter, with the operator told
 * "Saved playlist". The editor still offers only the three sources it has
 * controls for (`playlist.uis.json`); there is no layer editor here yet, and
 * offering a source whose required member nothing can fill would be a control
 * that can only ever fail. Listing it here is about round-tripping what the
 * server already holds, which is a different obligation from authoring it. */
export const PLAYLIST_ITEM_SOURCES = ["asset", "playable", "slide", "cast"] as const;
export type PlaylistItemSource = (typeof PLAYLIST_ITEM_SOURCES)[number];

/** What an `asset` item's bytes ARE, and therefore how a screen presents them
 * (DAT-041 `content_type`): an `image` is drawn as a still for the item's dwell
 * time, a `video` is played full-screen.
 *
 * It is the field that makes an uploaded video schedulable at all. Without it
 * the projections had no authored answer to read, so every asset item was served
 * as the wire's default `image` (REL-061a) and a scheduled MP4 reached the TV as
 * a Poster showing nothing. It is optional on the wire — an item that omits it
 * is still served as `image` — and only meaningful on `source: "asset"`; the
 * server refuses it on any other source rather than storing an intent nothing
 * honours, which is why it is listed under `asset` alone below. */
export type PlaylistContentType = "image" | "video";

/** A `source: "slide"` item's INLINE authored slide (DAT-041): one ordered layer
 * stack, carried on the item rather than referenced. The anonymous twin of a
 * `CastSlide` — no `id` (nothing outside the item can address it) and no
 * `duration_ms` (the item's own `duration_seconds` is its dwell time) — reusing
 * the same `SlideLayer` the cast editor draws, because an authored slide and the
 * served slide are one shape end to end. */
export interface PlaylistInlineSlide {
  layers: SlideLayer[];
}

/** A playlist item (DAT-041): `asset` (asset_ref + content_type), `playable`
 * (pack_id + content_id), `slide` (an inline `slide`) or `cast` (cast_id). */
export interface PlaylistItem {
  source: PlaylistItemSource;
  asset_ref?: string;
  content_type?: PlaylistContentType;
  pack_id?: string;
  content_id?: string;
  slide?: PlaylistInlineSlide;
  cast_id?: string;
  duration_seconds?: number;
}

/** The members that belong to each `source`, and nothing else.
 *
 * Every source in `PLAYLIST_ITEM_SOURCES` MUST have an entry, and the type says
 * so (`Record<PlaylistItemSource, …>`) rather than leaving it to a `?? []`
 * fallback in the normalizer. That fallback is what let `slide` be absent here
 * for two releases while reading as intentional: a source with no entry silently
 * normalizes to nothing but its own name, so the member that IS the item's whole
 * content was dropped on every save. A missing entry is now a compile error. */
const ITEM_FIELDS_BY_SOURCE: Record<PlaylistItemSource, readonly (keyof PlaylistItem)[]> = {
  asset: ["asset_ref", "content_type"],
  playable: ["pack_id", "content_id"],
  slide: ["slide"],
  cast: ["cast_id"],
};

/**
 * Drop the members that do not belong to an item's `source` before it is sent.
 *
 * An editor that lets an operator SWITCH an item's source leaves the previous
 * source's members behind on the object — flip an asset item to a cast and it
 * still carries `asset_ref: ""`. That matters twice over: the server validates
 * an item against its declared source, and `asset_ref` in particular is
 * reference-checked against the content origin (`validatePlaylistAssets`), so a
 * leftover from a source the item no longer declares is a stale claim about
 * content this entry does not play.
 *
 * Lives here, beside the type, rather than in the one page that edits playlists
 * today: any surface that writes an item has the same obligation, and a second
 * copy of this list is how the two drift.
 *
 * A source this build does not KNOW is passed through untouched rather than
 * rebuilt. That direction is deliberate and it is the lesson `slide` taught:
 * rebuilding from an empty field list does not "clean" an item this console does
 * not understand, it DELETES its content — and it does so on a Save the operator
 * asked for, reporting success. A console one release behind the server must be
 * able to open and re-save a playlist without destroying the parts of it that are
 * newer than itself.
 */
export function normalizePlaylistItem(item: PlaylistItem): PlaylistItem {
  const fields = ITEM_FIELDS_BY_SOURCE[item.source];
  if (!fields) return { ...item };
  const out: PlaylistItem = { source: item.source };
  for (const field of fields) {
    const value = item[field];
    if (value !== undefined) Object.assign(out, { [field]: value });
  }
  if (item.duration_seconds !== undefined) out.duration_seconds = item.duration_seconds;
  return out;
}

export interface Playlist {
  id: string;
  scope_node: string;
  name: string;
  items: PlaylistItem[];
  external_id?: string | null;
  labels?: Record<string, string>;
  revision: number;
  created_at: number;
  updated_at: number;
}
export interface PlaylistCreate {
  id?: string;
  scope_node: string;
  name: string;
  items: PlaylistItem[];
  external_id?: string | null;
  labels?: Record<string, string>;
}
export interface PlaylistUpdate {
  name?: string;
  items?: PlaylistItem[];
  external_id?: string | null;
  labels?: Record<string, string>;
}

export interface Schedule {
  id: string;
  scope_node: string;
  name: string;
  fallback_id?: string;
  priority?: number;
  misfire?: string;
  external_id?: string | null;
  labels?: Record<string, string>;
  revision: number;
  created_at: number;
  updated_at: number;
}
export interface ScheduleCreate {
  id?: string;
  scope_node: string;
  name: string;
  fallback_id?: string;
  priority?: number;
  misfire?: string;
  external_id?: string | null;
  labels?: Record<string, string>;
}
export interface ScheduleUpdate {
  name?: string;
  fallback_id?: string;
  priority?: number;
  misfire?: string;
  external_id?: string | null;
  labels?: Record<string, string>;
}

export interface Daypart {
  id: string;
  schedule_id: string;
  scope_node: string;
  days_of_week: number[];
  start_time: string;
  end_time: string;
  display_power: string;
  playlist_id?: string;
  preset_batch_id?: string;
  misfire?: string;
  name?: string;
  revision: number;
  created_at: number;
  updated_at: number;
}
export interface DaypartCreate {
  id?: string;
  schedule_id: string;
  scope_node: string;
  days_of_week: number[];
  start_time: string;
  end_time: string;
  display_power: string;
  playlist_id?: string;
  preset_batch_id?: string;
  misfire?: string;
  name?: string;
}
export interface DaypartUpdate {
  days_of_week?: number[];
  start_time?: string;
  end_time?: string;
  display_power?: string;
  playlist_id?: string;
  preset_batch_id?: string;
  misfire?: string;
  name?: string;
}

/** The content upload response (POST /api/v1/content): the server-computed,
 * content-addressed asset_ref plus the same-origin url a screen fetches from. */
export interface ContentUploadResult {
  asset_ref: string;
  url: string;
}

/** One row of the content-origin listing (GET /api/v1/content): the upload
 * response shape WIDENED with the size and store time a media browser shows and
 * sorts by (internal/app/api/content.go `contentEntry`). Deliberately the same
 * `{asset_ref, url}` vocabulary, so a ref pasted from either surface is the same
 * ref. */
export interface ContentAsset extends ContentUploadResult {
  size_bytes: number;
  stored_at: number;
}

// ── Resource module contract ────────────────────────────────────────────────

/** A resource read: the record plus its captured ETag — the If-Match a later
 * mutation carries. */
export type Resource<T> = Read<T>;

/** List query parameters. The cursor is never set by a caller directly — the
 * module threads it internally via `pages()`; `list()` accepts it only so the
 * iterator can drive the walk. */
export interface ListParams {
  selector?: string;
  limit?: number;
  cursor?: string | null;
}

/** The uniform api/1 CRUD surface every resource family shares. `update`/`remove`
 * REQUIRE an `etag`: there is no overload that omits it, so a mutation without a
 * prior read is impossible by construction (API-022 — no unconditional
 * overwrite). */
export interface ResourceModule<T, TCreate, TUpdate> {
  readonly path: string;
  list(params?: ListParams): Promise<Page<T>>;
  pages(params?: Omit<ListParams, "cursor">): AsyncGenerator<T, void, void>;
  get(id: string): Promise<Resource<T>>;
  create(body: TCreate): Promise<Resource<T>>;
  update(id: string, patch: TUpdate, etag: string): Promise<Resource<T>>;
  remove(id: string, etag: string): Promise<void>;
  /** The captured ETag for a previously-read resource, if any. */
  etagFor(id: string): string | undefined;
}

/** ETag for a resource at a known revision (API-020: the validator is derived
 * solely from `revision`, so a list row can be mutated without a re-GET). */
export function etagForRevision(revision: number): string {
  return `"${revision}"`;
}

// ── Automations: CRUD + run ──────────────────────────────────────────────────
//
// Bulk-enable (POST /automations/bulk-enable) and the Job-polling it returns
// (GET /jobs/{job_id}) are DEFERRED on the TYPED CLIENT — this module scopes
// per-automation enable/disable via If-Match PATCH, and no screen drives a
// fleet operation or polls a Job yet. Both routes are live on the server; what
// is deferred here is the client surface and the UI that would use it, so
// neither ships until it has one; resources.type-test.ts locks that out.

/** One definition a rule used to have (rules/1 RUL-394). */
export interface AutomationVersion {
  /** The revision this definition HELD — not the one that replaced it. */
  revision: number;
  superseded_at: number;
  definition: Record<string, unknown>;
}

export interface AutomationsModule extends ResourceModule<Automation, AutomationCreate, AutomationUpdate> {
  /** Run one automation now; returns its mode-evaluation disposition. */
  run(id: string, context?: Record<string, unknown>): Promise<AutomationRunResult>;
  /** The definitions this rule used to have, newest first (RUL-394). */
  versions(id: string): Promise<AutomationVersion[]>;
  /** Put an earlier definition back, as a NEW update (RUL-394).
   *
   * Does NOT change whether the rule is enabled: restoring the logic of a rule
   * you disabled while debugging is not a request for it to start firing. */
  restoreVersion(id: string, revision: number): Promise<void>;
}

function automationsModule(client: ApiClient): AutomationsModule {
  const base = crud<Automation, AutomationCreate, AutomationUpdate>(client, "/automations");
  return {
    ...base,
    run(id, context) {
      return client.action<AutomationRunResult>(
        `/automations/${encodeURIComponent(id)}/run`,
        context ? { context } : undefined,
      );
    },
    async versions(id) {
      const page = await client.list<AutomationVersion>(
        `/automations/${encodeURIComponent(id)}/versions`,
      );
      return page.items;
    },
    async restoreVersion(id, revision) {
      // `action`, so the POST carries an Idempotency-Key: a retry-on-timeout
      // must not append a second identical restore to the history.
      await client.action<unknown>(
        `/automations/${encodeURIComponent(id)}/versions/${revision}/restore`,
      );
    },
  };
}

// ── Screens: CRUD + pairing-code issuance ────────────────────────────────────

export interface ScreensModule extends ResourceModule<Screen, ScreenCreate, ScreenUpdate> {
  /** Mint a one-time pairing grant bound to this screen row and return the
   * human-enterable pairing code (or an explanatory
   * `code_unavailable_reason` when no relay is connected). The grant rides
   * the site's desired state to the relay either way; redemption happens at
   * the relay, so this reports issuance, never a "paired" state the app has
   * no evidence for. */
  issuePairingCode(id: string): Promise<PairingCodeResult>;

  /** PUSH NOW — impose this screen's program override (`data-model/1` DAT-004c):
   * show this cast, or a generated slide bearing a literal `message`, instead of
   * whatever its schedule resolves. `mode: "play"` is the everyday assignment;
   * `mode: "alert"` is the takeover. `ttl_seconds` makes it self-clearing.
   *
   * This is the ONE surface that imposes an override — `screens.update()` has no
   * `override` member, deliberately, because putting content on a wall is an
   * imperative act and not a field of a resource edit.
   *
   * It reports INTENT, not delivery. The server persists the override as desired
   * state and nudges the site's relays; the screen adopts it on its next
   * program poll (~10s) and, under `alert`, interrupts what it is showing rather
   * than waiting for it to finish. A console must not claim the screen IS
   * showing it — read `screenStatus.list()` for what the fleet has actually
   * observed. */
  pushNow(id: string, target: ScreenNowRequest): Promise<ScreenNow>;

  /** Clear this screen's push-now override; it returns to its schedule. Safe to
   * repeat — clearing nothing succeeds. */
  clearNow(id: string): Promise<void>;
}

function screensModule(client: ApiClient): ScreensModule {
  const base = crud<Screen, ScreenCreate, ScreenUpdate>(client, "/screens");
  const nowPath = (id: string) => `/screens/${encodeURIComponent(id)}/now`;
  return {
    ...base,
    issuePairingCode(id) {
      return client.action<PairingCodeResult>(`/screens/${encodeURIComponent(id)}/pairing-code`);
    },
    pushNow(id, target) {
      return client.replace<ScreenNow>(nowPath(id), target);
    },
    clearNow(id) {
      return client.discard(nowPath(id));
    },
  };
}

// ── Screen status: what the relays have OBSERVED ─────────────────────────────

export interface ScreenStatusModule {
  /** Every authored screen, joined to what the relays have observed of it and to
   * any push-now override on it. Paginated like every other list.
   *
   * Read the ages, not the words: each `*_age_ms` is milliseconds before the
   * response and `-1` means NEVER observed, which is a different state from a
   * large age. `reachability` is `live | fetching | stale | never_seen` and
   * deliberately never "offline" — the platform cannot tell a screen that is
   * switched off from one whose network dropped from one whose player crashed.
   * `fetching` is a screen that was handed a program and has not acknowledged
   * it inside `content_transfer_window_ms`: it is downloading content while the
   * previous program stays on the wall, so it is neither healthy-confirmed nor
   * a screen to go and look at. */
  list(params?: ListParams): Promise<Page<ScreenStatus>>;
}

function screenStatusModule(client: ApiClient): ScreenStatusModule {
  return {
    list(params = {}) {
      // The same query assembly crud() performs, and for the same reason its own
      // comment gives: an EMPTY cursor is not a keyset position and the server
      // rejects it (API-035), so the first page omits the key entirely.
      const query: Record<string, string | number | undefined> = {};
      if (params.selector) query.selector = params.selector;
      if (params.limit !== undefined) query.limit = params.limit;
      if (params.cursor) query.cursor = params.cursor;
      return client.list<ScreenStatus>("/screen-status", query);
    },
  };
}

// ── Content upload ───────────────────────────────────────────────────────────

export interface ContentModule {
  /** Upload raw bytes to the content origin; returns the content-addressed
   * {asset_ref, url}. */
  upload(bytes: BodyInit, contentType?: string): Promise<ContentUploadResult>;
  /** Every asset the origin currently serves — the read half upload used to
   * lack. A media browser and the Studio's image picker resolve against this. */
  list(): Promise<ContentAsset[]>;
}

function contentModule(client: ApiClient): ContentModule {
  return {
    upload(bytes, contentType) {
      return client.upload<ContentUploadResult>("/content", bytes, contentType);
    },
    async list() {
      // NOT `client.list`, and the difference is the server's, not a shortcut
      // here: the content origin answers `{content: [...]}` with no cursor
      // because it is a DIRECTORY of the origin (digest-ordered, complete),
      // not a keyset-paginated resource family. Feeding it through the paging
      // verb would look for an `items`/`cursor` pair that is never sent and
      // hand every caller an empty library. `read` is the plain
      // GET-and-parse-JSON verb; the ETag it also captures is unused here
      // (the listing is not a mutable resource) and harmless.
      const res = await client.read<{ content?: ContentAsset[] }>("/content");
      return res.data.content ?? [];
    },
  };
}

// ── The composed console API ─────────────────────────────────────────────────

export interface WaiveoApi {
  client: ApiClient;
  /** Sign in / sign out / read the current session, plus the first-boot claim
   * (`security-model.md`). No token handling — see api/auth.ts. */
  auth: AuthModule;
  scopeNodes: ResourceModule<ScopeNode, ScopeNodeCreate, ScopeNodeUpdate>;
  /** Screen identity rows (the row a `screen_id` names) — CRUD plus the
   * pairing-code issuance the operator pairing flow drives. */
  screens: ScreensModule;
  /** What the site's relays have OBSERVED each screen doing — a read model, not
   * authored state, so list-only (api/screenstatus semantics live on the
   * module). It is the honest half of the push-now flow: `screens.pushNow`
   * states intent, this reports what the fleet actually did with it. */
  screenStatus: ScreenStatusModule;
  schedules: ResourceModule<Schedule, ScheduleCreate, ScheduleUpdate>;
  dayparts: ResourceModule<Daypart, DaypartCreate, DaypartUpdate>;
  playlists: ResourceModule<Playlist, PlaylistCreate, PlaylistUpdate>;
  automations: AutomationsModule;
  /** Named, scope-placed scalars (`data-model/1` DAT-130–138) — the shared state
   * a rule's `variable` condition reads and its `variable_write` action writes.
   * Plain CRUD: a variable has no operation beyond the resource envelope, and
   * deliberately no value-only write path (see the `/variables` note in
   * `api/openapi.yaml`). */
  variables: ResourceModule<Variable, VariableCreate, VariableUpdate>;
  content: ContentModule;
  /** Devices a relay has DISCOVERED behind it — a read model the relay owns, so
   * list plus the single `adopt` operation and nothing else (api/devices.ts
   * explains why it is not a ResourceModule). */
  devices: DevicesModule;
  /** The entities those devices expose, plus the entity-addressed command
   * operation the virtual remote and rules/1 both dispatch through. */
  entities: EntitiesModule;
  /** The AUTHORED adoption records (DAT-004a). Discovery lists a device;
   * creating one of these rows is what adopts it. */
  adoptedDevices: ResourceModule<AdoptedDevice, AdoptedDeviceCreate, AdoptedDeviceUpdate>;
  /** Authored native-slide documents — the Studio's resource family. Its shapes
   * and its path live entirely in api/casts.ts (see that file's header: the
   * server routes are landing in parallel, so the guesswork is quarantined). */
  casts: CastsModule;
  /** Installed declarative packs — list/get/install/uninstall + page docs and
   * locale catalogs (manifest/1). */
  packs: PacksModule;
  /** The api/1 CRUD surface for one pack collection's universal-envelope rows
   * (MAN-051/052) — the SAME ResourceModule shape every core family uses, so a
   * pack's data is a first-class citizen (If-Match, Idempotency-Key, cursor). */
  packData(packId: string, collection: string): ResourceModule<PackRow, PackRowWrite, PackRowWrite>;
  /** The operator diagnostics reads (parity row 7.4): this box's own captured
   * log and its health summary. Both are `owner`-only and neither is authored
   * state, so this is not a ResourceModule — see api/diagnostics.ts. */
  diagnostics: DiagnosticsModule;
  /** Workspace backup (parity row 7.5): export, the containers that exist,
   * their download URLs, and restore. Not a ResourceModule — no id, no
   * revision, and the subject of all four is the workspace itself. */
  backup: BackupModule;
  /** Job polling (API-112). It ships now because a screen finally drives an
   * async operation: the backup page polls an export and a restore to a
   * terminal state. Deliberately its own module rather than a member of
   * `automations`, whose bulk-enable Job remains deferred with no UI. */
  jobs: JobsModule;
}

/** Build the whole console API surface over one ApiClient (one shared ETag map,
 * one trace capture). */
export function createApi(opts?: ApiClientOptions): WaiveoApi {
  const client = new ApiClient(opts);
  return {
    client,
    ...createDevicePlaneModules(client),
    auth: createAuthModule(client),
    scopeNodes: crud<ScopeNode, ScopeNodeCreate, ScopeNodeUpdate>(client, "/scope-nodes"),
    screens: screensModule(client),
    screenStatus: screenStatusModule(client),
    schedules: crud<Schedule, ScheduleCreate, ScheduleUpdate>(client, "/schedules"),
    dayparts: crud<Daypart, DaypartCreate, DaypartUpdate>(client, "/dayparts"),
    playlists: crud<Playlist, PlaylistCreate, PlaylistUpdate>(client, "/playlists"),
    automations: automationsModule(client),
    variables: crud<Variable, VariableCreate, VariableUpdate>(client, "/variables"),
    content: contentModule(client),
    casts: createCastsModule(client),
    packs: createPacksModule(client),
    packData: (packId, collection) => packData(client, packId, collection),
    diagnostics: createDiagnosticsModule(client),
    backup: createBackupModule(client),
    jobs: createJobsModule(client),
  };
}

// ── The standard optimistic-concurrency conflict flow ────────────────────────

/** The outcome of `updateWithReview`: either the update landed, or the resource
 * had changed elsewhere and the CURRENT server state is returned for review. */
export type UpdateOutcome<T> =
  | { status: "updated"; resource: Resource<T> }
  | { status: "conflict"; conflict: RevisionConflictError; current: Resource<T> };

/** The api/1 standard conflict flow, once, so every editor runs it identically:
 * attempt the update carrying the ETag last read; on a 412 REVISION_CONFLICT
 * re-READ the current state and return it for the user to REVIEW — NEVER a silent
 * retry that overwrites the change made elsewhere (API-022). The caller shows a
 * "changed elsewhere — review and retry" affordance from `current` and only
 * re-submits once the user has reconciled against `current.etag`. */
export async function updateWithReview<T, TCreate, TUpdate>(
  mod: ResourceModule<T, TCreate, TUpdate>,
  id: string,
  patch: TUpdate,
  etag: string,
): Promise<UpdateOutcome<T>> {
  try {
    const resource = await mod.update(id, patch, etag);
    return { status: "updated", resource };
  } catch (err) {
    if (err instanceof RevisionConflictError) {
      const current = await mod.get(id);
      return { status: "conflict", conflict: err, current };
    }
    throw err;
  }
}
