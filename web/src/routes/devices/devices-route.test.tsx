import { describe, it, expect, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { ThemeProvider } from "@/components/theme/theme-provider";
import DevicesRoute from "./devices-route";
import { createApi, type Device, type Entity } from "@/api";
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

/** The two reads the route makes on mount. Any of them can be overridden by a
 * later `server.use` in a test.
 *
 * Two, not four: the page no longer lists `/adopted-devices` (it has no key to
 * join those rows onto a device) or `/scope-nodes` (the server places an
 * adopted device itself). `onUnhandledRequest: "error"` is what keeps that
 * honest — if the route regained either fetch, every test here would fail
 * loudly rather than silently reading a stale stub. */
function seed({
  devices = [device()],
  entities = [entity()],
}: {
  devices?: Device[];
  entities?: Entity[];
} = {}) {
  server.use(
    http.get(`${TEST_BASE}/devices`, () => page(devices)),
    http.get(`${TEST_BASE}/entities`, () => page(entities)),
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

describe("Devices — adopting, clicked through", () => {
  it("adopts by device id with no body at all, and flips the row from the answer", async () => {
    let adoptedRows = [device()];
    let seen: { path: string; body: string; idempotencyKey: string | null } | null = null;
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
    );
    renderRoute();
    expect(await screen.findByRole("alert")).toHaveTextContent(/No relay is connected/);
  });
});
