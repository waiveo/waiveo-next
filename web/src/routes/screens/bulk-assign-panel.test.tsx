import { describe, it, expect, beforeAll, afterAll, afterEach, vi } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { ThemeProvider } from "@/components/theme/theme-provider";
import { Toaster } from "@/components/kit/toaster";
import { BulkAssignPanel, composeSelector } from "./bulk-assign-panel";
import { createApi, type Schedule, type ScopeNode, type Screen } from "@/api";
import { TEST_BASE, TRACE_ID, ULID_A, ULID_B, ULID_ROOT, ok, problem, scopeNode } from "@/api/test-support";

// The daily fleet gesture, clicked through. Two things these tests hold:
//
//   - the MATCH is the server's, not the browser's. The panel must send the
//     api/1 selector (including the `scope_node subtree` term) so the preview
//     and the writes agree; a panel that fetched every screen and filtered
//     locally would preview a set the server would never have chosen.
//   - the fan-out is CONDITIONAL and PARTIAL-honest. Every write carries an
//     If-Match, a 412 is reported as a conflict rather than retried, and the
//     screens that did move are still reported as moved.

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => {
  server.resetHandlers();
  window.localStorage.clear();
});
afterAll(() => server.close());

const SITE = ULID_A;
const GROUP = ULID_B;
const SCREEN_1 = "01J8ZSCREEN000000000000001";
const SCREEN_2 = "01J8ZSCREEN000000000000002";
const SCHEDULE_ID = "01J8ZSCHEDULE00000000000A1";

const site = scopeNode({ id: SITE, kind: "site", name: "The Hanger", parent_id: ULID_ROOT }) as ScopeNode;
const group = scopeNode({ id: GROUP, kind: "group", name: "West Wing", parent_id: SITE }) as ScopeNode;

function schedule(over: Partial<Schedule> = {}): Schedule {
  return {
    id: SCHEDULE_ID,
    scope_node: GROUP,
    name: "Lunch loop",
    labels: {},
    revision: 1,
    created_at: 1752537000000,
    updated_at: 1752537000000,
    ...over,
  };
}

function screenRow(over: Partial<Screen> = {}): Screen {
  return {
    id: SCREEN_1,
    external_id: null,
    name: "Lobby TV",
    scope_node: SITE,
    device_id: null,
    labels: {},
    revision: 3,
    created_at: 1752537000000,
    updated_at: 1752537000000,
    ...over,
  };
}

function page(items: unknown[]) {
  return HttpResponse.json({ items, cursor: null }, { headers: { "Trace-Id": TRACE_ID } });
}

function renderPanel(matches: Screen[] = [screenRow()]) {
  const selectors: (string | null)[] = [];
  server.use(
    http.get(`${TEST_BASE}/schedules`, () => page([schedule()])),
    http.get(`${TEST_BASE}/screens`, ({ request }) => {
      selectors.push(new URL(request.url).searchParams.get("selector"));
      return page(matches);
    }),
  );
  const api = createApi({ baseUrl: TEST_BASE });
  const onChanged = vi.fn(async () => {});
  render(
    <ThemeProvider>
      <BulkAssignPanel api={api} nodes={[site, group]} onChanged={onChanged} />
      <Toaster />
    </ThemeProvider>,
  );
  return { user: userEvent.setup(), onChanged, selectors };
}

describe("composeSelector", () => {
  it("ANDs a subtree term with the operator's label terms", () => {
    expect(composeSelector(GROUP, "zone=lobby")).toBe(`scope_node subtree ${GROUP},zone=lobby`);
  });

  it("is empty when neither control is set — which means every screen", () => {
    expect(composeSelector("", "  ")).toBe("");
  });

  it("carries either half on its own", () => {
    expect(composeSelector(GROUP, "")).toBe(`scope_node subtree ${GROUP}`);
    expect(composeSelector("", "tier in (a,b)")).toBe("tier in (a,b)");
  });
});

describe("BulkAssignPanel — matching the target set", () => {
  it("sends the composed selector to the server and reports the match", async () => {
    const { user, selectors } = renderPanel([screenRow(), screenRow({ id: SCREEN_2, name: "Cafe board" })]);
    await user.selectOptions(await screen.findByLabelText("To screens in"), GROUP);
    await user.type(screen.getByLabelText(/matching labels/), "zone=lobby");
    expect(screen.getByTestId("bulk-selector")).toHaveTextContent(
      `scope_node subtree ${GROUP},zone=lobby`,
    );

    await user.click(screen.getByRole("button", { name: "Preview matches" }));
    await waitFor(() => expect(selectors).toContain(`scope_node subtree ${GROUP},zone=lobby`));
    expect(await screen.findByText("2 screens matched.")).toBeInTheDocument();
    const table = screen.getByRole("table", { name: "Matched screens" });
    expect(within(table).getByText("Cafe board")).toBeInTheDocument();
  });

  it("surfaces a selector the grammar refuses instead of silently matching nothing", async () => {
    server.use(
      http.get(`${TEST_BASE}/schedules`, () => page([schedule()])),
      http.get(`${TEST_BASE}/screens`, () =>
        problem(400, "SELECTOR_INVALID", "selector: unexpected token at ' in in '"),
      ),
    );
    const api = createApi({ baseUrl: TEST_BASE });
    render(
      <ThemeProvider>
        <BulkAssignPanel api={api} nodes={[site, group]} onChanged={vi.fn(async () => {})} />
        <Toaster />
      </ThemeProvider>,
    );
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Preview matches" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(/unexpected token/);
  });

  it("will not assign before a schedule is chosen and a match previewed", async () => {
    const { user } = renderPanel();
    const assign = await screen.findByRole("button", { name: "Assign to matched screens" });
    expect(assign).toBeDisabled();
    await user.click(screen.getByRole("button", { name: "Preview matches" }));
    await screen.findByText("1 screen matched.");
    // Matched, but no destination yet — there is nowhere to move them to.
    expect(assign).toBeDisabled();
    await user.selectOptions(screen.getByLabelText("Assign this schedule"), SCHEDULE_ID);
    await waitFor(() => expect(assign).toBeEnabled());
  });
});

describe("BulkAssignPanel — the fan-out", () => {
  it("places every matched screen under the schedule's node, one conditional write each", async () => {
    const writes: { id: string; body: Record<string, unknown>; ifMatch: string | null }[] = [];
    const { user, onChanged } = renderPanel([
      screenRow({ revision: 3 }),
      screenRow({ id: SCREEN_2, name: "Cafe board", revision: 9 }),
    ]);
    server.use(
      http.patch(`${TEST_BASE}/screens/:id`, async ({ request, params }) => {
        writes.push({
          id: String(params["id"]),
          body: (await request.json()) as Record<string, unknown>,
          ifMatch: request.headers.get("If-Match"),
        });
        return ok(screenRow({ scope_node: GROUP, revision: 4 }), { revision: 4 });
      }),
    );

    await user.selectOptions(await screen.findByLabelText("Assign this schedule"), SCHEDULE_ID);
    await user.click(screen.getByRole("button", { name: "Preview matches" }));
    await screen.findByText("2 screens matched.");
    await user.click(screen.getByRole("button", { name: "Assign to matched screens" }));

    await waitFor(() => expect(writes).toHaveLength(2));
    // Placement IS the assignment: the write moves the screen under the node the
    // schedule is authored at, and nothing else.
    expect(writes[0]).toMatchObject({ id: SCREEN_1, body: { scope_node: GROUP }, ifMatch: '"3"' });
    expect(writes[1]).toMatchObject({ id: SCREEN_2, body: { scope_node: GROUP }, ifMatch: '"9"' });
    const results = await screen.findByRole("table", { name: "Bulk assign results" });
    expect(within(results).getAllByText("moved")).toHaveLength(2);
    await waitFor(() => expect(onChanged).toHaveBeenCalled());
  });

  it("skips a screen already under the destination rather than churning its revision", async () => {
    const writes: string[] = [];
    const { user } = renderPanel([
      screenRow({ scope_node: GROUP }),
      screenRow({ id: SCREEN_2, name: "Cafe board", scope_node: SITE }),
    ]);
    server.use(
      http.patch(`${TEST_BASE}/screens/:id`, ({ params }) => {
        writes.push(String(params["id"]));
        return ok(screenRow({ scope_node: GROUP, revision: 4 }), { revision: 4 });
      }),
    );
    await user.selectOptions(await screen.findByLabelText("Assign this schedule"), SCHEDULE_ID);
    await user.click(screen.getByRole("button", { name: "Preview matches" }));
    await screen.findByText("2 screens matched.");
    await user.click(screen.getByRole("button", { name: "Assign to matched screens" }));

    const results = await screen.findByRole("table", { name: "Bulk assign results" });
    await waitFor(() => expect(within(results).getByText("already there")).toBeInTheDocument());
    expect(writes).toEqual([SCREEN_2]);
  });

  it("reports a 412 as a conflict and still reports the screens that DID move", async () => {
    const { user } = renderPanel([
      screenRow({ revision: 3 }),
      screenRow({ id: SCREEN_2, name: "Cafe board", revision: 9 }),
    ]);
    server.use(
      http.patch(`${TEST_BASE}/screens/${SCREEN_1}`, () =>
        problem(412, "REVISION_CONFLICT", "The resource was modified concurrently.", {
          current_revision: 4,
        }),
      ),
      http.patch(`${TEST_BASE}/screens/${SCREEN_2}`, () =>
        ok(screenRow({ id: SCREEN_2, scope_node: GROUP, revision: 10 }), { revision: 10 }),
      ),
    );
    await user.selectOptions(await screen.findByLabelText("Assign this schedule"), SCHEDULE_ID);
    await user.click(screen.getByRole("button", { name: "Preview matches" }));
    await screen.findByText("2 screens matched.");
    await user.click(screen.getByRole("button", { name: "Assign to matched screens" }));

    const results = await screen.findByRole("table", { name: "Bulk assign results" });
    await waitFor(() => expect(within(results).getByText("conflict")).toBeInTheDocument());
    // The partial result is stated, not hidden behind a single "done".
    expect(within(results).getByText("moved")).toBeInTheDocument();
    expect(within(results).getByText(/changed elsewhere while this ran/)).toBeInTheDocument();
    expect(await screen.findByText(/1 moved, 1 did not/)).toBeInTheDocument();
  });
});
