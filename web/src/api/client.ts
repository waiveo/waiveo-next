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
//   - Session + CSRF (security-model/1 SEC-023/024): the session rides an
//     HttpOnly, host-only cookie the browser attaches to every same-origin
//     request automatically — there is no token for this client to hold, and
//     deliberately so, since a token it could read is a token a script injection
//     could steal. What it DOES carry is the double-submit CSRF token: a
//     companion, script-readable cookie whose value every mutating request
//     echoes in `X-CSRF-Token`. SameSite alone is explicitly not sufficient
//     (SEC-024), so this header is not optional decoration — a mutating request
//     without it is refused 403.
//   - 401 handling: an UNAUTHENTICATED response means the session is gone
//     (expired, revoked, never established). The client raises that ONCE through
//     an injectable `onUnauthenticated` hook so the shell can send the operator
//     to sign in, rather than every page inventing its own redirect.
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

/** The script-readable cookie carrying the double-submit CSRF token, and the
 * header a mutating request echoes it in (security-model/1 SEC-024). The names
 * match the server's own constants (internal/app/auth/middleware.go). */
export const CSRF_COOKIE = "csrf_token";
export const CSRF_HEADER = "X-CSRF-Token";

/** Reads a cookie by name from `document.cookie`.
 *
 * The CSRF cookie is deliberately NOT HttpOnly — the page's own script must be
 * able to read it in order to echo it back, and that readability is the
 * mechanism rather than a weakness: a cross-site page can cause the browser to
 * SEND the cookie but cannot READ it, which is exactly the asymmetry
 * double-submit exploits. The session cookie itself stays HttpOnly and is never
 * read here. */
export function readCookie(name: string, cookieString?: string): string | null {
  const raw = cookieString ?? (typeof document === "undefined" ? "" : document.cookie);
  for (const part of raw.split(";")) {
    const [k, ...rest] = part.trim().split("=");
    if (k === name) return decodeURIComponent(rest.join("="));
  }
  return null;
}

/** HTTP methods that cannot change state, and so carry no CSRF requirement. */
const SAFE_METHODS = new Set(["GET", "HEAD", "OPTIONS"]);

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
  /** Called when a response is 401 UNAUTHENTICATED — the session expired, was
   * revoked, or never existed. The default sends the browser to the sign-in
   * page, preserving where the operator was so they land back there. Injectable
   * so a test can observe the call without navigating, and so the shell can
   * substitute an in-app transition. It fires at most once per client, since a
   * page issuing four parallel reads should produce one redirect, not four. */
  onUnauthenticated?: () => void;
  /** Reads the CSRF cookie; defaults to `document.cookie`. Injectable for tests
   * running without a document. */
  readCsrfToken?: () => string | null;
}

/** The default 401 response: navigate to the sign-in page, carrying the current
 * location so the operator returns to it after signing in. */
function redirectToLogin(): void {
  if (typeof window === "undefined") return;
  const here = window.location.pathname + window.location.search;
  const next = here && here !== "/login" ? `?next=${encodeURIComponent(here)}` : "";
  window.location.assign(`/login${next}`);
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
  private readonly onUnauthenticated: () => void;
  private readonly readCsrf: () => string | null;
  /** Guards the 401 hook so N parallel in-flight requests produce ONE redirect. */
  private unauthenticatedRaised = false;

  constructor(opts: ApiClientOptions = {}) {
    this.baseUrl = (opts.baseUrl ?? API_BASE).replace(/\/$/, "");
    const f = opts.fetch ?? globalThis.fetch;
    this.doFetch = f.bind(globalThis);
    this.newKey = opts.newIdempotencyKey ?? (() => crypto.randomUUID());
    this.onUnauthenticated = opts.onUnauthenticated ?? redirectToLogin;
    this.readCsrf = opts.readCsrfToken ?? (() => readCookie(CSRF_COOKIE));
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
    // SEC-024's double-submit half, on every mutating request. The session
    // cookie itself needs no plumbing — the browser attaches it same-origin.
    if (!SAFE_METHODS.has(method.toUpperCase())) {
      const csrf = this.readCsrf();
      if (csrf) headers.set(CSRF_HEADER, csrf);
    }

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
   * body exactly once (the callers only reach here on `!res.ok`).
   *
   * `credentialExchange` suppresses the 401 hook — see `exchange`. */
  private async fail(res: Response, credentialExchange = false): Promise<never> {
    const traceId = res.headers.get("Trace-Id");
    let problem: Problem | null = null;
    try {
      problem = (await res.json()) as Problem;
    } catch {
      // Non-JSON (or empty) error body — there is no Problem to parse; the status
      // code alone drives the error below.
    }
    // A 401 means the session is gone. Raise it once so the shell can send the
    // operator to sign in, then still throw — the caller's own error path (a
    // toast, an error card) runs as it would for any other failure, because a
    // redirect is not instantaneous and a page must not be left mid-render
    // pretending the read succeeded.
    if (res.status === 401 && !credentialExchange) {
      if (!this.unauthenticatedRaised) {
        this.unauthenticatedRaised = true;
        this.onUnauthenticated();
      }
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

  /** POST a JSON body and read a JSON response, with NO Idempotency-Key.
   *
   * The `auth` family's verb. An Idempotency-Key is scoped to the AUTHENTICATED
   * principal (API-051), so it is meaningless on an operation whose caller does
   * not yet hold a session — a key there would be scoped to nothing, shared by
   * every anonymous caller at once.
   *
   * A 401 here keeps its ordinary meaning (the session is gone) and raises the
   * unauthenticated hook. The `security: []` operations for which it CANNOT mean
   * that use `exchange` instead — all three of them, which `exchange` names. */
  async post<T>(path: string, body: unknown): Promise<T> {
    const res = await this.send("POST", path, {
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    if (!res.ok) await this.fail(res);
    return (await res.json()) as T;
  }

  /** POST a credential-exchange operation — an api/1 `security: []` route — and
   * read its JSON response.
   *
   * api/1 declares `security: []` on exactly THREE operations (API-090/091):
   * `/auth/login`, `/auth/setup`, and `/auth/credential-reset/redeem`. The first
   * two are bound to this method. The third has no binding in this client at
   * all, and when it gets one it must NOT be bound to `postNoContent`, which is
   * where its shape would otherwise send an implementer — see that method.
   *
   * Identical to `post` in every respect but one: a 401 does NOT raise the
   * unauthenticated hook. That hook means "the session you were using is gone,
   * go and sign in", and it cannot mean that here — these operations never had a
   * session to lose, and their 401 is the refusal of the credentials presented
   * in this very request. Raising it navigates the page that is collecting those
   * credentials away from itself, which is worse than useless: a wrong password
   * or a wrong setup code reloads the form and takes the message explaining what
   * to fix with it, so the operator sees a blank form and no reason. The error is
   * still THROWN, so the page reports it exactly as it reports any other refusal.
   *
   * Same exemption, and the same reasoning, as `getOrNull`'s for the session
   * probe: a 401 that is an ANSWER is not a 401 that is a lost session. */
  async exchange<T>(path: string, body: unknown): Promise<T> {
    const res = await this.send("POST", path, {
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    if (!res.ok) await this.fail(res, true);
    return (await res.json()) as T;
  }

  /** POST expecting a 204 — the logout verb, and ONLY the logout verb.
   *
   * Like `post` and unlike `exchange`, a 401 here raises the unauthenticated
   * hook. That is right for logout, where a 401 genuinely does mean the session
   * is already gone, and wrong for every `security: []` operation, for the reason
   * `exchange` sets out at length.
   *
   * Which makes this a trap worth marking rather than a gap worth pre-filling.
   * api/1's third `security: []` operation, `/auth/credential-reset/redeem`,
   * answers 204 — so it is the one route needing a no-content verb AND the hook
   * exemption at once, and this method offers only the first half. Binding it
   * here would navigate the page collecting the new password away from itself the
   * moment a reset code is refused: precisely the bug `exchange` exists to
   * prevent, on the one flow whose caller is BY DEFINITION someone who cannot
   * sign in — so the redirect strands them on a form they have no way through.
   *
   * What it needs is an `exchangeNoContent`: `send`, then `fail(res, true)` on a
   * non-2xx, and no body read. That method is deliberately NOT written here. An
   * unused verb is dead code this repo's reachability gate would have to carry a
   * baseline entry for, and a marked trap outlives a speculative method — write
   * it WITH the binding that needs it, not before. */
  async postNoContent(path: string): Promise<void> {
    const res = await this.send("POST", path);
    if (!res.ok) await this.fail(res);
  }

  /** GET a resource, returning null on a 401 instead of throwing OR raising the
   * unauthenticated hook.
   *
   * This is the session probe, and the exemption is deliberate rather than a
   * convenience: "am I signed in?" is precisely the question whose answer is a
   * 401, so treating that as a failure would be wrong, and routing it through
   * the redirect hook would send the sign-in page itself to the sign-in page in
   * a loop. Every other 401 in this client keeps its normal meaning. */
  async getOrNull<T>(path: string): Promise<T | null> {
    const res = await this.send("GET", path);
    if (res.status === 401) return null;
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
