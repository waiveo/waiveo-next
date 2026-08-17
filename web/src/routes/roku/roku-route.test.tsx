import { describe, it, expect, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { MemoryRouter } from "react-router";
import { ThemeProvider } from "@/components/theme/theme-provider";
import RokuRoute from "./roku-route";
import { createApi, type AdoptedDevice, type Device, type Entity } from "@/api";
import { TEST_BASE, TRACE_ID, ok } from "@/api/test-support";

// The Roku console, DRIVEN — not rendered and inspected. What is worth testing
// here is not that a remote draws, it is:
//
//   1. Every button sends a command the media-player class actually declares
//      (REG-066). A button whose command does not exist can only ever earn
//      COMMAND_UNRESOLVED, and that is the defect this page exists to stop
//      shipping.
//   2. A press whose command the relay REFUSED is visible afterwards. The whole
//      failure mode this codebase keeps repeating is the button that silently
//      does nothing, and a toast is gone before the operator looks back.
//   3. The three preconditions the relay silently requires are stated BEFORE
//      the press, separately, and never gate the controls.
//   4. It works with exactly one device — the lab has one Roku, and a page that
//      needs a selection made first is a page that is broken there.

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => {
  server.resetHandlers();
  window.localStorage.clear();
});
afterAll(() => server.close());

const RELAY = "relay-0f1e2d3c4b5a69788796a5b4c3d2e1f0";
const DEVICE_ID = "01J8ZDEV1CE00000000000000A";
const ENTITY_ID = "01J8ZENT1TY00000000000000A";
const SITE = "01J8Z0ROOT0000000000000000";

function device(over: Partial<Device> = {}): Device {
  return {
    id: DEVICE_ID,
    external_id: null,
    relay_id: RELAY,
    device_class: "media-player",
    name: "The Hanger",
    scope_node: SITE,
    labels: {},
    address: "192.0.2.31:8060",
    model: "Roku Ultra",
    serial: "X0012345678",
    adopted: true,
    ignored: false,
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
    name: "The Hanger main",
    scope_node: SITE,
    labels: {},
    state: "idle",
    attributes: {
      power_mode: "PowerOn",
      active_app: "Roku",
      active_app_id: "562859",
      app_type: "screensaver",
      is_screensaver: "true",
      app_version: "1.2.3",
    },
    ...over,
  };
}

function record(over: Partial<AdoptedDevice> = {}): AdoptedDevice {
  return {
    id: DEVICE_ID,
    external_id: null,
    name: "The Hanger",
    scope_node: SITE,
    driver: "roku-ecp",
    native_id: "uuid:roku:ecp:AA11",
    poll_cadence_seconds: 30,
    entities: [
      {
        entity_id: ENTITY_ID,
        device_class: "media-player",
        enabled: true,
        hidden: false,
        display_name: "The Hanger main",
        category: "primary",
      },
    ],
    labels: {},
    revision: 1,
    created_at: 1_753_142_400_000,
    updated_at: 1_753_142_400_000,
    ...over,
  };
}

function page(items: unknown[]) {
  return HttpResponse.json({ items, cursor: null }, { headers: { "Trace-Id": TRACE_ID } });
}

/** The three reads the page makes on mount. `onUnhandledRequest: "error"` keeps
 * that list honest — a page that gained a fourth fails loudly here. */
function seed({
  devices = [device()],
  entities = [entity()],
  records = [record()],
}: {
  devices?: Device[];
  entities?: Entity[];
  records?: AdoptedDevice[];
} = {}) {
  server.use(
    http.get(`${TEST_BASE}/devices`, () => page(devices)),
    http.get(`${TEST_BASE}/entities`, () => page(entities)),
    http.get(`${TEST_BASE}/adopted-devices`, () => page(records)),
  );
}

function renderRoute() {
  const api = createApi({ baseUrl: TEST_BASE });
  render(
    <ThemeProvider>
      <MemoryRouter>
        <RokuRoute api={api} />
      </MemoryRouter>
    </ThemeProvider>,
  );
  return userEvent.setup();
}

/** Capture every command the page dispatches, answering `ok` unless told
 * otherwise. Returns the array so a test can assert the WIRE, not the UI's
 * account of the wire. */
function captureCommands(answer: () => Response = () => ok({ ok: true }) as Response) {
  const sent: Record<string, unknown>[] = [];
  server.use(
    http.post(`${TEST_BASE}/entities/${ENTITY_ID}/commands`, async ({ request }) => {
      sent.push((await request.json()) as Record<string, unknown>);
      return answer();
    }),
  );
  return sent;
}

describe("Roku console — one device, no selection required", () => {
  it("selects the only media player and shows what it is doing without a click", async () => {
    seed();
    renderRoute();
    const state = await screen.findByRole("region", { name: "Player state" });
    expect(within(state).getByText("The Hanger")).toBeInTheDocument();
    // The one line an operator reads first: on, and NOT showing content.
    expect(within(state).getByText(/showing its screensaver/)).toBeInTheDocument();
    expect(within(state).getByText("192.0.2.31:8060")).toBeInTheDocument();
    expect(within(state).getByText("X0012345678")).toBeInTheDocument();
    // The cadence comes off the adoption record, joined by entity id.
    expect(within(state).getByText("every 30s")).toBeInTheDocument();
  });

  it("shows every declared attribute, marking the ones nobody reported", async () => {
    seed({ entities: [entity({ attributes: { power_mode: "PowerOn" } })] });
    renderRoute();
    const panel = await screen.findByRole("region", { name: "Reported attributes" });
    expect(within(panel).getByText("PowerOn")).toBeInTheDocument();
    // Five of the six went unreported and each says so, rather than the rows
    // being hidden — "no state at all" is what an unpolled device looks like.
    expect(within(panel).getAllByText("not reported")).toHaveLength(5);
  });

  it("surfaces an attribute the class does not declare rather than dropping it", async () => {
    seed({ entities: [entity({ attributes: { power_mode: "PowerOn", tuner_channel: "7.1" } })] });
    renderRoute();
    const panel = await screen.findByRole("region", { name: "Reported attributes" });
    expect(within(panel).getByText("tuner_channel")).toBeInTheDocument();
    expect(within(panel).getByText("7.1")).toBeInTheDocument();
  });

  it("reads the driver off the adoption record, since the device row does not carry one", async () => {
    seed();
    renderRoute();
    expect(await screen.findByText("Roku (ECP)")).toBeInTheDocument();
  });

  it("says the driver is unknown for an unadopted device rather than assuming Roku", async () => {
    seed({ devices: [device({ adopted: false })], records: [] });
    renderRoute();
    expect(await screen.findByText(/Driver not published until adopted/)).toBeInTheDocument();
  });

  it("sends the operator to Devices when nothing is a media player", async () => {
    seed({ devices: [device({ device_class: "thermostat" })], entities: [], records: [] });
    renderRoute();
    expect(await screen.findByText("No media player has been discovered")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Open Devices" })).toHaveAttribute("href", "/devices");
  });
});

describe("Roku console — the controls dispatch real commands", () => {
  it("sends power on and power off as the class's own power command", async () => {
    seed();
    const sent = captureCommands();
    const user = renderRoute();
    const remote = await screen.findByRole("group", { name: /Remote — The Hanger main/ });
    await user.click(within(remote).getByRole("button", { name: "Power on" }));
    await waitFor(() => expect(sent).toHaveLength(1));
    await user.click(within(remote).getByRole("button", { name: "Power off" }));
    await waitFor(() => expect(sent).toHaveLength(2));
    expect(sent).toEqual([
      { command: "power", params: { state: "on" } },
      { command: "power", params: { state: "off" } },
    ]);
  });

  it("sends Home as the declared `home` command, not a keypress", async () => {
    // REG-066 declares home separately, and using it is what keeps a driver free
    // to implement "go home" as something other than an ECP keypress.
    seed();
    const sent = captureCommands();
    const user = renderRoute();
    const remote = await screen.findByRole("group", { name: /Remote/ });
    await user.click(within(remote).getByRole("button", { name: "Home" }));
    await waitFor(() => expect(sent).toEqual([{ command: "home" }]));
  });

  it("sends the legacy remote's full key set, each as a keypress with its ECP key", async () => {
    // Parity with the legacy Roku extension's remote, which had exactly these
    // keys. Info and Pause were the two missing here; they are plain ECP keys
    // the relay passes straight through, not invented commands.
    seed();
    const sent = captureCommands();
    const user = renderRoute();
    const remote = await screen.findByRole("group", { name: /Remote/ });
    const presses: [string, string][] = [
      ["Up", "Up"],
      ["Down", "Down"],
      ["Left", "Left"],
      ["Right", "Right"],
      ["OK", "Select"],
      ["Back", "Back"],
      ["Info", "Info"],
      ["Rewind", "Rev"],
      ["Play", "Play"],
      ["Pause", "Pause"],
      ["Fast forward", "Fwd"],
      ["Volume down", "VolumeDown"],
      ["Mute", "VolumeMute"],
      ["Volume up", "VolumeUp"],
    ];
    for (const [label] of presses) {
      await user.click(within(remote).getByRole("button", { name: label }));
    }
    await waitFor(() => expect(sent).toHaveLength(presses.length));
    expect(sent).toEqual(
      presses.map(([, key]) => ({ command: "keypress", params: { key } })),
    );
  });

  it("launches a channel by the id typed into the field", async () => {
    seed();
    const sent = captureCommands();
    const user = renderRoute();
    const remote = await screen.findByRole("group", { name: /Remote/ });
    await user.type(within(remote).getByLabelText(/Channel id/), "dev");
    await user.click(within(remote).getByRole("button", { name: "Launch" }));
    await waitFor(() => expect(sent).toEqual([{ command: "launch", params: { channel: "dev" } }]));
  });
});

describe("Roku console — a press that did nothing is visible", () => {
  it("logs a successful dispatch, so 'did the launch land' is answerable", async () => {
    seed();
    captureCommands();
    const user = renderRoute();
    const remote = await screen.findByRole("group", { name: /Remote/ });
    await user.click(within(remote).getByRole("button", { name: "Home" }));

    const log = await screen.findByRole("table", { name: "Command outcomes" });
    await waitFor(() => expect(within(log).getByText("Applied")).toBeInTheDocument());
    expect(within(log).getByText("Home")).toBeInTheDocument();
    expect(within(log).getByText("home")).toBeInTheDocument();
  });

  it("records the relay's OWN code for a refusal, plus what to do about it", async () => {
    // A 200 with ok:false is a completed exchange whose command did not take.
    // Rendering it as "failed" would throw away the entire diagnosis.
    seed();
    captureCommands(
      () =>
        ok({
          ok: false,
          error: {
            code: "COMMAND_UNRESOLVED",
            message: 'entity "…" resolves to no adopted device class',
          },
        }) as Response,
    );
    const user = renderRoute();
    const remote = await screen.findByRole("group", { name: /Remote/ });
    await user.click(within(remote).getByRole("button", { name: "Power on" }));

    const log = await screen.findByRole("table", { name: "Command outcomes" });
    await waitFor(() => expect(within(log).getByText("COMMAND_UNRESOLVED")).toBeInTheDocument());
    expect(within(log).getByText(/resolves to no adopted device class/)).toBeInTheDocument();
    // The diagnosis, not a generic failure: which preconditions to check, and
    // where the relay's own NOT DISPATCHED line actually is.
    expect(within(log).getByText(/before touching the device/)).toBeInTheDocument();
    expect(within(log).getByText(/journalctl -u waiveo-relay/)).toBeInTheDocument();
  });

  it("keeps a whole sequence, newest first, rather than one vanishing toast", async () => {
    seed();
    captureCommands();
    const user = renderRoute();
    const remote = await screen.findByRole("group", { name: /Remote/ });
    await user.click(within(remote).getByRole("button", { name: "Power on" }));
    await user.click(within(remote).getByRole("button", { name: "Home" }));
    await user.click(within(remote).getByRole("button", { name: "Up" }));

    const log = await screen.findByRole("table", { name: "Command outcomes" });
    await waitFor(() => expect(within(log).getAllByText("Applied")).toHaveLength(3));
    const pressed = within(log)
      .getAllByRole("row")
      .slice(1)
      .map((row) => within(row).getAllByRole("cell")[1]!.textContent);
    expect(pressed).toEqual(["Up", "Home", "Power on"]);
  });

  it("separates a failed REQUEST from a refusal the relay had an opinion about", async () => {
    seed();
    server.use(
      http.post(`${TEST_BASE}/entities/${ENTITY_ID}/commands`, () =>
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
    );
    const user = renderRoute();
    const remote = await screen.findByRole("group", { name: /Remote/ });
    await user.click(within(remote).getByRole("button", { name: "Home" }));

    const log = await screen.findByRole("table", { name: "Command outcomes" });
    await waitFor(() => expect(within(log).getByText("Request failed")).toBeInTheDocument());
    expect(within(log).getByText("No relay is connected.")).toBeInTheDocument();
  });

  it("clears the log on request", async () => {
    seed();
    captureCommands();
    const user = renderRoute();
    const remote = await screen.findByRole("group", { name: /Remote/ });
    await user.click(within(remote).getByRole("button", { name: "Home" }));
    const log = await screen.findByRole("table", { name: "Command outcomes" });
    await waitFor(() => expect(within(log).getByText("Applied")).toBeInTheDocument());
    await user.click(screen.getByRole("button", { name: "Clear" }));
    expect(await screen.findByText("Nothing sent yet")).toBeInTheDocument();
  });
});

describe("Roku console — the preconditions are stated before the press", () => {
  it("says it is ready when all three hold", async () => {
    seed();
    renderRoute();
    const panel = await screen.findByRole("region", { name: "Control readiness" });
    await waitFor(() => expect(within(panel).getByText("Ready")).toBeInTheDocument());
    expect(document.querySelector("[data-slot='readiness-problem']")).toBeNull();
  });

  it("names an unadopted device as the reason, before a button is pressed", async () => {
    seed({ devices: [device({ adopted: false })], records: [] });
    renderRoute();
    const panel = await screen.findByRole("region", { name: "Control readiness" });
    const problem = await within(panel).findByText("Not adopted");
    expect(problem.closest("[data-slot='readiness-problem']")).toHaveAttribute(
      "data-code",
      "not-adopted",
    );
  });

  it("names a disabled entity distinctly from an unadopted device", async () => {
    const disabled = record();
    disabled.entities[0]!.enabled = false;
    seed({ records: [disabled] });
    renderRoute();
    const panel = await screen.findByRole("region", { name: "Control readiness" });
    await within(panel).findByText("Disabled on the adoption record");
    expect(within(panel).queryByText("Not adopted")).toBeNull();
  });

  it("names a missing address on its own", async () => {
    const noAddress = device();
    delete noAddress.address;
    seed({ devices: [noAddress] });
    renderRoute();
    const panel = await screen.findByRole("region", { name: "Control readiness" });
    await within(panel).findByText("No address was reported");
  });

  it("does NOT disable the controls when a precondition fails", async () => {
    // A relay-side ECP override can pin an address this console cannot see. A
    // page that refused to let an operator try would be wrong exactly when it
    // mattered — predict, then report what actually happened.
    const noAddress = device();
    delete noAddress.address;
    seed({ devices: [noAddress] });
    const sent = captureCommands();
    const user = renderRoute();
    const remote = await screen.findByRole("group", { name: /Remote/ });
    const home = within(remote).getByRole("button", { name: "Home" });
    expect(home).toBeEnabled();
    await user.click(home);
    await waitFor(() => expect(sent).toEqual([{ command: "home" }]));
  });
});

describe("Roku console — when a read fails", () => {
  it("still offers the remote when only the adoption records are unreadable", async () => {
    // Policy is not the ability to drive. A page that went blank because it
    // could not read policy would deny an operator the remote over a detail.
    server.use(
      http.get(`${TEST_BASE}/devices`, () => page([device()])),
      http.get(`${TEST_BASE}/entities`, () => page([entity()])),
      http.get(`${TEST_BASE}/adopted-devices`, () =>
        HttpResponse.json(
          {
            type: "about:blank",
            title: "Forbidden",
            status: 403,
            code: "FORBIDDEN",
            detail: "Out of scope.",
            trace_id: TRACE_ID,
          },
          { status: 403, headers: { "Content-Type": "application/problem+json", "Trace-Id": TRACE_ID } },
        ),
      ),
    );
    renderRoute();
    expect(await screen.findByRole("group", { name: /Remote/ })).toBeInTheDocument();
    expect(await screen.findByText(/Adoption records could not be read/)).toBeInTheDocument();
    // …and the readiness panel says its checks are incomplete rather than
    // reporting a healthy device as broken without explanation.
    expect(await screen.findByText(/readiness checks below are incomplete/)).toBeInTheDocument();
  });

  it("surfaces a failed device-plane read as a Problem, not an empty page", async () => {
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
    );
    renderRoute();
    expect(await screen.findByRole("alert")).toHaveTextContent(/No relay is connected/);
  });
});
