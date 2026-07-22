import { describe, it, expect, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { ThemeProvider } from "@/components/theme/theme-provider";
import AutomationsRoute from "./automations-route";
import automationsPageDoc from "./page.uis.json";
import { validatePage } from "@/renderer/validate";
import { TRACE_ID, ULID_A, ULID_B, ULID_ROOT, automation, scopeNode, ok, problem } from "@/api/test-support";

// Automations is a DOGFOODED ui-schema/1 list-detail (name, execution mode + state
// badges) rendered through the SAME PageRenderer an extension page takes, plus the
// three sanctioned first-party affordances the grammar genuinely lacks: the raw
// rule-body code editor, the Run button (its disposition via StatusBadge), and the
// enable/disable + create flows where a server 422 compile error surfaces the
// compiler's message on the body field. These tests prove the document conforms,
// the renderer paints it, and every mutation carries the api/1 conventions.

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

/** Both live lists the page loads: the automations, and the scope-nodes a new
 * automation is placed on. A real server filters by selector; the mock branches so
 * the two are distinct. */
function liveLists(automations: unknown[], scopeNodes: unknown[]) {
  return [
    http.get("*/api/v1/automations", () => page(automations)),
    http.get("*/api/v1/scope-nodes", () => page(scopeNodes)),
  ];
}

describe("Automations — the ui-schema dogfood", () => {
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
        [scopeNode({ id: ULID_ROOT, kind: "site", name: "HQ" })],
      ),
    );
    renderRoute();
    const table = await screen.findByRole("table", { name: "Automations" });
    expect(within(table).getByText("Open the doors")).toBeInTheDocument();
    expect(within(table).getByText("Close at dusk")).toBeInTheDocument();
    // The wire-available execution mode is shown per row.
    expect(within(table).getByText("restart")).toBeInTheDocument();
  });

  it("shows the selected automation's mode and enabled-state badges in the detail", async () => {
    server.use(
      ...liveLists(
        [automation({ id: ULID_A, name: "Open the doors", mode: "queued", enabled: false })],
        [scopeNode({ id: ULID_ROOT, kind: "site", name: "HQ" })],
      ),
    );
    const user = userEvent.setup();
    renderRoute();
    await screen.findByRole("table", { name: "Automations" });
    await user.click(screen.getByText("Open the doors").closest("tr") as HTMLElement);
    // The dogfooded detail badges the mode and the (disabled) state.
    const detail = screen.getByRole("region", { name: "Detail" });
    expect(within(detail).getByText("queued")).toBeInTheDocument();
    expect(within(detail).getByText("Disabled")).toBeInTheDocument();
  });
});

describe("Automations — edit / enable / delete over api/1", () => {
  it("edits an automation name under its If-Match", async () => {
    const state = { rows: [automation({ id: ULID_A, name: "Open the doors", revision: 3 })] };
    let ifMatch: string | null = null;
    server.use(
      http.get("*/api/v1/scope-nodes", () => page([scopeNode({ id: ULID_ROOT, kind: "site", name: "HQ" })])),
      http.get("*/api/v1/automations", () => page(state.rows)),
      http.patch("*/api/v1/automations/:id", async ({ request }) => {
        ifMatch = request.headers.get("If-Match");
        const body = (await request.json()) as { name?: string };
        const updated = automation({ id: ULID_A, name: body.name ?? "Open the doors", revision: 4 });
        state.rows = [updated];
        return ok(updated, { revision: 4 });
      }),
    );

    const user = userEvent.setup();
    renderRoute();
    await screen.findByRole("table", { name: "Automations" });
    await user.click(screen.getByText("Open the doors").closest("tr") as HTMLElement);
    const nameInput = await screen.findByLabelText("Name");
    await user.clear(nameInput);
    await user.type(nameInput, "Open at dawn");
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() =>
      expect(within(screen.getByRole("table", { name: "Automations" })).getByText("Open at dawn")).toBeInTheDocument(),
    );
    expect(ifMatch).toBe('"3"');
  });

  it("enable/disable toggles `enabled` under the record's If-Match (never an unconditional overwrite)", async () => {
    const state = { rows: [automation({ id: ULID_A, name: "Open the doors", enabled: true, revision: 5 })] };
    let ifMatch: string | null = null;
    let patchedEnabled: unknown = "unset";
    server.use(
      http.get("*/api/v1/scope-nodes", () => page([scopeNode({ id: ULID_ROOT, kind: "site", name: "HQ" })])),
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
    await screen.findByRole("table", { name: "Automations" });
    await user.click(screen.getByText("Open the doors").closest("tr") as HTMLElement);
    await user.click(await screen.findByRole("button", { name: "Disable" }));

    await waitFor(() => expect(patchedEnabled).toBe(false));
    // The disable carried the If-Match derived from the record's revision (API-020).
    expect(ifMatch).toBe('"5"');
    // The dogfooded state badge now reads Disabled.
    await waitFor(() =>
      expect(within(screen.getByRole("region", { name: "Detail" })).getByText("Disabled")).toBeInTheDocument(),
    );
  });

  // Regression (Fix 1): a 412 on the enable/disable toggle must surface the same
  // standard conflict UX as the name-save and rule-save flows — a persistent
  // role=status review banner alongside the toast, never only a fading toast, and
  // never a silent retry-overwrite (exactly one write attempt).
  it("on a toggle 412, surfaces the persistent review banner (not only a toast) and re-reads — never a silent overwrite", async () => {
    const changed = automation({ id: ULID_A, name: "Open the doors", enabled: true, revision: 9 });
    const state = { rows: [automation({ id: ULID_A, name: "Open the doors", enabled: true, revision: 5 })] };
    let patchCount = 0;
    server.use(
      http.get("*/api/v1/scope-nodes", () => page([scopeNode({ id: ULID_ROOT, kind: "site", name: "HQ" })])),
      http.get("*/api/v1/automations", () => page(state.rows)),
      http.get("*/api/v1/automations/:id", () => ok(changed, { revision: 9 })),
      http.patch("*/api/v1/automations/:id", () => {
        patchCount += 1;
        state.rows = [changed];
        return problem(412, "REVISION_CONFLICT", "The resource was modified concurrently.", {
          current_revision: 9,
        });
      }),
    );

    const user = userEvent.setup();
    renderRoute();
    await screen.findByRole("table", { name: "Automations" });
    await user.click(screen.getByText("Open the doors").closest("tr") as HTMLElement);
    await user.click(await screen.findByRole("button", { name: "Disable" }));

    // A persistent review banner is rendered (role=status), the same as the two
    // sibling mutations — not just a toast that fades.
    const banner = await screen.findByText(/enabled state was changed elsewhere/i);
    expect(banner).toBeInTheDocument();
    expect(banner).toHaveAttribute("role", "status");
    // Exactly one write was attempted; the client re-read for review, never retried.
    expect(patchCount).toBe(1);
  });

  it("deletes an automation under its If-Match", async () => {
    const state = { rows: [automation({ id: ULID_A, name: "Open the doors", revision: 2 })] };
    let ifMatch: string | null = null;
    server.use(
      http.get("*/api/v1/scope-nodes", () => page([scopeNode({ id: ULID_ROOT, kind: "site", name: "HQ" })])),
      http.get("*/api/v1/automations", () => page(state.rows)),
      http.delete("*/api/v1/automations/:id", ({ request }) => {
        ifMatch = request.headers.get("If-Match");
        state.rows = [];
        return new HttpResponse(null, { status: 204, headers: { "Trace-Id": TRACE_ID } });
      }),
    );

    const user = userEvent.setup();
    renderRoute();
    await screen.findByRole("table", { name: "Automations" });
    await user.click(screen.getByText("Open the doors").closest("tr") as HTMLElement);
    await user.click(await screen.findByRole("button", { name: "Delete automation" }));

    await waitFor(() => expect(screen.queryByText("Open the doors")).not.toBeInTheDocument());
    expect(ifMatch).toBe('"2"');
  });

  it("on a 412 re-reads the current state for review — never a silent overwrite", async () => {
    const changed = automation({ id: ULID_A, name: "Changed elsewhere", revision: 9 });
    const state = { rows: [automation({ id: ULID_A, name: "Open the doors", revision: 3 })] };
    let patchCount = 0;
    server.use(
      http.get("*/api/v1/scope-nodes", () => page([scopeNode({ id: ULID_ROOT, kind: "site", name: "HQ" })])),
      http.get("*/api/v1/automations", () => page(state.rows)),
      http.get("*/api/v1/automations/:id", () => ok(changed, { revision: 9 })),
      http.patch("*/api/v1/automations/:id", () => {
        patchCount += 1;
        state.rows = [changed];
        return problem(412, "REVISION_CONFLICT", "The resource was modified concurrently.", {
          current_revision: 9,
        });
      }),
    );

    const user = userEvent.setup();
    renderRoute();
    await screen.findByRole("table", { name: "Automations" });
    await user.click(screen.getByText("Open the doors").closest("tr") as HTMLElement);
    const nameInput = await screen.findByLabelText("Name");
    await user.clear(nameInput);
    await user.type(nameInput, "My rename");
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    const banner = await screen.findByRole("status");
    expect(banner).toHaveTextContent(/changed elsewhere/i);
    expect(screen.queryByText("My rename")).not.toBeInTheDocument();
    expect(patchCount).toBe(1);
  });
});

describe("Automations — Run surfaces its mode-evaluation disposition", () => {
  it("runs the selected automation and shows the disposition via a StatusBadge, carrying an Idempotency-Key", async () => {
    let idempotencyKey: string | null = null;
    server.use(
      ...liveLists(
        [automation({ id: ULID_A, name: "Open the doors" })],
        [scopeNode({ id: ULID_ROOT, kind: "site", name: "HQ" })],
      ),
      http.post("*/api/v1/automations/:id/run", ({ request }) => {
        idempotencyKey = request.headers.get("Idempotency-Key");
        return HttpResponse.json(
          { run_id: ULID_B, disposition: "ran" },
          { headers: { "Trace-Id": TRACE_ID } },
        );
      }),
    );

    const user = userEvent.setup();
    renderRoute();
    await screen.findByRole("table", { name: "Automations" });
    await user.click(screen.getByText("Open the doors").closest("tr") as HTMLElement);
    await user.click(await screen.findByRole("button", { name: "Run now" }));

    // The disposition lands on a status chip.
    const chip = await screen.findByTestId("run-disposition");
    expect(chip).toHaveAttribute("data-status", "ok");
    expect(chip).toHaveTextContent(/ran/i);
    expect(idempotencyKey).toMatch(/^[0-9a-f-]{36}$/i);
  });

  it("surfaces the Problem when a run is refused (app-class / no synthesizable trigger)", async () => {
    server.use(
      ...liveLists(
        [automation({ id: ULID_A, name: "Open the doors" })],
        [scopeNode({ id: ULID_ROOT, kind: "site", name: "HQ" })],
      ),
      http.post("*/api/v1/automations/:id/run", () =>
        problem(400, "VALIDATION_FAILED", "This automation is app-class; a synchronous run is limited to edge automations."),
      ),
    );

    const user = userEvent.setup();
    renderRoute();
    await screen.findByRole("table", { name: "Automations" });
    await user.click(screen.getByText("Open the doors").closest("tr") as HTMLElement);
    await user.click(await screen.findByRole("button", { name: "Run now" }));

    expect(await screen.findByText(/app-class/i)).toBeInTheDocument();
    // No fabricated disposition chip on a refused run.
    expect(screen.queryByTestId("run-disposition")).not.toBeInTheDocument();
  });
});

describe("Automations — the rule-body code editor (grammar-gap)", () => {
  it("saves an edited rule body under its If-Match", async () => {
    const state = { rows: [automation({ id: ULID_A, name: "Open the doors", revision: 4 })] };
    let ifMatch: string | null = null;
    let patchedActions: unknown = "unset";
    server.use(
      http.get("*/api/v1/scope-nodes", () => page([scopeNode({ id: ULID_ROOT, kind: "site", name: "HQ" })])),
      http.get("*/api/v1/automations", () => page(state.rows)),
      http.patch("*/api/v1/automations/:id", async ({ request }) => {
        ifMatch = request.headers.get("If-Match");
        const body = (await request.json()) as { actions?: unknown };
        patchedActions = body.actions;
        const updated = automation({ id: ULID_A, name: "Open the doors", revision: 5 });
        state.rows = [updated];
        return ok(updated, { revision: 5 });
      }),
    );

    const user = userEvent.setup();
    renderRoute();
    await screen.findByRole("table", { name: "Automations" });
    await user.click(screen.getByText("Open the doors").closest("tr") as HTMLElement);

    const editor = await screen.findByLabelText("Rule body (JSON)");
    const nextRule = JSON.stringify({
      mode: "single",
      max: null,
      triggers: [{ type: "state", entity_id: ULID_B, to: ["on"] }],
      conditions: [],
      actions: [{ type: "device_command", entity_id: ULID_B, command: "power_off", params: {} }],
    });
    // Typing raw JSON through userEvent is brittle around braces; paste the whole
    // rule into the mono editor instead.
    await user.clear(editor);
    await user.click(editor);
    await user.paste(nextRule);
    await user.click(screen.getByRole("button", { name: "Save rule" }));

    await waitFor(() => expect(ifMatch).toBe('"4"'));
    expect(patchedActions).toEqual([
      { type: "device_command", entity_id: ULID_B, command: "power_off", params: {} },
    ]);
  });

  it("rejects invalid JSON inline without hitting the server", async () => {
    let patchCount = 0;
    server.use(
      ...liveLists(
        [automation({ id: ULID_A, name: "Open the doors" })],
        [scopeNode({ id: ULID_ROOT, kind: "site", name: "HQ" })],
      ),
      http.patch("*/api/v1/automations/:id", () => {
        patchCount += 1;
        return ok(automation({ id: ULID_A }), { revision: 2 });
      }),
    );

    const user = userEvent.setup();
    renderRoute();
    await screen.findByRole("table", { name: "Automations" });
    await user.click(screen.getByText("Open the doors").closest("tr") as HTMLElement);
    const editor = await screen.findByLabelText("Rule body (JSON)");
    await user.clear(editor);
    await user.click(editor);
    await user.paste("{ not valid json ");
    await user.click(screen.getByRole("button", { name: "Save rule" }));

    expect(await screen.findByText(/isn't valid json/i)).toBeInTheDocument();
    expect(patchCount).toBe(0);
  });

  it("on a rule-save 422, surfaces the compiler's message on the body field", async () => {
    server.use(
      ...liveLists(
        [automation({ id: ULID_A, name: "Open the doors" })],
        [scopeNode({ id: ULID_ROOT, kind: "site", name: "HQ" })],
      ),
      http.patch("*/api/v1/automations/:id", () =>
        problem(422, "VALIDATION_FAILED", "The rule did not compile.", {
          errors: [
            {
              field: "actions[0].type",
              code: "UNKNOWN_VOCABULARY_MEMBER",
              message: '"not_a_real_action" is not a member of the closed action vocabulary',
            },
          ],
        }),
      ),
    );

    const user = userEvent.setup();
    renderRoute();
    await screen.findByRole("table", { name: "Automations" });
    await user.click(screen.getByText("Open the doors").closest("tr") as HTMLElement);
    const editor = await screen.findByLabelText("Rule body (JSON)");
    await user.clear(editor);
    await user.click(editor);
    await user.paste(JSON.stringify({ mode: "single", triggers: [], conditions: [], actions: [{ type: "not_a_real_action" }] }));
    await user.click(screen.getByRole("button", { name: "Save rule" }));

    // The compiler's own message lands on the body field, not only a toast.
    expect(
      await screen.findByText('"not_a_real_action" is not a member of the closed action vocabulary'),
    ).toBeInTheDocument();
    const editorAfter = screen.getByLabelText("Rule body (JSON)");
    expect(editorAfter).toHaveAttribute("aria-invalid", "true");
  });
});

describe("Automations — create via the dogfood newAction", () => {
  // Creation lives in the ui-schema `newAction` verb + a first-party scope picker —
  // the SAME minimal pattern Screens and Schedules use — not a separate first-party
  // create modal that duplicates the ui-schema-expressible Name/scope fields. The
  // "New" affordance the renderer paints from `newAction` POSTs the starter rule
  // placed on the chosen scope, and the operator authors the real rule in the detail
  // pane (where a 422 compile error surfaces on the body field, proven above).
  it("the renderer's New verb creates from the starter template on the chosen scope, carrying an Idempotency-Key", async () => {
    const state = { rows: [] as unknown[] };
    let idempotencyKey: string | null = null;
    let postedBody: Record<string, unknown> = {};
    server.use(
      http.get("*/api/v1/scope-nodes", () => page([scopeNode({ id: ULID_ROOT, kind: "site", name: "HQ" })])),
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

    await waitFor(() =>
      expect(within(screen.getByRole("table", { name: "Automations" })).getByText("New automation")).toBeInTheDocument(),
    );
    // The create carried an Idempotency-Key (the client owns that convention) and
    // placed the starter rule on the chosen scope node — no separate create modal.
    expect(idempotencyKey).toMatch(/^[0-9a-f-]{36}$/i);
    expect(postedBody.scope_node).toBe(ULID_ROOT);
    expect(postedBody.name).toBe("New automation");
    expect(postedBody.mode).toBe("single");
    expect(Array.isArray(postedBody.triggers)).toBe(true);
    expect(Array.isArray(postedBody.actions)).toBe(true);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  // Regression (Fix 2): with an automation selected, the dogfooded detail pane
  // already mounts a "Name" text-input and the "Rule body (JSON)" editor. Creation
  // must NOT open a second first-party modal duplicating those — there was a bug
  // where "New automation" opened a modal with its own Name input + Rule-body
  // editor, putting two elements of each accessible name on the page at once.
  it("opening New with an automation selected does not duplicate the Name/Rule-body affordances (no create modal)", async () => {
    const state = { rows: [automation({ id: ULID_A, name: "Open the doors" })] as unknown[] };
    server.use(
      http.get("*/api/v1/scope-nodes", () => page([scopeNode({ id: ULID_ROOT, kind: "site", name: "HQ" })])),
      http.get("*/api/v1/automations", () => page(state.rows)),
      http.post("*/api/v1/automations", async () => {
        const created = automation({ id: ULID_B, name: "New automation", revision: 1 });
        state.rows = [automation({ id: ULID_A, name: "Open the doors" }), created];
        return ok(created, { status: 201, revision: 1 });
      }),
    );

    const user = userEvent.setup();
    renderRoute();
    await screen.findByRole("table", { name: "Automations" });
    await user.click(screen.getByText("Open the doors").closest("tr") as HTMLElement);
    // Baseline: exactly one Name field and one Rule-body editor (the detail pane).
    expect(screen.getAllByLabelText("Rule body (JSON)")).toHaveLength(1);
    expect(screen.getAllByRole("textbox", { name: "Name" })).toHaveLength(1);

    await user.click(screen.getByRole("button", { name: "New" }));
    await waitFor(() =>
      expect(within(screen.getByRole("table", { name: "Automations" })).getByText("New automation")).toBeInTheDocument(),
    );
    // No second Name/Rule-body surface, and no create dialog — creation is the
    // ui-schema newAction verb, not a duplicating first-party modal.
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(screen.getAllByLabelText("Rule body (JSON)")).toHaveLength(1);
    expect(screen.getAllByRole("textbox", { name: "Name" })).toHaveLength(1);
  });
});

describe("Automations — responsive at 360px", () => {
  it("switches the list to stacked cards (no wide table to overflow the page)", async () => {
    setViewport(true);
    server.use(
      ...liveLists(
        [automation({ id: ULID_A, name: "Open the doors" })],
        [scopeNode({ id: ULID_ROOT, kind: "site", name: "HQ" })],
      ),
    );
    renderRoute();
    await waitFor(() =>
      expect(document.querySelector('[data-slot="data-table"][data-layout="stacked"]')).not.toBeNull(),
    );
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
    expect(screen.getByText("Open the doors")).toBeInTheDocument();
  });
});
