import { describe, it, expect, beforeAll, afterAll, afterEach, vi } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
// The kit re-exports this exact sonner `toast` object, so spying here intercepts
// the panel's calls. The panel renders no <Toaster/> of its own — the route it
// mounts into owns one — so a toast has no DOM to be found in here.
import { toast as sonnerToast } from "sonner";
import { ThemeProvider } from "@/components/theme/theme-provider";
import { AdoptedDevices, cadenceOf } from "./adopted-devices";
import { createApi, type AdoptedDevice } from "@/api";

// The adopted-device policy panel. The whole capability was ABSENT: full CRUD
// has been live and mounted server-side since the device plane landed, and the
// console rendered nothing at all for an adopted row — so after adopting a
// device an operator could set no cadence, disable no noisy entity, rename none,
// and release nothing. The adopt dialog promised all of this was "refined
// afterwards on the adopted device" and there was no afterwards.
//
// Every case below drives the real controls. A panel that renders its inputs and
// sends the wrong body is the failure this replaces, not one it would catch.

const TEST_BASE = "http://api.test/api/v1";
const ULID_A = "01J8ZADOPTED0000000000000A";
const ULID_B = "01J8ZADOPTED0000000000000B";
const ENT_A = "01J8ZENT1TY00000000000000A";
const ENT_B = "01J8ZENT1TY00000000000000B";

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => {
  server.resetHandlers();
  vi.restoreAllMocks();
});
afterAll(() => server.close());

function record(over: Partial<AdoptedDevice> = {}): AdoptedDevice {
  return {
    id: ULID_A,
    external_id: null,
    name: "Hanger TV",
    scope_node: "01J8Z0B0000000000000000000",
    driver: "roku-ecp",
    native_id: "X001234567",
    poll_cadence_seconds: null,
    entities: [
      {
        entity_id: ENT_A,
        device_class: "media-player",
        enabled: true,
        hidden: false,
        display_name: "Hanger TV",
        category: "primary",
      },
      {
        entity_id: ENT_B,
        device_class: "sensor",
        enabled: true,
        hidden: false,
        display_name: "Signal strength",
        category: "diagnostic",
      },
    ],
    labels: {},
    revision: 3,
    created_at: "2026-08-01T00:00:00Z",
    updated_at: "2026-08-01T00:00:00Z",
    ...over,
  } as AdoptedDevice;
}

function page(items: unknown[]) {
  return HttpResponse.json({ items, cursor: null }, { headers: { "Trace-Id": "t" } });
}

function seed(items: AdoptedDevice[] = [record()]) {
  server.use(http.get(`${TEST_BASE}/adopted-devices`, () => page(items)));
}

function renderPanel() {
  render(
    <ThemeProvider>
      <AdoptedDevices api={createApi({ baseUrl: TEST_BASE })} />
    </ThemeProvider>,
  );
  return userEvent.setup();
}

describe("cadenceOf — blank is a DECISION, not a missing value", () => {
  // REL-063 fixes no default cadence, so "none stated" is a real, distinct
  // policy that must survive the round trip as null rather than collapsing to a
  // number nobody authored.
  it("reads blank as null and a positive integer as itself", () => {
    expect(cadenceOf("")).toBeNull();
    expect(cadenceOf("   ")).toBeNull();
    expect(cadenceOf("30")).toBe(30);
  });

  // Refused in the console, because the schema's minimum is 1: sending these
  // would have the server refuse an edit the operator was already told was saved.
  it("refuses anything that is not a whole number of seconds ≥ 1", () => {
    for (const bad of ["0", "-5", "1.5", "abc", "30s", "1e3", "٣٠"]) {
      expect(cadenceOf(bad), bad).toBe("invalid");
    }
  });
});

describe("Adopted devices — the records and the policy over them", () => {
  it("lists adopted records with their cadence and enabled-entity count", async () => {
    seed([record(), record({ id: ULID_B, name: "Lobby TV", poll_cadence_seconds: 30 })]);
    renderPanel();

    const table = await screen.findByRole("table", { name: "Adopted devices" });
    expect(within(table).getByText("Hanger TV")).toBeInTheDocument();
    expect(within(table).getByText("Lobby TV")).toBeInTheDocument();
    // A stated cadence reads as itself…
    expect(within(table).getByText("30s")).toBeInTheDocument();
    // …and an unstated one as its own fact, never as 0 and never blank.
    expect(within(table).getByText("none set")).toBeInTheDocument();
    // Both records have both entities enabled, so both rows say so.
    expect(within(table).getAllByText("2 of 2 enabled")).toHaveLength(2);
  });

  // The headline. Every member of the policy is a decision, and this proves the
  // decisions reach the server rather than only the screen.
  it("saves a full policy edit — name, cadence and per-entity decisions", async () => {
    seed();
    let sent: { etag: string | null; body: Record<string, unknown> } | null = null;
    server.use(
      http.patch(`${TEST_BASE}/adopted-devices/${ULID_A}`, async ({ request }) => {
        sent = {
          etag: request.headers.get("If-Match"),
          body: (await request.json()) as Record<string, unknown>,
        };
        return HttpResponse.json({ data: record({ revision: 4 }), revision: 4 }, { headers: { "Trace-Id": "t" } });
      }),
    );
    const user = renderPanel();

    await user.click(await screen.findByRole("button", { name: "Policy" }));

    // Rename, state a cadence where none was stated…
    const name = await screen.findByLabelText<HTMLInputElement>("Name");
    await user.clear(name);
    await user.type(name, "Hangar TV");
    await user.type(screen.getByLabelText("Poll cadence (seconds)"), "45");
    // …disable the noisy diagnostic entity, hide it, and rename it.
    await user.click(screen.getByRole("checkbox", { name: "Signal strength enabled" }));
    await user.click(screen.getByRole("checkbox", { name: "Signal strength hidden" }));
    const displayName = screen.getByLabelText<HTMLInputElement>("Display name — sensor");
    await user.clear(displayName);
    await user.type(displayName, "RSSI");
    await user.selectOptions(screen.getByLabelText("Category — media-player"), "diagnostic");

    await user.click(screen.getByRole("button", { name: "Save policy" }));

    await waitFor(() => expect(sent).not.toBeNull());
    const call = sent as unknown as { etag: string | null; body: Record<string, unknown> };
    expect(call.body.name).toBe("Hangar TV");
    expect(call.body.poll_cadence_seconds).toBe(45);
    // The optimistic-concurrency guard, from the record's own revision.
    expect(call.etag).toBe('"3"');

    const entities = call.body.entities as Array<Record<string, unknown>>;
    // The untouched entity is sent unchanged — a PATCH that dropped it would
    // silently un-adopt an entity the operator never mentioned.
    expect(entities[0]).toMatchObject({ entity_id: ENT_A, enabled: true, hidden: false, category: "diagnostic" });
    expect(entities[1]).toMatchObject({
      entity_id: ENT_B,
      enabled: false,
      hidden: true,
      display_name: "RSSI",
    });
  });

  // Blank means null, and null is what the resource must receive: omitting the
  // member would leave whatever cadence was there, which is the opposite of
  // "state no cadence".
  it("clears a stated cadence to null when the box is emptied", async () => {
    seed([record({ poll_cadence_seconds: 30 })]);
    let body: Record<string, unknown> | null = null;
    server.use(
      http.patch(`${TEST_BASE}/adopted-devices/${ULID_A}`, async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ data: record(), revision: 4 }, { headers: { "Trace-Id": "t" } });
      }),
    );
    const user = renderPanel();

    await user.click(await screen.findByRole("button", { name: "Policy" }));
    // It opens showing the CURRENT policy, not a blank form.
    const box = await screen.findByLabelText<HTMLInputElement>("Poll cadence (seconds)");
    expect(box.value).toBe("30");
    await user.clear(box);
    await user.click(screen.getByRole("button", { name: "Save policy" }));

    await waitFor(() => expect(body).not.toBeNull());
    const sentBody = body as unknown as Record<string, unknown>;
    expect(sentBody.poll_cadence_seconds).toBeNull();
    expect("poll_cadence_seconds" in sentBody).toBe(true);
  });

  // A cadence the schema would refuse must be refused HERE — after the modal
  // closed on an apparent save, a server 422 has nowhere useful to land.
  it("refuses an invalid cadence without sending anything", async () => {
    seed();
    let called = false;
    server.use(
      http.patch(`${TEST_BASE}/adopted-devices/${ULID_A}`, () => {
        called = true;
        return HttpResponse.json({ data: record(), revision: 4 }, { headers: { "Trace-Id": "t" } });
      }),
    );
    const errorSpy = vi.spyOn(sonnerToast, "error");
    const user = renderPanel();

    await user.click(await screen.findByRole("button", { name: "Policy" }));
    await user.type(screen.getByLabelText("Poll cadence (seconds)"), "0");
    await user.click(screen.getByRole("button", { name: "Save policy" }));

    await waitFor(() =>
      expect(errorSpy).toHaveBeenCalledWith(expect.stringMatching(/whole number of seconds/i)),
    );
    expect(called).toBe(false);
    // The modal stays OPEN on the rejected value, so the operator can fix it
    // rather than reopening and retyping the whole policy.
    expect(screen.getByRole("button", { name: "Save policy" })).toBeInTheDocument();
  });

  // A 412 is a specific fact and gets specific words: the generic "couldn't
  // save" sends an operator round the same edit against the same stale revision.
  it("names a lost concurrency race rather than reporting a generic failure", async () => {
    seed();
    server.use(
      http.patch(`${TEST_BASE}/adopted-devices/${ULID_A}`, () =>
        HttpResponse.json(
          { type: "about:blank", title: "Conflict", status: 412, code: "REVISION_CONFLICT", detail: "stale", trace_id: "t" },
          { status: 412, headers: { "Content-Type": "application/problem+json", "Trace-Id": "t" } },
        ),
      ),
    );
    const errorSpy = vi.spyOn(sonnerToast, "error");
    const user = renderPanel();

    await user.click(await screen.findByRole("button", { name: "Policy" }));
    await user.click(screen.getByRole("button", { name: "Save policy" }));

    await waitFor(() =>
      expect(errorSpy).toHaveBeenCalledWith(expect.stringMatching(/changed elsewhere/i)),
    );
  });

  it("releases a device under its If-Match and reloads the list", async () => {
    let releasedEtag: string | null | undefined;
    let listed = 0;
    server.use(
      http.get(`${TEST_BASE}/adopted-devices`, () => {
        listed += 1;
        return page(listed === 1 ? [record()] : []);
      }),
      http.delete(`${TEST_BASE}/adopted-devices/${ULID_A}`, ({ request }) => {
        releasedEtag = request.headers.get("If-Match");
        return new HttpResponse(null, { status: 204, headers: { "Trace-Id": "t" } });
      }),
    );
    const user = renderPanel();

    await user.click(await screen.findByRole("button", { name: "Release" }));
    // A confirm step: releasing stops a device being polled and commanded.
    await user.click(await screen.findByRole("button", { name: "Release", hidden: false }));

    await waitFor(() => expect(releasedEtag).toBe('"3"'));
    // The list re-reads, so the released device is gone from the table rather
    // than lingering until a manual refresh.
    expect(await screen.findByText("No devices adopted yet")).toBeInTheDocument();
  });

  it("says the list could not be read rather than showing an empty fleet", async () => {
    server.use(
      http.get(`${TEST_BASE}/adopted-devices`, () =>
        HttpResponse.json(
          { type: "about:blank", title: "Forbidden", status: 403, code: "FORBIDDEN", detail: "not yours", trace_id: "t" },
          { status: 403, headers: { "Content-Type": "application/problem+json", "Trace-Id": "t" } },
        ),
      ),
    );
    renderPanel();

    // "No devices adopted yet" would be a claim about the fleet; this is a claim
    // about the read, which is the only one the console can support.
    expect(await screen.findByText("Adopted devices could not be read")).toBeInTheDocument();
    expect(screen.queryByText("No devices adopted yet")).not.toBeInTheDocument();
  });
});
