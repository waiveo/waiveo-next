import { describe, it, expect, beforeAll, afterAll, afterEach, vi } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { ThemeProvider } from "@/components/theme/theme-provider";
import { Toaster } from "@/components/kit/toaster";
import { ScopeTreePanel } from "./scope-tree-panel";
import { createApi, type ScopeNode } from "@/api";
import { TEST_BASE, TRACE_ID, ULID_A, ULID_B, ULID_C, ULID_ROOT, ok, problem, scopeNode } from "@/api/test-support";

// The bootstrap this panel closes is the thing under test: on a box with an org
// node and nothing else, an operator must be able to reach a first screen. That
// path runs org → site → group, and every one of these tests drives it with real
// clicks and asserts the CREATE BODY, because a site created without tz/lat/long
// is refused by the server (DAT-031) and a panel that renders the three fields
// but forgets to send them would look completely correct.

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => {
  server.resetHandlers();
  window.localStorage.clear();
});
afterAll(() => server.close());

const org = scopeNode({ id: ULID_ROOT, kind: "org", name: "Waiveo", parent_id: null }) as ScopeNode;
const site = scopeNode({ id: ULID_A, kind: "site", name: "The Hanger", parent_id: ULID_ROOT }) as ScopeNode;
const group = scopeNode({ id: ULID_B, kind: "group", name: "West Wing", parent_id: ULID_A }) as ScopeNode;
const screenNode = scopeNode({
  id: ULID_C,
  kind: "screen",
  name: "Lobby display",
  parent_id: ULID_B,
  revision: 4,
}) as ScopeNode;

function renderPanel(nodes: ScopeNode[] = [org, site, group, screenNode]) {
  const api = createApi({ baseUrl: TEST_BASE });
  const onChanged = vi.fn(async () => {});
  render(
    <ThemeProvider>
      <ScopeTreePanel api={api} nodes={nodes} onChanged={onChanged} />
      <Toaster />
    </ThemeProvider>,
  );
  return { user: userEvent.setup(), onChanged };
}

describe("ScopeTreePanel — the hierarchy, rendered", () => {
  it("renders org > site > group > screen with each node's kind named", () => {
    renderPanel();
    const tree = screen.getByRole("region", { name: "Scope tree" });
    for (const name of ["Waiveo", "The Hanger", "West Wing", "Lobby display"]) {
      expect(within(tree).getByText(name)).toBeInTheDocument();
    }
    expect(within(tree).getByText("Organisation")).toBeInTheDocument();
    expect(within(tree).getByText("Site")).toBeInTheDocument();
    expect(within(tree).getByText("Group")).toBeInTheDocument();
    expect(within(tree).getByText("Screen")).toBeInTheDocument();
  });

  it("offers Add group only on the kinds DAT-003 lets carry one", () => {
    renderPanel();
    const tree = screen.getByRole("region", { name: "Scope tree" });
    // A site and a group may carry a group; the org may not (it carries sites)
    // and a screen is a leaf. Two Add group buttons, therefore — not four.
    expect(within(tree).getAllByRole("button", { name: /Add group/ })).toHaveLength(2);
  });

  it("never offers to delete the org node — DAT-022 makes it undeletable", () => {
    renderPanel();
    const tree = screen.getByRole("region", { name: "Scope tree" });
    expect(within(tree).queryByRole("button", { name: "Delete Waiveo" })).toBeNull();
    expect(within(tree).getByRole("button", { name: "Delete The Hanger" })).toBeInTheDocument();
  });

  it("explains an empty tree instead of showing an Add button with nowhere to add", () => {
    renderPanel([]);
    expect(screen.getByText("No scope tree")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /Add site/ })).toBeNull();
  });
});

describe("ScopeTreePanel — creating a site", () => {
  it("POSTs kind=site under the org with all three geo columns (DAT-031)", async () => {
    let body: Record<string, unknown> = {};
    server.use(
      http.post(`${TEST_BASE}/scope-nodes`, async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return ok(scopeNode({ id: ULID_A, kind: "site", name: "Cafe" }), { status: 201, revision: 1 });
      }),
    );
    const { user, onChanged } = renderPanel([org]);
    // The section-level Add site, the one an operator reaches on a bare box.
    await user.click(
      within(screen.getByRole("region", { name: "Scope tree" })).getAllByRole("button", {
        name: /Add site/,
      })[0]!,
    );
    const dialog = await screen.findByRole("dialog", { name: "Add a site" });
    await user.type(within(dialog).getByLabelText(/Name/), "Cafe");
    await user.type(within(dialog).getByLabelText(/Time zone/), "America/New_York");
    await user.type(within(dialog).getByLabelText(/Latitude/), "40.7128");
    await user.type(within(dialog).getByLabelText(/Longitude/), "-74.006");
    await user.click(within(dialog).getByRole("button", { name: "Add" }));

    await waitFor(() => expect(body["name"]).toBe("Cafe"));
    expect(body).toEqual({
      kind: "site",
      name: "Cafe",
      parent_id: ULID_ROOT,
      tz: "America/New_York",
      lat: 40.7128,
      long: -74.006,
    });
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
  });

  it("refuses to POST a site with no time zone, and says why", async () => {
    let posted = false;
    server.use(
      http.post(`${TEST_BASE}/scope-nodes`, () => {
        posted = true;
        return ok(scopeNode(), { status: 201, revision: 1 });
      }),
    );
    const { user } = renderPanel([org]);
    await user.click(
      within(screen.getByRole("region", { name: "Scope tree" })).getAllByRole("button", {
        name: /Add site/,
      })[0]!,
    );
    const dialog = await screen.findByRole("dialog", { name: "Add a site" });
    await user.type(within(dialog).getByLabelText(/Name/), "Cafe");
    await user.click(within(dialog).getByRole("button", { name: "Add" }));
    expect(await screen.findByText(/A site needs a time zone/)).toBeInTheDocument();
    expect(posted).toBe(false);
  });

  it("surfaces the server's own refusal rather than reporting success", async () => {
    server.use(
      http.post(`${TEST_BASE}/scope-nodes`, () =>
        problem(422, "VALIDATION_FAILED", "The body failed validation.", {
          errors: [{ field: "tz", code: "SCOPE_NODE_GEO_REQUIRED", message: "tz must be an IANA name" }],
        }),
      ),
    );
    const { user, onChanged } = renderPanel([org]);
    await user.click(
      within(screen.getByRole("region", { name: "Scope tree" })).getAllByRole("button", {
        name: /Add site/,
      })[0]!,
    );
    const dialog = await screen.findByRole("dialog", { name: "Add a site" });
    await user.type(within(dialog).getByLabelText(/Name/), "Cafe");
    await user.type(within(dialog).getByLabelText(/Time zone/), "Nowhere/Nothing");
    await user.type(within(dialog).getByLabelText(/Latitude/), "1");
    await user.type(within(dialog).getByLabelText(/Longitude/), "2");
    await user.click(within(dialog).getByRole("button", { name: "Add" }));
    expect(await screen.findByText(/tz must be an IANA name/)).toBeInTheDocument();
    expect(onChanged).not.toHaveBeenCalled();
  });
});

describe("ScopeTreePanel — creating a group", () => {
  it("POSTs kind=group under the node whose Add group was pressed, with no geo override", async () => {
    let body: Record<string, unknown> = {};
    server.use(
      http.post(`${TEST_BASE}/scope-nodes`, async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return ok(scopeNode({ kind: "group", name: "East Wing" }), { status: 201, revision: 1 });
      }),
    );
    const { user } = renderPanel([org, site]);
    await user.click(screen.getByRole("button", { name: /Add group/ }));
    const dialog = await screen.findByRole("dialog", { name: "Add a group" });
    expect(within(dialog).getByText("Under The Hanger")).toBeInTheDocument();
    // DAT-032 makes a geo override all-three-or-none, so the group form offers
    // none of the three and the group inherits its site's.
    expect(within(dialog).queryByLabelText(/Time zone/)).toBeNull();
    await user.type(within(dialog).getByLabelText(/Name/), "East Wing");
    await user.click(within(dialog).getByRole("button", { name: "Add" }));
    await waitFor(() => expect(body["name"]).toBe("East Wing"));
    expect(body).toEqual({ kind: "group", name: "East Wing", parent_id: ULID_A });
  });
});

describe("ScopeTreePanel — deleting", () => {
  it("deletes under the node's If-Match after the confirm", async () => {
    let deleted: { path: string; ifMatch: string | null } | null = null;
    server.use(
      http.delete(`${TEST_BASE}/scope-nodes/${ULID_C}`, ({ request }) => {
        deleted = { path: new URL(request.url).pathname, ifMatch: request.headers.get("If-Match") };
        return new HttpResponse(null, { status: 204, headers: { "Trace-Id": TRACE_ID } });
      }),
    );
    const { user, onChanged } = renderPanel();
    await user.click(screen.getByRole("button", { name: "Delete Lobby display" }));
    const confirm = await screen.findByRole("dialog", { name: /Delete Lobby display\?/ });
    await user.click(within(confirm).getByRole("button", { name: "Delete" }));
    await waitFor(() => expect(deleted).not.toBeNull());
    expect(deleted!.path).toBe(`/api/v1/scope-nodes/${ULID_C}`);
    expect(deleted!.ifMatch).toBe('"4"');
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
  });

  it("quotes the server's reason when a node still carries children", async () => {
    server.use(
      http.delete(`${TEST_BASE}/scope-nodes/${ULID_A}`, () =>
        problem(409, "SCOPE_NODE_NOT_EMPTY", "The scope node still has child nodes."),
      ),
    );
    const { user } = renderPanel();
    await user.click(screen.getByRole("button", { name: "Delete The Hanger" }));
    const confirm = await screen.findByRole("dialog", { name: /Delete The Hanger\?/ });
    await user.click(within(confirm).getByRole("button", { name: "Delete" }));
    expect(await screen.findByText(/still has child nodes/)).toBeInTheDocument();
  });
});
