// @vitest-environment node
//
// The device-plane client, against a mocked server (msw). What these tests are
// for is the two places this module could be quietly wrong in a way that only
// shows up on real hardware:
//
//   - `ok:false` must come back as a VALUE. A relay that refuses a command
//     answers 200 with a typed error, and a client that threw there would turn
//     "the TV is unplugged" into "the console is broken" — losing the one code
//     that says which it is.
//   - `deviceFacts` must not invent a fact. A reader that returned "" for a
//     fact nobody reported would have the UI render an empty address as though
//     a relay had stated one, and an operator cannot tell those apart.
//   - `adopt` must send the device id and NOTHING else. The adoption record's
//     identity tuple lives only on the server; a client that grew a request
//     body here would be inventing one.

import { afterAll, afterEach, beforeAll, describe, expect, it } from "vitest";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { createApi } from "./resources";
import { deviceFacts, launchableApps, type Device } from "./devices";
import { TEST_BASE, TRACE_ID, ULID_A, ULID_B, ULID_C, ok } from "./test-support";

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

const RELAY = "relay-0f1e2d3c4b5a69788796a5b4c3d2e1f0";

function api() {
  return createApi({ baseUrl: TEST_BASE });
}

function device(over: Partial<Device> = {}): Device {
  return {
    id: ULID_A,
    external_id: null,
    relay_id: RELAY,
    device_class: "media-player",
    name: "Hanger TV",
    scope_node: ULID_C,
    labels: {},
    adopted: false,
    ...over,
  };
}

function page(items: unknown[], cursor: string | null = null) {
  return HttpResponse.json({ items, cursor }, { headers: { "Trace-Id": TRACE_ID } });
}

describe("devices + entities — the read model", () => {
  it("lists devices and passes a selector through, with no cursor on the first page", async () => {
    let seen: URL | null = null;
    server.use(
      http.get(`${TEST_BASE}/devices`, ({ request }) => {
        seen = new URL(request.url);
        return page([device()]);
      }),
    );
    const result = await api().devices.list({ selector: "device_class=media-player" });
    expect(result.items).toHaveLength(1);
    expect(seen!.searchParams.get("selector")).toBe("device_class=media-player");
    // API-035: an empty cursor is not a keyset position and must not be sent.
    expect(seen!.searchParams.has("cursor")).toBe(false);
  });

  it("pages() walks the keyset cursor to exhaustion", async () => {
    const cursors: (string | null)[] = [];
    server.use(
      http.get(`${TEST_BASE}/entities`, ({ request }) => {
        const cursor = new URL(request.url).searchParams.get("cursor");
        cursors.push(cursor);
        return cursor === null
          ? page([{ id: ULID_A }], "next-page")
          : page([{ id: ULID_B }], null);
      }),
    );
    const seen: string[] = [];
    for await (const entity of api().entities.pages()) seen.push(entity.id);
    expect(seen).toEqual([ULID_A, ULID_B]);
    expect(cursors).toEqual([null, "next-page"]);
  });
});

describe("sendCommand — the entity-addressed dispatch", () => {
  it("POSTs {command, params} to the entity's commands path under an Idempotency-Key", async () => {
    let body: unknown = null;
    let idempotencyKey: string | null = null;
    let path = "";
    server.use(
      http.post(`${TEST_BASE}/entities/${ULID_B}/commands`, async ({ request }) => {
        body = await request.json();
        idempotencyKey = request.headers.get("Idempotency-Key");
        path = new URL(request.url).pathname;
        return ok({ ok: true });
      }),
    );
    const result = await api().entities.sendCommand(ULID_B, "keypress", { key: "Up" });
    expect(result.ok).toBe(true);
    expect(body).toEqual({ command: "keypress", params: { key: "Up" } });
    expect(path).toBe(`/api/v1/entities/${ULID_B}/commands`);
    // Firing `power` or `launch` twice on a retry is a real action twice, so
    // the key is not optional decoration here.
    expect(idempotencyKey).toBeTruthy();
  });

  it("omits `params` entirely for a no-argument command", async () => {
    let body: Record<string, unknown> = {};
    server.use(
      http.post(`${TEST_BASE}/entities/${ULID_B}/commands`, async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return ok({ ok: true });
      }),
    );
    await api().entities.sendCommand(ULID_B, "home");
    // `additionalProperties:false` — the key must be absent, not present-and-null.
    expect(Object.hasOwn(body, "params")).toBe(false);
    expect(body).toEqual({ command: "home" });
  });

  it("returns a relay refusal as a value, carrying the relay's own code", async () => {
    server.use(
      http.post(`${TEST_BASE}/entities/${ULID_B}/commands`, () =>
        ok({
          ok: false,
          error: { code: "COMMAND_TARGET_UNREACHABLE", message: "no response from 192.0.2.40" },
        }),
      ),
    );
    // Not a rejection: the exchange completed, the command did not take.
    const result = await api().entities.sendCommand(ULID_B, "power", { state: "on" });
    expect(result.ok).toBe(false);
    expect(result.error?.code).toBe("COMMAND_TARGET_UNREACHABLE");
  });
});

describe("adopt — the one decision this API makes about a discovered device", () => {
  it("POSTs the device's own path with no body, under an Idempotency-Key", async () => {
    let seen: { path: string; body: string; key: string | null; contentType: string | null } | null =
      null;
    server.use(
      http.post(`${TEST_BASE}/devices/${ULID_A}/adopt`, async ({ request }) => {
        seen = {
          path: new URL(request.url).pathname,
          body: await request.text(),
          key: request.headers.get("Idempotency-Key"),
          contentType: request.headers.get("Content-Type"),
        };
        return ok(device({ id: ULID_A, adopted: true }));
      }),
    );
    const adopted = await api().devices.adopt(ULID_A);

    expect(seen!.path).toBe(`/api/v1/devices/${ULID_A}/adopt`);
    // No body, and therefore no Content-Type claiming one. The operation
    // declares no request schema; the identity the record is keyed by is the
    // server's to supply, not this client's to guess.
    expect(seen!.body).toBe("");
    expect(seen!.contentType).toBeNull();
    // API-050/052: a retry-on-timeout must replay, not adopt a second time.
    expect(seen!.key).toBeTruthy();
    // The answer is the device as it now reads, so a caller can correct one row
    // without re-listing the fleet.
    expect(adopted.adopted).toBe(true);
  });

  it("percent-encodes the id rather than splicing it into the path raw", async () => {
    let path: string | null = null;
    server.use(
      http.post(`${TEST_BASE}/devices/:id/adopt`, ({ request }) => {
        path = new URL(request.url).pathname;
        return ok(device({ adopted: true }));
      }),
    );
    await api().devices.adopt("a/b");
    expect(path).toBe("/api/v1/devices/a%2Fb/adopt");
  });
});

describe("adopted devices — the authored half", () => {
  it("creates an adoption record under an Idempotency-Key and deletes under If-Match", async () => {
    let created: Record<string, unknown> = {};
    let ifMatch: string | null = null;
    server.use(
      http.post(`${TEST_BASE}/adopted-devices`, async ({ request }) => {
        created = (await request.json()) as Record<string, unknown>;
        return ok({ id: ULID_A, revision: 1 }, { status: 201, revision: 1 });
      }),
      http.delete(`${TEST_BASE}/adopted-devices/${ULID_A}`, ({ request }) => {
        ifMatch = request.headers.get("If-Match");
        return new HttpResponse(null, { status: 204, headers: { "Trace-Id": TRACE_ID } });
      }),
    );
    const a = api();
    await a.adoptedDevices.create({
      name: "Hanger TV",
      scope_node: ULID_C,
      driver: "roku",
      native_id: "uuid:roku:ecp:X1",
    });
    expect(created).toMatchObject({ driver: "roku", native_id: "uuid:roku:ecp:X1" });
    await a.adoptedDevices.remove(ULID_A, '"4"');
    expect(ifMatch).toBe('"4"');
  });
});

describe("deviceFacts — reading a discovery report honestly", () => {
  it("prefers a top-level member over the same-named label", () => {
    const both = device({
      labels: { address: "192.0.2.9", model: "old" },
      address: "192.0.2.10",
      model: "Roku Ultra",
    });
    expect(deviceFacts(both)).toMatchObject({ address: "192.0.2.10", model: "Roku Ultra" });
  });

  it("falls back to labels when the schema carries nothing", () => {
    const facts = deviceFacts(
      device({ labels: { address: "192.0.2.11", driver: "roku", native_id: "uuid:roku:ecp:X1" } }),
    );
    expect(facts).toEqual({
      address: "192.0.2.11",
      model: null,
      driver: "roku",
      nativeId: "uuid:roku:ecp:X1",
    });
  });

  it("reports a blank or missing fact as null, never as an empty string", () => {
    const facts = deviceFacts(device({ labels: { address: "   " } }));
    expect(facts.address).toBeNull();
    expect(facts.driver).toBeNull();
    expect(facts.nativeId).toBeNull();
  });
});

describe("launchableApps", () => {
  it("reads app.<channel> labels into launch shortcuts, sorted by name", () => {
    expect(
      launchableApps(
        device({ labels: { "app.12": "Netflix", "app.837": "YouTube", zone: "lobby" } }),
      ),
    ).toEqual([
      { channel: "12", name: "Netflix" },
      { channel: "837", name: "YouTube" },
    ]);
  });

  it("is empty when a deployment advertises no inventory", () => {
    expect(launchableApps(device())).toEqual([]);
    expect(launchableApps(device({ labels: { "app.": "nameless", "app.9": "  " } }))).toEqual([]);
  });
});
