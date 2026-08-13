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

// A conformant settings-form page (UIS-030/031) whose `source` names NO declared
// collection. MAN-064 refuses such a pack at install, so this describes a document
// the console should never receive — it is kept as the console's own defence: if
// one ever arrives, Save must report honestly rather than fabricate a success.
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

// The extra catalog entries these pages need, on top of the fixture pack's.
const ACTION_CATALOG = {
  ...PACK_EN_CATALOG,
  "act.run": "Run backup",
  "wiz.next": "Continue",
  "wiz.finish": "Finish",
};

// A `settings-form` carrying a `call-action` button BESIDE its Save. Beside, not
// instead: UIS-031 requires a settings-form to wire at least one submit, which is
// the contract being right — a form with no way to save is not a settings form.
const callActionDoc = {
  pageType: "settings-form",
  source: "settings",
  sections: [
    {
      titleMsg: "msg:detail.title",
      fields: [{ type: "text-input", bind: "greeting", props: { labelMsg: "msg:detail.name" } }],
    },
  ],
  actions: [
    {
      type: "button",
      props: { labelMsg: "msg:detail.save", style: "primary" },
      on: { press: { verb: "submit" } },
    },
    {
      type: "button",
      props: { labelMsg: "msg:act.run" },
      on: { press: { verb: "call-action", action: "run-backup", params: { full: true, note: "greeting" } } },
    },
  ],
};

// A `wizard` with NO draftSource: every step's Scope is the ephemeral `$ui.draft`
// (UIS-051), and `onFinish` is responsible for persisting it — here through a
// `call-action` whose params read the draft out. This is the exact shape UIS-051
// names, and it needed the callAction seam to exist at all.
const wizardDoc = {
  pageType: "wizard",
  steps: [
    {
      id: "name",
      titleMsg: "msg:detail.title",
      root: {
        type: "section",
        props: { titleMsg: "msg:detail.title" },
        children: [
          { type: "text-input", bind: "greeting", props: { labelMsg: "msg:detail.name" } },
          { type: "button", props: { labelMsg: "msg:wiz.next" }, on: { press: { verb: "wizard-next" } } },
        ],
      },
    },
    // No authored finish button: the wizard chrome renders its own Finish on the
    // last step, which is the control an operator actually presses.
    { id: "confirm", titleMsg: "msg:detail.title", root: { type: "text", props: { value: "greeting" } } },
  ],
  onFinish: { verb: "call-action", action: "run-backup", params: { greeting: "greeting" } },
};

// A `wizard` that DOES declare a draftSource: it progressively edits a real
// backing resource (UIS-051), here the fixture pack's singleton `settings`
// collection. Its onFinish is a plain target-less `submit` — the whole point of
// declaring a draftSource is that the wizard has somewhere to save to.
const backedWizardDoc = {
  pageType: "wizard",
  draftSource: "settings",
  steps: [
    {
      id: "greeting",
      titleMsg: "msg:detail.title",
      root: {
        type: "section",
        props: { titleMsg: "msg:detail.title" },
        children: [
          { type: "text-input", bind: "greeting", props: { labelMsg: "msg:detail.name" } },
          { type: "button", props: { labelMsg: "msg:wiz.next" }, on: { press: { verb: "wizard-next" } } },
        ],
      },
    },
    { id: "confirm", titleMsg: "msg:detail.title", root: { type: "text", props: { value: "greeting" } } },
  ],
  onFinish: { verb: "submit" },
};

// A conformant `dashboard` page (UIS-040) reading TWO declared collections from
// separate tiles, plus a third tile bound to nothing fetchable. A dashboard has no
// page-wide bound resource by design — each tile resolves its own root Binding —
// which is exactly what the route used to have no way to express.
const dashboardDoc = {
  pageType: "dashboard",
  tiles: [
    {
      size: "large",
      widget: {
        type: "table",
        id: "Menu items",
        props: {
          source: "menu_items",
          columns: [
            { headerMsg: "msg:col.name", cell: "item.name" },
            { headerMsg: "msg:col.section", cell: "item.section" },
          ],
        },
      },
    },
    {
      size: "small",
      widget: { type: "stat-tile", props: { labelMsg: "msg:col.name", value: "settings.greeting" } },
    },
    // Reads renderer state, not a collection: nothing must be fetched for it.
    { size: "small", widget: { type: "text", props: { value: "$ui.nothing" } } },
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
      node({ id: ULID_ROOT, kind: "site", parent_id: "01J8Z0DEM00RGANCEST0RB0VND" }),
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
          { id: SITE, kind: "site", parent_id: "01J8Z0DEM00RGANCEST0RB0VND", name: "Demo Site", revision: 1 },
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

  // Efficiency regression (bounded create-default query): the create target is
  // resolved from a bounded `kind in (org,site)` selector — a 1–2 row query on a
  // one-org/one-site deployment — NOT an unfiltered walk of every group and screen in
  // the fleet on every pack-page open. A resolver that fetched the whole scope-node
  // collection would fail the "only the bounded selector was issued" assertion here.
  it("resolves the create scope from a bounded kind in (org,site) query — never a full-tree walk", async () => {
    const SITE = ULID_ROOT; // the Demo Site node id (kind=site)
    const selectors: Array<string | null> = [];
    const state = { rows: [] as unknown[] };
    let postedScope: unknown = "unset";
    server.use(
      http.get(`${B}`, () => ok(pack(), { revision: 1 })),
      http.get("*/api/v1/scope-nodes", ({ request }) => {
        const selector = new URL(request.url).searchParams.get("selector");
        selectors.push(selector);
        // The server honors the selector: the bounded query returns just the site
        // (its org ancestor is a virtual boundary, never a row). Were the app to fall
        // to the unfiltered walk it would additionally drag in the screen — the very
        // rows the bounded query exists to avoid fetching.
        return selector === "kind in (org,site)"
          ? dataPage([{ id: SITE, kind: "site", parent_id: "01J8Z0DEM00RGANCEST0RB0VND", name: "Demo Site", revision: 1 }])
          : dataPage([
              { id: SITE, kind: "site", parent_id: "01J8Z0DEM00RGANCEST0RB0VND", name: "Demo Site", revision: 1 },
              { id: ULID_C, kind: "screen", parent_id: SITE, name: "Demo Screen", revision: 1 },
            ]);
      }),
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

    // The site resolved as the create target from the bounded query alone…
    await waitFor(() =>
      expect(within(screen.getByRole("table", { name: "Menu items" })).getByText("New item")).toBeInTheDocument(),
    );
    expect(postedScope).toBe(SITE);
    // …and the page issued ONLY the bounded selector — never the unfiltered full-tree
    // walk. This is the efficiency guard: a normal pack-page open must not fetch every
    // scope node in the deployment just to pick a create default.
    expect(selectors).toContain("kind in (org,site)");
    expect(selectors.every((s) => s === "kind in (org,site)")).toBe(true);
  });

  // The fallback path: a deployment with NEITHER an org nor a site (effectively
  // unreachable after `make dev-up`) still resolves a target — the bounded query
  // returns empty, so the resolver walks the full unfiltered set exactly once to pick
  // any root node (MAN-051: a pack row attaches to ANY scope node).
  it("falls back to the full unfiltered walk only when neither an org nor a site exists", async () => {
    const GROUP = ULID_A;
    const selectors: Array<string | null> = [];
    const state = { rows: [] as unknown[] };
    let postedScope: unknown = "unset";
    server.use(
      http.get(`${B}`, () => ok(pack(), { revision: 1 })),
      http.get("*/api/v1/scope-nodes", ({ request }) => {
        const selector = new URL(request.url).searchParams.get("selector");
        selectors.push(selector);
        // The bounded query is EMPTY (no org, no site); only the full walk surfaces
        // the group root the row must attach to.
        return selector === "kind in (org,site)"
          ? dataPage([])
          : dataPage([
              { id: GROUP, kind: "group", parent_id: "01J8Z0ABSENTPARENTBOUNDARY", name: "Lobby group", revision: 1 },
              { id: ULID_C, kind: "screen", parent_id: GROUP, name: "Lobby screen", revision: 1 },
            ]);
      }),
      http.get(`${B}/pages/menu-items`, () => jsonBody(menuItemsDoc)),
      http.get(`${B}/messages/en`, () => jsonBody(PACK_EN_CATALOG)),
      http.get(`${B}/data/menu_items`, () => dataPage(state.rows)),
      http.post(`${B}/data/menu_items`, async ({ request }) => {
        const body = (await request.json()) as { scope_node?: unknown };
        postedScope = body.scope_node;
        const created = packRow({ entity_id: ULID_B, name: "New item", scope_node: GROUP, revision: 1 });
        state.rows = [...state.rows, created];
        return ok(created, { status: 201, revision: 1 });
      }),
    );

    const user = userEvent.setup();
    renderPack();
    await screen.findByRole("table", { name: "Menu items" });
    await user.click(screen.getByRole("button", { name: "New" }));
    await user.click(await screen.findByRole("button", { name: "Save changes" }));

    await waitFor(() =>
      expect(within(screen.getByRole("table", { name: "Menu items" })).getByText("New item")).toBeInTheDocument(),
    );
    // The bounded query ran FIRST, then the fallback walk (selector-less) — and the
    // group root resolved the create target.
    expect(selectors).toEqual(["kind in (org,site)", null]);
    expect(postedScope).toBe(GROUP);
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

  // ── The settings-form save (MAN-056/064) ──────────────────────────────────
  //
  // Observed on a running box before this landed: the example pack's settings
  // page rendered, an operator typed into it, pressed Save, and got "Saving
  // isn't available for this page yet." A settings-form binds ONE record; the
  // pack ships no rows, so on a first visit that record does not exist and the
  // save has to CREATE it. These two cases are that whole lifecycle.

  // A settings-form doc whose source names the fixture manifest's singleton.
  const boundSettingsDoc = { ...settingsFormDoc, source: "settings" };

  function settingsHandlers(extra: Parameters<typeof server.use>) {
    return [
      http.get(`${B}`, () => ok(pack(), { revision: 1 })),
      http.get("*/api/v1/scope-nodes", () =>
        dataPage([{ id: ULID_ROOT, kind: "org", parent_id: null, name: "Org", revision: 1 }]),
      ),
      http.get(`${B}/pages/settings`, () => jsonBody(boundSettingsDoc)),
      http.get(`${B}/messages/en`, () => jsonBody(PACK_EN_CATALOG)),
      ...extra,
    ];
  }

  it("a settings-form's FIRST save creates its record, carrying the resolved scope_node", async () => {
    expect(validatePage(boundSettingsDoc).ok).toBe(true);
    const successSpy = vi.spyOn(sonnerToast, "success");
    const posted: Array<Record<string, unknown>> = [];
    const state: { rows: ReturnType<typeof packRow>[] } = { rows: [] };
    server.use(
      ...settingsHandlers([
        http.get(`${B}/data/settings`, () => dataPage(state.rows)),
        http.post(`${B}/data/settings`, async ({ request }) => {
          const body = (await request.json()) as Record<string, unknown>;
          posted.push(body);
          state.rows = [packRow({ entity_id: ULID_A, revision: 1, greeting: body.greeting })];
          return HttpResponse.json(state.rows[0], { status: 201, headers: { "Trace-Id": TRACE_ID } });
        }),
      ]),
    );

    const user = userEvent.setup();
    renderPack("settings");
    await user.type(await screen.findByLabelText("Item name"), "Hello");
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(posted).toHaveLength(1));
    // The typed value reached the server, under a scope the row can attach to
    // (MAN-051) — a create missing either is the shape that 422s in the field.
    expect(posted[0].greeting).toBe("Hello");
    expect(posted[0].scope_node).toBe(ULID_ROOT);
    await waitFor(() => expect(successSpy).toHaveBeenCalled());
  });

  it("a settings-form's LATER save updates the existing record under its If-Match, never creating a second", async () => {
    const patches: Array<{ ifMatch: string | null; body: Record<string, unknown> }> = [];
    let creates = 0;
    server.use(
      ...settingsHandlers([
        // The record already exists — the state a box is in on every visit after
        // the first.
        http.get(`${B}/data/settings`, () =>
          dataPage([packRow({ entity_id: ULID_A, revision: 4, greeting: "Hello" })]),
        ),
        http.post(`${B}/data/settings`, () => {
          creates++;
          return HttpResponse.json({}, { status: 201, headers: { "Trace-Id": TRACE_ID } });
        }),
        http.patch(`${B}/data/settings/${ULID_A}`, async ({ request }) => {
          patches.push({
            ifMatch: request.headers.get("If-Match"),
            body: (await request.json()) as Record<string, unknown>,
          });
          return ok(packRow({ entity_id: ULID_A, revision: 5, greeting: "Bonjour" }), { revision: 5 });
        }),
      ]),
    );

    const user = userEvent.setup();
    renderPack("settings");
    const field = await screen.findByLabelText("Item name");
    // The existing value is what the form opens on — a settings-form that always
    // rendered blank would silently discard the saved record on the next save.
    await waitFor(() => expect(field).toHaveValue("Hello"));
    await user.clear(field);
    await user.type(field, "Bonjour");
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(patches).toHaveLength(1));
    expect(patches[0].body.greeting).toBe("Bonjour");
    // Under the row's revision, not unconditionally (API-022).
    expect(patches[0].ifMatch).toBe('"4"');
    // And it did NOT take the create path — that would 409 against the singleton
    // bound on a real server (MAN-056), losing the operator's edit.
    expect(creates).toBe(0);
  });

  // The other half of the wizard row. A wizard that declares a draftSource edits
  // a REAL backing resource (UIS-051), so its onFinish `submit` must land — and
  // it did not: `primaryCollection` resolved a collection for list-detail and
  // settings-form only, so a wizard fell into the "Saving isn't available for
  // this page yet" branch however it was authored.
  //
  // The record does not exist on a pack's first visit (nothing seeds one at
  // install), and a wizard is if anything the most obvious create flow there is,
  // so the first Finish must CREATE it.
  it("a wizard with a draftSource creates its record on the first finish (UIS-051)", async () => {
    expect(validatePage(backedWizardDoc).ok).toBe(true);
    const successSpy = vi.spyOn(sonnerToast, "success");
    let created: Record<string, unknown> | null = null;
    server.use(
      ...baseHandlers({ doc: backedWizardDoc, en: ACTION_CATALOG }),
      // Empty: the singleton has no row yet, which is the cold-open case.
      http.get(`${B}/data/settings`, () => dataPage([])),
      http.post(`${B}/data/settings`, async ({ request }) => {
        created = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json(
          { data: packRow({ entity_id: ULID_C, greeting: "Good evening" }), revision: 1 },
          { status: 201, headers: { "Trace-Id": TRACE_ID } },
        );
      }),
    );
    renderPack();

    await userEvent.type(await screen.findByLabelText("Item name"), "Good evening");
    await userEvent.click(screen.getByRole("button", { name: "Continue" }));
    await userEvent.click(await screen.findByRole("button", { name: "Finish" }));

    await waitFor(() => expect(created).not.toBeNull());
    const body = created as unknown as Record<string, unknown>;
    expect(body.greeting).toBe("Good evening");
    // Attached under the deployment's resolved root scope — a row must carry one
    // (MAN-051), and a wizard's create has no other source for it.
    expect(body.scope_node).toBe(ULID_ROOT);
    expect(successSpy).toHaveBeenCalledWith("Saved changes");
  });

  // Second finish: the record exists now, so the save is an UPDATE under its
  // If-Match rather than a second create — the same rule a settings-form follows.
  it("a wizard with a draftSource updates the existing record under If-Match", async () => {
    let updated: { etag: string | null; body: Record<string, unknown> } | null = null;
    server.use(
      ...baseHandlers({ doc: backedWizardDoc, en: ACTION_CATALOG }),
      http.get(`${B}/data/settings`, () => dataPage([packRow({ entity_id: ULID_C, revision: 4, greeting: "Old" })])),
      http.patch(`${B}/data/settings/${ULID_C}`, async ({ request }) => {
        updated = {
          etag: request.headers.get("If-Match"),
          body: (await request.json()) as Record<string, unknown>,
        };
        return HttpResponse.json(
          { data: packRow({ entity_id: ULID_C, revision: 5, greeting: "New" }), revision: 5 },
          { headers: { "Trace-Id": TRACE_ID } },
        );
      }),
    );
    renderPack();

    // The wizard opens ON the existing record — the draftSource resolved to it —
    // so the field is seeded rather than blank. That is what "progressively
    // edits a real backing resource" means.
    const input = await screen.findByLabelText<HTMLInputElement>("Item name");
    expect(input.value).toBe("Old");
    await userEvent.clear(input);
    await userEvent.type(input, "New");
    await userEvent.click(screen.getByRole("button", { name: "Continue" }));
    await userEvent.click(await screen.findByRole("button", { name: "Finish" }));

    await waitFor(() => expect(updated).not.toBeNull());
    const call = updated as unknown as { etag: string | null; body: Record<string, unknown> };
    expect(call.body.greeting).toBe("New");
    // The optimistic-concurrency guard every other editor carries.
    expect(call.etag).toBe('"4"');
  });

    // A wizard whose draftSource names a collection holding MANY rows. UIS-051
  // calls draftSource "a real backing resource the wizard progressively edits",
  // and a whole collection is not one record — so there is nothing to save, and
  // no create either, because a create needs to know it is making the one row.
  //
  // This used to be a bare `return`: the operator pressed Finish and the page
  // said nothing at all. The same silent shape as the others in this family.
  it("reports honestly when a wizard's draftSource names a many-row collection", async () => {
    const doc = { ...backedWizardDoc, draftSource: "menu_items" };
    expect(validatePage(doc).ok).toBe(true);
    const successSpy = vi.spyOn(sonnerToast, "success");
    const errorSpy = vi.spyOn(sonnerToast, "error");
    server.use(
      ...baseHandlers({ doc, en: ACTION_CATALOG }),
      http.get(`${B}/data/menu_items`, () => dataPage([packRow({ entity_id: ULID_A }), packRow({ entity_id: ULID_B })])),
    );
    renderPack();

    await userEvent.click(await screen.findByRole("button", { name: "Continue" }));
    await userEvent.click(await screen.findByRole("button", { name: "Finish" }));

    await waitFor(() => expect(errorSpy).toHaveBeenCalledWith("There's no record to save here yet."));
    expect(successSpy).not.toHaveBeenCalled();
  });

  // Regression: the `call-action` seam was entirely unwired on a pack page, and
  // an unwired seam with no `outcomeTo` is a COMPLETE silent no-op — a pack
  // shipping a page with an action button had a button that did nothing at all.
  // Every other half of the actions plane existed; the pack's own UI was the one
  // caller that could not reach it.
  it("a pack page's call-action button invokes the pack's declared action (MAN-100/101)", async () => {
    expect(validatePage(callActionDoc).ok).toBe(true);
    const successSpy = vi.spyOn(sonnerToast, "success");
    let invoked: { url: string; body: unknown } | null = null;
    server.use(
      ...baseHandlers({ doc: callActionDoc, en: ACTION_CATALOG }),
      http.get(`${B}/data/settings`, () => dataPage([packRow({ entity_id: ULID_C, greeting: "Good morning" })])),
      http.post(`${B}/actions/run-backup`, async ({ request }) => {
        invoked = { url: request.url, body: await request.json() };
        return HttpResponse.json(
          { invocation_id: ULID_A, pack_id: "acme/menu-board", action: "run-backup", state: "pending" },
          { status: 202, headers: { "Trace-Id": TRACE_ID } },
        );
      }),
    );
    renderPack();

    await userEvent.click(await screen.findByRole("button", { name: "Run backup" }));

    // It reached the MAN-101 route for THIS action…
    await waitFor(() => expect(invoked).not.toBeNull());
    const call = invoked as unknown as { url: string; body: { params: Record<string, unknown> } };
    expect(call.url).toContain("/packs/acme/menu-board/actions/run-backup");
    // …carrying the declared params, with the Binding one RESOLVED against the
    // bound record rather than sent as the literal string "greeting".
    expect(call.body.params).toEqual({ full: true, note: "Good morning" });
    // Reported as STARTED, never "done": the call is queued and api/1 exposes no
    // way to read the invocation back, so acceptance is all the console knows.
    expect(successSpy).toHaveBeenCalledWith("Started run-backup");
  });

  it("reports a refused action honestly rather than a green toast", async () => {
    const successSpy = vi.spyOn(sonnerToast, "success");
    const errorSpy = vi.spyOn(sonnerToast, "error");
    server.use(
      ...baseHandlers({ doc: callActionDoc, en: ACTION_CATALOG }),
      http.get(`${B}/data/settings`, () => dataPage([packRow({ entity_id: ULID_C, greeting: "Good morning" })])),
      http.post(`${B}/actions/run-backup`, () =>
        problem(404, "NOT_FOUND", "This pack declares no action of that name."),
      ),
    );
    renderPack();

    await userEvent.click(await screen.findByRole("button", { name: "Run backup" }));

    await waitFor(() =>
      expect(errorSpy).toHaveBeenCalledWith(
        expect.stringContaining("This pack declares no action of that name."),
      ),
    );
    expect(successSpy).not.toHaveBeenCalled();
  });

  // The whole UIS-051 ephemeral-draft path, driven by clicking through the wizard
  // rather than by asserting on a render. A wizard with no `draftSource` edits
  // `$ui.draft`, which UIS-104 forbids from riding any implicit payload — so the
  // ONLY way its content reaches the host is an onFinish whose params read it
  // out. Without the callAction seam that path terminated in silence: the operator
  // filled in the wizard, pressed Finish, and nothing happened.
  it("a wizard with no draftSource persists through onFinish's call-action (UIS-051)", async () => {
    expect(validatePage(wizardDoc).ok).toBe(true);
    let body: { params?: Record<string, unknown> } | null = null;
    server.use(
      ...baseHandlers({ doc: wizardDoc, en: ACTION_CATALOG }),
      http.post(`${B}/actions/run-backup`, async ({ request }) => {
        body = (await request.json()) as { params?: Record<string, unknown> };
        return HttpResponse.json(
          { invocation_id: ULID_A, pack_id: "acme/menu-board", action: "run-backup", state: "pending" },
          { status: 202, headers: { "Trace-Id": TRACE_ID } },
        );
      }),
    );
    renderPack();

    // Step one: type into the draft, then advance.
    const input = await screen.findByLabelText("Item name");
    await userEvent.type(input, "Hello wizard");
    await userEvent.click(screen.getByRole("button", { name: "Continue" }));

    // Step two: the draft carried across the step boundary (UIS-052, one shared
    // Scope) — then finish through the wizard's own control.
    expect(await screen.findByText("Hello wizard")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Finish" }));

    await waitFor(() => expect(body).not.toBeNull());
    // What the operator typed into the EPHEMERAL draft reached the host.
    expect((body as unknown as { params: Record<string, unknown> }).params).toEqual({
      greeting: "Hello wizard",
    });
  });

  // Regression: a pack `dashboard` rendered COMPLETELY EMPTY. `primaryCollection`
  // returns null for a dashboard — correctly, since UIS-040 gives it no page-wide
  // bound resource — and the route read that null as "this page reads nothing",
  // handing the renderer `data = {}`. Every tile bound to a pack collection painted
  // blank, with no error to say why.
  it("a dashboard renders EVERY tile's collection, not an empty page (UIS-040)", async () => {
    expect(validatePage(dashboardDoc).ok).toBe(true);
    server.use(
      ...baseHandlers({ doc: dashboardDoc }),
      http.get(`${B}/data/menu_items`, () =>
        dataPage([
          packRow({ entity_id: ULID_A, name: "Cortado", section: "Coffee" }),
          packRow({ entity_id: ULID_B, name: "Almond croissant", section: "Pastry" }),
        ]),
      ),
      http.get(`${B}/data/settings`, () => dataPage([packRow({ entity_id: ULID_C, greeting: "Good morning" })])),
    );
    renderPack();

    // Tile one: the table's rows, from its own collection.
    const table = await screen.findByRole("table", { name: "Menu items" });
    expect(within(table).getByText("Cortado")).toBeInTheDocument();
    expect(within(table).getByText("Almond croissant")).toBeInTheDocument();
    // Tile two: a DIFFERENT collection, resolved independently — this is the half
    // a single page-wide collection could never have served.
    expect(await screen.findByText("Good morning")).toBeInTheDocument();
  });

  // The complement, and the reason the walk intersects with the DECLARED set: a
  // tile reading renderer state or a mistyped name must fetch nothing. msw is set
  // to error on an unhandled request, so a stray fetch fails this outright.
  it("a dashboard fetches nothing for a tile that names no declared collection", async () => {
    const doc = {
      pageType: "dashboard",
      tiles: [
        { size: "small", widget: { type: "text", props: { value: "$ui.nothing" } } },
        { size: "small", widget: { type: "text", props: { value: "menu_items_typo.name" } } },
      ],
    };
    expect(validatePage(doc).ok).toBe(true);
    // No data handler registered AT ALL: any fetch is an unhandled request.
    server.use(...baseHandlers({ doc }));
    renderPack();
    // The page still renders — an empty dashboard is a valid page, and the point
    // is that it got there without asking for a collection that does not exist.
    expect(await screen.findByRole("heading", { name: "Menu Items" })).toBeInTheDocument();
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
