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
import { createApi, normalizePlaylistItem, updateWithReview, type PlaylistItem } from "./resources";
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

describe("automations run — dry_run", () => {
  async function bodyOf(call: (a: ReturnType<typeof api>) => Promise<unknown>) {
    let raw: string | null = null;
    server.use(
      http.post(`${TEST_BASE}/automations/${ULID_A}/run`, async ({ request }) => {
        raw = await request.text();
        return HttpResponse.json({ run_id: ULID_B, disposition: "ran" }, { headers: { "Trace-Id": TRACE_ID } });
      }),
    );
    await call(api());
    return raw as string | null;
  }

  it("sends dry_run only when asked", async () => {
    expect(await bodyOf((a) => a.automations.run(ULID_A, { dryRun: true }))).toContain('"dry_run":true');
  });

  // Omitted, not sent false. The body's schema closes additionalProperties and
  // `false` is the server's own default, so sending it would put a flag in the
  // audit trail nobody set.
  // No body at all, not `{}` and not `{"dry_run":false}`: `false` is the
  // server's own default, and an empty request is what an ordinary run has
  // always sent.
  it("sends no body when dry_run is false or absent", async () => {
    expect(await bodyOf((a) => a.automations.run(ULID_A))).toBe("");
    expect(await bodyOf((a) => a.automations.run(ULID_A, { dryRun: false }))).toBe("");
  });

  // The two options are independent: asking for one must not drop the other.
  it("carries context and dry_run together", async () => {
    const body = await bodyOf((a) => a.automations.run(ULID_A, { context: { entity_id: ULID_B }, dryRun: true }));
    expect(body).toContain('"dry_run":true');
    expect(body).toContain(ULID_B);
  });

  it("still sends a context-only run with no dry_run member", async () => {
    const body = await bodyOf((a) => a.automations.run(ULID_A, { context: { entity_id: ULID_B } }));
    expect(body).toContain(ULID_B);
    expect(body).not.toContain("dry_run");
  });
});

describe("playlist item normalisation", () => {
  // An editor that lets an operator SWITCH an item's source leaves the previous
  // source's members on the object. That is not cosmetic: the server validates
  // an item against the source it declares, and `asset_ref` is
  // reference-checked against the content origin, so a leftover is a claim
  // about content the entry no longer plays.
  it("keeps only the members the declared source owns", () => {
    const switched = {
      source: "cast",
      cast_id: ULID_B,
      asset_ref: "sha256:aa11",
      pack_id: "left",
      content_id: "over",
      duration_seconds: 10,
    } as PlaylistItem;
    expect(normalizePlaylistItem(switched)).toEqual({
      source: "cast",
      cast_id: ULID_B,
      duration_seconds: 10,
    });
  });

  it("leaves an untouched item of every source exactly as it was", () => {
    const asset: PlaylistItem = { source: "asset", asset_ref: "sha256:aa11", duration_seconds: 5 };
    const playable: PlaylistItem = { source: "playable", pack_id: "p", content_id: "c" };
    const cast: PlaylistItem = { source: "cast", cast_id: ULID_A };
    expect(normalizePlaylistItem(asset)).toEqual(asset);
    expect(normalizePlaylistItem(playable)).toEqual(playable);
    expect(normalizePlaylistItem(cast)).toEqual(cast);
  });

  // An absent member is not the same statement as a present empty one, and the
  // request schema is additionalProperties:false — so nothing is invented.
  it("does not invent members the item never carried", () => {
    expect(normalizePlaylistItem({ source: "asset" })).toEqual({ source: "asset" });
  });

  // `content_type` is the field that makes a scheduled video PLAY rather than
  // being drawn as a still, so dropping it here would silently downgrade every
  // video an operator saved — the item would still be accepted, still be
  // scheduled, and still show nothing on the wall.
  it("carries an asset item's content_type through", () => {
    const video: PlaylistItem = { source: "asset", asset_ref: "sha256:aa11", content_type: "video" };
    expect(normalizePlaylistItem(video)).toEqual(video);
  });

  // And it belongs to `asset` alone: the server refuses it on any other source
  // (a cast item's content type is decided by its source), so a leftover from a
  // switched item would turn a save into a 422 the operator cannot explain.
  it("drops content_type left behind by switching an item away from asset", () => {
    const switched = {
      source: "cast",
      cast_id: ULID_B,
      content_type: "video",
    } as PlaylistItem;
    expect(normalizePlaylistItem(switched)).toEqual({ source: "cast", cast_id: ULID_B });
  });

  // THE ROUND TRIP. A `source: "slide"` item carries its whole content INLINE,
  // and this console has no layer editor — so the only thing it can do with one
  // is give it back unchanged. It did not: `slide` was missing from both the
  // source list and the per-source field map, so the normalizer rebuilt the item
  // from an empty list and the save that followed stored an item with no slide.
  // The operator was told "Saved playlist"; the screen played one item fewer.
  //
  // This is the exact sequence the console performs on Save
  // (schedules-route.tsx: every item through normalizePlaylistItem, then PATCH),
  // asserted on the object that would go on the wire.
  it("round-trips a playlist containing an inline slide without deleting the slide", () => {
    const inline: PlaylistItem = {
      source: "slide",
      slide: {
        layers: [
          { kind: "rect", x: 0, y: 0, w: 1920, h: 1080, color: "#101014" },
          { kind: "text", x: 80, y: 400, w: 1760, h: 200, text: "Reception", font_px: 96 },
          { kind: "ping", x: 80, y: 700, w: 600, h: 120, text: "Call for service", ping_name: "call_service" },
        ],
      },
      duration_seconds: 20,
    };
    const loaded: PlaylistItem[] = [
      { source: "asset", asset_ref: "sha256:aa11", content_type: "video" },
      inline,
      { source: "cast", cast_id: ULID_A },
    ];

    const saved = loaded.map((item) => normalizePlaylistItem(item));

    expect(saved).toEqual(loaded);
    // Named separately from the deep-equal above: the deep-equal would also pass
    // if `slide` were rebuilt as `undefined` under a looser matcher, and it is
    // the presence of the LAYERS that decides whether a screen draws anything.
    expect(saved[1].slide?.layers).toHaveLength(3);
    expect(saved[1].slide?.layers[2].ping_name).toBe("call_service");
  });

  // A source newer than this build must survive a save too, for the same reason
  // and by the opposite mechanism: there is no field list to rebuild from, so
  // the item is passed through rather than emptied.
  it("passes an unrecognised source through untouched instead of emptying it", () => {
    const future = { source: "hologram", projection_id: ULID_B, duration_seconds: 8 } as unknown as PlaylistItem;
    expect(normalizePlaylistItem(future)).toEqual(future);
  });
});
