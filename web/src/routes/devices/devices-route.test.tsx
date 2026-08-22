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
const THIRD_DEVICE_ID = "01J8ZDEV1CE00000000000000C";

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
    ignored: false,
    // The ordinary case a live deployment serves: an age this deployment
    // OBSERVED. Stated in the base fixture rather than left off, because the
    // server sends `first_seen_origin` on every row that has an age at all, and
    // a fixture that omitted it would be exercising only the cautious branch —
    // the exact opposite of the common one.
    first_seen_origin: "planted",
    ...over,
  };
}

/** A device the deployment has no age for: the two age members ABSENT, not
 * present-and-undefined. `exactOptionalPropertyTypes` makes that distinction a
 * type error rather than a style note, which is right — the API omits the members
 * and a fixture that carried explicit undefineds would describe a body no server
 * sends. */
function ageless(d: Device): Device {
  const rest: Device = { ...d };
  delete rest.first_seen;
  delete rest.first_seen_origin;
  return rest;
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
 * adopted device itself). It DOES read `/discovery/relays` (the operator-
 * readable connected-relay list), because relay connectivity is the only way to
 * tell "discovery is running and found nothing" from "nothing is discovering" —
 * see ./discovery. And it reads `/adopted-devices` for the policy panel, which
 * lists the RECORDS rather than joining them onto discovered rows.
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
  packs = [scannerPack()],
  scans = [scanStatus()],
  engines = [engineState()],
}: {
  devices?: Device[];
  entities?: Entity[];
  systemHealth?: SystemHealth;
  adopted?: unknown[];
  packs?: unknown[];
  scans?: unknown[];
  engines?: unknown[];
} = {}) {
  server.use(
    http.get(`${TEST_BASE}/devices`, () => page(devices)),
    http.get(`${TEST_BASE}/entities`, () => page(entities)),
    // Relay connectivity now comes from the operator-readable /discovery/relays,
    // not owner-only /system-health. The fixture reuses the health body's relay
    // list so a test still describes one connected relay in one place.
    http.get(`${TEST_BASE}/discovery/relays`, () => ok({ relays: systemHealth.relays })),
    http.get(`${TEST_BASE}/adopted-devices`, () => page(adopted as never[])),
    // The pack registry, read so the page can say WHO can start a scan. Stubbed
    // by default for exactly the reason the header above records: the route
    // catches this read's failure, so without a stub every test here would keep
    // passing against a permanently unreadable registry — the adopted-panel
    // mistake, repeated.
    http.get(`${TEST_BASE}/extensions`, () => page(packs as never[])),
    // The scan-engine state. Stubbed for the reason this fixture's header
    // records twice over: the route catches this read's failure, so an
    // unstubbed one would leave every test here passing against a panel that
    // permanently believes nothing is known.
    http.get(`${TEST_BASE}/discovery/scan-status`, () => ok({ scans })),
    // The discovery ENGINE state. Stubbed for the third time on the same
    // reasoning — and this one was caught by it: the strip was added, every test
    // in this file still passed, and the panel was rendering "engine state
    // unavailable" on every one of them because nothing served the route.
    http.get(`${TEST_BASE}/discovery/engine-state`, () => ok({ engines })),
  );
}

/** One relay's discovery-engine state — watching for something, nothing
 * undelivered, which is the healthy steady state. */
function engineState(over: Record<string, unknown> = {}) {
  return {
    relay_id: RELAY,
    ssdp_lane: true,
    mdns_lane: true,
    ssdp_watches: 3,
    mdns_watches: 2,
    pack_patterns: 4,
    mdns_undeliverable: 0,
    mac_oui_unimplemented: 0,
    malformed: 0,
    watching_nothing: false,
    reported_at_ms: Date.now() - 5_000,
    ...over,
  };
}

/** One relay's scan-engine state — idle, having finished a sweep a minute ago,
 * which is the ordinary steady state a connected relay reports. */
function scanStatus(over: Record<string, unknown> = {}) {
  return {
    relay_id: RELAY,
    state: "idle",
    reason: "scheduled",
    scan_id: "01M0SCAN00000000000000000A",
    started_at: Date.now() - 62_000,
    finished_at: Date.now() - 60_000,
    candidates: 3,
    ...over,
  };
}

/** An installed extension that declares a scan action — the deployment the box
 * actually runs. Defined here rather than inline because the DEFAULT matters:
 * a fixture with no scanner would make every unrelated test describe a
 * deployment that cannot scan, which is not the one being built. */
function scannerPack(over: Record<string, unknown> = {}) {
  return {
    id: "waiveo/discovery",
    revision: 1,
    version: "1.0.0",
    data_model_version: 1,
    created_at: 0,
    updated_at: 0,
    enabled: true,
    manifest: {
      id: "waiveo/discovery",
      version: "1.0.0",
      displayName: "Discovery",
      ui: { pages: [{ path: "settings", pageType: "settings-form", titleMsg: "msg:page.settings.title" }] },
      dataModel: { version: 1, collections: [] },
      actions: [{ name: "scan-now", capabilityScope: "discovery.scan" }],
    },
    ...over,
  };
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

  it("renders the two seen instants as ages, and an absent one as no answer", async () => {
    // The RENDERING half of the age columns, and deliberately no more than
    // that. Whether the served first_seen actually survives a relay restart is
    // a server-side property this test cannot see: the API is stubbed here, so
    // it would pass against a server that reset the value every minute. That
    // half is pinned where it lives — internal/app/store's
    // TestFirstSeenSurvivesARelayRestart and cmd/waiveo-feeder's
    // TestDeviceAgeSurvivesARelayRestartEndToEnd.
    //
    // What IS this page's own to get right: turning two epoch instants into the
    // comparison an operator is actually making (weeks-old furniture vs. a new
    // arrival), and refusing to answer at all for a device the deployment has
    // no record of, rather than dating it to 1970.
    const now = Date.now();
    seed({
      devices: [
        device({ first_seen: now - 21 * 86_400_000, last_seen: now - 4_000 }),
        device({
          id: OTHER_DEVICE_ID,
          name: "Cafe TV",
          first_seen: now - 90_000,
          last_seen: now - 90_000,
        }),
        // A device the deployment has never durably mirrored: the API omits
        // both members, and the page must say so rather than date it to 1970.
        device({ id: THIRD_DEVICE_ID, name: "Unmirrored TV" }),
      ],
    });
    renderRoute();
    const table = await screen.findByRole("table", { name: "Discovered devices" });

    const furniture = within(table).getByText("Hanger TV").closest("tr")!;
    expect(within(furniture).getByText("21d ago")).toBeInTheDocument();
    expect(within(furniture).getByText("4s ago")).toBeInTheDocument();

    const newcomer = within(table).getByText("Cafe TV").closest("tr")!;
    expect(within(newcomer).getAllByText("1m ago")).toHaveLength(2);

    const unknown = within(table).getByText("Unmirrored TV").closest("tr")!;
    expect(within(unknown).getAllByText("\u2014").length).toBeGreaterThanOrEqual(2);
  });

  it("marks an age it INHERITED rather than drawing it like one it observed", async () => {
    /* #197, on the surface it was filed about.
     *
     * `first_seen` is planted once and never moves, which is what keeps a relay
     * restart from re-dating a whole network as new. On a deployment upgraded
     * from a build predating the durable ledger, every existing age was instead
     * COPIED from an older column that had been written off the reporting
     * relay's own unattested clock. On the one measured box that is 64 of 64
     * rows, 57 of them sharing a single instant to the millisecond.
     *
     * Served identically to an observed instant, this page drew all of them as
     * exact ages ("3d ago") in a column headed "First seen" and ranked the fleet
     * on them — so an operator read "these 57 devices arrived on my network three
     * days ago", which is not what the number means and not something anything
     * observed. The caveat existed: in the boot log, in `waiveo-feeder
     * -store-check`, and in the OpenAPI description. None of those is where the
     * person reading the wrong number is looking.
     */
    const now = Date.now();
    seed({
      devices: [
        device({ first_seen: now - 3 * 86_400_000, first_seen_origin: "planted" }),
        device({
          id: OTHER_DEVICE_ID,
          name: "Inherited TV",
          first_seen: now - 3 * 86_400_000,
          first_seen_origin: "adopted",
        }),
        device({
          id: THIRD_DEVICE_ID,
          name: "Unrecorded TV",
          first_seen: now - 3 * 86_400_000,
          first_seen_origin: "unrecorded",
        }),
      ],
    });
    renderRoute();
    const table = await screen.findByRole("table", { name: "Discovered devices" });

    // The observed one is drawn exactly as before: this must not caveat an age
    // the deployment really did watch.
    const observed = within(table).getByText("Hanger TV").closest("tr")!;
    expect(within(observed).getByText("3d ago")).toBeInTheDocument();
    expect(within(observed).queryByText("~3d ago")).not.toBeInTheDocument();

    // The inherited ones are marked, and say why on hover. `unrecorded` gets the
    // same treatment as `adopted`: it cannot be shown NOT to be one.
    for (const name of ["Inherited TV", "Unrecorded TV"]) {
      const row = within(table).getByText(name).closest("tr")!;
      const cell = within(row).getByText("~3d ago");
      expect(cell).toBeInTheDocument();
      expect(cell).toHaveAttribute("title", expect.stringContaining("Inherited, not observed"));
      expect(cell).toHaveAttribute("title", expect.stringContaining("overstate"));
    }
  });

  it("offers Retire only on an inherited age, and clears it from the row", async () => {
    /* The other half of #197: marking the value and leaving the operator unable
     * to act on it is half a repair. The correction path shipped over `waiveo
     * call` and MCP only — so the person who could SEE the wrong number could not
     * act on it, and the caller who could act could not see it.
     *
     * It is withheld on an OBSERVED age deliberately. Retiring one would discard
     * a fact this deployment actually watched and let the next report replace it
     * with "now", making the device read younger than it is — the defect the
     * write-once rule exists to prevent, arriving through its own repair. */
    const now = Date.now();
    seed({
      devices: [
        device({ first_seen: now - 3 * 86_400_000, first_seen_origin: "planted" }),
        device({
          id: OTHER_DEVICE_ID,
          name: "Inherited TV",
          first_seen: now - 3 * 86_400_000,
          first_seen_origin: "adopted",
        }),
      ],
    });
    let retiredPath: string | null = null;
    server.use(
      http.delete(`${TEST_BASE}/devices/:id/first-seen`, ({ request, params }) => {
        retiredPath = new URL(request.url).pathname;
        return ok(ageless(device({ id: String(params.id), name: "Inherited TV" })));
      }),
    );

    const user = renderRoute();
    const table = await screen.findByRole("table", { name: "Discovered devices" });

    const observed = within(table).getByText("Hanger TV").closest("tr")!;
    expect(within(observed).queryByRole("button", { name: "Retire age" })).not.toBeInTheDocument();

    const inherited = within(table).getByText("Inherited TV").closest("tr")!;
    await user.click(within(inherited).getByRole("button", { name: "Retire age" }));

    // The dialog says the one thing this cannot do before it is confirmed.
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByText(/cannot restore or replace it/)).toBeInTheDocument();
    await user.click(within(dialog).getByRole("button", { name: "Retire age" }));

    await waitFor(() => expect(retiredPath).toBe(`/api/v1/devices/${OTHER_DEVICE_ID}/first-seen`));
    // The answer IS the device as it now reads, so the row corrects itself: the
    // age is gone and the action with it.
    await waitFor(() => {
      const after = within(
        screen.getByRole("table", { name: "Discovered devices" }),
      ).getByText("Inherited TV").closest("tr")!;
      expect(within(after).queryByText("~3d ago")).not.toBeInTheDocument();
      expect(within(after).queryByRole("button", { name: "Retire age" })).not.toBeInTheDocument();
    });
  });

  it("counts what discovery found, and says this page starts nothing", async () => {
    seed({ devices: [device(), device({ id: OTHER_DEVICE_ID, name: "Cafe TV" })] });
    renderRoute();
    const status = await screen.findByRole("region", { name: "Discovery status" });
    await waitFor(() => expect(within(status).getByText("Discovered")).toBeInTheDocument());
    expect(within(status).getByText("Relays reporting")).toBeInTheDocument();
    expect(within(status).getByText(/Refresh/)).toBeInTheDocument();
    // The page must NOT go back to claiming a scan cannot be started. It can —
    // by the extension that owns `discovery.scan` — and the old sentence was
    // read as "this platform has no on-demand scan" for as long as it stood.
    expect(within(status).queryByText(/there is no scan to start from here/)).toBeNull();
  });

  it("names the extension that CAN start a scan, and links straight to it", async () => {
    seed();
    renderRoute();
    const status = await screen.findByRole("region", { name: "Discovery status" });
    const link = await within(status).findByRole("link", { name: "Discovery" });
    // The link goes into the pack's own confined namespace — the only door to a
    // pack page since the rail section was removed.
    expect(link).toHaveAttribute("href", "/p/waiveo/discovery/settings");
    expect(within(status).getByText(/can scan this deployment's networks now/)).toBeInTheDocument();
  });

  it("says nothing can scan on demand only when nothing installed declares it", async () => {
    seed({ packs: [] });
    renderRoute();
    const status = await screen.findByRole("region", { name: "Discovery status" });
    await waitFor(() =>
      expect(within(status).getByText(/Nothing installed can scan on demand/)).toBeInTheDocument(),
    );
    expect(within(status).queryByRole("link", { name: "Discovery" })).toBeNull();
  });

  it("does NOT claim nothing can scan when the registry could not be read", async () => {
    // The distinction the whole page is built on, applied to one more read:
    // an empty registry is a fact about the deployment, a refused read is a
    // fact about this console. Collapsing them would have the page invent an
    // architectural limit out of its own missing permission.
    seed();
    server.use(
      http.get(`${TEST_BASE}/extensions`, () =>
        HttpResponse.json(
          {
            type: "about:blank",
            title: "Forbidden",
            status: 403,
            code: "FORBIDDEN",
            detail: "Only the workspace owner may read the pack registry.",
            trace_id: TRACE_ID,
          },
          { status: 403, headers: { "Content-Type": "application/problem+json", "Trace-Id": TRACE_ID } },
        ),
      ),
    );
    renderRoute();
    const status = await screen.findByRole("region", { name: "Discovery status" });
    await waitFor(() =>
      expect(
        within(status).getByText(/can also scan on demand could not be read here/),
      ).toBeInTheDocument(),
    );
    expect(within(status).queryByText(/Nothing installed can scan on demand/)).toBeNull();
  });

  it("offers no link into a DISABLED scanner, and says why rather than going quiet", async () => {
    seed({ packs: [scannerPack({ enabled: false })] });
    renderRoute();
    const status = await screen.findByRole("region", { name: "Discovery status" });
    await waitFor(() => expect(within(status).getByText("Discovery")).toBeInTheDocument());
    expect(within(status).queryByRole("link", { name: "Discovery" })).toBeNull();
    expect(within(status).getByText(/it is disabled/)).toBeInTheDocument();
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

  it("explains the sweep-freshness question rather than declaring it unpublishable", async () => {
    // This assertion used to pin the sentence "No sweep timestamp is published".
    // api/1 publishes one; the console simply never read it. Keeping the old
    // assertion would have made the test the reason the falsehood survived.
    seed();
    renderRoute();
    const status = await screen.findByRole("region", { name: "Discovery status" });
    await waitFor(() =>
      expect(within(status).getByText(/reports what its scan engine is doing/)).toBeInTheDocument(),
    );
    expect(within(status).queryByText(/No sweep timestamp is published/)).toBeNull();
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

  it("says it does not KNOW when relay health cannot be read, and does not claim a sweep ran", async () => {
    // /discovery/relays is operator-readable, so an admin is no longer blind by
    // owner-gate — but a genuine read failure (the endpoint unreachable) still
    // leaves the page unable to say whether anything is looking, and it must not
    // quietly render that as "a relay swept and found nothing".
    seed({ devices: [], entities: [] });
    server.use(
      http.get(`${TEST_BASE}/discovery/relays`, () =>
        HttpResponse.json(
          {
            type: "about:blank",
            title: "Internal Server Error",
            status: 500,
            code: "INTERNAL",
            detail: "An unexpected server error occurred.",
            trace_id: TRACE_ID,
          },
          { status: 500, headers: { "Content-Type": "application/problem+json", "Trace-Id": TRACE_ID } },
        ),
      ),
    );
    renderRoute();
    const state = await stateRegion();
    await waitFor(() => expect(state).toHaveAttribute("data-kind", "blind"));
    expect(state).toHaveTextContent(/it is not known whether anything is looking/);
    expect(state).toHaveTextContent(/Relay health could not be read/);
    // The two claims it must NOT make.
    expect(state).not.toHaveTextContent(/Discovery is running/);
    expect(state).not.toHaveTextContent(/Discovery is not running/);
    // And the relay list is omitted entirely rather than rendered empty: an
    // empty list would read as "no relays", which is the claim just refused.
    expect(screen.queryByText("Relays reporting")).not.toBeNull(); // the stat card label
    expect(document.querySelector("[data-slot='discovery-relay']")).toBeNull();
  });

  it("keeps the fleet readable when only relay health cannot be read", async () => {
    // The device plane and relay health are separate reads. A failure on relay
    // health must not blank a fleet the caller is perfectly entitled to see.
    seed();
    server.use(
      http.get(`${TEST_BASE}/discovery/relays`, () =>
        HttpResponse.json(
          {
            type: "about:blank",
            title: "Internal Server Error",
            status: 500,
            code: "INTERNAL",
            detail: "An unexpected server error occurred.",
            trace_id: TRACE_ID,
          },
          { status: 500, headers: { "Content-Type": "application/problem+json", "Trace-Id": TRACE_ID } },
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

describe("Devices — entities", () => {
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

  // The console strip (2026-08-19) removed BOTH control affordances this page
  // used to carry: the per-entity Remote dialog and the "Roku console" link. The
  // owner's rule is that a per-driver control surface belongs to a driver PACK,
  // not to core's discovery page ("Roku is its own extension, and Discovery is
  // its own extension"), and waiveo/roku is uninstalled — an adopted Roku is
  // discovered and unclassified, and nothing here drives it.
  //
  // This is asserted rather than merely deleted because a half-removal is the
  // failure shape: a Remote button whose component is gone renders a crash, and
  // a link to a deleted /roku renders the 404 inside the shell. A media-player
  // fleet is seeded on purpose, since that is exactly the state that used to
  // produce both.
  it("offers no control surface for a media player — a driver pack owns that", async () => {
    seed();
    renderRoute();
    const entities = await screen.findByRole("table", { name: "Entities" });
    await waitFor(() => expect(within(entities).getByText("Hanger TV main")).toBeInTheDocument());
    expect(screen.queryByRole("button", { name: "Remote" })).toBeNull();
    expect(screen.queryByRole("link", { name: "Roku console" })).toBeNull();
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

  it("filters by the decision, which is a BADGE cell and still filterable", async () => {
    // Labelled "Decision" rather than "Adoption" because the column now carries
    // three states: adoption is one decision an operator takes about a device
    // and ignoring is the other, so a filter named for only one of them hides
    // the existence of the second from the person looking for it.
    seed({ devices: fleet(), entities: [] });
    const user = renderRoute();
    await screen.findByRole("table", { name: "Discovered devices" });

    await user.selectOptions(screen.getByLabelText("Decision"), "Adopted");
    await waitFor(() => expect(screen.queryByText("Hanger TV")).not.toBeInTheDocument());
    expect(screen.getByText("Cafe TV")).toBeInTheDocument();
  });

  it("offers Ignored as a filter option only because a row carries it", async () => {
    // Faceted from the rows present, like every other enum filter on this page.
    seed({
      devices: [device(), device({ id: OTHER_DEVICE_ID, name: "Lobby TV", ignored: true })],
      entities: [],
    });
    const user = renderRoute();
    await screen.findByRole("table", { name: "Discovered devices" });

    const decision = screen.getByLabelText("Decision");
    expect(within(decision).getByRole("option", { name: "Ignored (1)" })).toBeInTheDocument();

    await user.selectOptions(decision, "Ignored");
    await waitFor(() => expect(screen.queryByText("Hanger TV")).not.toBeInTheDocument());
    expect(screen.getByText("Lobby TV")).toBeInTheDocument();
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

describe("Devices — ignoring, clicked through", () => {
  // Every test here CLICKS. The whole capability existed server-side — store
  // projection, both routes, idempotency, a required `ignored` member in the
  // schema — and shipped unreachable because nothing in the console called it.
  // A test that asserted the button rendered would have passed against that
  // exact bug, so these drive the control and read the request that came out.

  it("ignores by device id with no body, and flips the row from the answer", async () => {
    let rows = [device()];
    let seen: { method: string; path: string; body: string; idempotencyKey: string | null } | null =
      null;
    seed();
    server.use(
      http.get(`${TEST_BASE}/devices`, () => page(rows)),
      http.get(`${TEST_BASE}/entities`, () => page([entity()])),
      http.post(`${TEST_BASE}/devices/${DEVICE_ID}/ignore`, async ({ request }) => {
        seen = {
          method: request.method,
          path: new URL(request.url).pathname,
          body: await request.text(),
          idempotencyKey: request.headers.get("Idempotency-Key"),
        };
        rows = [device({ ignored: true })];
        return ok(device({ ignored: true }));
      }),
    );

    const user = renderRoute();
    // No confirm dialog, deliberately: the act reaches no relay and is reversed
    // by the button that replaces it. If a dialog is ever added, this click
    // stops working and the test says so rather than silently passing.
    await user.click(await screen.findByRole("button", { name: "Ignore" }));

    await waitFor(() => expect(seen).not.toBeNull());
    expect(seen!.method).toBe("POST");
    expect(seen!.path).toBe(`/api/v1/devices/${DEVICE_ID}/ignore`);
    expect(seen!.body).toBe("");
    // A double-click must not fire twice at the far end (API-050/052).
    expect(seen!.idempotencyKey).toBeTruthy();

    const table = await screen.findByRole("table", { name: "Discovered devices" });
    await waitFor(() => expect(within(table).getByText("Ignored")).toBeInTheDocument());
    // Reversible from the row it is on — the spec's "never a hidden trash can".
    expect(within(table).getByRole("button", { name: "Un-ignore" })).toBeInTheDocument();
  });

  it("un-ignores with a DELETE, and puts the row back", async () => {
    let rows = [device({ ignored: true })];
    let seen: { method: string; path: string } | null = null;
    seed();
    server.use(
      http.get(`${TEST_BASE}/devices`, () => page(rows)),
      http.get(`${TEST_BASE}/entities`, () => page([entity()])),
      http.delete(`${TEST_BASE}/devices/${DEVICE_ID}/ignore`, ({ request }) => {
        seen = { method: request.method, path: new URL(request.url).pathname };
        rows = [device({ ignored: false })];
        return ok(device({ ignored: false }));
      }),
    );

    const user = renderRoute();
    await user.click(await screen.findByRole("button", { name: "Un-ignore" }));

    await waitFor(() => expect(seen).not.toBeNull());
    expect(seen!.method).toBe("DELETE");
    expect(seen!.path).toBe(`/api/v1/devices/${DEVICE_ID}/ignore`);

    const table = await screen.findByRole("table", { name: "Discovered devices" });
    await waitFor(() => expect(within(table).getByText("Discovered")).toBeInTheDocument());
    expect(within(table).getByRole("button", { name: "Ignore" })).toBeInTheDocument();
  });

  it("still offers Adopt on an ignored device, because ignoring is not a veto", async () => {
    // An ignore is "not interested for now", not "never". The path from set-aside
    // to adopted has to stay open on the row, or changing your mind means first
    // finding the un-ignore, which is friction with no safety value behind it.
    seed();
    server.use(
      http.get(`${TEST_BASE}/devices`, () => page([device({ ignored: true })])),
      http.get(`${TEST_BASE}/entities`, () => page([entity()])),
    );
    renderRoute();
    const table = await screen.findByRole("table", { name: "Discovered devices" });
    expect(within(table).getByRole("button", { name: "Adopt" })).toBeInTheDocument();
    expect(within(table).getByRole("button", { name: "Un-ignore" })).toBeInTheDocument();
  });

  it("offers no ignore on an ADOPTED device, and reads it as adopted when both flags are set", async () => {
    // The flags are independent on the row and a device can carry both, so the
    // console's reading order is load-bearing: adoption supersedes ignoring
    // (internal/app/devices Device.Ignored). Showing a device that is actively
    // polled and driveable as merely "set aside" would be a false status.
    seed();
    server.use(
      http.get(`${TEST_BASE}/devices`, () => page([device({ adopted: true, ignored: true })])),
      http.get(`${TEST_BASE}/entities`, () => page([entity()])),
    );
    renderRoute();
    const table = await screen.findByRole("table", { name: "Discovered devices" });
    await waitFor(() => expect(within(table).getByText("Adopted")).toBeInTheDocument());
    expect(within(table).queryByText("Ignored")).toBeNull();
    expect(within(table).queryByRole("button", { name: "Ignore" })).toBeNull();
    expect(within(table).queryByRole("button", { name: "Un-ignore" })).toBeNull();
  });

  it("reports the Problem and leaves the device un-ignored when the ignore is refused", async () => {
    seed();
    server.use(
      http.get(`${TEST_BASE}/devices`, () => page([device()])),
      http.get(`${TEST_BASE}/entities`, () => page([entity()])),
      http.post(`${TEST_BASE}/devices/${DEVICE_ID}/ignore`, () =>
        HttpResponse.json(
          {
            type: "about:blank",
            title: "Forbidden",
            status: 403,
            code: "FORBIDDEN",
            detail: "This principal may not write devices.",
            trace_id: TRACE_ID,
          },
          { status: 403, headers: { "Content-Type": "application/problem+json", "Trace-Id": TRACE_ID } },
        ),
      ),
    );

    const user = renderRoute();
    await user.click(await screen.findByRole("button", { name: "Ignore" }));

    // The row must NOT optimistically flip: a device the server refused to
    // ignore that reads as ignored is the console lying about a decision.
    const table = await screen.findByRole("table", { name: "Discovered devices" });
    await waitFor(() =>
      expect(within(table).getByRole("button", { name: "Ignore" })).toBeInTheDocument(),
    );
    expect(within(table).getByText("Discovered")).toBeInTheDocument();
    expect(within(table).queryByText("Ignored")).toBeNull();
  });

  it("filters the fleet by the ignore decision, faceted from the rows present", async () => {
    seed();
    server.use(
      http.get(`${TEST_BASE}/devices`, () =>
        page([
          device(),
          device({ id: OTHER_DEVICE_ID, name: "Lobby TV", ignored: true }),
        ]),
      ),
      http.get(`${TEST_BASE}/entities`, () => page([entity()])),
    );
    renderRoute();
    const table = await screen.findByRole("table", { name: "Discovered devices" });
    await waitFor(() => expect(within(table).getByText("Lobby TV")).toBeInTheDocument());
    // "Ignored" is a badge cell, so it needs its own accessor to be filterable
    // at all — the same trap the adoption filter documents.
    expect(within(table).getByText("Ignored")).toBeInTheDocument();
    expect(within(table).getByText("Discovered")).toBeInTheDocument();
  });
});

describe("Devices — open ports, the three answers as an operator meets them", () => {
  // The console is the last place the absent/empty collapse could be
  // reintroduced. Five of six layers beneath it used to report "a scan looked
  // and found nothing open" as "nobody has looked"; drawing both as an em dash
  // here would restore that defect exactly where it is felt.

  it("draws NOT SCANNED and NONE OPEN as different answers, not both as blank", async () => {
    seed({
      devices: [
        device({ id: DEVICE_ID, name: "Never looked" }),
        device({ id: OTHER_DEVICE_ID, name: "Came back clean", open_ports: [] }),
      ],
      entities: [],
    });
    renderRoute();
    const table = await screen.findByRole("table", { name: "Discovered devices" });
    await waitFor(() => expect(within(table).getByText("Came back clean")).toBeInTheDocument());

    expect(within(table).getByText("Not scanned")).toBeInTheDocument();
    // A RESULT, not a gap — and specifically not the same cell as the one above.
    expect(within(table).getByText("None open")).toBeInTheDocument();
  });

  it("names what typically answers on a port, inline", async () => {
    seed({ devices: [device({ open_ports: [9100] })], entities: [] });
    renderRoute();
    const table = await screen.findByRole("table", { name: "Discovered devices" });
    // The column's whole operator value: this row is `unclassified` and this is
    // the only thing on the page saying it is a printer.
    await waitFor(() => expect(within(table).getByText(/9100 printer/)).toBeInTheDocument());
  });

  it("renders a port it knows nothing about as a bare number", async () => {
    seed({ devices: [device({ open_ports: [4711] })], entities: [] });
    renderRoute();
    const table = await screen.findByRole("table", { name: "Discovered devices" });
    const cell = await within(table).findByText(/4711/);
    expect(cell.textContent).toBe("4711");
  });

  it("finds the printer when an operator searches the WORD, not the port", async () => {
    seed({
      devices: [
        device({ id: DEVICE_ID, name: "Hanger TV", open_ports: [8060] }),
        device({ id: OTHER_DEVICE_ID, name: "Back office", open_ports: [9100] }),
      ],
      entities: [],
    });
    const user = renderRoute();
    await screen.findByRole("table", { name: "Discovered devices" });

    await user.type(screen.getByLabelText("Search devices"), "printer");
    await waitFor(() => expect(screen.queryByText("Hanger TV")).not.toBeInTheDocument());
    expect(screen.getByText("Back office")).toBeInTheDocument();
  });

  it("narrows to what nobody has scanned", async () => {
    seed({
      devices: [
        device({ id: DEVICE_ID, name: "Never looked" }),
        device({ id: OTHER_DEVICE_ID, name: "Came back clean", open_ports: [] }),
      ],
      entities: [],
    });
    const user = renderRoute();
    await screen.findByRole("table", { name: "Discovered devices" });

    await user.type(screen.getByLabelText("Search devices"), "not scanned");
    await waitFor(() => expect(screen.queryByText("Came back clean")).not.toBeInTheDocument());
    expect(screen.getByText("Never looked")).toBeInTheDocument();
  });
});

describe("Devices — hardware identity", () => {
  // The vendor was never missing, only unpublished: it was read out of the MAC
  // and spent on a fallback name, so it reached an operator only for a device
  // that could not name itself. On the box that left 12 of 63 — including BOTH
  // machines called "NAS" — with a vendor the platform knew and nothing showing
  // it.

  it("shows the vendor for a device that named ITSELF, which the name never could", async () => {
    seed({
      devices: [device({ name: "NAS", mac: "bc:24:11:3f:b9:4d", vendor: "Proxmox" })],
      entities: [],
    });
    renderRoute();
    const table = await screen.findByRole("table", { name: "Discovered devices" });
    await waitFor(() => expect(within(table).getByText("NAS")).toBeInTheDocument());
    expect(within(table).getByText("Proxmox")).toBeInTheDocument();
    expect(within(table).getByText("bc:24:11:3f:b9:4d")).toBeInTheDocument();
  });

  it("tells two identically-named devices apart", async () => {
    // The case that justifies the column: same name, and an IP DHCP may move.
    seed({
      devices: [
        device({ id: DEVICE_ID, name: "NAS", address: "192.0.2.10", mac: "bc:24:11:3f:b9:4d" }),
        device({ id: OTHER_DEVICE_ID, name: "NAS", address: "192.0.2.11", mac: "aa:bb:cc:dd:ee:ff" }),
      ],
      entities: [],
    });
    renderRoute();
    const table = await screen.findByRole("table", { name: "Discovered devices" });
    await waitFor(() => expect(within(table).getAllByText("NAS")).toHaveLength(2));
    expect(within(table).getByText("bc:24:11:3f:b9:4d")).toBeInTheDocument();
    expect(within(table).getByText("aa:bb:cc:dd:ee:ff")).toBeInTheDocument();
  });

  it("shows the MAC alone when the OUI is not recognized, inventing no vendor", async () => {
    seed({ devices: [device({ mac: "ce:41:9a:7b:22:10" })], entities: [] });
    renderRoute();
    const table = await screen.findByRole("table", { name: "Discovered devices" });
    await waitFor(() => expect(within(table).getByText("ce:41:9a:7b:22:10")).toBeInTheDocument());
  });

  it("draws an em dash when no hardware address was learned", async () => {
    // A real state, not a gap: a device a protocol lane named carries that
    // protocol's id and no MAC.
    seed({ devices: [device({ name: "Hanger TV" })], entities: [] });
    renderRoute();
    const table = await screen.findByRole("table", { name: "Discovered devices" });
    await waitFor(() => expect(within(table).getByText("Hanger TV")).toBeInTheDocument());
    expect(within(table).queryByText(/^..:..:..:..:..:..$/)).toBeNull();
  });

  it("finds a device by VENDOR and by a MAC fragment", async () => {
    seed({
      devices: [
        device({ id: DEVICE_ID, name: "Hanger TV" }),
        device({ id: OTHER_DEVICE_ID, name: "Rack box", mac: "bc:24:11:3f:b9:4d", vendor: "Proxmox" }),
      ],
      entities: [],
    });
    const user = renderRoute();
    await screen.findByRole("table", { name: "Discovered devices" });

    const search = screen.getByLabelText("Search devices");
    await user.type(search, "Proxmox");
    await waitFor(() => expect(screen.queryByText("Hanger TV")).not.toBeInTheDocument());
    expect(screen.getByText("Rack box")).toBeInTheDocument();

    await user.clear(search);
    await user.type(search, "bc:24");
    await waitFor(() => expect(screen.queryByText("Hanger TV")).not.toBeInTheDocument());
    expect(screen.getByText("Rack box")).toBeInTheDocument();
  });
});


describe("Devices — what each relay's scan engine is doing", () => {
  // This panel used to state, as a contract fact, that "No sweep timestamp is
  // published". api/1 publishes one — `/discovery/scan-status` carries each
  // relay's engine state and when its last scan started and finished — and
  // nothing in the console read it. The consequence the old paragraph named was
  // real and remained real: a relay that had quietly stopped sweeping looked
  // exactly like one whose network is empty.

  it("says when a relay last swept, instead of that nobody publishes it", async () => {
    seed();
    renderRoute();
    const status = await screen.findByRole("region", { name: "Discovery status" });
    await waitFor(() => expect(within(status).getByText(/last swept/)).toBeInTheDocument());
    // The retired claim must not come back.
    expect(within(status).queryByText(/No sweep timestamp is published/)).toBeNull();
  });

  it("says a relay is scanning NOW, and whether an operator asked for it", async () => {
    seed({ scans: [scanStatus({ state: "scanning", reason: "operator", finished_at: undefined })] });
    renderRoute();
    const status = await screen.findByRole("region", { name: "Discovery status" });
    await waitFor(() =>
      expect(within(status).getByText(/scanning now \(operator\)/)).toBeInTheDocument(),
    );
  });

  it("distinguishes a connected relay that has NEVER reported a scan", async () => {
    // The case the whole strip exists for: connected is not the same as
    // sweeping, and before this they rendered identically.
    seed({ scans: [] });
    renderRoute();
    const status = await screen.findByRole("region", { name: "Discovery status" });
    await waitFor(() => expect(within(status).getByText(/no scan reported/)).toBeInTheDocument());
  });

  it("does not claim a relay never scanned when the read FAILED", async () => {
    seed();
    server.use(
      http.get(`${TEST_BASE}/discovery/scan-status`, () =>
        HttpResponse.json(
          {
            type: "about:blank",
            title: "Forbidden",
            status: 403,
            code: "FORBIDDEN",
            detail: "Not readable by this principal.",
            trace_id: TRACE_ID,
          },
          { status: 403, headers: { "Content-Type": "application/problem+json", "Trace-Id": TRACE_ID } },
        ),
      ),
    );
    renderRoute();
    const status = await screen.findByRole("region", { name: "Discovery status" });
    // A refused read is a fact about this console, not about the relay. It must
    // not be rendered as the relay having reported nothing.
    await waitFor(() => expect(within(status).getByText(/relay-/)).toBeInTheDocument());
    expect(within(status).queryByText(/last swept/)).toBeNull();
    expect(within(status).queryByText(/no scan reported/)).toBeNull();
  });
});

describe("Devices — what each relay is WATCHING for", () => {
  // The third leg of "is discovery actually running", and the only one that can
  // answer it in the negative. A relay can be connected, idle rather than
  // mid-sweep, and watching for absolutely nothing — in which case the passive
  // lanes surface no device however long an operator waits, while every other
  // signal on this page reads healthy. The relay has always computed these
  // numbers; before `discovery.engine_state` they went only to its journal.

  it("says what the engine is watching, per lane", async () => {
    seed();
    renderRoute();
    const status = await screen.findByRole("region", { name: "Discovery status" });
    await waitFor(() =>
      expect(within(status).getByText(/watching 3 ssdp, 2 mdns/)).toBeInTheDocument(),
    );
    // And says NOTHING about packs when packs are declaring — the default
    // fixture carries 4 patterns. Asserted explicitly because a note that fires
    // unconditionally would read as an alarm on every healthy relay, and every
    // positive test would still pass.
    expect(within(status).queryByText(/no pack declares a device/)).toBeNull();
  });

  it("names a lane that is OFF rather than reporting it as zero watches", async () => {
    // The distinction the lane booleans exist for: no live mDNS watch because
    // this deployment never bound multicast is a different problem, with a
    // different owner, from a generation that declared no mDNS patterns.
    seed({ engines: [engineState({ mdns_lane: false, mdns_watches: 0 })] });
    renderRoute();
    const status = await screen.findByRole("region", { name: "Discovery status" });
    await waitFor(() => expect(within(status).getByText(/mdns off/)).toBeInTheDocument());
    expect(within(status).queryByText(/0 mdns/)).toBeNull();
  });

  it("RAISES watching-for-nothing, and says why", async () => {
    // The condition the whole frame exists to surface.
    seed({
      engines: [
        engineState({ ssdp_watches: 0, mdns_watches: 0, pack_patterns: 0, watching_nothing: true }),
      ],
    });
    renderRoute();
    const status = await screen.findByRole("region", { name: "Discovery status" });
    await waitFor(() =>
      expect(
        within(status).getByText(/watching for nothing — no pack declares a device/),
      ).toBeInTheDocument(),
    );
  });

  it("tells 'declared but undelivered' apart from 'nothing declared'", async () => {
    // Both render zero watches. Only one of them is a misconfiguration, and an
    // operator cannot act on the right one if they read the same.
    seed({
      engines: [
        engineState({
          ssdp_watches: 0,
          mdns_watches: 0,
          pack_patterns: 2,
          mdns_undeliverable: 2,
          watching_nothing: true,
        }),
      ],
    });
    renderRoute();
    const status = await screen.findByRole("region", { name: "Discovery status" });
    await waitFor(() =>
      expect(
        within(status).getByText(/watching for nothing — all 2 declaration\(s\) undelivered/),
      ).toBeInTheDocument(),
    );
  });

  it("counts undelivered declarations beside a healthy watch set, and explains each", async () => {
    seed({
      engines: [
        engineState({ mdns_undeliverable: 1, mac_oui_unimplemented: 2, malformed: 1 }),
      ],
    });
    renderRoute();
    const status = await screen.findByRole("region", { name: "Discovery status" });
    const cell = await waitFor(() => within(status).getByText(/4 undelivered/));
    // The breakdown rides as hover text: it explains a number already on screen,
    // and the page stays readable without it.
    expect(cell.closest("[data-slot='relay-engine-state']")).toHaveAttribute(
      "title",
      expect.stringContaining("never bound multicast"),
    );
  });

  it("says 'no pack declares a device' even when a lane holds a BUILTIN watch", async () => {
    // THE DEV BOX'S ACTUAL STATE, pinned as a regression case. It reports
    // `0 ssdp, 1 mdns live (0 pack pattern(s))`: one builtin mDNS watch, nothing
    // pack-declared. watching_nothing is correctly FALSE — a lane does hold a
    // watch — so the alarm branch never fires, and before this the strip read
    // "watching 0 ssdp, 1 mdns" and an operator could not see that zero of it
    // came from a pack. That is the exact number #218 was opened about.
    //
    // No fixture caught it because every one of them had either a pack pattern
    // or an empty lane. Only the box had both at once.
    seed({
      engines: [
        engineState({ ssdp_watches: 0, mdns_watches: 1, pack_patterns: 0, watching_nothing: false }),
      ],
    });
    renderRoute();
    const status = await screen.findByRole("region", { name: "Discovery status" });
    await waitFor(() =>
      expect(
        within(status).getByText(/watching 0 ssdp, 1 mdns · no pack declares a device/),
      ).toBeInTheDocument(),
    );
  });

  it("does not turn a REFUSED read into a claim about the relay", async () => {
    // The same absent-vs-empty rule the scan cell beside it follows: a read this
    // console was refused is a fact about the console.
    seed();
    server.use(
      http.get(`${TEST_BASE}/discovery/engine-state`, () =>
        HttpResponse.json({ type: "about:blank", title: "Forbidden", status: 403, code: "FORBIDDEN" }, { status: 403 }),
      ),
    );
    renderRoute();
    const status = await screen.findByRole("region", { name: "Discovery status" });
    await waitFor(() => expect(within(status).getByText(/relay-/)).toBeInTheDocument());
    expect(within(status).getByText(/engine state unavailable/)).toBeInTheDocument();
    expect(within(status).queryByText(/engine state not reported/)).toBeNull();
    expect(within(status).queryByText(/watching for nothing/)).toBeNull();
  });

  it("tells a relay that has not reported apart from one watching nothing", async () => {
    // Zeroes are an alarm; silence is not. Minting the alarm for a relay that
    // has simply not spoken would fire it on every fresh feeder.
    seed({ engines: [] });
    renderRoute();
    const status = await screen.findByRole("region", { name: "Discovery status" });
    await waitFor(() =>
      expect(within(status).getByText(/engine state not reported/)).toBeInTheDocument(),
    );
    expect(within(status).queryByText(/watching for nothing/)).toBeNull();
  });
});

describe("Devices — why everything reads unclassified", () => {
  // The question the device table provokes on a real deployment: fifty of
  // sixty-three rows say `unclassified`, and unexplained that reads as
  // classification being broken. It is absent by configuration — core declares
  // no pattern of its own on purpose, and no installed pack declares one either.

  it("says nothing installed recognises a device kind, when nothing does", async () => {
    // The dev box exactly: one pack, owning scan policy, teaching no recognition.
    seed({ packs: [scannerPack()] });
    renderRoute();
    const status = await screen.findByRole("region", { name: "Discovery status" });
    await waitFor(() =>
      expect(within(status).getByText(/Nothing installed recognises a device kind/)).toBeInTheDocument(),
    );
    // And says the unclassified row is the CORRECT answer, not a failure.
    expect(within(status).getByText(/correct answer rather than a failure/)).toBeInTheDocument();
  });

  it("names the recogniser and its classes when one is installed", async () => {
    seed({
      packs: [
        scannerPack(),
        {
          ...scannerPack(),
          id: "waiveo/roku",
          manifest: {
            ...scannerPack().manifest,
            id: "waiveo/roku",
            displayName: "Roku",
            devices: [{ deviceClass: "media-player", match: [{ ssdp: "roku:ecp" }] }],
          },
        },
      ],
    });
    renderRoute();
    const status = await screen.findByRole("region", { name: "Discovery status" });
    await waitFor(() =>
      expect(within(status).getByText(/recognises media-player/)).toBeInTheDocument(),
    );
    expect(within(status).queryByText(/Nothing installed recognises/)).toBeNull();
  });

  it("does NOT claim nothing recognises anything when the registry read failed", async () => {
    seed();
    server.use(
      http.get(`${TEST_BASE}/extensions`, () =>
        HttpResponse.json(
          { type: "about:blank", title: "Forbidden", status: 403, code: "FORBIDDEN",
            detail: "no", trace_id: TRACE_ID },
          { status: 403, headers: { "Content-Type": "application/problem+json", "Trace-Id": TRACE_ID } },
        ),
      ),
    );
    renderRoute();
    const status = await screen.findByRole("region", { name: "Discovery status" });
    await waitFor(() =>
      expect(
        within(status).getByText(/recognise a device kind could not be read here/),
      ).toBeInTheDocument(),
    );
    expect(within(status).queryByText(/Nothing installed recognises a device kind/)).toBeNull();
  });
});
