import { describe, it, expect, beforeAll, afterAll, afterEach } from "vitest";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { createApi, ApiError, RevisionConflictError } from "./index";
import { TEST_BASE, TRACE_ID, ULID_A, ULID_B, ULID_ROOT, PACK_ID, pack, packRow, ok, problem } from "./test-support";

// The declarative-packs client surface, exercised against a mock server: the pack
// registry (list/get/install/uninstall + page docs + locale catalogs) and the
// pack-data collections, proving each rides the SAME api/1 conventions the ApiClient
// owns — Idempotency-Key on install/create, If-Match on uninstall/edit/delete, the
// 412 conflict surfaced distinctly, a 422's field errors mapped, keyset pagination.

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

function api() {
  return createApi({ baseUrl: TEST_BASE });
}

describe("packs registry client", () => {
  it("lists installed packs as a keyset page", async () => {
    server.use(
      http.get(`${TEST_BASE}/extensions`, () =>
        HttpResponse.json({ items: [pack()], cursor: null }, { headers: { "Trace-Id": TRACE_ID } }),
      ),
    );
    const page = await api().packs.list();
    expect(page.items).toHaveLength(1);
    expect(page.items[0].id).toBe(PACK_ID);
    expect(page.items[0].manifest.ui.pages).toHaveLength(2);
  });

  it("reads one pack by its slash-bearing id WITHOUT percent-encoding the separator", async () => {
    let hitPath: string | null = null;
    server.use(
      http.get(`${TEST_BASE}/extensions/acme/menu-board`, ({ request }) => {
        hitPath = new URL(request.url).pathname;
        return ok(pack(), { revision: 1 });
      }),
    );
    const read = await api().packs.get(PACK_ID);
    expect(read.data.id).toBe(PACK_ID);
    // The single slash in `acme/menu-board` is a real path separator (two server
    // segments), not an id byte to percent-encode.
    expect(hitPath).toBe("/api/v1/extensions/acme/menu-board");
    expect(read.etag).toBe('"1"');
  });

  it("installs a raw zip artifact carrying an Idempotency-Key + application/zip", async () => {
    let idempotencyKey: string | null = null;
    let contentType: string | null = null;
    server.use(
      http.post(`${TEST_BASE}/extensions`, ({ request }) => {
        idempotencyKey = request.headers.get("Idempotency-Key");
        contentType = request.headers.get("Content-Type");
        return HttpResponse.json(
          { id: PACK_ID, version: "1.0.0", pages: ["menu-items", "settings"], collections: ["menu_items"], locales: ["en"] },
          { status: 201, headers: { "Trace-Id": TRACE_ID } },
        );
      }),
    );
    const zip = new Uint8Array([0x50, 0x4b, 0x03, 0x04]);
    const result = await api().packs.install(zip);
    expect(result.id).toBe(PACK_ID);
    expect(result.collections).toEqual(["menu_items"]);
    expect(idempotencyKey).toMatch(/^[0-9a-f-]{36}$/i);
    expect(contentType).toBe("application/zip");
  });

  it("surfaces a manifest-refused install as a 422 ApiError with the field errors mapped", async () => {
    server.use(
      http.post(`${TEST_BASE}/extensions`, () =>
        problem(422, "VALIDATION_FAILED", "The pack manifest failed validation.", {
          errors: [{ field: "capabilities[0]", code: "UNKNOWN_CAPABILITY", message: "unknown capability" }],
        }),
      ),
    );
    const err = await api()
      .packs.install(new Uint8Array([1]))
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(422);
    expect((err as ApiError).fieldErrors["capabilities[0]"]).toBe("unknown capability");
  });

  it("uninstalls under an If-Match derived from the pack revision", async () => {
    let ifMatch: string | null = null;
    server.use(
      http.delete(`${TEST_BASE}/extensions/acme/menu-board`, ({ request }) => {
        ifMatch = request.headers.get("If-Match");
        return new HttpResponse(null, { status: 204, headers: { "Trace-Id": TRACE_ID } });
      }),
    );
    await api().packs.remove(PACK_ID, '"1"');
    expect(ifMatch).toBe('"1"');
  });

  it("fetches a page document verbatim (the renderer's input)", async () => {
    const doc = { pageType: "list-detail", list: { source: "menu_items" } };
    server.use(
      http.get(`${TEST_BASE}/extensions/acme/menu-board/pages/menu-items`, () =>
        HttpResponse.json(doc, { headers: { "Trace-Id": TRACE_ID } }),
      ),
    );
    await expect(api().packs.pageDoc(PACK_ID, "menu-items")).resolves.toEqual(doc);
  });

  it("confines a crafted page path to the pack's own /pages/ namespace (no traversal escape)", async () => {
    // A single catch-all records every request that leaves the client, so the
    // assertions are about where the fetch actually went — not about which mock
    // happened to answer.
    const requested: string[] = [];
    server.use(
      http.get(/.*/, ({ request }) => {
        requested.push(new URL(request.url).pathname);
        return HttpResponse.json({ ok: true }, { headers: { "Trace-Id": TRACE_ID } });
      }),
    );

    // (a) A literal dot-segment path is refused BEFORE any request leaves — the
    // WHATWG URL parser fetch() uses would otherwise collapse `../` onto another
    // api/1 endpoint (`/api/v1/extensions/acme/scope-nodes`).
    await expect(api().packs.pageDoc(PACK_ID, "../../scope-nodes")).rejects.toThrow();
    expect(requested).toHaveLength(0);

    // (b) A PERCENT-ENCODED dot-segment (`%2e%2e`, which react-router keeps in the
    // route splat and the URL parser would ALSO collapse) is encoded per-segment to
    // an inert `%252e%252e`: the request stays under the pack's `/pages/`, never
    // escaping to a sibling pack or another resource.
    await api().packs.pageDoc(PACK_ID, "%2e%2e/%2e%2e/scope-nodes");
    expect(requested).toHaveLength(1);
    expect(requested[0].startsWith("/api/v1/extensions/acme/menu-board/pages/")).toBe(true);
  });

  it("fetches a locale catalog verbatim (bare keys)", async () => {
    server.use(
      http.get(`${TEST_BASE}/extensions/acme/menu-board/messages/en`, () =>
        HttpResponse.json({ "page.menuItems.title": "Menu Items" }, { headers: { "Trace-Id": TRACE_ID } }),
      ),
    );
    await expect(api().packs.messages(PACK_ID, "en")).resolves.toEqual({ "page.menuItems.title": "Menu Items" });
  });

  it("raises a 404 ApiError for a missing locale (the en fallback is the caller's job)", async () => {
    server.use(
      http.get(`${TEST_BASE}/extensions/acme/menu-board/messages/fr`, () =>
        problem(404, "NOT_FOUND", "No such locale."),
      ),
    );
    const err = await api()
      .packs.messages(PACK_ID, "fr")
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).status).toBe(404);
  });
});

// The marketplace half of the same registry: resolve-and-install, the on-demand
// update check, and the install-record history. Same endpoint for install, same
// pipeline, same conventions — only the way the bytes are LOCATED differs.
describe("packs marketplace client", () => {
  it("installs by REFERENCE as JSON on the same endpoint, carrying an Idempotency-Key", async () => {
    let contentType: string | null = null;
    let idempotencyKey: string | null = null;
    let body: Record<string, unknown> | null = null;
    server.use(
      http.post(`${TEST_BASE}/extensions`, async ({ request }) => {
        contentType = request.headers.get("Content-Type");
        idempotencyKey = request.headers.get("Idempotency-Key");
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json(
          { id: PACK_ID, version: "1.0.0", pages: [], collections: [], locales: ["en"] },
          { status: 201, headers: { "Trace-Id": TRACE_ID } },
        );
      }),
    );
    const result = await api().packs.installRef({ pack_id: PACK_ID, trust_channel: "community" });
    expect(result.id).toBe(PACK_ID);
    // The JSON content type IS the discriminant that says "resolve this reference"
    // rather than "install these bytes".
    expect(contentType).toBe("application/json");
    expect(idempotencyKey).toMatch(/^[0-9a-f-]{36}$/i);
    // The server decodes this body with unknown members REFUSED, so an absent
    // optional must be OMITTED — never sent as an empty string.
    expect(body).toEqual({ pack_id: PACK_ID, trust_channel: "community" });
  });

  it("carries an explicit source and pinned version when the operator supplies them", async () => {
    let body: Record<string, unknown> | null = null;
    server.use(
      http.post(`${TEST_BASE}/extensions`, async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json(
          { id: PACK_ID, version: "1.2.0", pages: [], collections: [], locales: ["en"] },
          { status: 200, headers: { "Trace-Id": TRACE_ID } },
        );
      }),
    );
    await api().packs.installRef({
      pack_id: PACK_ID,
      trust_channel: "verified",
      source: "https://registry.example/index.json",
      version: "1.2.0",
    });
    expect(body).toEqual({
      pack_id: PACK_ID,
      trust_channel: "verified",
      source: "https://registry.example/index.json",
      version: "1.2.0",
    });
  });

  it("surfaces a refused reference with its own marketplace code", async () => {
    server.use(
      http.post(`${TEST_BASE}/extensions`, () =>
        problem(422, "VALIDATION_FAILED", "the pack reference must name one of the four trust channels", {
          errors: [{ field: "artifact", code: "TRUST_CHANNEL_UNKNOWN", message: "got \"\"" }],
        }),
      ),
    );
    const err = await api()
      .packs.installRef({ pack_id: PACK_ID, trust_channel: "community" })
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).fieldErrors["artifact"]).toContain("got");
  });

  it("runs an update check as a bodyless POST on the pack's own /update path", async () => {
    let idempotencyKey: string | null = null;
    let hitPath: string | null = null;
    let raw: string | null = null;
    server.use(
      http.post(`${TEST_BASE}/extensions/acme/menu-board/update`, async ({ request }) => {
        idempotencyKey = request.headers.get("Idempotency-Key");
        hitPath = new URL(request.url).pathname;
        raw = await request.text();
        return HttpResponse.json(
          { action: "updated", id: PACK_ID, from_version: "1.0.0", to_version: "1.1.0", pages: ["menu-items"] },
          { headers: { "Trace-Id": TRACE_ID } },
        );
      }),
    );
    const result = await api().packs.update(PACK_ID);
    expect(result.action).toBe("updated");
    expect(result.to_version).toBe("1.1.0");
    // The pack id's single slash stays a real path separator.
    expect(hitPath).toBe("/api/v1/extensions/acme/menu-board/update");
    // Nothing about HOW the pack is re-resolved is sent: the channel pin lives in
    // the install record server-side (MKT-094).
    expect(raw).toBe("");
    // A retriable mutating POST outside plain creation still carries the key.
    expect(idempotencyKey).toMatch(/^[0-9a-f-]{36}$/i);
  });

  it("lists the install-record history as a keyset page, oldest first", async () => {
    const rec = (over: Record<string, unknown>) => ({
      id: ULID_A,
      pack_id: PACK_ID,
      resolved_version: "1.0.0",
      trust_channel: "community",
      source: "https://registry.example/index.json",
      stale_source: false,
      content_digest: "sha256:aa11",
      key_id: "key-1",
      artifact_digest: null,
      installed_at: 1_753_000_000_000,
      ...over,
    });
    server.use(
      http.get(`${TEST_BASE}/extensions/acme/menu-board/installs`, () =>
        HttpResponse.json(
          { items: [rec({ id: ULID_A }), rec({ id: ULID_B, resolved_version: "1.1.0" })], cursor: null },
          { headers: { "Trace-Id": TRACE_ID } },
        ),
      ),
    );
    const page = await api().packs.installs(PACK_ID);
    expect(page.items.map((r) => r.resolved_version)).toEqual(["1.0.0", "1.1.0"]);
    // A direct install genuinely has no channel: `null` is meaningful, not missing.
    expect(page.items[0].trust_channel).toBe("community");
  });

  it("404s the history of a pack that is not installed (its records went with it)", async () => {
    server.use(
      http.get(`${TEST_BASE}/extensions/acme/menu-board/installs`, () =>
        problem(404, "NOT_FOUND", "No pack exists at this identifier."),
      ),
    );
    const err = await api()
      .packs.installs(PACK_ID)
      .catch((e: unknown) => e);
    expect((err as ApiError).status).toBe(404);
  });
});

describe("pack-data collections client — a first-class api/1 citizen", () => {
  const base = `${TEST_BASE}/extensions/acme/menu-board/data/menu_items`;

  it("lists rows over the keyset cursor", async () => {
    server.use(
      http.get(base, () =>
        HttpResponse.json({ items: [packRow(), packRow({ entity_id: ULID_B, name: "Latte" })], cursor: null }, { headers: { "Trace-Id": TRACE_ID } }),
      ),
    );
    const page = await api().packData(PACK_ID, "menu_items").list();
    expect(page.items.map((r) => r.name)).toEqual(["Cortado", "Latte"]);
  });

  it("creates a row carrying an Idempotency-Key and a required scope_node", async () => {
    let idempotencyKey: string | null = null;
    let body: Record<string, unknown> | null = null;
    server.use(
      http.post(base, async ({ request }) => {
        idempotencyKey = request.headers.get("Idempotency-Key");
        body = (await request.json()) as Record<string, unknown>;
        return ok(packRow({ entity_id: ULID_B, name: "Flat white" }), { status: 201, revision: 1 });
      }),
    );
    const created = await api()
      .packData(PACK_ID, "menu_items")
      .create({ scope_node: ULID_ROOT, name: "Flat white" });
    expect(created.data.entity_id).toBe(ULID_B);
    expect(idempotencyKey).toMatch(/^[0-9a-f-]{36}$/i);
    expect(body).toMatchObject({ scope_node: ULID_ROOT, name: "Flat white" });
  });

  it("edits a row under its If-Match (no unconditional overwrite)", async () => {
    let ifMatch: string | null = null;
    server.use(
      http.patch(`${base}/${ULID_A}`, ({ request }) => {
        ifMatch = request.headers.get("If-Match");
        return ok(packRow({ name: "Renamed", revision: 2 }), { revision: 2 });
      }),
    );
    const updated = await api().packData(PACK_ID, "menu_items").update(ULID_A, { scope_node: ULID_ROOT, name: "Renamed" }, '"1"');
    expect(updated.data.name).toBe("Renamed");
    expect(ifMatch).toBe('"1"');
  });

  it("surfaces a 412 as a distinct RevisionConflictError", async () => {
    server.use(
      http.patch(`${base}/${ULID_A}`, () =>
        problem(412, "REVISION_CONFLICT", "The row was modified concurrently.", { current_revision: 7 }),
      ),
    );
    const err = await api()
      .packData(PACK_ID, "menu_items")
      .update(ULID_A, { scope_node: ULID_ROOT, name: "x" }, '"1"')
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(RevisionConflictError);
    expect((err as RevisionConflictError).currentRevision).toBe(7);
  });

  it("maps a 422 write rejection's per-field errors", async () => {
    server.use(
      http.post(base, () =>
        problem(422, "VALIDATION_FAILED", "One or more fields failed validation.", {
          errors: [{ field: "price", code: "INVALID_TYPE", message: 'field "price" must be of type "number"' }],
        }),
      ),
    );
    const err = await api()
      .packData(PACK_ID, "menu_items")
      .create({ scope_node: ULID_ROOT, price: "free" })
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiError);
    expect((err as ApiError).fieldErrors.price).toMatch(/must be of type "number"/);
  });

  it("deletes a row under its If-Match", async () => {
    let ifMatch: string | null = null;
    server.use(
      http.delete(`${base}/${ULID_A}`, ({ request }) => {
        ifMatch = request.headers.get("If-Match");
        return new HttpResponse(null, { status: 204, headers: { "Trace-Id": TRACE_ID } });
      }),
    );
    await api().packData(PACK_ID, "menu_items").remove(ULID_A, '"3"');
    expect(ifMatch).toBe('"3"');
  });
});
