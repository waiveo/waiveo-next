import { describe, it, expect, beforeAll, afterAll, afterEach, vi } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
// The kit re-exports this exact sonner `toast` object, so spying here intercepts
// the route's `toast.success`/`toast.error` calls deterministically.
import { toast as sonnerToast } from "sonner";
import { MemoryRouter, Route, Routes } from "react-router";
import { ThemeProvider } from "@/components/theme/theme-provider";
import PackPageRoute, { resolveDefaultScopeNode } from "./pack-page-route";
import { validatePage } from "@/renderer/validate";
import type { ScopeNode } from "@/api";
import {
  TRACE_ID,
  ULID_A,
  ULID_B,
  ULID_C,
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

// A conformant settings-form page (UIS-030/031) — no manifest-declared collection
// backs it (only list-detail binds one this wave), and it MUST wire a submit action.
const settingsFormDoc = {
  pageType: "settings-form",
  source: "prefs",
  sections: [
    {
      titleMsg: "msg:detail.title",
      fields: [{ type: "text-input", bind: "greeting", props: { labelMsg: "msg:detail.name" } }],
    },
  ],
  actions: [
    { type: "button", props: { labelMsg: "msg:detail.save", style: "primary" }, on: { press: { verb: "submit" } } },
  ],
};

// A conformant list-detail page whose list.source is the spec-legal PAGINATED form
// (UIS-023/024): its rows are fetched page-by-page by the renderer through the
// ActionHandler.fetchPage seam, NOT from the eagerly preloaded resource tree.
const paginatedDoc = {
  pageType: "list-detail",
  list: {
    source: { path: "menu_items", paginated: true, limit: 1 },
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
      children: [{ type: "text-input", bind: "name", props: { labelMsg: "msg:detail.name" } }],
    },
  },
};

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => {
  server.resetHandlers();
  setViewport(false);
  setLanguage("en-US");
  window.localStorage.clear();
  vi.restoreAllMocks();
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

// A minimal ScopeNode fixture for the pure resolver: only the fields the resolver
// reads (id/kind/parent_id) matter; the rest satisfy the type.
function node(over: Partial<ScopeNode> & Pick<ScopeNode, "id" | "kind">): ScopeNode {
  return {
    parent_id: null,
    name: "Node",
    labels: [],
    revision: 1,
    created_at: "2026-07-22T00:00:00Z",
    updated_at: "2026-07-22T00:00:00Z",
    ...over,
  } as ScopeNode;
}

describe("resolveDefaultScopeNode — the create target from the deployment's scope tree", () => {
  it("returns null only for a genuinely scope-less deployment", () => {
    expect(resolveDefaultScopeNode([])).toBeNull();
  });

  it("prefers the org root over a site and a screen", () => {
    const nodes = [
      node({ id: ULID_C, kind: "screen", parent_id: ULID_B }),
      node({ id: ULID_B, kind: "site", parent_id: ULID_ROOT }),
      node({ id: ULID_ROOT, kind: "org", parent_id: null }),
    ];
    expect(resolveDefaultScopeNode(nodes)).toBe(ULID_ROOT);
  });

  // The exact make-dev-up shape: a Demo Site whose org ancestor is a virtual boundary
  // (never an inserted row) plus a screen under it — no org row at all. This is the
  // cold-open create that shipped broken; the resolver MUST fall through to the site.
  it("falls through to the site when no org row exists (the make dev-up seed)", () => {
    const nodes = [
      node({ id: ULID_ROOT, kind: "site", parent_id: "01J8Z0DEMOORGANCESTORBOUND" }),
      node({ id: ULID_C, kind: "screen", parent_id: ULID_ROOT }),
    ];
    expect(resolveDefaultScopeNode(nodes)).toBe(ULID_ROOT);
  });

  it("falls through to the topmost node of any kind when neither org nor site exists", () => {
    // A group whose parent is absent from the queryable set is the topmost; its child
    // screen is not chosen.
    const nodes = [
      node({ id: ULID_C, kind: "screen", parent_id: ULID_A }),
      node({ id: ULID_A, kind: "group", parent_id: "01J8Z0ABSENTPARENTBOUNDARY" }),
    ];
    expect(resolveDefaultScopeNode(nodes)).toBe(ULID_A);
  });

  it("is deterministic — a sortable-ULID tiebreak when several nodes share the winning kind", () => {
    const nodes = [
      node({ id: ULID_B, kind: "org", parent_id: null }),
      node({ id: ULID_A, kind: "org", parent_id: null }),
    ];
    // ULID_A sorts before ULID_B; the choice does not depend on input order.
    expect(resolveDefaultScopeNode(nodes)).toBe(ULID_A);
    expect(resolveDefaultScopeNode([...nodes].reverse())).toBe(ULID_A);
  });
});

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
    // New opens a create draft (seeded from itemDefault, name "New item"); Save
    // commits it through the pack-data create path (UIS-021).
    await user.click(screen.getByRole("button", { name: "New" }));
    await user.click(await screen.findByRole("button", { name: "Save changes" }));

    await waitFor(() =>
      expect(within(screen.getByRole("table", { name: "Menu items" })).getByText("New item")).toBeInTheDocument(),
    );
    expect(idempotencyKey).toMatch(/^[0-9a-f-]{36}$/i);
    // The row was attached under the org scope (MAN-051 requires a scope_node ULID).
    expect(postedScope).toBe(ULID_ROOT);
  });

  // The cold-open regression: a stock `make dev-up` seeds ONLY a Demo Site (kind=site)
  // whose org ancestor is a virtual boundary (never an inserted row) plus a screen —
  // there is NO kind=org row. The create used to resolve its scope from `kind=org`
  // alone, find nothing, and refuse with "no scope to attach records to yet". It must
  // instead fall through to the site and post that scope_node, so an out-of-box create
  // works with no operator-provisioned org.
  it("resolves the scope from the site when a site-only deployment carries no org row (MAN-051)", async () => {
    const SITE = ULID_ROOT; // the Demo Site node id (kind=site)
    const state = { rows: [] as unknown[] };
    let postedScope: unknown = "unset";
    const errorSpy = vi.spyOn(sonnerToast, "error");
    server.use(
      http.get(`${B}`, () => ok(pack(), { revision: 1 })),
      // A site-only tree: the site's org ancestor is a virtual boundary absent from
      // the returned set, and a screen sits under the site — exactly the seed shape.
      http.get("*/api/v1/scope-nodes", () =>
        dataPage([
          { id: SITE, kind: "site", parent_id: "01J8Z0DEMOORGANCESTORBOUND", name: "Demo Site", revision: 1 },
          { id: ULID_C, kind: "screen", parent_id: SITE, name: "Demo Screen", revision: 1 },
        ]),
      ),
      http.get(`${B}/pages/menu-items`, () => jsonBody(menuItemsDoc)),
      http.get(`${B}/messages/en`, () => jsonBody(PACK_EN_CATALOG)),
      http.get(`${B}/data/menu_items`, () => dataPage(state.rows)),
      http.post(`${B}/data/menu_items`, async ({ request }) => {
        const body = (await request.json()) as { scope_node?: unknown };
        postedScope = body.scope_node;
        const created = packRow({ entity_id: ULID_B, name: "New item", scope_node: SITE, revision: 1 });
        state.rows = [...state.rows, created];
        return ok(created, { status: 201, revision: 1 });
      }),
    );

    const user = userEvent.setup();
    renderPack();
    await screen.findByRole("table", { name: "Menu items" });
    await user.click(screen.getByRole("button", { name: "New" }));
    await user.click(await screen.findByRole("button", { name: "Save changes" }));

    // The row was created AND attached to the SITE scope — never refused, never the
    // screen (site is preferred as the topmost node the invariant guarantees).
    await waitFor(() =>
      expect(within(screen.getByRole("table", { name: "Menu items" })).getByText("New item")).toBeInTheDocument(),
    );
    expect(postedScope).toBe(SITE);
    expect(errorSpy).not.toHaveBeenCalled();
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

describe("Pack page — safety + spec-form regressions", () => {
  // Regression: a settings-form (or any page binding no manifest-declared
  // collection) Save reported a green "Saved" without persisting anything — a
  // false-positive success. The submit must never fabricate a save that did not
  // happen; it reports honestly instead.
  it("a settings-form Save never fabricates a green 'Saved' when nothing is persisted", async () => {
    // The fixture is a conformant page (so the Save button is real) that binds no
    // collection — the whole precondition of the buggy branch.
    expect(validatePage(settingsFormDoc).ok).toBe(true);
    const successSpy = vi.spyOn(sonnerToast, "success");
    const errorSpy = vi.spyOn(sonnerToast, "error");
    server.use(
      http.get(`${B}`, () => ok(pack(), { revision: 1 })),
      http.get("*/api/v1/scope-nodes", () =>
        dataPage([{ id: ULID_ROOT, kind: "org", parent_id: null, name: "Org", revision: 1 }]),
      ),
      http.get(`${B}/pages/settings`, () => jsonBody(settingsFormDoc)),
      http.get(`${B}/messages/en`, () => jsonBody(PACK_EN_CATALOG)),
    );

    const user = userEvent.setup();
    renderPack("settings");
    await user.click(await screen.findByRole("button", { name: "Save changes" }));

    // The honest signal fires; the false-positive success toast never does.
    await waitFor(() => expect(errorSpy).toHaveBeenCalled());
    expect(successSpy).not.toHaveBeenCalled();
  });

  // Regression: a spec-legal PAGINATED list.source (UIS-023/024) rendered zero rows
  // forever because the ActionHandler never wired fetchPage — the renderer's
  // PaginatedList no-ops without it. It must fetch, render, and page.
  it("a paginated list.source renders its rows through fetchPage, with a working 'Load more'", async () => {
    expect(validatePage(paginatedDoc).ok).toBe(true);
    server.use(
      http.get(`${B}`, () => ok(pack(), { revision: 1 })),
      http.get("*/api/v1/scope-nodes", () =>
        dataPage([{ id: ULID_ROOT, kind: "org", parent_id: null, name: "Org", revision: 1 }]),
      ),
      http.get(`${B}/pages/menu-items`, () => jsonBody(paginatedDoc)),
      http.get(`${B}/messages/en`, () => jsonBody(PACK_EN_CATALOG)),
      // One keyset row per page: the first carries a continuation cursor, the second
      // closes it (cursor: null → the final page, no further "Load more").
      http.get(`${B}/data/menu_items`, ({ request }) => {
        const cursor = new URL(request.url).searchParams.get("cursor");
        return cursor
          ? HttpResponse.json(
              { items: [packRow({ entity_id: ULID_B, name: "Almond croissant", section: "Pastry" })], cursor: null },
              { headers: { "Trace-Id": TRACE_ID } },
            )
          : HttpResponse.json(
              { items: [packRow({ entity_id: ULID_A, name: "Cortado", section: "Coffee" })], cursor: "CURSOR-2" },
              { headers: { "Trace-Id": TRACE_ID } },
            );
      }),
    );

    const user = userEvent.setup();
    renderPack();
    await screen.findByRole("table", { name: "Menu items" });
    // The first keyset page rendered THROUGH fetchPage — not zero rows forever.
    expect(await screen.findByText("Cortado")).toBeInTheDocument();
    // The second page is not shown until the affordance is used…
    expect(screen.queryByText("Almond croissant")).not.toBeInTheDocument();
    // …and "Load more" fetches it (the opaque cursor threaded back verbatim).
    await user.click(screen.getByRole("button", { name: "Load more" }));
    expect(await screen.findByText("Almond croissant")).toBeInTheDocument();
    // The final page closed the cursor — no further "Load more".
    expect(screen.queryByRole("button", { name: "Load more" })).not.toBeInTheDocument();
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
