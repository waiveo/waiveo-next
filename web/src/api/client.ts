// api/1 typed fetch client — the single door the console talks to the feeder
// through, implementing the cross-cutting conventions once so no page re-derives
// one (contracts/api-1.md).
//
// The conventions, as CLIENT LAW:
//   - Problem (RFC 9457): EVERY non-2xx body is parsed into a Problem and thrown
//     as an ApiError carrying `code`/`detail`/`trace_id`; a 422 VALIDATION_FAILED
//     additionally maps its `errors[]` onto a field→message map for form fields.
//   - Optimistic concurrency (ETag/If-Match): every single-resource response's
//     ETag is captured; an update/delete MUST carry an If-Match (there is no
//     unconditional-overwrite path — the resource modules require the ETag by
//     type). A 412 REVISION_CONFLICT is surfaced as a distinct
//     RevisionConflictError so the UI can run the standard re-read/review flow
//     (never a silent retry-overwrite).
//   - Idempotency-Key: a create (and any retriable mutating POST) carries a
//     freshly generated key so a retry-on-timeout cannot double-create.
//   - Trace-Id: every response's Trace-Id is captured (also onto each error) so a
//     toast can quote the id that correlates the failure server-side.
//
// The generated OpenAPI types (api/gen/ts/api.d.ts, produced by
// openapi-typescript) are the source of truth for the Problem/ErrorCode shapes.

import type { components } from "../../../api/gen/ts/api";

/** RFC 9457 problem+json, extended by api/1 with `code` + `trace_id` (API-010). */
export type Problem = components["schemas"]["Problem"];
/** The stable, closed machine-readable error registry (API-011). */
export type ErrorCode = components["schemas"]["ErrorCode"];
/** One field-level failure inside a VALIDATION_FAILED Problem's `errors` array. */
export type ValidationFieldError = components["schemas"]["ValidationFieldError"];

/** Field errors keyed by the offending field's dot-path (or query-parameter
 * name) — the shape a form maps straight onto its FormField error slots. */
export type FieldErrors = Record<string, string>;

/** The api/1 versioned mount every operation hangs off. Same-origin: the feeder
 * serves the SPA, this API, and the event stream on ONE origin, so the default is
 * a root-relative path with no host (the vite dev proxy covers `web-dev`). */
export const API_BASE = "/api/v1";

/** Every non-2xx api/1 response, parsed. Carries the machine-readable `code` a
 * caller asserts on, the human `detail`/`title`, the `traceId` for a toast, and —
 * for VALIDATION_FAILED — `fieldErrors` mapped from the Problem's `errors[]`. */
export class ApiError extends Error {
  readonly status: number;
  readonly code: string;
  readonly detail: string | undefined;
  readonly title: string | undefined;
  readonly traceId: string | null;
  readonly problem: Problem | null;
  readonly fieldErrors: FieldErrors;

  constructor(status: number, problem: Problem | null, traceId: string | null) {
    super(problem?.detail ?? problem?.title ?? `Request failed with status ${status}`);
    this.name = "ApiError";
    this.status = status;
    this.code = problem?.code ?? "INTERNAL";
    this.detail = problem?.detail;
    this.title = problem?.title;
    this.traceId = traceId ?? problem?.trace_id ?? null;
    this.problem = problem;
    this.fieldErrors = mapFieldErrors(problem);
  }
}

/** A 412 REVISION_CONFLICT: the resource changed elsewhere since it was read. A
 * distinct type so a caller runs the STANDARD conflict flow — re-read the current
 * state and present it for review — rather than blindly retrying (API-022/023:
 * there is no unconditional overwrite). `currentRevision` is the server's current
 * revision, carried on the Problem so the UI can name what changed. */
export class RevisionConflictError extends ApiError {
  readonly currentRevision: number | undefined;

  constructor(status: number, problem: Problem | null, traceId: string | null) {
    super(status, problem, traceId);
    this.name = "RevisionConflictError";
    this.currentRevision = problem?.current_revision;
  }
}

function mapFieldErrors(problem: Problem | null): FieldErrors {
  const out: FieldErrors = {};
  if (problem?.code === "VALIDATION_FAILED" && Array.isArray(problem.errors)) {
    for (const e of problem.errors) out[e.field] = e.message;
  }
  return out;
}

/** A single resource read: the record plus its captured ETag — the If-Match a
 * later update/delete carries. Returning them together is what makes "mutate
 * without first reading" impossible by construction. */
export interface Read<T> {
  data: T;
  etag: string;
}

type QueryValue = string | number | undefined;

interface SendOptions {
  headers?: Record<string, string>;
  body?: BodyInit | null;
  ifMatch?: string;
  idempotencyKey?: string;
  query?: Record<string, QueryValue>;
}

export interface ApiClientOptions {
  /** API base; defaults to the same-origin `/api/v1`. Tests pass an absolute
   * documentation-range base so the mock server intercepts a parseable URL. */
  baseUrl?: string;
  /** Injectable fetch (defaults to the global). */
  fetch?: typeof fetch;
  /** Idempotency-Key generator; defaults to `crypto.randomUUID`. Injectable so a
   * test can assert a create carried a specific key. */
  newIdempotencyKey?: () => string;
}

/** The low-level api/1 transport. The typed per-resource modules (resources.ts)
 * are the surface pages use; this class owns the conventions they share. */
export class ApiClient {
  readonly baseUrl: string;
  /** Captured ETags, keyed by resource path (`/scope-nodes/<id>`). Refreshed on
   * every response that carries one, so the get→update flow always has the latest
   * If-Match without a page threading it by hand. */
  readonly etags = new Map<string, string>();
  /** The Trace-Id of the most recent response (success or error). */
  lastTraceId: string | null = null;

  private readonly doFetch: typeof fetch;
  private readonly newKey: () => string;

  constructor(opts: ApiClientOptions = {}) {
    this.baseUrl = (opts.baseUrl ?? API_BASE).replace(/\/$/, "");
    const f = opts.fetch ?? globalThis.fetch;
    this.doFetch = f.bind(globalThis);
    this.newKey = opts.newIdempotencyKey ?? (() => crypto.randomUUID());
  }

  /** The captured ETag for a resource path, if one has been observed. */
  etagFor(path: string): string | undefined {
    return this.etags.get(path);
  }

  private buildUrl(path: string, query?: Record<string, QueryValue>): string {
    let url = this.baseUrl + path;
    if (query) {
      const qs = new URLSearchParams();
      for (const [k, v] of Object.entries(query)) {
        if (v !== undefined && v !== "") qs.set(k, String(v));
      }
      const s = qs.toString();
      if (s) url += "?" + s;
    }
    return url;
  }

  private async send(method: string, path: string, opts: SendOptions = {}): Promise<Response> {
    const headers = new Headers(opts.headers);
    if (opts.ifMatch !== undefined) headers.set("If-Match", opts.ifMatch);
    if (opts.idempotencyKey !== undefined) headers.set("Idempotency-Key", opts.idempotencyKey);

    const res = await this.doFetch(this.buildUrl(path, opts.query), {
      method,
      headers,
      body: opts.body ?? null,
    });

    const trace = res.headers.get("Trace-Id");
    if (trace) this.lastTraceId = trace;
    const etag = res.headers.get("ETag");
    if (etag) this.etags.set(path, etag);
    return res;
  }

  /** Parse a non-2xx body into a Problem and throw the matching error. Reads the
   * body exactly once (the callers only reach here on `!res.ok`). */
  private async fail(res: Response): Promise<never> {
    const traceId = res.headers.get("Trace-Id");
    let problem: Problem | null = null;
    try {
      problem = (await res.json()) as Problem;
    } catch {
      // Non-JSON (or empty) error body — there is no Problem to parse; the status
      // code alone drives the error below.
    }
    if (res.status === 412 || problem?.code === "REVISION_CONFLICT") {
      throw new RevisionConflictError(res.status, problem, traceId);
    }
    throw new ApiError(res.status, problem, traceId);
  }

  private etagOf(res: Response): string {
    // A single mutable-resource response always carries a strong ETag (API-020);
    // the empty fallback only guards a contract violation and would itself 412.
    return res.headers.get("ETag") ?? "";
  }

  // ── Typed verbs the resource modules build on ────────────────────────────

  /** GET a single resource; captures + returns its ETag. */
  async read<T>(path: string): Promise<Read<T>> {
    const res = await this.send("GET", path);
    if (!res.ok) await this.fail(res);
    return { data: (await res.json()) as T, etag: this.etagOf(res) };
  }

  /** GET a keyset page (`{items, cursor}`); `query` carries cursor/limit/selector. */
  async list<T>(path: string, query?: Record<string, QueryValue>): Promise<{ items: T[]; cursor: string | null }> {
    const res = await this.send("GET", path, query ? { query } : {});
    if (!res.ok) await this.fail(res);
    return (await res.json()) as { items: T[]; cursor: string | null };
  }

  /** POST a create; carries a fresh Idempotency-Key and returns the ETag. */
  async create<T>(path: string, body: unknown): Promise<Read<T>> {
    const res = await this.send("POST", path, {
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
      idempotencyKey: this.newKey(),
    });
    if (!res.ok) await this.fail(res);
    return { data: (await res.json()) as T, etag: this.etagOf(res) };
  }

  /** PATCH a resource under an If-Match precondition; returns the new ETag. */
  async update<T>(path: string, body: unknown, etag: string): Promise<Read<T>> {
    const res = await this.send("PATCH", path, {
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
      ifMatch: etag,
    });
    if (!res.ok) await this.fail(res);
    return { data: (await res.json()) as T, etag: this.etagOf(res) };
  }

  /** DELETE a resource under an If-Match precondition. */
  async remove(path: string, etag: string): Promise<void> {
    const res = await this.send("DELETE", path, { ifMatch: etag });
    if (!res.ok) await this.fail(res);
  }

  /** POST a non-create mutating action (run, bulk-enable, …); carries an
   * Idempotency-Key so a retry-on-timeout cannot double-fire it. */
  async action<T>(path: string, body?: unknown): Promise<T> {
    const res = await this.send("POST", path, {
      headers: body === undefined ? {} : { "Content-Type": "application/json" },
      body: body === undefined ? null : JSON.stringify(body),
      idempotencyKey: this.newKey(),
    });
    if (!res.ok) await this.fail(res);
    return (await res.json()) as T;
  }

  /** POST raw bytes (a content upload); carries an Idempotency-Key. Content is
   * content-addressed server-side, so a repost of identical bytes is inherently
   * idempotent — the key guards the retry window regardless. */
  async upload<T>(path: string, bytes: BodyInit, contentType?: string): Promise<T> {
    const res = await this.send("POST", path, {
      headers: contentType ? { "Content-Type": contentType } : {},
      body: bytes,
      idempotencyKey: this.newKey(),
    });
    if (!res.ok) await this.fail(res);
    return (await res.json()) as T;
  }
}
