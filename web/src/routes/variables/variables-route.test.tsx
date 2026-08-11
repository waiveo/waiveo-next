import { describe, it, expect, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { ThemeProvider } from "@/components/theme/theme-provider";
import VariablesRoute from "./variables-route";
import variablesPageDoc from "./page.uis.json";
import { validatePage } from "@/renderer/validate";
import { TRACE_ID, ULID_A, ULID_B, ULID_C, ULID_ROOT, scopeNode, ok } from "@/api/test-support";

// variables-route.test.tsx DRIVES the page — clicks and types — rather than
// asserting it rendered. Every behaviour below goes through a real userEvent
// gesture against the real PageRenderer, because a page that renders and does
// nothing is the exact defect the variables track exists to remove one layer
// down.

function renderVariables() {
  return render(
    <ThemeProvider>
      <VariablesRoute />
    </ThemeProvider>,
  );
}

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => {
  server.resetHandlers();
  window.localStorage.clear();
});
afterAll(() => server.close());

function page(items: unknown[]) {
  return HttpResponse.json({ items, cursor: null }, { headers: { "Trace-Id": TRACE_ID } });
}

/** One variable row in the api/1 shape the route consumes. */
function variable(over: {
  id?: string;
  name?: string;
  value?: unknown;
  scope_node?: string;
  revision?: number;
}) {
  return {
    id: over.id ?? ULID_A,
    name: over.name ?? "guest_mode",
    value: over.value ?? false,
    scope_node: over.scope_node ?? ULID_ROOT,
    labels: {},
    revision: over.revision ?? 1,
    created_at: 1_700_000_000_000,
    updated_at: 1_700_000_000_000,
  };
}

const nodes = [scopeNode({ id: ULID_ROOT, name: "Demo Site" })];

function variableRow(name: string): HTMLElement {
  const table = screen.getByRole("table", { name: "Variables" });
  return within(table).getByText(name).closest("tr") as HTMLElement;
}

describe("Variables — the ui-schema document", () => {
  it("its page.uis.json passes validatePage (the same gate an extension page clears)", () => {
    const result = validatePage(variablesPageDoc);
    expect(result.ok).toBe(true);
    expect((variablesPageDoc as { pageType: string }).pageType).toBe("list-detail");
  });

  it("lists the stored variables with their value and type", async () => {
    server.use(
      http.get("*/api/v1/variables", () =>
        page([
          variable({ id: ULID_A, name: "guest_mode", value: true }),
          variable({ id: ULID_B, name: "max_volume", value: 42 }),
        ]),
      ),
      http.get("*/api/v1/scope-nodes", () => page(nodes)),
    );
    renderVariables();

    const table = await screen.findByRole("table", { name: "Variables" });
    expect(within(table).getByText("guest_mode")).toBeInTheDocument();
    expect(within(table).getByText("max_volume")).toBeInTheDocument();
    // The VALUE is shown, not merely the name — an operator's whole reason for
    // opening this page is to see what a variable currently holds.
    expect(within(table).getByText("true")).toBeInTheDocument();
    expect(within(table).getByText("42")).toBeInTheDocument();
    // …and the placement reads as a NAME, not a ULID.
    expect(within(table).getAllByText("Demo Site").length).toBeGreaterThan(0);
  });
});

describe("Variables — create / edit / delete, driven", () => {
  it("creates a variable carrying an Idempotency-Key and shows the fresh row", async () => {
    const state = { rows: [variable({ id: ULID_A, name: "guest_mode", value: false })] };
    let idempotencyKey: string | null = null;
    let postedName: unknown = "unset";
    server.use(
      http.get("*/api/v1/variables", () => page(state.rows)),
      http.get("*/api/v1/scope-nodes", () => page(nodes)),
      http.post("*/api/v1/variables", async ({ request }) => {
        idempotencyKey = request.headers.get("Idempotency-Key");
        const body = (await request.json()) as { name?: unknown; value?: unknown };
        postedName = body.name;
        const created = variable({ id: ULID_B, name: String(body.name ?? ""), value: body.value });
        state.rows = [...state.rows, created];
        return ok(created, { status: 201, revision: 1 });
      }),
    );

    const user = userEvent.setup();
    renderVariables();
    await screen.findByRole("table", { name: "Variables" });

    await user.click(screen.getByRole("button", { name: "New" }));
    await user.click(await screen.findByRole("button", { name: "Save changes" }));

    await waitFor(() =>
      expect(
        within(screen.getByRole("table", { name: "Variables" })).getByText("new_variable"),
      ).toBeInTheDocument(),
    );
    expect(idempotencyKey).toMatch(/^[0-9a-f-]{36}$/i);
    expect(postedName).toBe("new_variable");
  });

  it("edits a variable's VALUE under its If-Match and persists it", async () => {
    const state = { rows: [variable({ id: ULID_A, name: "greeting", value: "hello", revision: 3 })] };
    let ifMatch: string | null = null;
    let patched: unknown = "unset";
    server.use(
      http.get("*/api/v1/variables", () => page(state.rows)),
      http.get("*/api/v1/scope-nodes", () => page(nodes)),
      http.patch("*/api/v1/variables/:id", async ({ request }) => {
        ifMatch = request.headers.get("If-Match");
        const body = (await request.json()) as { value?: unknown };
        patched = body.value;
        const updated = variable({ id: ULID_A, name: "greeting", value: body.value, revision: 4 });
        state.rows = [updated];
        return ok(updated, { revision: 4 });
      }),
    );

    const user = userEvent.setup();
    renderVariables();
    await screen.findByRole("table", { name: "Variables" });

    await user.click(variableRow("greeting"));
    const valueInput = await screen.findByLabelText("Value");
    await user.clear(valueInput);
    await user.type(valueInput, "goodbye");
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() =>
      expect(
        within(screen.getByRole("table", { name: "Variables" })).getByText("goodbye"),
      ).toBeInTheDocument(),
    );
    // The edit carried the If-Match derived from the record's revision (API-020).
    expect(ifMatch).toBe('"3"');
    // …and it sent a STRING, not a stringified anything-else.
    expect(patched).toBe("goodbye");
  });

  it("changing the type swaps the input and sends the new scalar TYPE, not a string", async () => {
    const state = { rows: [variable({ id: ULID_A, name: "max_volume", value: "7", revision: 1 })] };
    let patched: unknown = "unset";
    server.use(
      http.get("*/api/v1/variables", () => page(state.rows)),
      http.get("*/api/v1/scope-nodes", () => page(nodes)),
      http.patch("*/api/v1/variables/:id", async ({ request }) => {
        const body = (await request.json()) as { value?: unknown };
        patched = body.value;
        const updated = variable({ id: ULID_A, name: "max_volume", value: body.value, revision: 2 });
        state.rows = [updated];
        return ok(updated, { revision: 2 });
      }),
    );

    const user = userEvent.setup();
    renderVariables();
    await screen.findByRole("table", { name: "Variables" });

    await user.click(variableRow("max_volume"));
    // It starts as text…
    expect(await screen.findByLabelText("Value")).toHaveAttribute("type", "text");

    // …switch it to Number, and the switch widget swaps the input.
    await user.selectOptions(await screen.findByLabelText("Type"), "number");
    const numberInput = await screen.findByLabelText("Value");
    expect(numberInput).toHaveAttribute("type", "number");

    await user.clear(numberInput);
    await user.type(numberInput, "11");
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    // DAT-132 admits a NUMBER; sending "11" as a string would be a different
    // value that a `numeric`-style comparison could not read.
    await waitFor(() => expect(patched).toBe(11));
  });

  it("switching to True/false sends a BOOLEAN", async () => {
    const state = { rows: [variable({ id: ULID_A, name: "guest_mode", value: "", revision: 1 })] };
    let patched: unknown = "unset";
    server.use(
      http.get("*/api/v1/variables", () => page(state.rows)),
      http.get("*/api/v1/scope-nodes", () => page(nodes)),
      http.patch("*/api/v1/variables/:id", async ({ request }) => {
        const body = (await request.json()) as { value?: unknown };
        patched = body.value;
        const updated = variable({ id: ULID_A, name: "guest_mode", value: body.value, revision: 2 });
        state.rows = [updated];
        return ok(updated, { revision: 2 });
      }),
    );

    const user = userEvent.setup();
    renderVariables();
    await screen.findByRole("table", { name: "Variables" });

    await user.click(variableRow("guest_mode"));
    await user.selectOptions(await screen.findByLabelText("Type"), "boolean");
    await user.click(await screen.findByRole("switch", { name: "Value" }));
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() => expect(patched).toBe(true));
  });

  it("deletes a variable under its If-Match and drops it from the list", async () => {
    const state = { rows: [variable({ id: ULID_A, name: "guest_mode", revision: 5 })] };
    let ifMatch: string | null = null;
    server.use(
      http.get("*/api/v1/variables", () => page(state.rows)),
      http.get("*/api/v1/scope-nodes", () => page(nodes)),
      http.delete("*/api/v1/variables/:id", ({ request }) => {
        ifMatch = request.headers.get("If-Match");
        state.rows = [];
        return new HttpResponse(null, { status: 204, headers: { "Trace-Id": TRACE_ID } });
      }),
    );

    const user = userEvent.setup();
    renderVariables();
    await screen.findByRole("table", { name: "Variables" });

    await user.click(variableRow("guest_mode"));
    await user.click(await screen.findByRole("button", { name: "Delete variable" }));

    await waitFor(() => expect(screen.queryByText("guest_mode")).not.toBeInTheDocument());
    expect(ifMatch).toBe('"5"');
  });
});

describe("Variables — refusals reach the operator", () => {
  it("a 422 VARIABLE_NAME_DUPLICATE lands on the name field, and nothing is claimed to have saved", async () => {
    const state = { rows: [variable({ id: ULID_A, name: "guest_mode", revision: 1 })] };
    server.use(
      http.get("*/api/v1/variables", () => page(state.rows)),
      http.get("*/api/v1/scope-nodes", () => page(nodes)),
      http.patch("*/api/v1/variables/:id", () =>
        HttpResponse.json(
          {
            type: "about:blank",
            title: "Validation Failed",
            status: 422,
            code: "VALIDATION_FAILED",
            trace_id: TRACE_ID,
            detail: "One or more fields failed validation.",
            errors: [
              {
                field: "name",
                code: "VARIABLE_NAME_DUPLICATE",
                message: "another variable named \"taken\" already exists at this scope node",
              },
            ],
          },
          { status: 422, headers: { "Trace-Id": TRACE_ID } },
        ),
      ),
    );

    const user = userEvent.setup();
    renderVariables();
    await screen.findByRole("table", { name: "Variables" });

    await user.click(variableRow("guest_mode"));
    const nameInput = await screen.findByLabelText("Name");
    await user.clear(nameInput);
    await user.type(nameInput, "taken");
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    // The refusal is ON THE FIELD, not only in a toast that disappears.
    await waitFor(() =>
      expect(
        screen.getByText(/another variable named "taken" already exists at this scope node/),
      ).toBeInTheDocument(),
    );
    // …and the form stayed open on the operator's own text so they can fix it,
    // rather than being reset to the stored value.
    expect(screen.getByLabelText("Name")).toHaveValue("taken");
  });

  it("clearing a number value refuses locally rather than sending null (DAT-133)", async () => {
    const state = { rows: [variable({ id: ULID_A, name: "max_volume", value: 7, revision: 1 })] };
    let patchCalls = 0;
    server.use(
      http.get("*/api/v1/variables", () => page(state.rows)),
      http.get("*/api/v1/scope-nodes", () => page(nodes)),
      http.patch("*/api/v1/variables/:id", () => {
        patchCalls += 1;
        return ok(variable({ id: ULID_A, revision: 2 }), { revision: 2 });
      }),
    );

    const user = userEvent.setup();
    renderVariables();
    await screen.findByRole("table", { name: "Variables" });

    await user.click(variableRow("max_volume"));
    const valueInput = await screen.findByLabelText("Value");
    await user.clear(valueInput); // a cleared number-input binds null (UIS-072)
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    // null is NOT a settable value (DAT-133): the request is never sent, and the
    // operator is told why on the field itself.
    await waitFor(() =>
      expect(screen.getByText(/a variable cannot be left unset/i)).toBeInTheDocument(),
    );
    expect(patchCalls).toBe(0);
  });
});

describe("Variables — the secrets prohibition is on the page", () => {
  // DAT-138 is a MUST that only an operator can violate, so the only place it
  // can be enforced is where they are standing when they would.
  it("warns against storing credentials, where an operator would type one", async () => {
    server.use(
      http.get("*/api/v1/variables", () => page([])),
      http.get("*/api/v1/scope-nodes", () => page(nodes)),
    );
    renderVariables();
    expect(
      await screen.findByText(/Don't put passwords, API keys or tokens here/i),
    ).toBeInTheDocument();
  });
});

describe("Variables — a create carries what the operator actually chose (HV-17)", () => {
  // The regression these pin SHIPPED. `create`'s handler built its body from the
  // page's `itemDefault`, which UIS-108/109 requires to be a literal-only seed —
  // so it is what the draft STARTED as, never what the operator made it. Every
  // edit was discarded silently: choosing Demo Screen / Number / 42 stored an
  // empty string at the first scope node.
  //
  // The existing create test could not catch it, and it is worth naming why: it
  // clicks New then Save immediately, so the defaults ARE the correct answer for
  // that case. Only a create with a NON-DEFAULT selection can see the bug.
  const twoNodes = [
    scopeNode({ id: ULID_ROOT, name: "Demo Site" }),
    scopeNode({ id: ULID_C, name: "Demo Screen" }),
  ];

  async function createWith(fill: (user: ReturnType<typeof userEvent.setup>) => Promise<void>) {
    const posted: { body?: Record<string, unknown> } = {};
    server.use(
      http.get("*/api/v1/variables", () => page([])),
      http.get("*/api/v1/scope-nodes", () => page(twoNodes)),
      http.post("*/api/v1/variables", async ({ request }) => {
        const body = (await request.json()) as Record<string, unknown>;
        posted.body = body;
        return ok(variable({ id: ULID_B, name: String(body.name ?? ""), value: body.value }), {
          status: 201,
          revision: 1,
        });
      }),
    );
    const user = userEvent.setup();
    renderVariables();
    await screen.findByRole("table", { name: "Variables" });
    await user.click(screen.getByRole("button", { name: "New" }));
    await screen.findByRole("button", { name: "Save changes" });
    await fill(user);
    await user.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(posted.body).toBeDefined());
    return posted.body as Record<string, unknown>;
  }

  it("posts the CHOSEN scope node, not the first one in the list", async () => {
    const body = await createWith(async (user) => {
      await user.selectOptions(screen.getByLabelText("Scope node"), "Demo Screen");
    });
    // Not ULID_ROOT. Placing at a non-default node is also the only way to author
    // a DAT-134/135 override — the same name at a node and its ancestor — so this
    // failing made overrides unreachable from the console entirely.
    expect(body.scope_node).toBe(ULID_C);
  });

  it("posts a NUMBER when the operator picked Number, not an empty string", async () => {
    const body = await createWith(async (user) => {
      await user.selectOptions(screen.getByLabelText("Type"), "Number");
      await user.clear(screen.getByLabelText("Value"));
      await user.type(screen.getByLabelText("Value"), "42");
    });
    expect(body.value).toBe(42);
  });

  it("posts a BOOLEAN when the operator picked True/false", async () => {
    const body = await createWith(async (user) => {
      await user.selectOptions(screen.getByLabelText("Type"), "True / false");
      await user.click(screen.getByRole("switch"));
    });
    expect(body.value).toBe(true);
  });

  it("posts the typed NAME, not the seed", async () => {
    const body = await createWith(async (user) => {
      await user.clear(screen.getByLabelText("Name"));
      await user.type(screen.getByLabelText("Name"), "lobby_open");
    });
    expect(body.name).toBe("lobby_open");
  });

  it("refuses a cleared Number on CREATE the same way it does on edit", async () => {
    // valueFromView is shared with submit precisely so the two cannot disagree
    // about what an unset number means; null is not a settable value (DAT-133).
    server.use(
      http.get("*/api/v1/variables", () => page([])),
      http.get("*/api/v1/scope-nodes", () => page(twoNodes)),
      http.post("*/api/v1/variables", () => {
        throw new Error("create must not be attempted with an unsendable value");
      }),
    );
    const user = userEvent.setup();
    renderVariables();
    await screen.findByRole("table", { name: "Variables" });
    await user.click(screen.getByRole("button", { name: "New" }));
    await screen.findByRole("button", { name: "Save changes" });
    await user.selectOptions(screen.getByLabelText("Type"), "Number");
    await user.clear(screen.getByLabelText("Value"));
    await user.click(screen.getByRole("button", { name: "Save changes" }));
    expect(await screen.findByText(/Enter a number/)).toBeInTheDocument();
  });
});
