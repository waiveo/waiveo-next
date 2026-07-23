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
      http.get(`${TEST_BASE}/packs`, () =>
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
      http.get(`${TEST_BASE}/packs/acme/menu-board`, ({ request }) => {
        hitPath = new URL(request.url).pathname;
        return ok(pack(), { revision: 1 });
      }),
    );
    const read = await api().packs.get(PACK_ID);
    expect(read.data.id).toBe(PACK_ID);
    // The single slash in `acme/menu-board` is a real path separator (two server
    // segments), not an id byte to percent-encode.
    expect(hitPath).toBe("/api/v1/packs/acme/menu-board");
    expect(read.etag).toBe('"1"');
  });

  it("installs a raw zip artifact carrying an Idempotency-Key + application/zip", async () => {
    let idempotencyKey: string | null = null;
    let contentType: string | null = null;
    server.use(
      http.post(`${TEST_BASE}/packs`, ({ request }) => {
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
      http.post(`${TEST_BASE}/packs`, () =>
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
      http.delete(`${TEST_BASE}/packs/acme/menu-board`, ({ request }) => {
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
      http.get(`${TEST_BASE}/packs/acme/menu-board/pages/menu-items`, () =>
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
    // api/1 endpoint (`/api/v1/packs/acme/scope-nodes`).
    await expect(api().packs.pageDoc(PACK_ID, "../../scope-nodes")).rejects.toThrow();
    expect(requested).toHaveLength(0);

    // (b) A PERCENT-ENCODED dot-segment (`%2e%2e`, which react-router keeps in the
    // route splat and the URL parser would ALSO collapse) is encoded per-segment to
    // an inert `%252e%252e`: the request stays under the pack's `/pages/`, never
    // escaping to a sibling pack or another resource.
    await api().packs.pageDoc(PACK_ID, "%2e%2e/%2e%2e/scope-nodes");
    expect(requested).toHaveLength(1);
    expect(requested[0].startsWith("/api/v1/packs/acme/menu-board/pages/")).toBe(true);
  });

  it("fetches a locale catalog verbatim (bare keys)", async () => {
    server.use(
      http.get(`${TEST_BASE}/packs/acme/menu-board/messages/en`, () =>
        HttpResponse.json({ "page.menuItems.title": "Menu Items" }, { headers: { "Trace-Id": TRACE_ID } }),
      ),
    );
    await expect(api().packs.messages(PACK_ID, "en")).resolves.toEqual({ "page.menuItems.title": "Menu Items" });
  });

  it("raises a 404 ApiError for a missing locale (the en fallback is the caller's job)", async () => {
    server.use(
      http.get(`${TEST_BASE}/packs/acme/menu-board/messages/fr`, () =>
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

describe("pack-data collections client — a first-class api/1 citizen", () => {
  const base = `${TEST_BASE}/packs/acme/menu-board/data/menu_items`;

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
