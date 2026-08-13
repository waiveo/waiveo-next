import { describe, it, expect, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { MemoryRouter } from "react-router";
import { ThemeProvider } from "@/components/theme/theme-provider";
import DevicesRoute from "./devices-route";
import { createApi, type Device, type Entity, type SystemHealth } from "@/api";
import { TEST_BASE, TRACE_ID, ULID_A, ok } from "@/api/test-support";

// The Devices route, clicked through. What is worth testing here is not that a
// table renders — it is the judgements the page makes that a human would
// otherwise have to make from raw JSON:
//
//   1. Adopted vs discovered is `Device.adopted`, the flag the row itself
//      carries. The page must not offer "Adopt" on a device that is already
//      adopted, and must not report an adopted fleet as un-adopted.
//   2. Adopting is ONE call to POST /devices/{id}/adopt, with no body. The
//      record's identity tuple is not on the discovered row at all, so a page
//      that composed a create body would be composing it out of nothing — see
//      the route's own header.

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => {
  server.resetHandlers();
  window.localStorage.clear();
});
afterAll(() => server.close());

const RELAY = "relay-0f1e2d3c4b5a69788796a5b4c3d2e1f0";
const DEVICE_ID = "01J8ZDEV1CE00000000000000A";
const OTHER_DEVICE_ID = "01J8ZDEV1CE00000000000000B";
const ENTITY_ID = "01J8ZENT1TY00000000000000A";
const OTHER_ENTITY_ID = "01J8ZENT1TY00000000000000B";

function device(over: Partial<Device> = {}): Device {
  return {
    id: DEVICE_ID,
    external_id: null,
    relay_id: RELAY,
    device_class: "media-player",
    name: "Hanger TV",
    scope_node: ULID_A,
    labels: {},
    // The widened schema's own members. `labels` stays empty because that is
    // what the server actually mints a discovered device with — labels are
    // api/1 AUTHORED data (internal/app/devices/intake.go) and discovery never
    // writes into them, so a fixture that stuffed the facts in there would be
    // testing the fallback path against a report no deployment produces.
    address: "192.0.2.40",
    model: "Roku Ultra",
    adopted: false,
    ...over,
  };
}

function entity(over: Partial<Entity> = {}): Entity {
  return {
    id: ENTITY_ID,
    external_id: null,
    device_id: DEVICE_ID,
    relay_id: RELAY,
    device_class: "media-player",
    name: "Hanger TV main",
    scope_node: ULID_A,
    labels: {},
    state: "idle",
    ...over,
  };
}

function page(items: unknown[]) {
  return HttpResponse.json({ items, cursor: null }, { headers: { "Trace-Id": TRACE_ID } });
}

/** A `/system-health` body carrying one connected relay. Only `relays` is read
 * by this route, but the whole required shape is served because the client
 * types it from the generated schema and a partial body would let a test pass
 * against a response the server has never produced. */
function health(over: Partial<SystemHealth> = {}): SystemHealth {
  return {
    status: "ok",
    checked_at_ms: 1_753_142_400_000,
    started_at_ms: 1_753_142_400_000 - 3_600_000,
    uptime_ms: 3_600_000,
    version: "test",
    services: [],
    storage: { path: "/", status: "ok", detail: "plenty of room" },
    relays: [{ relay_id: RELAY, address: "192.0.2.9:7421", screen_count: 1 }],
    screens: {
      total: 0,
      live: 0,
      fetching: 0,
      rejected: 0,
      stale: 0,
      never_seen: 0,
      paired: 0,
      overridden: 0,
      live_window_ms: 30_000,
      content_transfer_window_ms: 120_000,
      fetching_max_unacked_pulls: 3,
    },
    ...over,
  };
}

/** The FOUR reads the route makes on mount. Any of them can be overridden by a
 * later `server.use` in a test.
 *
 * Four, not five: the page does not read `/scope-nodes` (the server places an
 * adopted device itself). It DOES read `/system-health`, because relay
 * connectivity is the only way to tell "discovery is running and found nothing"
 * from "nothing is discovering" — see ./discovery. And it reads
 * `/adopted-devices` for the policy panel, which lists the RECORDS rather than
 * joining them onto discovered rows (the two share no member).
 *
 * `onUnhandledRequest: "error"` was described here as what keeps this honest —
 * "a route that gained or lost a fetch fails every test here loudly". It did NOT
 * when the adopted panel was added, because the panel catches its own read
 * failure and renders an error state, so every test kept passing against a panel
 * that was permanently broken. The stub below is the fix; the caveat is recorded
 * so the next fetch-gaining change is not trusted to fail loudly by itself. */
function seed({
  devices = [device()],
  entities = [entity()],
  systemHealth = health(),
  adopted = [],
}: {
  devices?: Device[];
  entities?: Entity[];
  systemHealth?: SystemHealth;
  adopted?: unknown[];
} = {}) {
  server.use(
    http.get(`${TEST_BASE}/devices`, () => page(devices)),
    http.get(`${TEST_BASE}/entities`, () => page(entities)),
    http.get(`${TEST_BASE}/system-health`, () => ok(systemHealth)),
    http.get(`${TEST_BASE}/adopted-devices`, () => page(adopted as never[])),
  );
}

/** Rendered inside a router: the page links to the Roku console, and a `<Link>`
 * with no router context throws rather than degrading. */
function renderRoute() {
  const api = createApi({ baseUrl: TEST_BASE });
  render(
    <ThemeProvider>
      <MemoryRouter>
        <DevicesRoute api={api} />
      </MemoryRouter>
    </ThemeProvider>,
  );
  return userEvent.setup();
}

describe("Devices — the discovered fleet", () => {
  it("shows a discovered device with the address and model its relay reported", async () => {
    seed();
    renderRoute();
    const table = await screen.findByRole("table", { name: "Discovered devices" });
    expect(within(table).getByText("Hanger TV")).toBeInTheDocument();
    expect(within(table).getByText("192.0.2.40")).toBeInTheDocument();
    expect(within(table).getByText("Roku Ultra")).toBeInTheDocument();
    expect(within(table).getByText("Discovered")).toBeInTheDocument();
  });

  it("counts what discovery found, and says the console cannot start a sweep", async () => {
    seed({ devices: [device(), device({ id: OTHER_DEVICE_ID, name: "Cafe TV" })] });
    renderRoute();
    const status = await screen.findByRole("region", { name: "Discovery status" });
    await waitFor(() => expect(within(status).getByText("Discovered")).toBeInTheDocument());
    expect(within(status).getByText("Relays reporting")).toBeInTheDocument();
    expect(within(status).getByText(/there is no scan to start from here/)).toBeInTheDocument();
  });

  it("names the relay that reported the devices, with the address it dials on", async () => {
    // "Which relay found this" is the first question after "why is this
    // missing", and the page had no answer at all: it counted distinct relay
    // ids and showed nothing about any of them.
    seed({ devices: [device(), device({ id: OTHER_DEVICE_ID, name: "Cafe TV" })] });
    renderRoute();
    const status = await screen.findByRole("region", { name: "Discovery status" });
    const relay = await within(status).findByText(RELAY);
    const row = relay.closest("[data-slot='discovery-relay']")!;
    expect(within(row as HTMLElement).getByText("192.0.2.9:7421")).toBeInTheDocument();
    expect(within(row as HTMLElement).getByText("2 devices")).toBeInTheDocument();
  });

  it("says outright that no sweep timestamp is published, rather than omitting the field", async () => {
    // "When did it last sweep" is the obvious next question. The API cannot
    // answer it, and an omitted field reads as an oversight — so the page says
    // what its silence means and what is load-bearing instead.
    seed();
    renderRoute();
    const status = await screen.findByRole("region", { name: "Discovery status" });
    await waitFor(() =>
      expect(within(status).getByText(/No sweep timestamp is published/)).toBeInTheDocument(),
    );
    expect(
      within(status).getByText(/which relays are connected now, not when each last swept/),
    ).toBeInTheDocument();
  });

  it("explains why an expected device might not be listed, on demand", async () => {
    seed();
    const user = renderRoute();
    const status = await screen.findByRole("region", { name: "Discovery status" });
    // Collapsed by default — available without shouting.
    expect(within(status).queryByText(/neither protocol crosses subnets/)).toBeNull();
    await user.click(
      within(status).getByRole("button", { name: /Why is a device I expected not listed/ }),
    );
    expect(within(status).getByText(/neither protocol crosses subnets/)).toBeInTheDocument();
    expect(within(status).getByText(/one malformed candidate refuses the entire report/)).toBeInTheDocument();
  });

  it("reads adopted straight off the row, and does not offer to adopt it twice", async () => {
    seed({ devices: [device({ adopted: true })] });
    renderRoute();
    const table = await screen.findByRole("table", { name: "Discovered devices" });
    await waitFor(() => expect(within(table).getByText("Adopted")).toBeInTheDocument());
    // A second adoption record for one physical TV is exactly what REL-153
    // exists to prevent, so an adopted row offers no Adopt at all.
    expect(within(table).queryByRole("button", { name: "Adopt" })).toBeNull();
  });

  it("still surfaces an address the deployment reported as a label rather than a member", async () => {
    // The label fallback in deviceFacts, exercised against a device whose
    // top-level members are absent. This is the pre-widening representation and
    // an unwidened deployment can still be on it.
    // Built by OMISSION rather than by passing `address: undefined`: under
    // exactOptionalPropertyTypes an explicit undefined is not the same thing as
    // an absent member, and absent is what an unwidened server actually sends.
    const unwidened = device({ labels: { address: "192.0.2.41" } });
    delete unwidened.address;
    delete unwidened.model;
    seed({ devices: [unwidened] });
    renderRoute();
    const table = await screen.findByRole("table", { name: "Discovered devices" });
    await waitFor(() => expect(within(table).getByText("192.0.2.41")).toBeInTheDocument());
    // Nothing gates on the identity tuple any more: adopting needs only the id.
    expect(within(table).getByRole("button", { name: "Adopt" })).toBeEnabled();
  });
});

// The defect this whole block exists for: an empty devices table used to read
// identically for four completely different situations, and an operator cannot
// act on "no devices reported". Each test below asserts that ONE of those
// situations says something the others do not — and, crucially, that the wrong
// sentence is absent, since a page that said all four things at once would pass
// a positive-only assertion while being just as useless.
describe("Devices — the four ways a device list can be empty", () => {
  async function stateRegion() {
    const status = await screen.findByRole("region", { name: "Discovery status" });
    return within(status).getByRole("status");
  }

  it("says discovery is NOT RUNNING when no relay is connected", async () => {
    seed({ devices: [], entities: [], systemHealth: health({ relays: [] }) });
    renderRoute();
    const state = await stateRegion();
    await waitFor(() => expect(state).toHaveAttribute("data-kind", "no-relay"));
    expect(state).toHaveTextContent(/Discovery is not running — no relay is connected/);
    // The distinguishing claim: an empty list here says nothing about the LAN.
    expect(state).toHaveTextContent(/says nothing about what is on the network/);
  });

  it("says discovery IS running and found nothing when a relay is connected", async () => {
    seed({ devices: [], entities: [] });
    renderRoute();
    const state = await stateRegion();
    await waitFor(() => expect(state).toHaveAttribute("data-kind", "searching"));
    expect(state).toHaveTextContent(/Discovery is running on 1 relay — nothing found yet/);
    // …and points at the actual next thing to check, which is the network, not
    // the relay.
    expect(state).toHaveTextContent(/a relay only ever sees its own LAN/);
    expect(state).not.toHaveTextContent(/no relay is connected/);
  });

  it("says everything found is ALREADY ADOPTED rather than showing a bare list", async () => {
    seed({ devices: [device({ adopted: true })] });
    renderRoute();
    const state = await stateRegion();
    await waitFor(() => expect(state).toHaveAttribute("data-kind", "all-adopted"));
    expect(state).toHaveTextContent(/Everything found is adopted/);
    expect(state).toHaveTextContent(/This is the steady state, not an empty result/);
  });

  it("says it does not KNOW when relay health is refused, and does not claim a sweep ran", async () => {
    // /system-health is owner-only. A site admin gets 403 — and the page must
    // not quietly render that as "a relay swept and found nothing", which is a
    // claim it has no basis for.
    seed({ devices: [], entities: [] });
    server.use(
      http.get(`${TEST_BASE}/system-health`, () =>
        HttpResponse.json(
          {
            type: "about:blank",
            title: "Forbidden",
            status: 403,
            code: "FORBIDDEN",
            detail: "Only the workspace owner may read this.",
            trace_id: TRACE_ID,
          },
          { status: 403, headers: { "Content-Type": "application/problem+json", "Trace-Id": TRACE_ID } },
        ),
      ),
    );
    renderRoute();
    const state = await stateRegion();
    await waitFor(() => expect(state).toHaveAttribute("data-kind", "blind"));
    expect(state).toHaveTextContent(/it is not known whether anything is looking/);
    expect(state).toHaveTextContent(/Only the workspace owner can read relay health/);
    // The two claims it must NOT make.
    expect(state).not.toHaveTextContent(/Discovery is running/);
    expect(state).not.toHaveTextContent(/Discovery is not running/);
    // And the relay list is omitted entirely rather than rendered empty: an
    // empty list would read as "no relays", which is the claim just refused.
    expect(screen.queryByText("Relays reporting")).not.toBeNull(); // the stat card label
    expect(document.querySelector("[data-slot='discovery-relay']")).toBeNull();
  });

  it("keeps the fleet readable when only health is refused", async () => {
    // The device plane and health are separate reads. A 403 on health must not
    // blank a fleet the caller is perfectly entitled to see.
    seed();
    server.use(
      http.get(`${TEST_BASE}/system-health`, () =>
        HttpResponse.json(
          {
            type: "about:blank",
            title: "Forbidden",
            status: 403,
            code: "FORBIDDEN",
            detail: "Only the workspace owner may read this.",
            trace_id: TRACE_ID,
          },
          { status: 403, headers: { "Content-Type": "application/problem+json", "Trace-Id": TRACE_ID } },
        ),
      ),
    );
    renderRoute();
    const table = await screen.findByRole("table", { name: "Discovered devices" });
    expect(within(table).getByText("Hanger TV")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).toBeNull();
    // The caveat is stated rather than the uncertainty being hidden.
    const state = await stateRegion();
    await waitFor(() =>
      expect(state).toHaveTextContent(/whether it is running NOW is not known here/),
    );
  });
});

describe("Devices — adopting, clicked through", () => {
  it("adopts by device id with no body at all, and flips the row from the answer", async () => {
    let adoptedRows = [device()];
    let seen: { path: string; body: string; idempotencyKey: string | null } | null = null;
    seed();
    server.use(
      http.get(`${TEST_BASE}/devices`, () => page(adoptedRows)),
      http.get(`${TEST_BASE}/entities`, () => page([entity()])),
      http.post(`${TEST_BASE}/devices/${DEVICE_ID}/adopt`, async ({ request }) => {
        seen = {
          path: new URL(request.url).pathname,
          body: await request.text(),
          idempotencyKey: request.headers.get("Idempotency-Key"),
        };
        adoptedRows = [device({ adopted: true })];
        return ok(device({ adopted: true }));
      }),
    );

    const user = renderRoute();
    await user.click(await screen.findByRole("button", { name: "Adopt" }));
    const dialog = await screen.findByRole("dialog", { name: /Adopt Hanger TV/ });
    await user.click(within(dialog).getByRole("button", { name: "Adopt" }));

    await waitFor(() => expect(seen).not.toBeNull());
    expect(seen!.path).toBe(`/api/v1/devices/${DEVICE_ID}/adopt`);
    // The operation declares no request body, and the console sends none —
    // there is nothing about the adoption it is in a position to state.
    expect(seen!.body).toBe("");
    // A double-click on Adopt must not adopt twice at the far end (API-050/052).
    expect(seen!.idempotencyKey).toBeTruthy();

    const table = await screen.findByRole("table", { name: "Discovered devices" });
    await waitFor(() => expect(within(table).getByText("Adopted")).toBeInTheDocument());
    expect(within(table).queryByRole("button", { name: "Adopt" })).toBeNull();
  });

  it("keeps the row correct from the adopt response even when the reload fails", async () => {
    // The response IS the adopted device, so the status column is right without
    // a second round trip. This matters because the reload after it exists to
    // pick up the entities the adoption enabled — and a failure there must not
    // leave the operator looking at a device they just adopted still labelled
    // Discovered, which reads as "the adopt did not work".
    let adoptCalls = 0;
    seed();
    server.use(
      http.get(`${TEST_BASE}/devices`, () => {
        if (adoptCalls === 0) return page([device()]);
        return HttpResponse.json(
          {
            type: "about:blank",
            title: "Service Unavailable",
            status: 503,
            code: "UNAVAILABLE",
            detail: "No relay is connected.",
            trace_id: TRACE_ID,
          },
          { status: 503, headers: { "Content-Type": "application/problem+json", "Trace-Id": TRACE_ID } },
        );
      }),
      http.get(`${TEST_BASE}/entities`, () => page([entity()])),
      http.post(`${TEST_BASE}/devices/${DEVICE_ID}/adopt`, () => {
        adoptCalls += 1;
        return ok(device({ adopted: true }));
      }),
    );

    const user = renderRoute();
    await user.click(await screen.findByRole("button", { name: "Adopt" }));
    const dialog = await screen.findByRole("dialog", { name: /Adopt Hanger TV/ });
    await user.click(within(dialog).getByRole("button", { name: "Adopt" }));

    await waitFor(() => expect(adoptCalls).toBe(1));
    expect(await screen.findByRole("alert")).toHaveTextContent(/No relay is connected/);
  });

  it("reports the Problem and leaves the device un-adopted when the adopt is refused", async () => {
    seed();
    server.use(
      http.post(`${TEST_BASE}/devices/${DEVICE_ID}/adopt`, () =>
        HttpResponse.json(
          {
            type: "about:blank",
            title: "Service Unavailable",
            status: 503,
            code: "UNAVAILABLE",
            detail: "This device has been reported but not yet recorded; retry shortly.",
            trace_id: TRACE_ID,
          },
          { status: 503, headers: { "Content-Type": "application/problem+json", "Trace-Id": TRACE_ID } },
        ),
      ),
    );
    const user = renderRoute();
    await user.click(await screen.findByRole("button", { name: "Adopt" }));
    const dialog = await screen.findByRole("dialog", { name: /Adopt Hanger TV/ });
    await user.click(within(dialog).getByRole("button", { name: "Adopt" }));

    // The server's own detail, not a generic failure: "retry shortly" is
    // actionable and "couldn't adopt the device" is not.
    expect(await screen.findByText(/retry shortly/)).toBeInTheDocument();
    // The dialog stays OPEN on a refusal — this one is explicitly retryable,
    // and closing it would make the operator find the device again to try the
    // thing the server just told them to try.
    const stillOpen = await screen.findByRole("dialog", { name: /Adopt Hanger TV/ });
    await user.click(within(stillOpen).getByRole("button", { name: "Cancel" }));

    const table = await screen.findByRole("table", { name: "Discovered devices" });
    expect(within(table).getByText("Discovered")).toBeInTheDocument();
    expect(within(table).getByRole("button", { name: "Adopt" })).toBeEnabled();
  });

  // Pressing a DEVICE ROW narrows the entities table; pressing the Adopt button
  // INSIDE that row must not do both. The button sits inside the row's own
  // click target, so without a guard the operator gets an entities filter they
  // never asked for — and cancelling the modal leaves it applied with nothing
  // on screen to explain it.
  it("does not narrow the entities table when Adopt is pressed inside a row", async () => {
    seed({
      devices: [device(), device({ id: OTHER_DEVICE_ID, name: "Cafe TV" })],
      entities: [
        entity(),
        entity({ id: OTHER_ENTITY_ID, device_id: OTHER_DEVICE_ID, name: "Cafe TV main" }),
      ],
    });
    const user = renderRoute();
    const devices = await screen.findByRole("table", { name: "Discovered devices" });
    const hangerRow = within(devices).getByText("Hanger TV").closest("tr")!;
    await user.click(within(hangerRow).getByRole("button", { name: "Adopt" }));

    // The dialog opened — the button did its own job…
    const dialog = await screen.findByRole("dialog", { name: /Adopt Hanger TV/ });
    await user.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());

    // …and nothing else: no selection was made, so both devices' entities are
    // still listed and there is no filter to release.
    expect(screen.queryByRole("button", { name: "Show all entities" })).toBeNull();
    const entitiesTable = screen.getByRole("table", { name: "Entities" });
    expect(within(entitiesTable).getByText("Cafe TV main")).toBeInTheDocument();
    expect(within(entitiesTable).getByText("Hanger TV main")).toBeInTheDocument();
  });
});

describe("Devices — entities and the remote", () => {
  it("narrows the entities table to the device whose row was pressed", async () => {
    seed({
      devices: [device(), device({ id: OTHER_DEVICE_ID, name: "Cafe TV", labels: {} })],
      entities: [
        entity(),
        entity({ id: OTHER_ENTITY_ID, device_id: OTHER_DEVICE_ID, name: "Cafe TV main" }),
      ],
    });
    const user = renderRoute();
    const entities = await screen.findByRole("table", { name: "Entities" });
    await waitFor(() => expect(within(entities).getByText("Cafe TV main")).toBeInTheDocument());

    const devices = screen.getByRole("table", { name: "Discovered devices" });
    await user.click(within(devices).getByRole("button", { name: /Hanger TV/ }));

    await waitFor(() =>
      expect(within(screen.getByRole("table", { name: "Entities" })).queryByText("Cafe TV main")).toBeNull(),
    );
    expect(within(screen.getByRole("table", { name: "Entities" })).getByText("Hanger TV main")).toBeInTheDocument();

    // …and the filter is releasable.
    await user.click(screen.getByRole("button", { name: "Show all entities" }));
    await waitFor(() =>
      expect(within(screen.getByRole("table", { name: "Entities" })).getByText("Cafe TV main")).toBeInTheDocument(),
    );
  });

  it("offers the remote only for an entity whose class the remote can actually drive", async () => {
    seed({
      entities: [
        entity(),
        entity({ id: OTHER_ENTITY_ID, name: "Hanger thermostat", device_class: "thermostat" }),
      ],
    });
    renderRoute();
    const entities = await screen.findByRole("table", { name: "Entities" });
    await waitFor(() => expect(within(entities).getByText("Hanger thermostat")).toBeInTheDocument());
    // One Remote button, for the one media-player entity — a remote built from
    // media-player's vocabulary would only ever earn COMMAND_UNRESOLVED against
    // the thermostat.
    expect(within(entities).getAllByRole("button", { name: "Remote" })).toHaveLength(1);
  });

  it("links to the Roku console when a media player has been discovered", async () => {
    seed();
    renderRoute();
    const link = await screen.findByRole("link", { name: "Roku console" });
    expect(link).toHaveAttribute("href", "/roku");
  });

  it("offers no Roku console link when nothing on the fleet is a media player", async () => {
    // The link is an offer to go and drive something. A deployment of
    // thermostats has nothing to drive there, and a dead-end link is the same
    // "button that does nothing" defect in a different shape.
    seed({
      devices: [device({ device_class: "thermostat" })],
      entities: [entity({ device_class: "thermostat" })],
    });
    renderRoute();
    await screen.findByRole("table", { name: "Discovered devices" });
    expect(screen.queryByRole("link", { name: "Roku console" })).toBeNull();
  });

  it("opens the remote and dispatches a real command at the chosen entity", async () => {
    let sent: Record<string, unknown> | null = null;
    seed();
    server.use(
      http.post(`${TEST_BASE}/entities/${ENTITY_ID}/commands`, async ({ request }) => {
        sent = (await request.json()) as Record<string, unknown>;
        return ok({ ok: true });
      }),
    );
    const user = renderRoute();
    await user.click(await screen.findByRole("button", { name: "Remote" }));
    const dialog = await screen.findByRole("dialog", { name: /Remote — Hanger TV main/ });
    await user.click(within(dialog).getByRole("button", { name: "Home" }));
    await waitFor(() => expect(sent).not.toBeNull());
    expect(sent).toEqual({ command: "home" });
  });
});

describe("Devices — finding one device in a fleet", () => {
  /** Enough devices that the fleet is not readable at a glance, spread across
   * two classes and two adoption states so the filters have something to
   * separate. */
  function fleet(): Device[] {
    return [
      device({ id: DEVICE_ID, name: "Hanger TV", address: "192.0.2.40" }),
      device({ id: OTHER_DEVICE_ID, name: "Cafe TV", address: "192.0.2.41", adopted: true }),
      device({
        id: "01J8ZDEV1CE00000000000000C",
        name: "Stock room light",
        address: "192.0.2.42",
        device_class: "light",
        model: "Hue A19",
      }),
    ];
  }

  it("narrows the table to what was TYPED in the search box", async () => {
    seed({ devices: fleet(), entities: [] });
    const user = renderRoute();
    const table = await screen.findByRole("table", { name: "Discovered devices" });
    expect(within(table).getByText("Cafe TV")).toBeInTheDocument();

    const box = screen.getByLabelText("Search devices");
    await user.type(box, "Stock");
    await waitFor(() => expect(screen.queryByText("Cafe TV")).not.toBeInTheDocument());
    expect(screen.getByText("Stock room light")).toBeInTheDocument();

    // Matching on ADDRESS too — the column an operator has in front of them on a
    // router page, and one of the seven fields legacy's free-text search covered.
    await user.clear(box);
    await user.type(box, "192.0.2.41");
    await waitFor(() => expect(screen.getByText("Cafe TV")).toBeInTheDocument());
    expect(screen.queryByText("Stock room light")).not.toBeInTheDocument();
  });

  it("filters by a device class the ROWS declare, not by a hardcoded list", async () => {
    seed({ devices: fleet(), entities: [] });
    const user = renderRoute();
    await screen.findByRole("table", { name: "Discovered devices" });

    const classFilter = screen.getByLabelText("Device class");
    // "light" is only ever offered because a row has it — nothing in the console
    // enumerates device classes, and the registry is server-authoritative.
    expect(within(classFilter).getByRole("option", { name: "light (1)" })).toBeInTheDocument();
    expect(within(classFilter).getByRole("option", { name: "media-player (2)" })).toBeInTheDocument();

    await user.selectOptions(classFilter, "light");
    await waitFor(() => expect(screen.queryByText("Hanger TV")).not.toBeInTheDocument());
    expect(screen.getByText("Stock room light")).toBeInTheDocument();
  });

  it("filters by adoption state, which is a BADGE cell and still filterable", async () => {
    seed({ devices: fleet(), entities: [] });
    const user = renderRoute();
    await screen.findByRole("table", { name: "Discovered devices" });

    await user.selectOptions(screen.getByLabelText("Adoption"), "Adopted");
    await waitFor(() => expect(screen.queryByText("Hanger TV")).not.toBeInTheDocument());
    expect(screen.getByText("Cafe TV")).toBeInTheDocument();
  });
});

describe("Devices — when the device plane cannot be read", () => {
  it("surfaces the Problem rather than an empty fleet with no explanation", async () => {
    seed();
    server.use(
      http.get(`${TEST_BASE}/devices`, () =>
        HttpResponse.json(
          {
            type: "about:blank",
            title: "Service Unavailable",
            status: 503,
            code: "UNAVAILABLE",
            detail: "No relay is connected.",
            trace_id: TRACE_ID,
          },
          { status: 503, headers: { "Content-Type": "application/problem+json", "Trace-Id": TRACE_ID } },
        ),
      ),
      http.get(`${TEST_BASE}/entities`, () => page([])),
    );
    renderRoute();
    expect(await screen.findByRole("alert")).toHaveTextContent(/No relay is connected/);
  });
});
