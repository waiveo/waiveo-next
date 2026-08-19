import { describe, it, expect, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { MemoryRouter } from "react-router";
import OverviewRoute from "./overview-route";
import { TRACE_ID, ULID_A, pack, problem } from "@/api/test-support";

// The Overview reports LIVE counts walked from the api/1 store — devices found,
// devices adopted, the entities they expose, and installed extensions. The four
// load independently, so a failing family degrades only its own card. These
// tests drive the real route against a mocked feeder (msw) over the same-origin
// relative client the shipped app uses (wildcard patterns match localhost).
//
// The four families changed on 2026-08-19 with the console strip: screens,
// schedules, automations and playlists all left core with their routes, so a
// home page counting them would have been reporting a product this box does not
// run. What is asserted here is the same three properties as before — a live
// walk, an independent degradation, and a first-run panel that only appears on
// an answered-and-empty box — against the families that remain.

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

const RELAY = "relay-0f1e2d3c4b5a69788796a5b4c3d2e1f0";

function page(items: unknown[]) {
  return HttpResponse.json({ items, cursor: null }, { headers: { "Trace-Id": TRACE_ID } });
}

function deviceRow(id: string) {
  return {
    id,
    external_id: null,
    relay_id: RELAY,
    device_class: "media-player",
    name: `Device ${id}`,
    scope_node: ULID_A,
    labels: {},
    adopted: false,
    ignored: false,
  };
}
function adoptedRow(id: string) {
  return {
    id,
    external_id: null,
    name: `Adopted ${id}`,
    scope_node: ULID_A,
    driver: "roku-ecp",
    native_id: id,
    poll_cadence_seconds: null,
    entities: [],
    revision: 1,
    created_at: 0,
    updated_at: 0,
  };
}
function entityRow(id: string) {
  return {
    id,
    external_id: null,
    device_id: "01J8ZDEV1CE00000000000000A",
    relay_id: RELAY,
    device_class: "media-player",
    name: `Entity ${id}`,
    scope_node: ULID_A,
    labels: {},
    state: "idle",
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
      http.get("*/api/v1/devices", () => page([deviceRow("d1"), deviceRow("d2"), deviceRow("d3")])),
      http.get("*/api/v1/adopted-devices", () => page([adoptedRow("a1")])),
      http.get("*/api/v1/entities", () => page([entityRow("e1"), entityRow("e2")])),
      http.get("*/api/v1/extensions", () => page([pack()])),
    );

    render(
      <MemoryRouter>
        <OverviewRoute />
      </MemoryRouter>,
    );

    await waitFor(() => expect(within(card("Devices found")).getByText("3")).toBeInTheDocument());
    // Adoption is its OWN figure, not a fraction of the one above: the two are
    // different facts with different owners, and a device can be adopted while
    // its relay is offline (so it is not in the discovered list at all).
    expect(within(card("Adopted")).getByText("1")).toBeInTheDocument();
    expect(within(card("Entities")).getByText("2")).toBeInTheDocument();
    expect(within(card("Extensions")).getByText("1")).toBeInTheDocument();
  });

  it("shows a graceful zero-state when everything is empty", async () => {
    server.use(
      http.get("*/api/v1/devices", () => page([])),
      http.get("*/api/v1/adopted-devices", () => page([])),
      http.get("*/api/v1/entities", () => page([])),
      http.get("*/api/v1/extensions", () => page([])),
    );

    render(
      <MemoryRouter>
        <OverviewRoute />
      </MemoryRouter>,
    );

    await waitFor(() => expect(within(card("Devices found")).getByText("0")).toBeInTheDocument());
    expect(within(card("Devices found")).getByText(/nothing on the network yet/i)).toBeInTheDocument();
    expect(within(card("Extensions")).getByText("0")).toBeInTheDocument();
    expect(within(card("Extensions")).getByText(/no packs installed/i)).toBeInTheDocument();
  });

  it("degrades only the failing card, keeping the others live", async () => {
    server.use(
      http.get("*/api/v1/devices", () => page([deviceRow("d1"), deviceRow("d2")])),
      http.get("*/api/v1/adopted-devices", () =>
        problem(500, "INTERNAL", "The adoption store is unavailable."),
      ),
      http.get("*/api/v1/entities", () => page([entityRow("e1")])),
      http.get("*/api/v1/extensions", () => page([])),
    );

    render(
      <MemoryRouter>
        <OverviewRoute />
      </MemoryRouter>,
    );

    // The healthy families still resolve...
    await waitFor(() => expect(within(card("Devices found")).getByText("2")).toBeInTheDocument());
    expect(within(card("Entities")).getByText("1")).toBeInTheDocument();
    // ...and the failing one surfaces its Problem detail, not a crash.
    await waitFor(() =>
      expect(within(card("Adopted")).getByText(/couldn't load/i)).toBeInTheDocument(),
    );
    expect(within(card("Adopted")).getByText(/the adoption store is unavailable/i)).toBeInTheDocument();
  });
});

// The first-run panel. A freshly claimed box lands here (setup navigates to
// "/"), and until this existed it showed four zeros and no next step — the
// register's "a freshly claimed box arrives with no way to install anything".
// It matters MORE now that eight of the nine capability areas ship as packs: on
// a stripped console, installing an extension is very nearly the only thing an
// empty box can usefully do.
describe("OverviewRoute — first run", () => {
  function allEmpty() {
    server.use(
      http.get("*/api/v1/devices", () => page([])),
      http.get("*/api/v1/adopted-devices", () => page([])),
      http.get("*/api/v1/entities", () => page([])),
      http.get("*/api/v1/extensions", () => page([])),
    );
  }

  it("points an empty box at the first thing to do", async () => {
    allEmpty();
    render(
      <MemoryRouter>
        <OverviewRoute />
      </MemoryRouter>,
    );

    const panel = await screen.findByRole("region", { name: /start here/i });
    // Extensions leads: a box can do almost nothing until one is installed.
    const first = within(panel).getByRole("link", { name: /install an extension/i });
    expect(first).toHaveAttribute("href", "/extensions");
    // …and every step goes somewhere that still EXISTS. This is the assertion
    // the strip needed: the old panel's three links were /extensions, /screens
    // and /casts, two of which are now the 404.
    const devices = within(panel).getByRole("link", { name: /what is on the network/i });
    expect(devices).toHaveAttribute("href", "/devices");
    expect(within(panel).getByRole("link", { name: /where it is/i })).toHaveAttribute(
      "href",
      "/settings",
    );
  });

  it("does not appear once the box has anything at all", async () => {
    server.use(
      http.get("*/api/v1/devices", () => page([deviceRow("d1")])),
      http.get("*/api/v1/adopted-devices", () => page([])),
      http.get("*/api/v1/entities", () => page([])),
      http.get("*/api/v1/extensions", () => page([])),
    );
    render(
      <MemoryRouter>
        <OverviewRoute />
      </MemoryRouter>,
    );

    await waitFor(() => expect(within(card("Devices found")).getByText("1")).toBeInTheDocument());
    expect(screen.queryByRole("region", { name: /start here/i })).not.toBeInTheDocument();
  });

  // The rule that matters most. A box whose store is UNREACHABLE is not an
  // empty box, and greeting it with "this box is claimed and empty" would state
  // as fact something no answer supports — the same absence-of-evidence mistake
  // the update badge and the catalog each had to avoid.
  it("does not greet a box whose counts could not be read as empty", async () => {
    server.use(
      http.get("*/api/v1/devices", () => problem(500, "INTERNAL", "The store is unavailable.")),
      http.get("*/api/v1/adopted-devices", () => page([])),
      http.get("*/api/v1/entities", () => page([])),
      http.get("*/api/v1/extensions", () => page([])),
    );
    render(
      <MemoryRouter>
        <OverviewRoute />
      </MemoryRouter>,
    );

    await waitFor(() => expect(within(card("Adopted")).getByText("0")).toBeInTheDocument());
    expect(screen.queryByRole("region", { name: /start here/i })).not.toBeInTheDocument();
  });
});
