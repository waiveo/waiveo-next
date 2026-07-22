// @vitest-environment node
//
// The typed resource modules + the standard conflict flow, against a mocked
// server (msw). Proves the dogfood-critical UX contract at the client layer: a
// 412 REVISION_CONFLICT drives a re-READ + REVIEW (the current server state is
// returned), NEVER a silent retry that overwrites the concurrent change. Also:
// pages() walks cursors, a content upload carries an Idempotency-Key, and an
// automation run returns its disposition.

import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { createApi, updateWithReview } from "./resources";
import { TEST_BASE, TRACE_ID, ULID_A, ULID_B, ULID_C, etag, ok, problem, scopeNode } from "./test-support";

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

function api() {
  return createApi({ baseUrl: TEST_BASE });
}

describe("standard conflict flow (updateWithReview)", () => {
  it("on a happy update returns { status: 'updated' }", async () => {
    server.use(
      http.patch(`${TEST_BASE}/scope-nodes/${ULID_A}`, () =>
        ok(scopeNode({ revision: 2, name: "Renamed" }), { revision: 2 }),
      ),
    );
    const a = api();
    const outcome = await updateWithReview(a.scopeNodes, ULID_A, { name: "Renamed" }, etag(1));
    expect(outcome.status).toBe("updated");
    if (outcome.status === "updated") {
      expect(outcome.resource.data.revision).toBe(2);
      expect(outcome.resource.etag).toBe(etag(2));
    }
  });

  it("on 412 re-reads and returns the current state for review — never overwrites", async () => {
    let patchCount = 0;
    let getCount = 0;
    server.use(
      http.patch(`${TEST_BASE}/scope-nodes/${ULID_A}`, () => {
        patchCount++;
        return problem(412, "REVISION_CONFLICT", "The resource was modified concurrently.", {
          current_revision: 7,
        });
      }),
      http.get(`${TEST_BASE}/scope-nodes/${ULID_A}`, () => {
        getCount++;
        return ok(scopeNode({ revision: 7, name: "Changed elsewhere" }), { revision: 7 });
      }),
    );

    const a = api();
    const outcome = await updateWithReview(a.scopeNodes, ULID_A, { name: "Mine" }, etag(1));

    expect(outcome.status).toBe("conflict");
    if (outcome.status === "conflict") {
      expect(outcome.conflict.currentRevision).toBe(7);
      // The REVIEW state: the current server truth, freshly re-read.
      expect(outcome.current.data.name).toBe("Changed elsewhere");
      expect(outcome.current.data.revision).toBe(7);
      expect(outcome.current.etag).toBe(etag(7));
    }
    // Exactly ONE write attempt (no unconditional-overwrite retry) and exactly
    // ONE re-read — the conflict flow, not a silent clobber.
    expect(patchCount).toBe(1);
    expect(getCount).toBe(1);
    // The re-read refreshed the captured If-Match for the next attempt.
    expect(a.scopeNodes.etagFor(ULID_A)).toBe(etag(7));
  });
});

describe("cursor pagination through a resource module", () => {
  it("pages() walks every schedule across cursors, cursor omitted on page 1", async () => {
    const requestedCursors: (string | null)[] = [];
    const sched = (id: string, name: string) => ({
      id,
      scope_node: ULID_A,
      name,
      revision: 1,
      created_at: 0,
      updated_at: 0,
    });
    server.use(
      http.get(`${TEST_BASE}/schedules`, ({ request }) => {
        const url = new URL(request.url);
        const cursor = url.searchParams.get("cursor");
        requestedCursors.push(cursor);
        if (cursor === null) {
          return HttpResponse.json(
            { items: [sched(ULID_A, "Weekdays"), sched(ULID_B, "Weekend")], cursor: `schedules_${ULID_B}` },
            { headers: { "Trace-Id": TRACE_ID } },
          );
        }
        return HttpResponse.json(
          { items: [sched(ULID_C, "Holiday")], cursor: null },
          { headers: { "Trace-Id": TRACE_ID } },
        );
      }),
    );

    const names: string[] = [];
    for await (const s of api().schedules.pages()) names.push(s.name);

    expect(names).toEqual(["Weekdays", "Weekend", "Holiday"]);
    // Page 1 sends no cursor (an empty cursor is not a keyset position); page 2
    // replays the opaque token verbatim.
    expect(requestedCursors).toEqual([null, `schedules_${ULID_B}`]);
  });
});

describe("content upload", () => {
  it("POSTs bytes with an Idempotency-Key and returns the content-addressed ref", async () => {
    let key: string | null = null;
    server.use(
      http.post(`${TEST_BASE}/content`, ({ request }) => {
        key = request.headers.get("Idempotency-Key");
        return HttpResponse.json(
          {
            asset_ref: "sha256:0123456789abcdef",
            url: "/content/0123456789abcdef",
          },
          { status: 201, headers: { "Trace-Id": TRACE_ID } },
        );
      }),
    );
    const result = await api().content.upload(new Blob(["hello"]));
    expect(key).toMatch(UUID_RE);
    expect(result.asset_ref).toBe("sha256:0123456789abcdef");
    expect(result.url).toBe("/content/0123456789abcdef");
  });
});

describe("automations run", () => {
  it("returns the mode-evaluation disposition and carries an Idempotency-Key", async () => {
    let key: string | null = null;
    server.use(
      http.post(`${TEST_BASE}/automations/${ULID_A}/run`, ({ request }) => {
        key = request.headers.get("Idempotency-Key");
        return HttpResponse.json(
          { run_id: ULID_B, disposition: "ran" },
          { headers: { "Trace-Id": TRACE_ID } },
        );
      }),
    );
    const result = await api().automations.run(ULID_A);
    expect(key).toMatch(UUID_RE);
    expect(result.disposition).toBe("ran");
    expect(result.run_id).toBe(ULID_B);
  });
});
