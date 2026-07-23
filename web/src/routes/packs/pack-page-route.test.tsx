import { describe, it, expect, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { MemoryRouter, Route, Routes } from "react-router";
import { ThemeProvider } from "@/components/theme/theme-provider";
import PackPageRoute from "./pack-page-route";
import { validatePage } from "@/renderer/validate";
import {
  TRACE_ID,
  ULID_A,
  ULID_B,
  ULID_ROOT,
  pack,
  packRow,
  PACK_EN_CATALOG,
  ok,
  problem,
} from "@/api/test-support";

// A pack's page is rendered through the SAME validate → PageRenderer → Horizon kit
// path a core page takes — these tests prove an installed pack's list-detail page
// renders its pack-data rows, that an invalid document shows the standard error
// EmptyState (never a crash), that `msg:` refs resolve from the pack's catalog with
// the en fallback (MAN-111), and that its create/edit verbs ride the pack-data
// api/1 conventions (Idempotency-Key, If-Match/412, 422 field mapping) — all with
// zero pack code executing (the doc/catalog/rows are data the console fetched).

// A conformant list-detail page for the `menu_items` collection: `entity_id` is the
// row identity (pack rows carry the universal envelope, not a bare `id`).
const menuItemsDoc = {
  pageType: "list-detail",
  list: {
    source: "menu_items",
    display: {
      type: "table",
      id: "Menu items",
      props: {
        source: "menu_items",
        columns: [
          { headerMsg: "msg:col.name", cell: "item.name" },
          { headerMsg: "msg:col.section", cell: "item.section" },
        ],
      },
      on: { rowPress: { verb: "set", target: "$ui.selected", value: "item.entity_id" } },
    },
  },
  detail: {
    source: "menu_items[entity_id=$ui.selected]",
    emptyMsg: "msg:detail.empty",
    root: {
      type: "section",
      props: { titleMsg: "msg:detail.title" },
      children: [
        { type: "text-input", bind: "name", props: { labelMsg: "msg:detail.name" } },
        { type: "text-input", bind: "section", props: { labelMsg: "msg:detail.section" } },
        { type: "button", props: { labelMsg: "msg:detail.save", style: "primary" }, on: { press: { verb: "submit" } } },
        { type: "button", props: { labelMsg: "msg:detail.delete", style: "destructive" }, on: { press: { verb: "delete", target: "$root" } } },
      ],
    },
  },
  newAction: { verb: "create", target: "menu_items", itemDefault: { name: "New item" } },
};

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => {
  server.resetHandlers();
  setViewport(false);
  setLanguage("en-US");
  window.localStorage.clear();
});
afterAll(() => server.close());

function setViewport(narrow: boolean) {
  window.matchMedia = ((query: string) =>
    ({
      matches: /max-width/.test(query) ? narrow : !narrow,
      media: query,
      onchange: null,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
    }) as unknown as MediaQueryList) as unknown as typeof window.matchMedia;
}

function setLanguage(lang: string) {
  Object.defineProperty(window.navigator, "language", { value: lang, configurable: true });
}

function jsonBody(body: Parameters<typeof HttpResponse.json>[0]) {
  return HttpResponse.json(body, { headers: { "Trace-Id": TRACE_ID } });
}

function dataPage(rows: unknown[]) {
  return HttpResponse.json({ items: rows, cursor: null }, { headers: { "Trace-Id": TRACE_ID } });
}

const B = "*/api/v1/packs/acme/menu-board";

/** The handlers every render needs: the pack, the org scope-node, and (by default)
 * the list-detail page doc + en catalog. Tests override the doc/catalog/data. */
function baseHandlers(over: { doc?: unknown; en?: Record<string, string> } = {}) {
  return [
    http.get(`${B}`, () => ok(pack(), { revision: 1 })),
    http.get("*/api/v1/scope-nodes", () =>
      dataPage([{ id: ULID_ROOT, kind: "org", parent_id: null, name: "Org", revision: 1 }]),
    ),
    http.get(`${B}/pages/menu-items`, () => jsonBody(over.doc ?? menuItemsDoc)),
    http.get(`${B}/messages/en`, () => jsonBody(over.en ?? PACK_EN_CATALOG)),
  ];
}

function renderPack(path = "menu-items") {
  return render(
    <ThemeProvider>
      <MemoryRouter initialEntries={[`/p/acme/menu-board/${path}`]}>
        <Routes>
          <Route path="/p/:publisher/:name/*" element={<PackPageRoute />} />
        </Routes>
      </MemoryRouter>
    </ThemeProvider>,
  );
}

describe("Pack page — rendered through the shared renderer", () => {
  it("the example page doc passes validatePage (the same gate every page clears)", () => {
    expect(validatePage(menuItemsDoc).ok).toBe(true);
  });

  it("renders an installed pack's list-detail page with its pack-data rows", async () => {
    server.use(
      ...baseHandlers(),
      http.get(`${B}/data/menu_items`, () =>
        dataPage([
          packRow({ entity_id: ULID_A, name: "Cortado", section: "Coffee" }),
          packRow({ entity_id: ULID_B, name: "Almond croissant", section: "Pastry" }),
        ]),
      ),
    );
    renderPack();
    // The page header carries the catalog-resolved page title + the pack name.
    expect(await screen.findByRole("heading", { name: "Menu Items" })).toBeInTheDocument();
    // A real DataTable painted by the renderer, with the pack's rows.
    const table = await screen.findByRole("table", { name: "Menu items" });
    expect(within(table).getByText("Cortado")).toBeInTheDocument();
    expect(within(table).getByText("Almond croissant")).toBeInTheDocument();
    // Column headers came from the pack's own catalog.
    expect(within(table).getByText("Name")).toBeInTheDocument();
    expect(within(table).getByText("Section")).toBeInTheDocument();
  });

  it("shows the standard error EmptyState with the taxonomy code for an invalid document — never a crash or partial render", async () => {
    server.use(...baseHandlers({ doc: { pageType: "teleporter" } }));
    renderPack();
    const invalid = await screen.findByText(/This page could not be displayed/i);
    expect(invalid).toBeInTheDocument();
    // The taxonomy code is surfaced (a driver asserts on the code, UIS-200).
    expect(screen.getAllByText(/UNKNOWN_PAGE_TYPE/).length).toBeGreaterThan(0);
    // No table was painted — the invalid doc never reached the renderer.
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
  });

  it("resolves msg: refs from the active locale, falling back to en for missing keys (MAN-111)", async () => {
    setLanguage("es");
    server.use(
      ...baseHandlers(),
      // The es catalog translates only the name column; section + the rest fall
      // back to the guaranteed en base.
      http.get(`${B}/messages/es`, () => jsonBody({ "col.name": "Nombre" })),
      http.get(`${B}/data/menu_items`, () => dataPage([packRow()])),
    );
    renderPack();
    const table = await screen.findByRole("table", { name: "Menu items" });
    // es override wins…
    expect(within(table).getByText("Nombre")).toBeInTheDocument();
    // …and a key absent from es resolves through the en fallback.
    expect(within(table).getByText("Section")).toBeInTheDocument();
  });
});

describe("Pack page — pack-data create / edit over the api/1 conventions", () => {
  it("creates a row carrying an Idempotency-Key + the required scope_node, then shows it", async () => {
    const state = { rows: [packRow({ entity_id: ULID_A, name: "Cortado" })] as unknown[] };
    let idempotencyKey: string | null = null;
    let postedScope: unknown = "unset";
    server.use(
      ...baseHandlers(),
      http.get(`${B}/data/menu_items`, () => dataPage(state.rows)),
      http.post(`${B}/data/menu_items`, async ({ request }) => {
        idempotencyKey = request.headers.get("Idempotency-Key");
        const body = (await request.json()) as { scope_node?: unknown };
        postedScope = body.scope_node;
        const created = packRow({ entity_id: ULID_B, name: "New item", revision: 1 });
        state.rows = [...state.rows, created];
        return ok(created, { status: 201, revision: 1 });
      }),
    );

    const user = userEvent.setup();
    renderPack();
    await screen.findByRole("table", { name: "Menu items" });
    await user.click(screen.getByRole("button", { name: "New" }));

    await waitFor(() =>
      expect(within(screen.getByRole("table", { name: "Menu items" })).getByText("New item")).toBeInTheDocument(),
    );
    expect(idempotencyKey).toMatch(/^[0-9a-f-]{36}$/i);
    // The row was attached under the org scope (MAN-051 requires a scope_node ULID).
    expect(postedScope).toBe(ULID_ROOT);
  });

  it("edits a row under its If-Match and persists the change", async () => {
    const state = { rows: [packRow({ entity_id: ULID_A, name: "Cortado", revision: 3 })] as unknown[] };
    let ifMatch: string | null = null;
    server.use(
      ...baseHandlers(),
      http.get(`${B}/data/menu_items`, () => dataPage(state.rows)),
      http.patch(`${B}/data/menu_items/${ULID_A}`, async ({ request }) => {
        ifMatch = request.headers.get("If-Match");
        const body = (await request.json()) as { name?: string };
        const updated = packRow({ entity_id: ULID_A, name: body.name ?? "Cortado", revision: 4 });
        state.rows = [updated];
        return ok(updated, { revision: 4 });
      }),
    );

    const user = userEvent.setup();
    renderPack();
    await screen.findByRole("table", { name: "Menu items" });

    await user.click(screen.getByText("Cortado").closest("tr") as HTMLElement);
    const nameInput = await screen.findByLabelText("Item name");
    await user.clear(nameInput);
    await user.type(nameInput, "Flat white");
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() =>
      expect(within(screen.getByRole("table", { name: "Menu items" })).getByText("Flat white")).toBeInTheDocument(),
    );
    // The edit carried the If-Match derived from the row's revision (API-020).
    expect(ifMatch).toBe('"3"');
  });

  it("maps a 422 write rejection's per-field error onto the FormField (not only a toast) and keeps the edit", async () => {
    const state = { rows: [packRow({ entity_id: ULID_A, name: "Cortado", revision: 3 })] as unknown[] };
    let patchCount = 0;
    server.use(
      ...baseHandlers(),
      http.get(`${B}/data/menu_items`, () => dataPage(state.rows)),
      http.patch(`${B}/data/menu_items/${ULID_A}`, () => {
        patchCount += 1;
        return problem(422, "VALIDATION_FAILED", "One or more fields failed validation.", {
          errors: [{ field: "name", code: "ALREADY_EXISTS", message: "A menu item with this name already exists." }],
        });
      }),
    );

    const user = userEvent.setup();
    renderPack();
    await screen.findByRole("table", { name: "Menu items" });

    await user.click(screen.getByText("Cortado").closest("tr") as HTMLElement);
    const nameInput = await screen.findByLabelText("Item name");
    await user.clear(nameInput);
    await user.type(nameInput, "Latte");
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    const inlineError = await screen.findByText("A menu item with this name already exists.");
    expect(inlineError).toHaveAttribute("role", "alert");
    const fieldAfter = screen.getByLabelText("Item name") as HTMLInputElement;
    expect(fieldAfter).toHaveAttribute("aria-invalid", "true");
    expect(fieldAfter.value).toBe("Latte");
    expect(patchCount).toBe(1);
  });

  it("on a 412 re-reads the current state for review — never a silent overwrite", async () => {
    const changed = packRow({ entity_id: ULID_A, name: "Changed elsewhere", revision: 9 });
    const state = { rows: [packRow({ entity_id: ULID_A, name: "Cortado", revision: 3 })] as unknown[] };
    let patchCount = 0;
    server.use(
      ...baseHandlers(),
      http.get(`${B}/data/menu_items`, () => dataPage(state.rows)),
      http.get(`${B}/data/menu_items/${ULID_A}`, () => ok(changed, { revision: 9 })),
      http.patch(`${B}/data/menu_items/${ULID_A}`, () => {
        patchCount += 1;
        state.rows = [changed];
        return problem(412, "REVISION_CONFLICT", "The row was modified concurrently.", { current_revision: 9 });
      }),
    );

    const user = userEvent.setup();
    renderPack();
    await screen.findByRole("table", { name: "Menu items" });

    await user.click(screen.getByText("Cortado").closest("tr") as HTMLElement);
    const nameInput = await screen.findByLabelText("Item name");
    await user.clear(nameInput);
    await user.type(nameInput, "My rename");
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    // A review state: the current server value surfaces, the user's rename was NOT
    // written, and exactly one write was attempted (no retry-overwrite).
    const banner = await screen.findByRole("status");
    expect(banner).toHaveTextContent(/changed elsewhere/i);
    await waitFor(() =>
      expect(within(screen.getByRole("table", { name: "Menu items" })).getByText("Changed elsewhere")).toBeInTheDocument(),
    );
    expect(screen.queryByText("My rename")).not.toBeInTheDocument();
    expect(patchCount).toBe(1);
  });
});

describe("Pack page — responsive at 360px", () => {
  it("stacks the list into cards (no wide table to overflow the page)", async () => {
    setViewport(true);
    server.use(...baseHandlers(), http.get(`${B}/data/menu_items`, () => dataPage([packRow()])));
    renderPack();
    await waitFor(() =>
      expect(document.querySelector('[data-slot="data-table"][data-layout="stacked"]')).not.toBeNull(),
    );
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
    expect(screen.getByText("Cortado")).toBeInTheDocument();
  });
});
