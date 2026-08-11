import { describe, it, expect, beforeAll, afterAll, afterEach, vi } from "vitest";
import { fireEvent, render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { delay, http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { ThemeProvider } from "@/components/theme/theme-provider";
import AutomationsRoute from "./automations-route";
import automationsPageDoc from "./page.uis.json";
import { validatePage } from "@/renderer/validate";
import { TRACE_ID, ULID_A, ULID_B, ULID_C, ULID_ROOT, automation, scopeNode, ok, problem } from "@/api/test-support";

// The Automations route is a RULE BUILDER, not a JSON textarea: `page.uis.json`
// declares the whole trigger/condition/action editor and the shared PageRenderer
// paints it, with this route as the host seam onto the api/1 client.
//
// These tests are written to the rule this codebase learned the hard way — a
// surface that ACCEPTS work it never performs ships green when tests only assert
// rendering. So almost every case here BUILDS a rule by clicking (add a trigger,
// pick its kind, pick an entity, set a time, nest a condition group, choose a
// command and fill its parameter) and then asserts the EXACT JSON the PATCH
// carried. If a control were bound to a path that cannot be written, the assertion
// on the saved body fails — the click alone would not.

// An explicit budget rather than the 5s default. The cases below drive fifteen-odd
// real user-event interactions each (every one of which flushes React and awaits a
// timer), and the suite runs its files in a shared worker pool alongside one that
// spawns a real ESLint process — under that contention a case that takes ~0.7s
// alone crosses 5s and times out. That is the same cascade the virtual-device
// flake turned out to be: nothing wrong with the test, a budget sized for an
// uncontended machine. Sized generously; a genuine hang still fails.
vi.setConfig({ testTimeout: 30_000 });

function renderRoute() {
  return render(
    <ThemeProvider>
      <AutomationsRoute />
    </ThemeProvider>,
  );
}

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => {
  server.resetHandlers();
  setViewport(false);
  window.localStorage.clear();
});
afterAll(() => server.close());

function setViewport(narrow: boolean) {
  window.matchMedia = ((query: string) =>
    ({
      matches: /max-width/.test(query) ? narrow : !narrow,
      media: query,
      onchange: null,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
    }) as unknown as MediaQueryList) as unknown as typeof window.matchMedia;
}

function page(items: unknown[]) {
  return HttpResponse.json({ items, cursor: null }, { headers: { "Trace-Id": TRACE_ID } });
}

/** Two entities the picker's `data` OptionSource draws its options from — the
 * relay-owned `/entities` read model, fed into the document as `$context.entities`. */
const LOBBY_TV = {
  id: ULID_B,
  device_id: ULID_C,
  relay_id: "relay-1",
  device_class: "media-player",
  name: "Lobby TV",
  scope_node: ULID_ROOT,
  labels: {},
  state: "off",
};
const BAR_TV = {
  id: ULID_C,
  device_id: ULID_C,
  relay_id: "relay-1",
  device_class: "media-player",
  name: "Bar TV",
  scope_node: ULID_ROOT,
  labels: {},
  state: "on",
};

/** Every live list the page loads: the automations, the scope-nodes a new
 * automation is placed on, and the entities the pickers offer. */
function liveLists(automations: unknown[], scopeNodes: unknown[], entities: unknown[] = [LOBBY_TV, BAR_TV]) {
  return [
    http.get("*/api/v1/automations", () => page(automations)),
    http.get("*/api/v1/scope-nodes", () => page(scopeNodes)),
    http.get("*/api/v1/entities", () => page(entities)),
  ];
}

const HQ = [scopeNode({ id: ULID_ROOT, kind: "site", name: "HQ" })];

/** Set an `<input type="time">`. user-event drives segmented time inputs
 * unreliably under jsdom (typing "06:45:00" lands on 06:59), so the control's own
 * change event is dispatched directly — the same event React binds. */
function setTime(el: HTMLElement, value: string) {
  fireEvent.change(el, { target: { value } });
}

/** The `section` a group heading titles — the scope its own add/remove buttons
 * live in, so a nested group's controls are addressed by the group they belong to
 * rather than by a document-order index. */
function group(name: string): HTMLElement {
  return screen.getByRole("heading", { name }).closest("section") as HTMLElement;
}

/** Select the one automation in the list and wait for the builder to paint. */
async function openBuilder(user: ReturnType<typeof userEvent.setup>, name: string) {
  await screen.findByRole("table", { name: "Automations" });
  await user.click(screen.getByText(name).closest("tr") as HTMLElement);
  return screen.findByRole("region", { name: "Detail" });
}

/** Capture every PATCH this test's server saw, with its If-Match. */
function patchRecorder(next: (body: Record<string, unknown>) => Record<string, unknown>) {
  const seen: { body: Record<string, unknown>; ifMatch: string | null }[] = [];
  const handler = http.patch("*/api/v1/automations/:id", async ({ request }) => {
    const body = (await request.json()) as Record<string, unknown>;
    seen.push({ body, ifMatch: request.headers.get("If-Match") });
    return ok(next(body), { revision: 99 });
  });
  return { seen, handler };
}

describe("Automations — the ui-schema rule-builder document", () => {
  it("its page.uis.json passes validatePage (the same gate an extension page clears)", () => {
    const result = validatePage(automationsPageDoc);
    expect(result.ok).toBe(true);
    expect((automationsPageDoc as { pageType: string }).pageType).toBe("list-detail");
  });

  it("renders the live automations through the renderer as a real table", async () => {
    server.use(
      ...liveLists(
        [
          automation({ id: ULID_A, name: "Open the doors", mode: "single" }),
          automation({ id: ULID_B, name: "Close at dusk", mode: "restart" }),
        ],
        HQ,
      ),
    );
    renderRoute();
    const table = await screen.findByRole("table", { name: "Automations" });
    expect(within(table).getByText("Open the doors")).toBeInTheDocument();
    expect(within(table).getByText("Close at dusk")).toBeInTheDocument();
    expect(within(table).getByText("restart")).toBeInTheDocument();
  });

  it("paints one editor per trigger and per action, each already on its own kind", async () => {
    server.use(
      ...liveLists(
        [
          automation({
            id: ULID_A,
            name: "Open the doors",
            triggers: [
              { type: "state", entity_id: ULID_B, to: ["on"] },
              { type: "time", at: "07:30:00" },
            ],
            actions: [
              { type: "device_command", entity_id: ULID_B, command: "launch", params: { channel: "dev" } },
              { type: "delay", duration_seconds: 5 },
            ],
          }),
        ],
        HQ,
      ),
    );
    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");

    const triggerKinds = screen.getAllByLabelText("Trigger type");
    expect(triggerKinds.map((s) => (s as HTMLSelectElement).value)).toEqual(["state", "time"]);
    // The `switch` painted each trigger's OWN fields: only the time trigger has an
    // "At", only the state trigger has a "To state" group.
    expect(screen.getByLabelText("At")).toHaveValue("07:30:00");
    // The state trigger's `to` is the ARRAY form on the wire, so it gets the
    // exact-membership multi-select (RUL-021) — see the scalar/array case below.
    expect(screen.getAllByRole("group", { name: "To exactly one of" })).toHaveLength(1);

    const actionKinds = screen.getAllByLabelText("Action type");
    expect(actionKinds.map((s) => (s as HTMLSelectElement).value)).toEqual(["device_command", "delay"]);
    expect(screen.getByLabelText("Command")).toHaveValue("launch");
    expect(screen.getByLabelText("Channel")).toHaveValue("dev");
    expect(screen.getByLabelText("Delay (sec)")).toHaveValue(5);
  });

  it("offers the live entities as the entity picker's options, labelled by name", async () => {
    server.use(...liveLists([automation({ id: ULID_A, name: "Open the doors" })], HQ));
    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");

    // The trigger's picker and the action's picker are both fed from /entities.
    const pickers = screen.getAllByLabelText("Entity");
    expect(pickers).toHaveLength(2);
    for (const picker of pickers) {
      expect(within(picker).getByRole("option", { name: "Lobby TV" })).toBeInTheDocument();
      expect(within(picker).getByRole("option", { name: "Bar TV" })).toBeInTheDocument();
    }
    // And each shows the id the record already carries.
    expect(pickers[0]).toHaveValue(ULID_B);
  });

  it("fills the pickers with entities that land AFTER the first paint", async () => {
    // The load is deliberately two round trips: /automations + /scope-nodes
    // together, then /entities on its own so an unreachable relay cannot fail the
    // page. On a real box the second lands well after the builder has painted —
    // the case the other tests here never reach, because msw answers all three
    // inside one tick. A 120ms delay is what a network is.
    //
    // This is the HV-9-shaped defect in miniature: both halves were correct alone.
    // The fetch ran and returned the entity, `page.uis.json` bound the select to
    // `$context.entities`, and the binding resolved against the renderer's
    // MOUNT-TIME seed — so both pickers offered their placeholder and nothing
    // else, for as long as the page was open, with no error anywhere. The exact
    // measured signature on the box was nine selects with option counts
    // 4, 9, 1, 9, 9, 10, 1, 5, 5 — the two 1s being these two pickers.
    server.use(
      http.get("*/api/v1/automations", () => page([automation({ id: ULID_A, name: "Open the doors" })])),
      http.get("*/api/v1/scope-nodes", () => page(HQ)),
      http.get("*/api/v1/entities", async () => {
        await delay(120);
        return page([LOBBY_TV, BAR_TV]);
      }),
    );
    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");

    const named = async (name: string) =>
      waitFor(() => {
        for (const picker of screen.getAllByLabelText("Entity")) {
          expect(within(picker).getByRole("option", { name })).toBeInTheDocument();
        }
      });
    await named("Lobby TV");
    await named("Bar TV");
    // Both pickers, not just the trigger's — the action side is what names the
    // device a rule acts ON, and it was empty too.
    expect(screen.getAllByLabelText("Entity")).toHaveLength(2);
  });

  it("says WHICH empty it is — nothing adopted reads differently from a failed read", async () => {
    // An empty <select> is the same shape for both, and they are different
    // problems: adopt a device, versus go and look at the relay. Rendering them
    // identically is what let a wiring bug pass for an empty fleet.
    server.use(...liveLists([automation({ id: ULID_A, name: "Open the doors" })], HQ, []));
    const { unmount } = renderRoute();
    await screen.findByRole("table", { name: "Automations" });
    expect(await screen.findByTestId("entities-empty")).toHaveTextContent(/No devices adopted yet/);
    expect(screen.queryByTestId("entities-unavailable")).toBeNull();
    unmount();

    server.resetHandlers();
    server.use(
      http.get("*/api/v1/automations", () => page([automation({ id: ULID_A, name: "Open the doors" })])),
      http.get("*/api/v1/scope-nodes", () => page(HQ)),
      http.get("*/api/v1/entities", () => problem(500, "INTERNAL", "No relay is connected.")),
    );
    renderRoute();
    await screen.findByRole("table", { name: "Automations" });
    const failed = await screen.findByTestId("entities-unavailable");
    expect(failed).toHaveTextContent(/Couldn't load the device list/);
    expect(failed).toHaveTextContent(/No relay is connected/);
    expect(screen.queryByTestId("entities-empty")).toBeNull();
  });

  it("refuses no page when /entities is unavailable — the picker degrades, the builder still paints", async () => {
    server.use(
      http.get("*/api/v1/automations", () => page([automation({ id: ULID_A, name: "Open the doors" })])),
      http.get("*/api/v1/scope-nodes", () => page(HQ)),
      // The relay read model is not reachable — a 500 the client surfaces as an
      // ApiError, which this page must absorb rather than fail the whole load on.
      http.get("*/api/v1/entities", () => problem(500, "INTERNAL", "No relay is connected.")),
    );
    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");
    // No options to pick from, but the advanced id field still carries the value.
    expect(within(screen.getAllByLabelText("Entity")[0]).queryByRole("option", { name: "Lobby TV" })).toBeNull();
    expect(screen.getAllByLabelText("Entity ID (advanced)")[0]).toHaveValue(ULID_B);
  });
});

describe("Automations — building a rule by clicking", () => {
  it("adds a trigger, a condition group with a leaf, and an action — and saves the exact rule it built", async () => {
    const state = {
      rows: [
        automation({
          id: ULID_A,
          name: "Open the doors",
          revision: 4,
          triggers: [{ type: "state", entity_id: ULID_B, to: ["on"] }],
          conditions: [],
          actions: [{ type: "device_command", entity_id: ULID_B, command: "home" }],
        }),
      ],
    };
    const rec = patchRecorder((body) => ({ ...state.rows[0], ...body, revision: 5 }));
    server.use(...liveLists(state.rows, HQ), rec.handler);

    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");

    // ── a second trigger: at 06:45 every day ────────────────────────────────
    await user.click(screen.getByRole("button", { name: "Add trigger" }));
    const triggerKinds = screen.getAllByLabelText("Trigger type");
    expect(triggerKinds).toHaveLength(2);
    await user.selectOptions(triggerKinds[1], "time");
    // The switch swapped the second trigger's fields to the `time` arm.
    setTime(screen.getByLabelText("At"), "06:45:00");
    await user.selectOptions(screen.getByLabelText("If a firing is missed"), "skip");

    // ── an ALL-of condition group holding one state leaf ────────────────────
    await user.click(screen.getByRole("button", { name: "Add ALL-of group" }));
    expect(screen.getByRole("heading", { name: "All of" })).toBeInTheDocument();
    await user.click(within(group("All of")).getByRole("button", { name: "Add condition here" }));
    await user.selectOptions(screen.getByLabelText("Condition type"), "state");
    // Entity pickers in document order: the state trigger's, then the condition's,
    // then the device-command action's.
    await user.selectOptions(screen.getAllByLabelText("Entity")[1], ULID_C);
    // A fresh leaf carries no `state`, so the builder offers the scalar (semantic
    // group) form — the friendly default, and what it saves.
    await user.selectOptions(screen.getByLabelText("State is (or its group)"), "playing");

    // ── a second action: wait 30 seconds ────────────────────────────────────
    await user.click(screen.getByRole("button", { name: "Add action" }));
    const actionKinds = screen.getAllByLabelText("Action type");
    expect(actionKinds).toHaveLength(2);
    await user.selectOptions(actionKinds[1], "delay");
    await user.type(screen.getByLabelText("Delay (sec)"), "30");

    await user.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(rec.seen).toHaveLength(1));
    // The EXACT rule the clicks built — every field, and nothing the builder
    // scaffolded to make its nested binds writable.
    expect(rec.seen[0].body).toEqual({
      name: "Open the doors",
      mode: "single",
      max: null,
      triggers: [
        { type: "state", entity_id: ULID_B, to: ["on"] },
        { type: "time", at: "06:45:00", misfire: "skip" },
      ],
      conditions: [{ and: [{ type: "state", entity_id: ULID_C, state: "playing" }] }],
      actions: [
        { type: "device_command", entity_id: ULID_B, command: "home" },
        { type: "delay", duration_seconds: 30 },
      ],
    });
    // Under the record's own revision — never an unconditional overwrite (API-020).
    expect(rec.seen[0].ifMatch).toBe('"4"');
  });

  it("writes a device command's own parameter through its nested params container", async () => {
    // The regression this case exists for: `item.params.channel` cannot be written
    // when the action carries no `params` key — a ui-schema write resolves to a
    // null location and is dropped, so the Channel field would accept typing and
    // save nothing. The action below deliberately has NO params on the wire.
    const state = {
      rows: [
        automation({
          id: ULID_A,
          name: "Open the doors",
          triggers: [{ type: "state", entity_id: ULID_B, to: ["on"] }],
          actions: [{ type: "log", message: "hello" }],
        }),
      ],
    };
    const rec = patchRecorder((body) => ({ ...state.rows[0], ...body, revision: 2 }));
    server.use(...liveLists(state.rows, HQ), rec.handler);

    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");

    // Re-type the existing `log` action as a device command, then fill it in.
    await user.selectOptions(screen.getByLabelText("Action type"), "device_command");
    // [0] is the state trigger's picker; [1] is the action's, just painted.
    await user.selectOptions(screen.getAllByLabelText("Entity")[1], ULID_C);
    await user.selectOptions(screen.getByLabelText("Command"), "launch");
    await user.type(screen.getByLabelText("Channel"), "waiveo");
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(rec.seen).toHaveLength(1));
    expect(rec.seen[0].body.actions).toEqual([
      // `message` is the member the operator did not clear — carried through, as
      // everything the builder does not rewrite is.
      { type: "device_command", message: "hello", entity_id: ULID_C, command: "launch", params: { channel: "waiveo" } },
    ]);
  });

  // rules/1 RUL-021: a SCALAR `to`/`from`/`state` matches through the device
  // class's semantic groups ("on" also matches playing/paused/idle/buffering,
  // REG-063); an ARRAY matches exact literals only. One control cannot serve both,
  // and the failure would be silent: a multi-select painted over a scalar shows
  // nothing selected, and the operator's first click rewrites a group match into a
  // literal one without ever saying so. The builder shows the form the rule holds.
  it("shows a scalar state bound as a scalar, and saves it as one (RUL-021 group form)", async () => {
    const state = {
      rows: [
        automation({
          id: ULID_A,
          name: "Open the doors",
          // The scalar form — the semantic-group match.
          triggers: [{ type: "state", entity_id: ULID_B, to: "on" }],
        }),
      ],
    };
    const rec = patchRecorder((body) => ({ ...state.rows[0], ...body, revision: 2 }));
    server.use(...liveLists(state.rows, HQ), rec.handler);

    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");

    // It is painted as a single select showing the value — NOT an all-unchecked
    // multi-select that hides the bound entirely.
    const scalar = screen.getByLabelText("To state (or its group)");
    expect(scalar).toHaveValue("on");
    expect(screen.queryByRole("group", { name: "To exactly one of" })).toBeNull();

    await user.selectOptions(scalar, "playing");
    await user.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(rec.seen).toHaveLength(1));
    // Still a scalar: editing a group match never silently narrows it to a literal.
    expect(rec.seen[0].body.triggers).toEqual([{ type: "state", entity_id: ULID_B, to: "playing" }]);
  });

  it("shows an array state bound as an exact-membership multi-select, and keeps it an array", async () => {
    const state = {
      rows: [
        automation({
          id: ULID_A,
          name: "Open the doors",
          triggers: [{ type: "state", entity_id: ULID_B, to: ["on", "playing"] }],
        }),
      ],
    };
    const rec = patchRecorder((body) => ({ ...state.rows[0], ...body, revision: 2 }));
    server.use(...liveLists(state.rows, HQ), rec.handler);

    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");

    const exact = screen.getByRole("group", { name: "To exactly one of" });
    expect(within(exact).getByRole("checkbox", { name: "on" })).toBeChecked();
    expect(within(exact).getByRole("checkbox", { name: "playing" })).toBeChecked();
    expect(within(exact).getByRole("checkbox", { name: "off" })).not.toBeChecked();
    expect(screen.queryByLabelText("To state (or its group)")).toBeNull();

    await user.click(within(exact).getByRole("checkbox", { name: "playing" }));
    await user.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(rec.seen).toHaveLength(1));
    expect(rec.seen[0].body.triggers).toEqual([{ type: "state", entity_id: ULID_B, to: ["on"] }]);
  });

  it("removes a trigger and an action through their own remove buttons", async () => {
    const state = {
      rows: [
        automation({
          id: ULID_A,
          name: "Open the doors",
          triggers: [
            { type: "state", entity_id: ULID_B, to: ["on"] },
            { type: "time", at: "07:30:00" },
          ],
          actions: [
            { type: "log", message: "first" },
            { type: "log", message: "second" },
          ],
        }),
      ],
    };
    const rec = patchRecorder((body) => ({ ...state.rows[0], ...body, revision: 2 }));
    server.use(...liveLists(state.rows, HQ), rec.handler);

    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");

    await user.click(screen.getAllByRole("button", { name: "Remove trigger" })[0]);
    await user.click(screen.getAllByRole("button", { name: "Remove action" })[1]);
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(rec.seen).toHaveLength(1));
    expect(rec.seen[0].body.triggers).toEqual([{ type: "time", at: "07:30:00" }]);
    expect(rec.seen[0].body.actions).toEqual([{ type: "log", message: "first" }]);
  });

  it("nests a condition inside an existing group and paints the whole and/or/not tree", async () => {
    const state = {
      rows: [
        automation({
          id: ULID_A,
          name: "Open the doors",
          conditions: [
            {
              and: [
                { type: "state", entity_id: ULID_B, state: ["on"] },
                { or: [{ not: { type: "state", entity_id: ULID_C, state: ["off"] } }] },
              ],
            },
          ],
        }),
      ],
    };
    const rec = patchRecorder((body) => ({ ...state.rows[0], ...body, revision: 2 }));
    server.use(...liveLists(state.rows, HQ), rec.handler);

    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");

    // The self-referential fragment descended the whole tree: one group heading
    // per composition node, and a kind select for each of the two leaves.
    expect(screen.getAllByRole("heading", { name: "All of" })).toHaveLength(1);
    expect(screen.getAllByRole("heading", { name: "Any of" })).toHaveLength(1);
    expect(screen.getAllByRole("heading", { name: "Not" })).toHaveLength(1);
    expect(screen.getAllByLabelText("Condition type")).toHaveLength(2);
    // …and never tripped the recursion ceiling.
    expect(screen.queryByRole("alert")).toBeNull();

    // Add a numeric leaf INSIDE the inner "Any of" group (the second such button
    // in document order belongs to the nested group).
    expect(screen.getAllByRole("button", { name: "Add condition here" })).toHaveLength(2);
    await user.click(within(group("Any of")).getByRole("button", { name: "Add condition here" }));
    const kinds = screen.getAllByLabelText("Condition type");
    expect(kinds).toHaveLength(3);
    await user.selectOptions(kinds[2], "numeric");
    await user.type(screen.getByLabelText("Above"), "40");
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(rec.seen).toHaveLength(1));
    expect(rec.seen[0].body.conditions).toEqual([
      {
        and: [
          { type: "state", entity_id: ULID_B, state: ["on"] },
          { or: [{ not: { type: "state", entity_id: ULID_C, state: ["off"] } }, { type: "numeric", above: 40 }] },
        ],
      },
    ]);
  });

  it("couples mode and max the way the compiler requires (RUL-244)", async () => {
    const state = {
      rows: [automation({ id: ULID_A, name: "Open the doors", mode: "parallel", max: 3 })],
    };
    const rec = patchRecorder((body) => ({ ...state.rows[0], ...body, revision: 2 }));
    server.use(...liveLists(state.rows, HQ), rec.handler);

    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");

    // `max` is visible only under parallel…
    expect(screen.getByLabelText("Max parallel runs")).toHaveValue(3);
    await user.selectOptions(screen.getByLabelText("Mode"), "single");
    // …and gone the moment the mode changes.
    expect(screen.queryByLabelText("Max parallel runs")).toBeNull();
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(rec.seen).toHaveLength(1));
    // The stale 3 is cleared rather than sent — the compiler rejects a max under
    // any non-parallel mode (MODE_MAX_NOT_APPLICABLE).
    expect(rec.seen[0].body.mode).toBe("single");
    expect(rec.seen[0].body.max).toBeNull();
  });

  it("tells the operator plainly that pack_action is not implemented, and offers no field to fill", async () => {
    server.use(...liveLists([automation({ id: ULID_A, name: "Open the doors" })], HQ));
    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");

    await user.selectOptions(screen.getByLabelText("Action type"), "pack_action");
    expect(screen.getByText(/does not implement it yet/i)).toBeInTheDocument();
    // No input is offered for a kind whose save the compiler refuses — a field
    // here would be a surface accepting work it never performs. (The one entity
    // picker still on the page is the state trigger's, not the action's.)
    expect(screen.queryByLabelText("Command")).toBeNull();
    expect(screen.getAllByLabelText("Entity")).toHaveLength(1);
  });
});

describe("Automations — re-typing a condition leaf between its two BOUNDED kinds", () => {
  // The defect this pins: a `time` condition's `after` is the string "18:00:00"
  // and a `sun` condition's is `{event, offset?}`. Switching the kind and filling
  // in the new field used to write through the old scalar — the renderer's
  // clone spread the string, so the PATCH carried
  // {"0":"1","1":"8",…,"event":"sunset"} under a green "Saved changes", and the
  // server's evaluator (which reads `after` as a *sunBoundSpec) then evaluated
  // the condition false forever. Both directions are driven here, and the exact
  // saved JSON is the assertion — a rendering-only check cannot see any of this.
  it("time → sun: writes a real sun bound and drops the time string it replaced", async () => {
    const loaded = automation({
      id: ULID_A,
      name: "Close at dusk",
      revision: 3,
      conditions: [{ type: "time", after: "18:00:00", before: "23:00:00" }],
    });
    const rec = patchRecorder((body) => ({ ...loaded, ...body, revision: 4 }));
    server.use(...liveLists([loaded], HQ), rec.handler);

    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Close at dusk");
    expect(screen.getByLabelText("After")).toHaveValue("18:00:00");

    await user.selectOptions(screen.getByLabelText("Condition type"), "sun");
    await user.selectOptions(screen.getByLabelText("After sun event"), "sunset");
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(rec.seen).toHaveLength(1));
    // Exactly the rule the sun arm was SHOWING: an after bound of {event}, and no
    // `before` — the time string the operator can no longer see is not smuggled
    // through in a shape the sun evaluator cannot read.
    expect(rec.seen[0].body.conditions).toEqual([{ type: "sun", after: { event: "sunset" } }]);
  });

  it("time → sun: keeps the offset field on the same bound", async () => {
    const loaded = automation({
      id: ULID_A,
      name: "Close at dusk",
      conditions: [{ type: "time", after: "18:00:00" }],
    });
    const rec = patchRecorder((body) => ({ ...loaded, ...body, revision: 2 }));
    server.use(...liveLists([loaded], HQ), rec.handler);

    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Close at dusk");
    await user.selectOptions(screen.getByLabelText("Condition type"), "sun");
    await user.selectOptions(screen.getByLabelText("After sun event"), "sunrise");
    await user.type(screen.getByLabelText("After offset (seconds)"), "-900");
    await user.selectOptions(screen.getByLabelText("Before sun event"), "sunset");
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(rec.seen).toHaveLength(1));
    expect(rec.seen[0].body.conditions).toEqual([
      { type: "sun", after: { event: "sunrise", offset: -900 }, before: { event: "sunset" } },
    ]);
  });

  it("sun → time: writes a real time bound and drops the sun object it replaced", async () => {
    const loaded = automation({
      id: ULID_A,
      name: "Close at dusk",
      conditions: [{ type: "sun", after: { event: "sunset" }, before: { event: "sunrise", offset: 600 } }],
    });
    const rec = patchRecorder((body) => ({ ...loaded, ...body, revision: 2 }));
    server.use(...liveLists([loaded], HQ), rec.handler);

    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Close at dusk");
    expect(screen.getByLabelText("After sun event")).toHaveValue("sunset");

    await user.selectOptions(screen.getByLabelText("Condition type"), "time");
    setTime(screen.getByLabelText("Before"), "23:00:00");
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(rec.seen).toHaveLength(1));
    expect(rec.seen[0].body.conditions).toEqual([{ type: "time", before: "23:00:00" }]);
  });
});

describe("Automations — round-trip fidelity", () => {
  it("carries members the builder has no editor for through an edit, byte for byte", async () => {
    // Two shapes the builder deliberately does not model: a `choose` action's
    // branch tree (RUL-180) and an `event` trigger's `match` map (RUL-081), plus a
    // key from no rules/1 version this console knows.
    const choose = {
      type: "choose",
      branches: [
        {
          condition: { type: "state", entity_id: ULID_B, state: ["playing"] },
          actions: [{ type: "log", message: "already playing" }],
        },
      ],
      default: [{ type: "device_command", entity_id: ULID_B, command: "home" }],
    };
    const eventTrigger = { type: "event", event: "pack.menu.updated", match: { section: "Coffee" } };
    const state = {
      rows: [
        automation({
          id: ULID_A,
          name: "Open the doors",
          triggers: [eventTrigger],
          conditions: [],
          actions: [choose, { type: "log", message: "done", from_the_future: { nested: true } }],
        }),
      ],
    };
    const rec = patchRecorder((body) => ({ ...state.rows[0], ...body, revision: 2 }));
    server.use(...liveLists(state.rows, HQ), rec.handler);

    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");

    // The builder is honest about what it cannot paint…
    expect(screen.getByText(/choose action nests conditions and actions/i)).toBeInTheDocument();
    expect(screen.getByLabelText("Event name")).toHaveValue("pack.menu.updated");
    // …and an ordinary edit elsewhere leaves all of it exactly as it was.
    const name = screen.getByLabelText("Name");
    await user.clear(name);
    await user.type(name, "Open at dawn");
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(rec.seen).toHaveLength(1));
    expect(rec.seen[0].body.name).toBe("Open at dawn");
    expect(rec.seen[0].body.triggers).toEqual([eventTrigger]);
    expect(rec.seen[0].body.actions).toEqual([choose, { type: "log", message: "done", from_the_future: { nested: true } }]);
  });

  it("does not invent members: a rule saved without touching it is the rule that was loaded", async () => {
    const loaded = automation({
      id: ULID_A,
      name: "Open the doors",
      triggers: [{ type: "sun", event: "sunset", offset: -600 }],
      conditions: [{ type: "time", after: "18:00:00", before: "23:00:00" }],
      actions: [{ type: "device_command", entity_id: ULID_B, command: "power", params: { state: "on" } }],
    });
    const rec = patchRecorder((body) => ({ ...loaded, ...body, revision: 2 }));
    server.use(...liveLists([loaded], HQ), rec.handler);

    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");
    // The sun / time / power arms all painted their own values.
    expect(screen.getByLabelText("Sun event")).toHaveValue("sunset");
    expect(screen.getByLabelText("Offset (seconds)")).toHaveValue(-600);
    expect(screen.getByLabelText("After")).toHaveValue("18:00:00");
    expect(screen.getByLabelText("Power state")).toHaveValue("on");

    await user.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(rec.seen).toHaveLength(1));
    expect(rec.seen[0].body).toEqual({
      name: loaded.name,
      mode: loaded.mode,
      max: loaded.max,
      triggers: loaded.triggers,
      conditions: loaded.conditions,
      actions: loaded.actions,
    });
  });
});

describe("Automations — the raw-JSON escape hatch", () => {
  it("applies pasted JSON into the builder, and the builder's Save persists it", async () => {
    const state = { rows: [automation({ id: ULID_A, name: "Open the doors", revision: 7 })] };
    const rec = patchRecorder((body) => ({ ...state.rows[0], ...body, revision: 8 }));
    server.use(...liveLists(state.rows, HQ), rec.handler);

    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");

    // The hatch is closed until asked for — the builder is the primary surface.
    expect(screen.queryByLabelText("Rule body (JSON)")).toBeNull();
    await user.click(screen.getByRole("button", { name: "Edit as JSON" }));
    const editor = screen.getByLabelText("Rule body (JSON)");
    expect((editor as HTMLTextAreaElement).value).toContain('"triggers"');

    const pasted = {
      mode: "restart",
      max: null,
      triggers: [{ type: "time_pattern", minutes: "/15" }],
      conditions: [],
      actions: [
        {
          type: "choose",
          branches: [{ condition: { type: "state", entity_id: ULID_B, state: ["off"] }, actions: [{ type: "log", message: "was off" }] }],
        },
      ],
    };
    await user.clear(editor);
    await user.click(editor);
    await user.paste(JSON.stringify(pasted));
    await user.click(screen.getByRole("button", { name: "Apply to builder" }));

    // Applying writes nothing: it re-seeds the BUILDER, which now shows the pasted
    // rule — the cron-pattern trigger's own field, and the honest note for choose.
    expect(rec.seen).toHaveLength(0);
    expect(await screen.findByLabelText("Minutes")).toHaveValue("/15");
    expect(screen.getByLabelText("Mode")).toHaveValue("restart");
    expect(screen.getByText(/choose action nests conditions and actions/i)).toBeInTheDocument();

    // One save path: the builder's. It carries the applied rule verbatim.
    await user.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(rec.seen).toHaveLength(1));
    expect(rec.seen[0].body).toEqual({ name: "Open the doors", ...pasted });
    expect(rec.seen[0].ifMatch).toBe('"7"');
  });

  it("rejects invalid JSON inline, without touching the builder or the server", async () => {
    const rec = patchRecorder((body) => body);
    server.use(...liveLists([automation({ id: ULID_A, name: "Open the doors" })], HQ), rec.handler);

    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");
    await user.click(screen.getByRole("button", { name: "Edit as JSON" }));
    const editor = screen.getByLabelText("Rule body (JSON)");
    await user.clear(editor);
    await user.click(editor);
    await user.paste("{ not valid json ");
    await user.click(screen.getByRole("button", { name: "Apply to builder" }));

    expect(await screen.findByText(/isn't valid json/i)).toBeInTheDocument();
    expect(rec.seen).toHaveLength(0);
    // The builder still shows the record it was loaded with.
    expect(screen.getAllByLabelText("Trigger type")[0]).toHaveValue("state");
  });

  it("applies only the rule keys the pasted object carries, leaving the rest alone", async () => {
    const state = { rows: [automation({ id: ULID_A, name: "Open the doors", mode: "queued" })] };
    const rec = patchRecorder((body) => ({ ...state.rows[0], ...body, revision: 2 }));
    server.use(...liveLists(state.rows, HQ), rec.handler);

    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");
    await user.click(screen.getByRole("button", { name: "Edit as JSON" }));
    const editor = screen.getByLabelText("Rule body (JSON)");
    await user.clear(editor);
    await user.click(editor);
    await user.paste(JSON.stringify({ actions: [{ type: "log", message: "only the actions" }] }));
    await user.click(screen.getByRole("button", { name: "Apply to builder" }));
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(rec.seen).toHaveLength(1));
    expect(rec.seen[0].body.actions).toEqual([{ type: "log", message: "only the actions" }]);
    // The untouched half of the rule survived the apply.
    expect(rec.seen[0].body.mode).toBe("queued");
    expect(rec.seen[0].body.triggers).toEqual(state.rows[0].triggers);
  });
});

describe("Automations — the JSON hatch mirrors the BUILDER, not the last server read", () => {
  it("opens on the unsaved builder edits, and Apply cannot discard them", async () => {
    // The loss path: the textarea was seeded only from server state, so the
    // moment the operator touched the builder the JSON view showed a rule that
    // predated their work — and "Apply to builder" then wrote that stale rule
    // back over the builder. A `delay` action added by hand vanished with no
    // warning, under a green toast.
    const state = {
      rows: [
        automation({
          id: ULID_A,
          name: "Open the doors",
          revision: 3,
          actions: [{ type: "log", message: "original" }],
        }),
      ],
    };
    const rec = patchRecorder((body) => ({ ...state.rows[0], ...body, revision: 4 }));
    server.use(...liveLists(state.rows, HQ), rec.handler);

    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");

    // Build in the builder: a second action, a 30s wait.
    await user.click(screen.getByRole("button", { name: "Add action" }));
    await user.selectOptions(screen.getAllByLabelText("Action type")[1], "delay");
    await user.type(screen.getByLabelText("Delay (sec)"), "30");

    // Drop to JSON — it shows what is in front of the operator, not the record
    // the server last sent.
    await user.click(screen.getByRole("button", { name: "Edit as JSON" }));
    const editor = screen.getByLabelText("Rule body (JSON)") as HTMLTextAreaElement;
    expect(JSON.parse(editor.value).actions).toEqual([
      { type: "log", message: "original" },
      { type: "delay", duration_seconds: 30 },
    ]);

    // …and applying it back is a no-op on the work, not a silent delete of it.
    await user.click(screen.getByRole("button", { name: "Apply to builder" }));
    await user.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(rec.seen).toHaveLength(1));
    expect(rec.seen[0].body.actions).toEqual([
      { type: "log", message: "original" },
      { type: "delay", duration_seconds: 30 },
    ]);
  });

  it("tracks a builder edit made while the hatch is open and untouched", async () => {
    server.use(
      ...liveLists([automation({ id: ULID_A, name: "Open the doors", actions: [{ type: "log", message: "original" }] })], HQ),
    );
    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");
    await user.click(screen.getByRole("button", { name: "Edit as JSON" }));
    const editor = screen.getByLabelText("Rule body (JSON)") as HTMLTextAreaElement;
    expect(editor.value).not.toContain("edited in the builder");

    const message = screen.getByLabelText("Message");
    await user.clear(message);
    await user.type(message, "edited in the builder");
    await waitFor(() => expect((screen.getByLabelText("Rule body (JSON)") as HTMLTextAreaElement).value).toContain("edited in the builder"));
  });

  it("warns before an Apply that would overwrite builder edits made after the JSON was typed", async () => {
    server.use(
      ...liveLists([automation({ id: ULID_A, name: "Open the doors", actions: [{ type: "log", message: "original" }] })], HQ),
    );
    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");
    await user.click(screen.getByRole("button", { name: "Edit as JSON" }));

    // The operator edits the JSON — from here it is theirs, never overwritten…
    const editor = screen.getByLabelText("Rule body (JSON)");
    await user.clear(editor);
    await user.click(editor);
    await user.paste(JSON.stringify({ actions: [{ type: "log", message: "from the json" }] }));
    expect(screen.queryByTestId("json-diverged")).toBeNull();

    // …and then goes back to the builder, so the two surfaces disagree. Apply is
    // still available (it is the operator's call), but it is not silent.
    const message = screen.getByLabelText("Message");
    await user.clear(message);
    await user.type(message, "later builder edit");
    const warning = await screen.findByTestId("json-diverged");
    expect(warning).toHaveTextContent(/will replace those builder edits/i);

    // …and the way back is offered: reload the textarea from the builder.
    await user.click(within(warning).getByRole("button", { name: /reload from the builder/i }));
    expect((screen.getByLabelText("Rule body (JSON)") as HTMLTextAreaElement).value).toContain("later builder edit");
    expect(screen.queryByTestId("json-diverged")).toBeNull();
  });
});

describe("Automations — closing the JSON hatch is a fold, not a discard", () => {
  /** One automation whose single action is a log, the shape both directions below
   * assert through (the builder paints its message as an editable field). */
  function oneLogRule() {
    return liveLists(
      [automation({ id: ULID_A, name: "Open the doors", revision: 3, actions: [{ type: "log", message: "original" }] })],
      HQ,
    );
  }

  it("keeps an unapplied JSON draft across Hide JSON → Edit as JSON", async () => {
    // The loss this closes is the MIRROR of the one the hatch's builder-mirroring
    // fixed: seeding on every open threw away text the operator had typed and not
    // yet applied, with no warning and no undo. "Hide JSON" reads as folding a
    // panel away, and a fold that deletes work is the same silent-discard defect
    // seen from the other surface.
    server.use(...oneLogRule());
    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");

    await user.click(screen.getByRole("button", { name: "Edit as JSON" }));
    const editor = screen.getByLabelText("Rule body (JSON)");
    await user.clear(editor);
    await user.click(editor);
    await user.paste(JSON.stringify({ actions: [{ type: "log", message: "TYPED BY THE OPERATOR" }] }));

    await user.click(screen.getByRole("button", { name: "Hide JSON" }));
    expect(screen.queryByLabelText("Rule body (JSON)")).toBeNull();
    await user.click(screen.getByRole("button", { name: "Edit as JSON" }));

    const reopened = screen.getByLabelText("Rule body (JSON)") as HTMLTextAreaElement;
    expect(reopened.value).toContain("TYPED BY THE OPERATOR");

    // …and the draft that came back is LIVE, not a fossil: applying it still
    // reaches the builder, which is the only thing that makes preserving it worth
    // anything.
    await user.click(screen.getByRole("button", { name: "Apply to builder" }));
    await waitFor(() => expect(screen.getByLabelText("Message")).toHaveValue("TYPED BY THE OPERATOR"));
  });

  it("still re-mirrors the BUILDER on reopen when the hatch was left untouched", async () => {
    // The other direction, which must not be traded away for the one above: an
    // UNTOUCHED hatch is a view, and a view that reopens onto the record the
    // server last sent is how "Apply to builder" destroys work that was never on
    // screen (the original defect). The operator typed nothing here, so they own
    // nothing — the builder does.
    server.use(...oneLogRule());
    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");

    await user.click(screen.getByRole("button", { name: "Edit as JSON" }));
    expect((screen.getByLabelText("Rule body (JSON)") as HTMLTextAreaElement).value).toContain("original");
    await user.click(screen.getByRole("button", { name: "Hide JSON" }));

    // Build while the hatch is folded away.
    const message = screen.getByLabelText("Message");
    await user.clear(message);
    await user.type(message, "edited while the hatch was closed");

    await user.click(screen.getByRole("button", { name: "Edit as JSON" }));
    const reopened = screen.getByLabelText("Rule body (JSON)") as HTMLTextAreaElement;
    expect(JSON.parse(reopened.value).actions).toEqual([
      { type: "log", message: "edited while the hatch was closed" },
    ]);
    // Nothing diverged: both surfaces show the same rule.
    expect(screen.queryByTestId("json-diverged")).toBeNull();
  });

  it("warns on reopen when BOTH surfaces moved while the hatch was closed", async () => {
    // Preserving the draft cannot mean hiding that the builder went somewhere
    // else in the meantime — an Apply would overwrite those edits. The same
    // warning a divergence gets while the hatch is open is owed to one that
    // happened while it was shut.
    server.use(...oneLogRule());
    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");

    await user.click(screen.getByRole("button", { name: "Edit as JSON" }));
    const editor = screen.getByLabelText("Rule body (JSON)");
    await user.clear(editor);
    await user.click(editor);
    await user.paste(JSON.stringify({ actions: [{ type: "log", message: "from the json" }] }));
    await user.click(screen.getByRole("button", { name: "Hide JSON" }));

    const message = screen.getByLabelText("Message");
    await user.clear(message);
    await user.type(message, "later builder edit");

    await user.click(screen.getByRole("button", { name: "Edit as JSON" }));
    expect((screen.getByLabelText("Rule body (JSON)") as HTMLTextAreaElement).value).toContain("from the json");
    expect(await screen.findByTestId("json-diverged")).toHaveTextContent(/will replace those builder edits/i);
  });
});

describe("Automations — save conventions over api/1", () => {
  it("on a 422, surfaces the compiler's message naming the rule member it rejected", async () => {
    server.use(
      ...liveLists([automation({ id: ULID_A, name: "Open the doors" })], HQ),
      http.patch("*/api/v1/automations/:id", () =>
        problem(422, "VALIDATION_FAILED", "The rule did not compile.", {
          errors: [
            {
              field: "actions[0].type",
              code: "UNKNOWN_VOCABULARY_MEMBER",
              message: '"pack_action" is not a member of the closed action vocabulary',
            },
          ],
        }),
      ),
    );
    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");
    await user.selectOptions(screen.getByLabelText("Action type"), "pack_action");
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    const alert = await screen.findByTestId("compile-error");
    expect(alert).toHaveTextContent("actions[0].type");
    expect(alert).toHaveTextContent('"pack_action" is not a member of the closed action vocabulary');
  });

  it("on a 422 naming a field the builder binds, surfaces it on that field", async () => {
    server.use(
      ...liveLists([automation({ id: ULID_A, name: "Open the doors" })], HQ),
      http.patch("*/api/v1/automations/:id", () =>
        problem(422, "VALIDATION_FAILED", "Invalid.", {
          errors: [{ field: "name", code: "VALIDATION_FAILED", message: "name must not be blank" }],
        }),
      ),
    );
    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    expect(await screen.findByText("name must not be blank")).toBeInTheDocument();
    expect(screen.getByLabelText("Name")).toHaveAttribute("aria-invalid", "true");
  });

  it("on a 412, re-reads and shows the review banner — never a silent retry-overwrite", async () => {
    const changed = automation({ id: ULID_A, name: "Changed elsewhere", revision: 9 });
    const state = { rows: [automation({ id: ULID_A, name: "Open the doors", revision: 3 })] };
    let patchCount = 0;
    server.use(
      http.get("*/api/v1/scope-nodes", () => page(HQ)),
      http.get("*/api/v1/entities", () => page([LOBBY_TV, BAR_TV])),
      http.get("*/api/v1/automations", () => page(state.rows)),
      http.get("*/api/v1/automations/:id", () => ok(changed, { revision: 9 })),
      http.patch("*/api/v1/automations/:id", () => {
        patchCount++;
        state.rows = [changed];
        return problem(412, "REVISION_CONFLICT", "The automation changed.");
      }),
    );
    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    expect(await screen.findByRole("status")).toHaveTextContent(/changed elsewhere while you were editing/i);
    // Exactly one write attempt: the conflict is surfaced, never retried over.
    expect(patchCount).toBe(1);
  });

  it("deletes an automation under its If-Match", async () => {
    const state = { rows: [automation({ id: ULID_A, name: "Open the doors", revision: 6 })] };
    let ifMatch: string | null = null;
    server.use(
      ...liveLists(state.rows, HQ),
      http.delete("*/api/v1/automations/:id", ({ request }) => {
        ifMatch = request.headers.get("If-Match");
        state.rows = [];
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");
    await user.click(screen.getByRole("button", { name: "Delete automation" }));
    await waitFor(() => expect(ifMatch).toBe('"6"'));
  });
});

describe("Automations — run / enable / create", () => {
  it("Run now dispatches and badges the run's disposition", async () => {
    server.use(
      ...liveLists([automation({ id: ULID_A, name: "Open the doors" })], HQ),
      http.post("*/api/v1/automations/:id/run", () =>
        HttpResponse.json({ run_id: ULID_C, disposition: "ran" }, { headers: { "Trace-Id": TRACE_ID } }),
      ),
    );
    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");
    await user.click(screen.getByRole("button", { name: "Run now" }));
    const badge = await screen.findByTestId("run-disposition");
    expect(badge).toHaveAttribute("data-status", "ok");
    expect(badge).toHaveTextContent("Ran");
  });

  it("a run whose every target REFUSED is not a success — the chip errors and the refusals are shown", async () => {
    // The defect: `runNow` read only `disposition`. A run where the relay refused
    // every command still answers 200 `ran`, so the console painted a green chip
    // and a "Run ran" toast over an automation that changed nothing.
    server.use(
      ...liveLists([automation({ id: ULID_A, name: "Open the doors" })], HQ),
      http.post("*/api/v1/automations/:id/run", () =>
        HttpResponse.json(
          {
            run_id: ULID_C,
            disposition: "ran",
            dry_run: false,
            commands: [
              { entity_id: ULID_B, command: "launch", ok: false, error: "the relay is offline" },
              { entity_id: ULID_C, command: "power", ok: false, error: "entity is not adopted" },
            ],
            signage: [],
            logs: [],
          },
          { headers: { "Trace-Id": TRACE_ID } },
        ),
      ),
    );
    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");
    await user.click(screen.getByRole("button", { name: "Run now" }));

    const badge = await screen.findByTestId("run-disposition");
    expect(badge).toHaveAttribute("data-status", "error");
    expect(badge).toHaveTextContent("2 of 2 failed");

    const report = screen.getByTestId("run-report");
    const rows = within(report).getAllByTestId("run-target");
    expect(rows).toHaveLength(2);
    expect(rows.every((r) => r.getAttribute("data-ok") === "false")).toBe(true);
    expect(report).toHaveTextContent(`launch → ${ULID_B}`);
    expect(report).toHaveTextContent("the relay is offline");
    expect(report).toHaveTextContent("entity is not adopted");
  });

  it("a PARTIAL run warns, and reports the signage screen that failed alongside the one that took", async () => {
    server.use(
      ...liveLists([automation({ id: ULID_A, name: "Open the doors" })], HQ),
      http.post("*/api/v1/automations/:id/run", () =>
        HttpResponse.json(
          {
            run_id: ULID_C,
            disposition: "ran",
            dry_run: false,
            commands: [{ entity_id: ULID_B, command: "launch", ok: true }],
            signage: [
              {
                action: "play_cast",
                outcome: "partial",
                screens: [
                  { screen_id: ULID_B, ok: true },
                  { screen_id: ULID_C, ok: false, error: "that cast is a template" },
                ],
              },
            ],
            logs: [{ level: "info", message: "doors opened" }],
          },
          { headers: { "Trace-Id": TRACE_ID } },
        ),
      ),
    );
    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");
    await user.click(screen.getByRole("button", { name: "Run now" }));

    const badge = await screen.findByTestId("run-disposition");
    expect(badge).toHaveAttribute("data-status", "warn");
    expect(badge).toHaveTextContent("1 of 3 failed");

    const report = screen.getByTestId("run-report");
    expect(within(report).getAllByTestId("run-target")).toHaveLength(3);
    expect(report).toHaveTextContent(`play_cast → ${ULID_C}`);
    expect(report).toHaveTextContent("that cast is a template");
    // …and the rule's own log line, which is also part of what the run did.
    expect(within(report).getByTestId("run-log")).toHaveTextContent("doors opened");
  });

  it("a run that touched nothing SAYS so rather than implying an effect", async () => {
    server.use(
      ...liveLists([automation({ id: ULID_A, name: "Open the doors" })], HQ),
      http.post("*/api/v1/automations/:id/run", () =>
        HttpResponse.json(
          { run_id: ULID_C, disposition: "skipped", dry_run: false, commands: [], signage: [], logs: [] },
          { headers: { "Trace-Id": TRACE_ID } },
        ),
      ),
    );
    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");
    await user.click(screen.getByRole("button", { name: "Run now" }));

    expect(await screen.findByTestId("run-disposition")).toHaveAttribute("data-status", "off");
    expect(screen.getByTestId("run-report")).toHaveTextContent(/changed nothing/i);
  });

  it("a signage action that matched NO screen is reported, not dropped for having no rows", async () => {
    server.use(
      ...liveLists([automation({ id: ULID_A, name: "Open the doors" })], HQ),
      http.post("*/api/v1/automations/:id/run", () =>
        HttpResponse.json(
          {
            run_id: ULID_C,
            disposition: "ran",
            dry_run: false,
            commands: [],
            signage: [{ action: "show_alert", outcome: "failed", screens: [] }],
            logs: [],
          },
          { headers: { "Trace-Id": TRACE_ID } },
        ),
      ),
    );
    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");
    await user.click(screen.getByRole("button", { name: "Run now" }));

    expect(await screen.findByTestId("run-disposition")).toHaveAttribute("data-status", "error");
    expect(screen.getByTestId("run-report")).toHaveTextContent("show_alert → (no screen matched)");
  });

  it("enable/disable toggles `enabled` under the record's If-Match", async () => {
    const state = { rows: [automation({ id: ULID_A, name: "Open the doors", enabled: true, revision: 5 })] };
    let ifMatch: string | null = null;
    let patchedEnabled: unknown = "unset";
    server.use(
      http.get("*/api/v1/scope-nodes", () => page(HQ)),
      http.get("*/api/v1/entities", () => page([LOBBY_TV, BAR_TV])),
      http.get("*/api/v1/automations", () => page(state.rows)),
      http.patch("*/api/v1/automations/:id", async ({ request }) => {
        ifMatch = request.headers.get("If-Match");
        const body = (await request.json()) as { enabled?: boolean };
        patchedEnabled = body.enabled;
        const updated = automation({ id: ULID_A, name: "Open the doors", enabled: body.enabled ?? true, revision: 6 });
        state.rows = [updated];
        return ok(updated, { revision: 6 });
      }),
    );
    const user = userEvent.setup();
    renderRoute();
    await openBuilder(user, "Open the doors");
    await user.click(await screen.findByRole("button", { name: "Disable" }));

    await waitFor(() => expect(patchedEnabled).toBe(false));
    expect(ifMatch).toBe('"5"');
    await waitFor(() =>
      expect(within(screen.getByRole("region", { name: "Detail" })).getByText("Disabled")).toBeInTheDocument(),
    );
  });

  // A create that the compiler refuses has NO selected record to hang the message
  // on — only the draft form is on screen. The refusal must still be visible, or
  // the operator presses Save on a rule that will never save and is told nothing
  // but a toast that fades.
  it("shows the compiler's refusal for a CREATE, where there is no selection to hang it on", async () => {
    server.use(
      http.get("*/api/v1/scope-nodes", () => page(HQ)),
      http.get("*/api/v1/entities", () => page([LOBBY_TV, BAR_TV])),
      http.get("*/api/v1/automations", () => page([])),
      http.post("*/api/v1/automations", () =>
        problem(422, "VALIDATION_FAILED", "The rule did not compile.", {
          errors: [
            { field: "triggers[0].type", code: "UNKNOWN_VOCABULARY_MEMBER", message: "not a trigger" },
          ],
        }),
      ),
    );
    const user = userEvent.setup();
    renderRoute();
    await screen.findByRole("table", { name: "Automations" });
    await user.click(await screen.findByRole("button", { name: "New" }));
    await user.click(await screen.findByRole("button", { name: "Save changes" }));

    const alert = await screen.findByTestId("compile-error");
    expect(alert).toHaveTextContent("triggers[0].type");
    expect(alert).toHaveTextContent("not a trigger");
  });

  it("New opens a blank builder draft and creates from what was built in it", async () => {
    const state = { rows: [] as unknown[] };
    let idempotencyKey: string | null = null;
    let postedBody: Record<string, unknown> = {};
    server.use(
      http.get("*/api/v1/scope-nodes", () => page(HQ)),
      http.get("*/api/v1/entities", () => page([LOBBY_TV, BAR_TV])),
      http.get("*/api/v1/automations", () => page(state.rows)),
      http.post("*/api/v1/automations", async ({ request }) => {
        idempotencyKey = request.headers.get("Idempotency-Key");
        postedBody = (await request.json()) as Record<string, unknown>;
        const created = automation({ id: ULID_B, name: "New automation", revision: 1 });
        state.rows = [created];
        return ok(created, { status: 201, revision: 1 });
      }),
    );
    const user = userEvent.setup();
    renderRoute();
    await screen.findByRole("table", { name: "Automations" });
    await user.click(await screen.findByRole("button", { name: "New" }));

    // The draft is the SAME builder — author the rule before it is ever created.
    await user.selectOptions(await screen.findByLabelText("Trigger type"), "sun");
    await user.selectOptions(screen.getByLabelText("Sun event"), "sunset");
    await user.selectOptions(screen.getByLabelText("Action type"), "log");
    await user.type(screen.getByLabelText("Message"), "dusk");
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() =>
      expect(within(screen.getByRole("table", { name: "Automations" })).getByText("New automation")).toBeInTheDocument(),
    );
    expect(idempotencyKey).toMatch(/^[0-9a-f-]{36}$/i);
    expect(postedBody.scope_node).toBe(ULID_ROOT);
    expect(postedBody.name).toBe("New automation");
    expect(postedBody.triggers).toEqual([{ type: "sun", event: "sunset" }]);
    expect(postedBody.actions).toEqual([{ type: "log", message: "dusk" }]);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });
});

describe("Automations — responsive at 360px", () => {
  it("switches the list to stacked cards (no wide table to overflow the page)", async () => {
    setViewport(true);
    server.use(...liveLists([automation({ id: ULID_A, name: "Open the doors" })], HQ));
    renderRoute();
    await waitFor(() =>
      expect(document.querySelector('[data-slot="data-table"][data-layout="stacked"]')).not.toBeNull(),
    );
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
    expect(screen.getByText("Open the doors")).toBeInTheDocument();
  });
});

describe("Automations list — whether a rule is even ON", () => {
  // The list showed name, mode, trigger count and action count, and NOT `enabled`
  // — which the Automation schema has carried all along. A disabled rule was
  // indistinguishable from a live one, so "why did this not fire?" meant opening
  // every rule in turn. The detail already had a Disable/Enable button; only the
  // list could not answer the question.
  it("shows Disabled for a rule that is off and Enabled for one that is on", async () => {
    server.use(
      ...liveLists(
        [
          automation({ id: ULID_A, name: "Morning open", enabled: true }),
          automation({ id: ULID_B, name: "Holiday sign", enabled: false }),
        ],
        HQ,
      ),
    );
    renderRoute();

    const table = await screen.findByRole("table", { name: /automations/i });
    const on = within(table).getByText("Morning open").closest("tr") as HTMLElement;
    const off = within(table).getByText("Holiday sign").closest("tr") as HTMLElement;

    expect(within(on).getByText("Enabled")).toBeInTheDocument();
    expect(within(off).getByText("Disabled")).toBeInTheDocument();
    // Not the raw boolean — a cell bound straight to `enabled` renders "true".
    expect(within(off).queryByText("false")).not.toBeInTheDocument();
  });
});

describe("Automations builder — a clock-based trigger says it will not fire (HV-20)", () => {
  // The platform accepts a `time`/`time_pattern`/`sun` trigger, stores it, ships
  // it to the relay and loads it as an edge rule — and it then never fires.
  // Engine.dispatchSchedule refuses to read the wall clock while the clock is
  // untrusted (correct, RUL-370); a relay boots untrusted and never persists the
  // state (REL-131); and clocktrust.Controller.ApplyVerifiedTime — documented as
  // the ONLY path to trusted — has no production caller. Driven on hardware: a
  // time rule authored twice at two future site-local times, generation applies
  // confirmed both times, zero dispatches.
  //
  // Until a verified-time source exists, saying so where the choice is made is
  // the honest minimum. The guard below is what keeps this from turning into a
  // stale warning once it does.
  async function chooseTriggerKind(kind: string) {
    const user = userEvent.setup();
    server.use(...liveLists([automation({ id: ULID_A, name: "Open the doors" })], HQ));
    renderRoute();
    await openBuilder(user, "Open the doors");
    await user.selectOptions(screen.getAllByLabelText("Trigger type")[0], kind);
    return user;
  }

  it.each(["time", "time_pattern", "sun"])("warns for a %s trigger", async (kind) => {
    await chooseTriggerKind(kind);
    expect(await screen.findByText(/will not fire yet/i)).toBeInTheDocument();
  });

  it("does NOT warn for a state trigger, which works", async () => {
    await chooseTriggerKind("state");
    expect(screen.queryByText(/will not fire yet/i)).not.toBeInTheDocument();
  });
});
