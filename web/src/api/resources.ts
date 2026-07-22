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
import { paginate, type Page, type PageFetcher } from "./pagination";

// ── Generated (contract-canonical) types ────────────────────────────────────

export type ScopeNode = components["schemas"]["ScopeNode"];
export type ScopeNodeCreate = components["schemas"]["ScopeNodeCreate"];
export type ScopeNodeUpdate = components["schemas"]["ScopeNodeUpdate"];

export type Automation = components["schemas"]["Automation"];
export type AutomationCreate = components["schemas"]["AutomationCreate"];
export type AutomationUpdate = components["schemas"]["AutomationUpdate"];
export type AutomationRunResult = components["schemas"]["AutomationRunResult"];
export type Job = components["schemas"]["Job"];

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

function crud<T, TCreate, TUpdate>(client: ApiClient, path: string): ResourceModule<T, TCreate, TUpdate> {
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
    pages(params = {}) {
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

// ── Automations: CRUD + run + bulk-enable + the Job it returns ───────────────

export interface AutomationsModule extends ResourceModule<Automation, AutomationCreate, AutomationUpdate> {
  /** Run one automation now; returns its mode-evaluation disposition. */
  run(id: string, context?: Record<string, unknown>): Promise<AutomationRunResult>;
  /** Enable/disable a selector-matched set; returns a Job to poll. */
  bulkEnable(selector: string, enabled: boolean): Promise<Job>;
  /** Poll a Job returned by a fleet-mutating operation. */
  getJob(jobId: string): Promise<Job>;
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
    bulkEnable(selector, enabled) {
      return client.action<Job>("/automations/bulk-enable", { selector, enabled });
    },
    async getJob(jobId) {
      const r = await client.read<Job>(`/jobs/${encodeURIComponent(jobId)}`);
      return r.data;
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
  scopeNodes: ResourceModule<ScopeNode, ScopeNodeCreate, ScopeNodeUpdate>;
  schedules: ResourceModule<Schedule, ScheduleCreate, ScheduleUpdate>;
  dayparts: ResourceModule<Daypart, DaypartCreate, DaypartUpdate>;
  playlists: ResourceModule<Playlist, PlaylistCreate, PlaylistUpdate>;
  automations: AutomationsModule;
  content: ContentModule;
}

/** Build the whole console API surface over one ApiClient (one shared ETag map,
 * one trace capture). */
export function createApi(opts?: ApiClientOptions): WaiveoApi {
  const client = new ApiClient(opts);
  return {
    client,
    scopeNodes: crud<ScopeNode, ScopeNodeCreate, ScopeNodeUpdate>(client, "/scope-nodes"),
    schedules: crud<Schedule, ScheduleCreate, ScheduleUpdate>(client, "/schedules"),
    dayparts: crud<Daypart, DaypartCreate, DaypartUpdate>(client, "/dayparts"),
    playlists: crud<Playlist, PlaylistCreate, PlaylistUpdate>(client, "/playlists"),
    automations: automationsModule(client),
    content: contentModule(client),
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
