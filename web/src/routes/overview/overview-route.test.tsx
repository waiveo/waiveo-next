import { describe, it, expect, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import OverviewRoute from "./overview-route";
import { TRACE_ID, ULID_A, ULID_B, scopeNode, problem } from "@/api/test-support";

// The Overview reports LIVE counts walked from the api/1 store — screens,
// schedules, automations, and (derived from playlists) the assets in play. The
// four load independently, so a failing family degrades only its own card. These
// tests drive the real route against a mocked feeder (msw) over the same-origin
// relative client the shipped app uses (wildcard patterns match localhost).

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

function page(items: unknown[]) {
  return HttpResponse.json({ items, cursor: null }, { headers: { "Trace-Id": TRACE_ID } });
}

function scheduleRow(id: string) {
  return { id, scope_node: ULID_A, name: `Schedule ${id}`, revision: 1, created_at: 0, updated_at: 0 };
}
function automationRow(id: string) {
  return { id, scope_node: ULID_A, name: `Automation ${id}`, revision: 1, created_at: 0, updated_at: 0 };
}
function playlistRow(id: string, refs: string[]) {
  return {
    id,
    scope_node: ULID_A,
    name: `Playlist ${id}`,
    items: refs.map((r) => ({ source: "asset", asset_ref: r })),
    revision: 1,
    created_at: 0,
    updated_at: 0,
  };
}

/** The stat card whose label matches — used to scope value/hint assertions. */
function card(label: string): HTMLElement {
  const el = screen.getByText(label);
  const c = el.closest('[data-slot="stat-card"]');
  if (!c) throw new Error(`no stat-card for label ${label}`);
  return c as HTMLElement;
}

describe("OverviewRoute — live counts", () => {
  it("renders each figure walked from its resource's cursors", async () => {
    server.use(
      http.get("*/api/v1/scope-nodes", () => page([scopeNode({ id: ULID_A }), scopeNode({ id: ULID_B })])),
      http.get("*/api/v1/schedules", () => page([scheduleRow("s1"), scheduleRow("s2"), scheduleRow("s3")])),
      http.get("*/api/v1/automations", () => page([automationRow("a1")])),
      http.get("*/api/v1/playlists", () =>
        page([playlistRow("p1", ["sha256:aaa", "sha256:bbb"]), playlistRow("p2", ["sha256:aaa"])]),
      ),
    );

    render(<OverviewRoute />);

    await waitFor(() => expect(within(card("Screens")).getByText("2")).toBeInTheDocument());
    expect(within(card("Schedules")).getByText("3")).toBeInTheDocument();
    expect(within(card("Automations")).getByText("1")).toBeInTheDocument();
    // Two DISTINCT asset_refs across the two playlists (sha256:aaa deduped).
    expect(within(card("Assets in play")).getByText("2")).toBeInTheDocument();
  });

  it("shows a graceful zero-state when everything is empty", async () => {
    server.use(
      http.get("*/api/v1/scope-nodes", () => page([])),
      http.get("*/api/v1/schedules", () => page([])),
      http.get("*/api/v1/automations", () => page([])),
      http.get("*/api/v1/playlists", () => page([])),
    );

    render(<OverviewRoute />);

    await waitFor(() => expect(within(card("Screens")).getByText("0")).toBeInTheDocument());
    expect(within(card("Screens")).getByText(/no screens yet/i)).toBeInTheDocument();
    expect(within(card("Assets in play")).getByText("0")).toBeInTheDocument();
    expect(within(card("Assets in play")).getByText(/no assets in play/i)).toBeInTheDocument();
  });

  it("degrades only the failing card, keeping the others live", async () => {
    server.use(
      http.get("*/api/v1/scope-nodes", () => page([scopeNode({ id: ULID_A }), scopeNode({ id: ULID_B })])),
      http.get("*/api/v1/schedules", () =>
        problem(500, "INTERNAL", "The schedule store is unavailable."),
      ),
      http.get("*/api/v1/automations", () => page([automationRow("a1")])),
      http.get("*/api/v1/playlists", () => page([])),
    );

    render(<OverviewRoute />);

    // The healthy families still resolve...
    await waitFor(() => expect(within(card("Screens")).getByText("2")).toBeInTheDocument());
    expect(within(card("Automations")).getByText("1")).toBeInTheDocument();
    // ...and the failing one surfaces its Problem detail, not a crash.
    await waitFor(() =>
      expect(within(card("Schedules")).getByText(/couldn't load/i)).toBeInTheDocument(),
    );
    expect(within(card("Schedules")).getByText(/the schedule store is unavailable/i)).toBeInTheDocument();
  });
});
