// @vitest-environment node
//
// The api/1 conventions proven as CLIENT LAW against a mocked server (msw): every
// non-2xx is a parsed Problem, a 422 maps field errors, a create carries an
// Idempotency-Key, an update carries If-Match and a 412 becomes a distinct
// RevisionConflictError WITHOUT any silent retry-overwrite, and Trace-Id/ETag are
// captured off every response.

import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";
import { http } from "msw";
import { setupServer } from "msw/node";
import { ApiClient, ApiError, RevisionConflictError } from "./client";
import { TEST_BASE, TRACE_ID, ULID_A, etag, ok, problem, scopeNode } from "./test-support";

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

function client() {
  return new ApiClient({ baseUrl: TEST_BASE });
}

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

describe("Problem parsing (API-010/011)", () => {
  it("surfaces code + detail + trace_id on a 404", async () => {
    server.use(
      http.get(`${TEST_BASE}/scope-nodes/${ULID_A}`, () =>
        problem(404, "NOT_FOUND", "No scope node exists with this identifier."),
      ),
    );
    const err = await client()
      .read(`/scope-nodes/${ULID_A}`)
      .then(() => null)
      .catch((e: unknown) => e);

    expect(err).toBeInstanceOf(ApiError);
    const api = err as ApiError;
    expect(api.status).toBe(404);
    expect(api.code).toBe("NOT_FOUND");
    expect(api.detail).toBe("No scope node exists with this identifier.");
    expect(api.traceId).toBe(TRACE_ID);
  });

  it("maps a 422 VALIDATION_FAILED errors[] onto field errors", async () => {
    server.use(
      http.post(`${TEST_BASE}/scope-nodes`, () =>
        problem(422, "VALIDATION_FAILED", "One or more fields failed validation.", {
          errors: [
            { field: "name", code: "REQUIRED", message: "Name is required." },
            { field: "tz", code: "INVALID", message: "Not an IANA time zone." },
          ],
        }),
      ),
    );
    const err = (await client()
      .create("/scope-nodes", { kind: "screen" })
      .catch((e: unknown) => e)) as ApiError;

    expect(err).toBeInstanceOf(ApiError);
    expect(err.status).toBe(422);
    expect(err.code).toBe("VALIDATION_FAILED");
    expect(err.fieldErrors).toEqual({
      name: "Name is required.",
      tz: "Not an IANA time zone.",
    });
  });
});

describe("Idempotency-Key on creates (API-050)", () => {
  it("sends a fresh crypto.randomUUID key on POST", async () => {
    let key: string | null = "unset";
    server.use(
      http.post(`${TEST_BASE}/scope-nodes`, ({ request }) => {
        key = request.headers.get("Idempotency-Key");
        return ok(scopeNode(), { status: 201, revision: 1 });
      }),
    );
    await client().create("/scope-nodes", { kind: "screen", name: "Lobby" });
    expect(key).toMatch(UUID_RE);
  });

  it("uses an injected key generator when provided", async () => {
    let key: string | null = null;
    server.use(
      http.post(`${TEST_BASE}/scope-nodes`, ({ request }) => {
        key = request.headers.get("Idempotency-Key");
        return ok(scopeNode(), { status: 201, revision: 1 });
      }),
    );
    const c = new ApiClient({ baseUrl: TEST_BASE, newIdempotencyKey: () => "fixed-key-123" });
    await c.create("/scope-nodes", { kind: "screen", name: "Lobby" });
    expect(key).toBe("fixed-key-123");
  });
});

describe("Optimistic concurrency: If-Match + 412 (API-020–023)", () => {
  it("sends If-Match from the supplied ETag on PATCH", async () => {
    let ifMatch: string | null = null;
    server.use(
      http.patch(`${TEST_BASE}/scope-nodes/${ULID_A}`, ({ request }) => {
        ifMatch = request.headers.get("If-Match");
        return ok(scopeNode({ revision: 2, name: "Renamed" }), { revision: 2 });
      }),
    );
    const c = client();
    const res = await c.update(`/scope-nodes/${ULID_A}`, { name: "Renamed" }, etag(1));
    expect(ifMatch).toBe(etag(1));
    expect(res.etag).toBe(etag(2));
  });

  it("throws RevisionConflictError on 412 and NEVER silently retries the write", async () => {
    let patchCount = 0;
    server.use(
      http.patch(`${TEST_BASE}/scope-nodes/${ULID_A}`, () => {
        patchCount++;
        return problem(412, "REVISION_CONFLICT", "The resource was modified concurrently.", {
          current_revision: 7,
        });
      }),
    );
    const c = client();
    const err = (await c
      .update(`/scope-nodes/${ULID_A}`, { name: "Renamed" }, etag(1))
      .catch((e: unknown) => e)) as RevisionConflictError;

    expect(err).toBeInstanceOf(RevisionConflictError);
    expect(err).toBeInstanceOf(ApiError); // it IS an ApiError subtype
    expect(err.status).toBe(412);
    expect(err.code).toBe("REVISION_CONFLICT");
    expect(err.currentRevision).toBe(7);
    // The client made exactly ONE write attempt — no unconditional-overwrite retry.
    expect(patchCount).toBe(1);
  });

  it("sends If-Match on DELETE", async () => {
    let ifMatch: string | null = null;
    server.use(
      http.delete(`${TEST_BASE}/scope-nodes/${ULID_A}`, ({ request }) => {
        ifMatch = request.headers.get("If-Match");
        return new Response(null, { status: 204, headers: { "Trace-Id": TRACE_ID } });
      }),
    );
    await client().remove(`/scope-nodes/${ULID_A}`, etag(3));
    expect(ifMatch).toBe(etag(3));
  });
});

describe("Trace-Id + ETag capture", () => {
  it("captures Trace-Id from a successful response", async () => {
    server.use(
      http.get(`${TEST_BASE}/scope-nodes/${ULID_A}`, () => ok(scopeNode(), { revision: 1 })),
    );
    const c = client();
    await c.read(`/scope-nodes/${ULID_A}`);
    expect(c.lastTraceId).toBe(TRACE_ID);
  });

  it("captures the ETag into the map keyed by resource path", async () => {
    server.use(
      http.get(`${TEST_BASE}/scope-nodes/${ULID_A}`, () =>
        ok(scopeNode({ revision: 5 }), { revision: 5 }),
      ),
    );
    const c = client();
    const read = await c.read(`/scope-nodes/${ULID_A}`);
    expect(read.etag).toBe(etag(5));
    expect(c.etagFor(`/scope-nodes/${ULID_A}`)).toBe(etag(5));
  });
});
