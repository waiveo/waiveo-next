import { describe, it, expect, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { ThemeProvider } from "@/components/theme/theme-provider";
import DevicesRoute from "./devices-route";
import { createApi, type AdoptedDevice, type Device, type Entity } from "@/api";
import { TEST_BASE, TRACE_ID, ULID_A, ULID_B, ULID_C, ULID_ROOT, ok, scopeNode } from "@/api/test-support";

// The Devices route, clicked through. What is worth testing here is not that a
// table renders — it is the two judgements the page makes that a human would
// otherwise have to make from raw JSON:
//
//   1. Adopted vs discovered is a JOIN over REL-153's (driver, native_id), not a
//      field. A page that got the join wrong would offer "Adopt" on a device
//      that is already adopted, and a second adoption record for one physical
//      TV is exactly what REL-153 exists to prevent.
//   2. Adopt is REFUSED when the identity tuple was not reported, because the
//      alternative is a create body built out of guesses.

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
const ADOPTED_ID = "01J8ZADOPTED0000000000000A";

function device(over: Partial<Device> = {}): Device {
  return {
    id: DEVICE_ID,
    external_id: null,
    relay_id: RELAY,
    device_class: "media-player",
    name: "Hanger TV",
    scope_node: ULID_A,
    labels: { address: "192.0.2.40", model: "Roku Ultra", driver: "roku", native_id: "uuid:roku:ecp:X1" },
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

function adoptedDevice(over: Partial<AdoptedDevice> = {}): AdoptedDevice {
  return {
    id: ADOPTED_ID,
    external_id: null,
    name: "Hanger TV",
    scope_node: ULID_A,
    driver: "roku",
    native_id: "uuid:roku:ecp:X1",
    poll_cadence_seconds: null,
    entities: [],
    labels: {},
    revision: 2,
    created_at: 1752537000000,
    updated_at: 1752537000000,
    ...over,
  };
}

function page(items: unknown[]) {
  return HttpResponse.json({ items, cursor: null }, { headers: { "Trace-Id": TRACE_ID } });
}

/** The four reads the route makes on mount. Any of them can be overridden by a
 * later `server.use` in a test. */
function seed({
  devices = [device()],
  entities = [entity()],
  adopted = [] as AdoptedDevice[],
  nodes = [
    scopeNode({ id: ULID_A, kind: "site", name: "The Hanger", parent_id: ULID_ROOT }),
    scopeNode({ id: ULID_B, kind: "group", name: "West Wing", parent_id: ULID_A }),
  ],
}: {
  devices?: Device[];
  entities?: Entity[];
  adopted?: AdoptedDevice[];
  nodes?: unknown[];
} = {}) {
  server.use(
    http.get(`${TEST_BASE}/devices`, () => page(devices)),
    http.get(`${TEST_BASE}/entities`, () => page(entities)),
    http.get(`${TEST_BASE}/adopted-devices`, () => page(adopted)),
    http.get(`${TEST_BASE}/scope-nodes`, () => page(nodes)),
  );
}

function renderRoute() {
  const api = createApi({ baseUrl: TEST_BASE });
  render(
    <ThemeProvider>
      <DevicesRoute api={api} />
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

  it("marks a device adopted by matching the adoption record's (driver, native_id)", async () => {
    seed({ adopted: [adoptedDevice()] });
    renderRoute();
    const table = await screen.findByRole("table", { name: "Discovered devices" });
    await waitFor(() => expect(within(table).getByText("Adopted")).toBeInTheDocument());
    // Already adopted — the offer is to release it, never to adopt it twice.
    expect(within(table).getByRole("button", { name: "Release" })).toBeInTheDocument();
    expect(within(table).queryByRole("button", { name: "Adopt" })).toBeNull();
  });

  it("does NOT match an adoption record for a different native_id", async () => {
    seed({ adopted: [adoptedDevice({ native_id: "uuid:roku:ecp:SOMETHING-ELSE" })] });
    renderRoute();
    const table = await screen.findByRole("table", { name: "Discovered devices" });
    await waitFor(() => expect(within(table).getByText("Discovered")).toBeInTheDocument());
    expect(within(table).getByRole("button", { name: "Adopt" })).toBeEnabled();
  });

  it("disables Adopt when the relay reported no identity tuple to adopt against", async () => {
    seed({ devices: [device({ labels: { address: "192.0.2.41" } })] });
    renderRoute();
    const table = await screen.findByRole("table", { name: "Discovered devices" });
    await waitFor(() => expect(within(table).getByRole("button", { name: "Adopt" })).toBeDisabled());
  });
});

describe("Devices — adopting, clicked through", () => {
  it("creates the adoption record with the identity tuple, the chosen placement, and enabled entities", async () => {
    let body: Record<string, unknown> = {};
    let adoptedRows: AdoptedDevice[] = [];
    server.use(
      http.get(`${TEST_BASE}/devices`, () => page([device()])),
      http.get(`${TEST_BASE}/entities`, () => page([entity()])),
      http.get(`${TEST_BASE}/adopted-devices`, () => page(adoptedRows)),
      http.get(`${TEST_BASE}/scope-nodes`, () =>
        page([
          scopeNode({ id: ULID_A, kind: "site", name: "The Hanger", parent_id: ULID_ROOT }),
          scopeNode({ id: ULID_B, kind: "group", name: "West Wing", parent_id: ULID_A }),
        ]),
      ),
      http.post(`${TEST_BASE}/adopted-devices`, async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        adoptedRows = [adoptedDevice()];
        return ok(adoptedDevice(), { status: 201, revision: 1 });
      }),
    );

    const user = renderRoute();
    await user.click(await screen.findByRole("button", { name: "Adopt" }));
    const dialog = await screen.findByRole("dialog", { name: /Adopt Hanger TV/ });
    await user.selectOptions(within(dialog).getByLabelText("Adopt into"), ULID_B);
    await user.type(within(dialog).getByLabelText("Poll cadence (seconds)"), "30");
    await user.click(within(dialog).getByRole("button", { name: "Adopt" }));

    await waitFor(() => expect(body["driver"]).toBe("roku"));
    expect(body).toMatchObject({
      name: "Hanger TV",
      scope_node: ULID_B,
      driver: "roku",
      native_id: "uuid:roku:ecp:X1",
      poll_cadence_seconds: 30,
    });
    // Adoption POLICY (REL-063) — an adoption that enabled nothing would ship a
    // device_inventory entry no relay would ever poll.
    expect(body["entities"]).toEqual([
      {
        entity_id: ENTITY_ID,
        device_class: "media-player",
        enabled: true,
        hidden: false,
        display_name: "Hanger TV main",
        category: "primary",
      },
    ]);
    // …and the reload shows the device as adopted.
    const table = await screen.findByRole("table", { name: "Discovered devices" });
    await waitFor(() => expect(within(table).getByText("Adopted")).toBeInTheDocument());
  });

  it("omits poll_cadence_seconds entirely when the operator states none", async () => {
    let body: Record<string, unknown> = {};
    seed();
    server.use(
      http.post(`${TEST_BASE}/adopted-devices`, async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return ok(adoptedDevice(), { status: 201, revision: 1 });
      }),
    );
    const user = renderRoute();
    await user.click(await screen.findByRole("button", { name: "Adopt" }));
    const dialog = await screen.findByRole("dialog", { name: /Adopt Hanger TV/ });
    await user.click(within(dialog).getByRole("button", { name: "Adopt" }));
    await waitFor(() => expect(body["driver"]).toBe("roku"));
    // REL-063 fixes no default cadence; sending one the console invented would
    // be a polling policy nobody authored.
    expect(Object.hasOwn(body, "poll_cadence_seconds")).toBe(false);
  });

  it("releases an adopted device under its If-Match after the confirm", async () => {
    let deleted: { path: string; ifMatch: string | null } | null = null;
    seed({ adopted: [adoptedDevice({ revision: 5 })] });
    server.use(
      http.delete(`${TEST_BASE}/adopted-devices/${ADOPTED_ID}`, ({ request }) => {
        deleted = { path: new URL(request.url).pathname, ifMatch: request.headers.get("If-Match") };
        return new HttpResponse(null, { status: 204, headers: { "Trace-Id": TRACE_ID } });
      }),
    );
    const user = renderRoute();
    await user.click(await screen.findByRole("button", { name: "Release" }));
    const confirm = await screen.findByRole("dialog", { name: /Release Hanger TV\?/ });
    await user.click(within(confirm).getByRole("button", { name: "Release" }));
    await waitFor(() => expect(deleted).not.toBeNull());
    expect(deleted!.path).toBe(`/api/v1/adopted-devices/${ADOPTED_ID}`);
    expect(deleted!.ifMatch).toBe('"5"');
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

describe("Devices — when the device plane cannot be read", () => {
  it("surfaces the Problem rather than an empty fleet with no explanation", async () => {
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
      http.get(`${TEST_BASE}/adopted-devices`, () => page([])),
      http.get(`${TEST_BASE}/scope-nodes`, () => page([scopeNode({ id: ULID_C, kind: "site" })])),
    );
    renderRoute();
    expect(await screen.findByRole("alert")).toHaveTextContent(/No relay is connected/);
  });
});
