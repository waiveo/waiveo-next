/// <reference types="node" />
import { existsSync, readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, it, expect, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { MemoryRouter, Route, Routes } from "react-router";
import { ThemeProvider } from "@/components/theme/theme-provider";
import PackPageRoute from "./pack-page-route";
import { validatePage } from "@/renderer/validate";
import { TRACE_ID, ULID_A, ULID_ROOT, ok } from "@/api/test-support";

// The living proof for the web: the REAL in-repo example pack
// (examples/packs/menu-board) — the very files `make example-pack` zips and the
// feeder installs at `make dev` — rendered through the SAME validate → PageRenderer
// → Horizon kit path a core console page takes. Nothing here is a hand-written
// fixture: the manifest, the page document, and the locale catalog are read off
// disk, so if the shipped pack ever drifts out of what the renderer accepts, this
// test fails. NO pack code executes — the doc, the catalog, and the rows are data.

// Read the shipped pack off disk at RUNTIME with Node fs (never a `new URL(...,
// import.meta.url)` — Vite rewrites that into an asset import Vite's fs-allow then
// denies for a path outside the web project). Vitest runs with cwd = the web/
// project dir, so the repo root is its parent; the pack lives under examples/. The
// pack is DATA — we read and JSON.parse it, we never execute it.
function repoRoot(): string {
  const cwd = process.cwd();
  for (const candidate of [resolve(cwd, ".."), cwd]) {
    if (existsSync(resolve(candidate, "examples/packs/menu-board/manifest.json"))) return candidate;
  }
  throw new Error(`cannot locate the example pack from cwd ${cwd}`);
}

function packFile(rel: string): string {
  return readFileSync(resolve(repoRoot(), "examples/packs/menu-board", rel), "utf8");
}

const realManifest = JSON.parse(packFile("manifest.json")) as Record<string, unknown>;
const realMenuItemsDoc = JSON.parse(packFile("ui/menu-items.json")) as Record<string, unknown>;
const realSettingsDoc = JSON.parse(packFile("ui/settings.json")) as Record<string, unknown>;
const realEnCatalog = JSON.parse(packFile("messages/en.json")) as Record<string, string>;

// The registry envelope the console reads (its `.manifest` is the console-relevant
// slice; the real file carries every section, which is a superset — fine).
function realPackEnvelope() {
  return {
    id: "waiveo/menu-board",
    revision: 1,
    version: "1.0.0",
    data_model_version: 1,
    created_at: 1_753_000_000,
    updated_at: 1_753_000_000,
    manifest: realManifest,
  };
}

const B = "*/api/v1/packs/waiveo/menu-board";

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

function jsonBody(body: Parameters<typeof HttpResponse.json>[0]) {
  return HttpResponse.json(body, { headers: { "Trace-Id": TRACE_ID } });
}

function dataPage(rows: unknown[]) {
  return HttpResponse.json({ items: rows, cursor: null }, { headers: { "Trace-Id": TRACE_ID } });
}

// A menu_items row as the pack-data api/1 surface returns it (declared fields flat
// with the universal envelope).
function menuRow(over: Record<string, unknown> = {}) {
  return {
    entity_id: ULID_A,
    revision: 1,
    lifecycle_state: "published",
    scope_node: ULID_ROOT,
    labels: [],
    template_ref: null,
    params: null,
    name: "Cortado",
    section: "Coffee",
    price: 4.5,
    available: true,
    created_at: 1_753_000_000,
    updated_at: 1_753_000_000,
    ...over,
  };
}

// The handlers a render of the real menu-items page needs: the pack, the org scope
// (a created row attaches under it — MAN-051 requires a scope_node ULID), the real
// page doc, and the real en catalog. The caller supplies the rows handler.
function baseHandlers() {
  return [
    http.get(`${B}`, () => ok(realPackEnvelope(), { revision: 1 })),
    http.get("*/api/v1/scope-nodes", () =>
      dataPage([{ id: ULID_ROOT, kind: "org", parent_id: null, name: "Org", revision: 1 }]),
    ),
    http.get(`${B}/pages/menu-items`, () => jsonBody(realMenuItemsDoc)),
    http.get(`${B}/messages/en`, () => jsonBody(realEnCatalog)),
  ];
}

function renderPack(path = "menu-items") {
  return render(
    <ThemeProvider>
      <MemoryRouter initialEntries={[`/p/waiveo/menu-board/${path}`]}>
        <Routes>
          <Route path="/p/:publisher/:name/*" element={<PackPageRoute />} />
        </Routes>
      </MemoryRouter>
    </ThemeProvider>,
  );
}

describe("The menu-board example pack renders through the console", () => {
  it("both shipped page documents pass validatePage — the same gate every page clears", () => {
    expect(validatePage(realMenuItemsDoc).ok).toBe(true);
    expect(validatePage(realSettingsDoc).ok).toBe(true);
  });

  it("renders the shipped list-detail page with a menu_items row, titles + columns from the pack's own catalog", async () => {
    server.use(
      ...baseHandlers(),
      http.get(`${B}/data/menu_items`, () => dataPage([menuRow()])),
    );
    renderPack();

    // The page header carries the catalog-resolved page title (page.menuItems.title).
    expect(await screen.findByRole("heading", { name: "Menu Items" })).toBeInTheDocument();
    // The real DataTable, painted by the renderer, carries the created menu item.
    const table = await screen.findByRole("table", { name: "Menu items" });
    expect(within(table).getByText("Cortado")).toBeInTheDocument();
    expect(within(table).getByText("Coffee")).toBeInTheDocument();
    // The column headers resolved through the pack's own en catalog (col.name etc).
    expect(within(table).getByText("Item")).toBeInTheDocument();
    expect(within(table).getByText("Section")).toBeInTheDocument();
    expect(within(table).getByText("Price")).toBeInTheDocument();
  });

  it("creates a menu item over the pack-data api/1 surface and shows it in the list", async () => {
    // Start empty; the create posts under the org scope, then the list reflects it.
    const state = { rows: [] as unknown[] };
    let postedScope: unknown = "unset";
    let postedLifecycle: unknown = "unset";
    let idempotencyKey: string | null = null;
    server.use(
      ...baseHandlers(),
      http.get(`${B}/data/menu_items`, () => dataPage(state.rows)),
      http.post(`${B}/data/menu_items`, async ({ request }) => {
        idempotencyKey = request.headers.get("Idempotency-Key");
        const body = (await request.json()) as { scope_node?: unknown; name?: unknown; lifecycle_state?: unknown };
        postedScope = body.scope_node;
        postedLifecycle = body.lifecycle_state;
        const created = menuRow({ entity_id: ULID_A, name: String(body.name ?? "New item") });
        state.rows = [created];
        return ok(created, { status: 201, revision: 1 });
      }),
    );

    const user = userEvent.setup();
    renderPack();
    await screen.findByRole("table", { name: "Menu items" });

    // New opens a BLANK create draft (the shipped page seeds no name); fill it, then
    // Save commits it through the pack-data create path (the create idiom, UIS-021).
    await user.click(screen.getByRole("button", { name: "New" }));
    await user.type(await screen.findByLabelText("Item name"), "Espresso");
    await user.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() =>
      expect(
        within(screen.getByRole("table", { name: "Menu items" })).getByText("Espresso"),
      ).toBeInTheDocument(),
    );
    // The write carried the required scope_node (MAN-051), a fresh Idempotency-Key,
    // and the page-declared lifecycle_state (a draft-publish collection starts draft).
    expect(postedScope).toBe(ULID_ROOT);
    expect(idempotencyKey).toMatch(/^[0-9a-f-]{36}$/i);
    expect(postedLifecycle).toBe("draft");
  });
});
