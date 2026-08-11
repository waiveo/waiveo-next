import { describe, it, expect, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http } from "msw";
import { setupServer } from "msw/node";
import { ThemeProvider } from "@/components/theme/theme-provider";
import { Toaster } from "@/components/kit/toaster";
import { RokuRemote } from "./roku-remote";
import type { CommandDispatch } from "./command-outcome";
import { createApi, type Entity } from "@/api";
import { TEST_BASE, ULID_A, ULID_B, ok, problem } from "@/api/test-support";

// These tests PRESS the remote. Every assertion is about the frame that left the
// browser — which command, which params, which entity — because a remote whose
// buttons render beautifully and send `keypress {key:"up"}` (lowercase, which
// the driver does not know) is a remote that fails silently on a wall-mounted TV
// and passes a render-only test.

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => {
  server.resetHandlers();
  window.localStorage.clear();
});
afterAll(() => server.close());

const ENTITY_ID = ULID_B;

function entity(over: Partial<Entity> = {}): Entity {
  return {
    id: ENTITY_ID,
    external_id: null,
    device_id: ULID_A,
    relay_id: "relay-0f1e2d3c4b5a69788796a5b4c3d2e1f0",
    device_class: "media-player",
    name: "Hanger TV",
    scope_node: ULID_A,
    labels: {},
    state: "on",
    ...over,
  };
}

/** Capture every dispatched command body, in order. */
function captureCommands(respond: () => ReturnType<typeof ok> = () => ok({ ok: true })) {
  const sent: { command: string; params?: Record<string, unknown> }[] = [];
  server.use(
    http.post(`${TEST_BASE}/entities/${ENTITY_ID}/commands`, async ({ request }) => {
      sent.push((await request.json()) as { command: string; params?: Record<string, unknown> });
      return respond();
    }),
  );
  return sent;
}

/** Press one button and wait for its dispatch to land. The panel holds itself
 * to ONE outstanding command (a relay serialises per device), so the next click
 * would hit a disabled button — every press has to be awaited. */
async function press(
  user: ReturnType<typeof userEvent.setup>,
  sent: unknown[],
  label: string,
): Promise<void> {
  const before = sent.length;
  await user.click(screen.getByRole("button", { name: label }));
  await waitFor(() => expect(sent.length).toBe(before + 1));
}

function renderRemote(props: Partial<Parameters<typeof RokuRemote>[0]> = {}) {
  const api = createApi({ baseUrl: TEST_BASE });
  render(
    <ThemeProvider>
      <RokuRemote api={api} entity={entity()} {...props} />
      <Toaster />
    </ThemeProvider>,
  );
  return userEvent.setup();
}

describe("RokuRemote — the pad sends the device class's own commands", () => {
  it("sends keypress with the ECP key name for each D-pad direction and OK", async () => {
    const sent = captureCommands();
    const user = renderRemote();
    for (const label of ["Up", "Down", "Left", "Right", "OK"]) {
      await press(user, sent, label);
    }
    expect(sent).toEqual([
      { command: "keypress", params: { key: "Up" } },
      { command: "keypress", params: { key: "Down" } },
      { command: "keypress", params: { key: "Left" } },
      { command: "keypress", params: { key: "Right" } },
      { command: "keypress", params: { key: "Select" } },
    ]);
  });

  it("sends Back as a keypress and Home as the class's own `home` command", async () => {
    const sent = captureCommands();
    const user = renderRemote();
    await press(user, sent, "Back");
    await press(user, sent, "Home");
    expect(sent[0]).toEqual({ command: "keypress", params: { key: "Back" } });
    // REG-066 declares `home` separately; using it keeps a driver free to
    // implement "go home" as something other than an ECP keypress.
    expect(sent[1]).toEqual({ command: "home" });
  });

  it("sends the transport and volume keys", async () => {
    const sent = captureCommands();
    const user = renderRemote();
    for (const label of [
      "Rewind",
      "Play",
      "Pause",
      "Fast forward",
      "Volume down",
      "Mute",
      "Volume up",
    ]) {
      await press(user, sent, label);
    }
    expect(sent.map((s) => s.params?.["key"])).toEqual([
      "Rev",
      // Play and Pause are separate keys, as the legacy Roku remote had them:
      // Roku's Play toggles on most surfaces but not all, and an operator who
      // means "pause" should be able to say so rather than pressing a toggle
      // and reading the screen to find out what it did.
      "Play",
      "Pause",
      "Fwd",
      "VolumeDown",
      "VolumeMute",
      "VolumeUp",
    ]);
  });

  it("sends Info, the one legacy key this remote was missing", async () => {
    // Legacy's remote had Back / Home / Info as one row. `keypress` takes a
    // driver-defined key and the relay's ECP path builder URL-escapes whatever
    // it is given rather than whitelisting a set, so Info is a real command —
    // not an invented one.
    const sent = captureCommands();
    const user = renderRemote();
    await press(user, sent, "Info");
    expect(sent[0]).toEqual({ command: "keypress", params: { key: "Info" } });
  });

  it("sends power as the class's stated on/off command, never a toggle", async () => {
    const sent = captureCommands();
    const user = renderRemote();
    await press(user, sent, "Power on");
    await press(user, sent, "Power off");
    expect(sent).toEqual([
      { command: "power", params: { state: "on" } },
      { command: "power", params: { state: "off" } },
    ]);
  });
});

describe("RokuRemote — launching a channel", () => {
  it("launches the typed channel id", async () => {
    const sent = captureCommands();
    const user = renderRemote();
    await user.type(screen.getByLabelText("Channel id"), "12");
    await user.click(screen.getByRole("button", { name: "Launch" }));
    await waitFor(() => expect(sent).toHaveLength(1));
    expect(sent[0]).toEqual({ command: "launch", params: { channel: "12" } });
  });

  it("refuses an empty channel rather than sending a launch with no target", async () => {
    const sent = captureCommands();
    const user = renderRemote();
    await user.click(screen.getByRole("button", { name: "Launch" }));
    expect(await screen.findByText(/Enter a channel id/)).toBeInTheDocument();
    expect(sent).toHaveLength(0);
  });

  it("offers one-press launch for an advertised inventory", async () => {
    const sent = captureCommands();
    const user = renderRemote({ apps: [{ channel: "837", name: "YouTube" }] });
    await user.click(screen.getByRole("button", { name: "YouTube" }));
    await waitFor(() => expect(sent).toHaveLength(1));
    expect(sent[0]).toEqual({ command: "launch", params: { channel: "837" } });
  });
});

describe("RokuRemote — keyboard control", () => {
  it("drives the pad from the arrow keys while the panel has focus", async () => {
    const sent = captureCommands();
    const user = renderRemote();
    const panel = screen.getByRole("group", { name: "Remote — Hanger TV" });
    panel.focus();
    await user.keyboard("{ArrowUp}");
    await waitFor(() => expect(sent).toHaveLength(1));
    await user.keyboard("{ArrowRight}");
    await waitFor(() => expect(sent).toHaveLength(2));
    await user.keyboard("{Backspace}");
    await waitFor(() => expect(sent).toHaveLength(3));
    expect(sent).toEqual([
      { command: "keypress", params: { key: "Up" } },
      { command: "keypress", params: { key: "Right" } },
      { command: "keypress", params: { key: "Back" } },
    ]);
  });

  it("leaves the channel field's own keys alone — typing is not a D-pad press", async () => {
    const sent = captureCommands();
    const user = renderRemote();
    const field = screen.getByLabelText("Channel id");
    await user.click(field);
    await user.keyboard("12{ArrowLeft}{Backspace}");
    // Nothing dispatched: ArrowLeft moved the caret and Backspace deleted a
    // character, which is what they must do inside a text field.
    expect(sent).toHaveLength(0);
    expect(field).toHaveValue("2");
  });
});

describe("RokuRemote — reporting what the relay said", () => {
  it("surfaces a refusal's own code and message, and does not claim success", async () => {
    captureCommands(() =>
      ok({
        ok: false,
        error: { code: "COMMAND_TARGET_UNREACHABLE", message: "no response from 192.0.2.40" },
      }),
    );
    const user = renderRemote();
    await user.click(screen.getByRole("button", { name: "Power on" }));
    const status = await screen.findByRole("status");
    await waitFor(() =>
      expect(status).toHaveTextContent(/COMMAND_TARGET_UNREACHABLE: no response from 192\.0\.2\.40/),
    );
    expect(status).not.toHaveTextContent(/sent/);
  });

  it("reports a press that landed", async () => {
    captureCommands();
    const user = renderRemote();
    await user.click(screen.getByRole("button", { name: "Home" }));
    await waitFor(() => expect(screen.getByRole("status")).toHaveTextContent("Home sent."));
  });
});

describe("RokuRemote — reporting each dispatch to a host page", () => {
  // The panel's own live region reports the LAST press. A host page needs the
  // whole sequence, because driving a device is "power on, home, launch, four
  // D-pad presses" and the question afterwards is which of those landed. The
  // callback is what lets a page keep that; without it, a page would have to
  // duplicate the dispatch path and the two would drift.
  it("reports a successful dispatch with what was actually sent", async () => {
    const dispatches: CommandDispatch[] = [];
    const sent = captureCommands();
    const user = renderRemote({ onDispatch: (d) => dispatches.push(d) });
    await press(user, sent, "Up");
    await waitFor(() => expect(dispatches).toHaveLength(1));
    expect(dispatches[0]!.label).toBe("Up");
    expect(dispatches[0]!.command).toBe("keypress");
    expect(dispatches[0]!.params).toEqual({ key: "Up" });
    expect(dispatches[0]!.outcome).toEqual({ kind: "ok" });
  });

  it("reports a REFUSAL with the relay's own code, distinctly from a failed request", async () => {
    const dispatches: CommandDispatch[] = [];
    captureCommands(() =>
      ok({ ok: false, error: { code: "COMMAND_UNRESOLVED", message: "no adopted device class" } }),
    );
    const user = renderRemote({ onDispatch: (d) => dispatches.push(d) });
    await user.click(screen.getByRole("button", { name: "Home" }));
    await waitFor(() => expect(dispatches).toHaveLength(1));
    expect(dispatches[0]!.outcome).toEqual({
      kind: "refused",
      code: "COMMAND_UNRESOLVED",
      message: "no adopted device class",
    });
  });

  it("reports a failed request as `failed`, never as a refusal", async () => {
    // A refusal means the relay was reached and had an opinion. A 503 means it
    // was not. Collapsing them would tell an operator to check adoption when
    // the box is simply unreachable.
    const dispatches: CommandDispatch[] = [];
    server.use(
      http.post(`${TEST_BASE}/entities/${ENTITY_ID}/commands`, () =>
        problem(503, "UNAVAILABLE", "No relay is connected."),
      ),
    );
    const user = renderRemote({ onDispatch: (d) => dispatches.push(d) });
    await user.click(screen.getByRole("button", { name: "Home" }));
    await waitFor(() => expect(dispatches).toHaveLength(1));
    expect(dispatches[0]!.outcome).toEqual({
      kind: "failed",
      detail: "No relay is connected.",
    });
  });

  it("gives every dispatch its own sequence number, so a log can key on it", async () => {
    const dispatches: CommandDispatch[] = [];
    const sent = captureCommands();
    const user = renderRemote({ onDispatch: (d) => dispatches.push(d) });
    await press(user, sent, "Home");
    await press(user, sent, "Home");
    await waitFor(() => expect(dispatches).toHaveLength(2));
    expect(dispatches.map((d) => d.seq)).toEqual([1, 2]);
  });
});
