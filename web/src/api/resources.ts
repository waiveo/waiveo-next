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

// ── Generated (contract-canonical) types ────────────────────────────────────

export type ScopeNode = components["schemas"]["ScopeNode"];
export type ScopeNodeCreate = components["schemas"]["ScopeNodeCreate"];
export type ScopeNodeUpdate = components["schemas"]["ScopeNodeUpdate"];

export type Automation = components["schemas"]["Automation"];
export type AutomationCreate = components["schemas"]["AutomationCreate"];
export type AutomationUpdate = components["schemas"]["AutomationUpdate"];
export type AutomationRunResult = components["schemas"]["AutomationRunResult"];

export type Screen = components["schemas"]["Screen"];
export type ScreenCreate = components["schemas"]["ScreenCreate"];
export type ScreenUpdate = components["schemas"]["ScreenUpdate"];
export type PairingCodeResult = components["schemas"]["PairingCodeResult"];

// ── Scheduling-core Wire shapes (data-model/1; not yet in the OpenAPI) ───────

/** A playlist item (DAT-041): `asset` (asset_ref present) or `playable` (pack). */
export interface PlaylistItem {
  source: "asset" | "playable";
  asset_ref?: string;
  pack_id?: string;
  content_id?: string;
  duration_seconds?: number;
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

export interface AutomationsModule extends ResourceModule<Automation, AutomationCreate, AutomationUpdate> {
  /** Run one automation now; returns its mode-evaluation disposition. */
  run(id: string, context?: Record<string, unknown>): Promise<AutomationRunResult>;
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
}

function screensModule(client: ApiClient): ScreensModule {
  const base = crud<Screen, ScreenCreate, ScreenUpdate>(client, "/screens");
  return {
    ...base,
    issuePairingCode(id) {
      return client.action<PairingCodeResult>(`/screens/${encodeURIComponent(id)}/pairing-code`);
    },
  };
}

// ── Content upload ───────────────────────────────────────────────────────────

export interface ContentModule {
  /** Upload raw bytes to the content origin; returns the content-addressed
   * {asset_ref, url}. */
  upload(bytes: BodyInit, contentType?: string): Promise<ContentUploadResult>;
}

function contentModule(client: ApiClient): ContentModule {
  return {
    upload(bytes, contentType) {
      return client.upload<ContentUploadResult>("/content", bytes, contentType);
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
  schedules: ResourceModule<Schedule, ScheduleCreate, ScheduleUpdate>;
  dayparts: ResourceModule<Daypart, DaypartCreate, DaypartUpdate>;
  playlists: ResourceModule<Playlist, PlaylistCreate, PlaylistUpdate>;
  automations: AutomationsModule;
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
  /** Installed declarative packs — list/get/install/uninstall + page docs and
   * locale catalogs (manifest/1). */
  packs: PacksModule;
  /** The api/1 CRUD surface for one pack collection's universal-envelope rows
   * (MAN-051/052) — the SAME ResourceModule shape every core family uses, so a
   * pack's data is a first-class citizen (If-Match, Idempotency-Key, cursor). */
  packData(packId: string, collection: string): ResourceModule<PackRow, PackRowWrite, PackRowWrite>;
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
    schedules: crud<Schedule, ScheduleCreate, ScheduleUpdate>(client, "/schedules"),
    dayparts: crud<Daypart, DaypartCreate, DaypartUpdate>(client, "/dayparts"),
    playlists: crud<Playlist, PlaylistCreate, PlaylistUpdate>(client, "/playlists"),
    automations: automationsModule(client),
    content: contentModule(client),
    packs: createPacksModule(client),
    packData: (packId, collection) => packData(client, packId, collection),
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
