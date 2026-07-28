import { describe, it, expect, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { ThemeProvider } from "@/components/theme/theme-provider";
import { PairingPanel } from "./pairing-panel";
import { createApi, type Screen, type ScopeNode } from "@/api";
import { TEST_BASE, TRACE_ID, ULID_A, ULID_B, scopeNode, ok } from "@/api/test-support";

// pairing-panel.test.tsx CLICKS the pairing flow through real DOM events over
// msw-intercepted HTTP — the create + issue requests must actually be made
// (with the api/1 conventions the client owns) and the code the server
// returns must actually be shown. A panel that renders but whose "Create &
// get code" does nothing fails here.

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => {
  server.resetHandlers();
  window.localStorage.clear();
});
afterAll(() => server.close());

const SCREEN_ROW_ID = "01J8ZSCREENR0WAAAAAAAAAAA1";
const PAIRING_CODE = "ABCDE-FGHIJ-KLMNO-PQRST-UV234";

function screenRow(over: Partial<Screen> = {}): Screen {
  return {
    id: SCREEN_ROW_ID,
    name: "Lobby TV",
    scope_node: ULID_A,
    device_id: null,
    labels: {},
    revision: 1,
    created_at: 1752537000000,
    updated_at: 1752537000000,
    ...over,
  };
}

function pairingCodeResult(over: Record<string, unknown> = {}) {
  return {
    grant_id: "grant-0123456789abcdef0123456789abcdef",
    screen_id: SCREEN_ROW_ID,
    ttl_seconds: 900,
    redemption_mode: "one-time",
    issued_at: 1752537000000,
    expires_at: 1752537900000,
    pairing_code: PAIRING_CODE,
    relay_id: "relay-1",
    ...over,
  };
}

function page(items: unknown[]) {
  return HttpResponse.json({ items, cursor: null }, { headers: { "Trace-Id": TRACE_ID } });
}

const site: ScopeNode = scopeNode({ id: ULID_A, name: "Demo Site", kind: "site" });
const group: ScopeNode = scopeNode({ id: ULID_B, name: "West Wing", kind: "group" });

function renderPanel(parents: ScopeNode[] = [site, group]) {
  const api = createApi({ baseUrl: TEST_BASE });
  return render(
    <ThemeProvider>
      <PairingPanel
        api={api}
        parents={parents}
        nodeNames={new Map(parents.map((p) => [p.id, p.name]))}
      />
    </ThemeProvider>,
  );
}

describe("PairingPanel — the displays list", () => {
  it("renders the screen identity rows with their placement resolved by name", async () => {
    server.use(http.get(`${TEST_BASE}/screens`, () => page([screenRow()])));
    renderPanel();
    const table = await screen.findByRole("table", { name: "Displays" });
    expect(within(table).getByText("Lobby TV")).toBeInTheDocument();
    expect(within(table).getByText("Demo Site")).toBeInTheDocument();
  });

  it("shows the empty state when no screen rows exist", async () => {
    server.use(http.get(`${TEST_BASE}/screens`, () => page([])));
    renderPanel();
    expect(await screen.findByText("No displays yet")).toBeInTheDocument();
  });
});

describe("PairingPanel — pair a new screen, clicked through", () => {
  it("creates the row under the chosen placement, issues its code, and SHOWS the code", async () => {
    const created: { body?: Record<string, unknown>; idempotencyKey?: string | null } = {};
    let issuedFor: string | null = null;
    let listCalls = 0;
    server.use(
      http.get(`${TEST_BASE}/screens`, () => {
        listCalls += 1;
        return page(listCalls === 1 ? [] : [screenRow({ name: "Cafe Board", scope_node: ULID_B })]);
      }),
      http.post(`${TEST_BASE}/screens`, async ({ request }) => {
        created.body = (await request.json()) as Record<string, unknown>;
        created.idempotencyKey = request.headers.get("Idempotency-Key");
        return ok(screenRow({ name: "Cafe Board", scope_node: ULID_B }), { status: 201, revision: 1 });
      }),
      http.post(`${TEST_BASE}/screens/${SCREEN_ROW_ID}/pairing-code`, ({ request }) => {
        issuedFor = new URL(request.url).pathname;
        return ok(pairingCodeResult(), { status: 201 });
      }),
    );

    renderPanel();
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Pair a new screen" }));

    const dialog = await screen.findByRole("dialog", { name: "Pair a new screen" });
    await user.type(within(dialog).getByLabelText("Screen name"), "Cafe Board");
    await user.selectOptions(within(dialog).getByLabelText("Placed under"), ULID_B);
    await user.click(within(dialog).getByRole("button", { name: "Create & get code" }));

    // The REAL create request carried the typed body and the client's own
    // Idempotency-Key convention.
    await waitFor(() => expect(created.body).toBeDefined());
    expect(created.body).toMatchObject({ name: "Cafe Board", scope_node: ULID_B });
    expect(created.idempotencyKey).toBeTruthy();

    // The issue request targeted the created row, and the CODE the server
    // returned is on screen with its expiry.
    const codeDialog = await screen.findByRole("dialog", { name: /Pairing code — Cafe Board/ });
    expect(issuedFor).toBe(`/api/v1/screens/${SCREEN_ROW_ID}/pairing-code`);
    expect(within(codeDialog).getByTestId("pairing-code")).toHaveTextContent(PAIRING_CODE);
    expect(within(codeDialog).getByText(/expires at/i)).toBeInTheDocument();

    // Close the code dialog (Radix hides the background from the a11y tree
    // while it is open), then the fresh row is in the table after the reload.
    await user.click(within(codeDialog).getByRole("button", { name: "Done" }));
    const table = await screen.findByRole("table", { name: "Displays" });
    await waitFor(() => expect(within(table).getByText("Cafe Board")).toBeInTheDocument());
  });

  it("issues a fresh code for an existing row from its Pairing code button", async () => {
    server.use(
      http.get(`${TEST_BASE}/screens`, () => page([screenRow()])),
      http.post(`${TEST_BASE}/screens/${SCREEN_ROW_ID}/pairing-code`, () =>
        ok(pairingCodeResult(), { status: 201 }),
      ),
    );
    renderPanel();
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Pairing code" }));
    const codeDialog = await screen.findByRole("dialog", { name: /Pairing code — Lobby TV/ });
    expect(within(codeDialog).getByTestId("pairing-code")).toHaveTextContent(PAIRING_CODE);
  });

  it("shows the server's honest reason when no code could be formed", async () => {
    server.use(
      http.get(`${TEST_BASE}/screens`, () => page([screenRow()])),
      http.post(`${TEST_BASE}/screens/${SCREEN_ROW_ID}/pairing-code`, () =>
        ok(
          pairingCodeResult({
            pairing_code: undefined,
            relay_id: undefined,
            code_unavailable_reason: "no relay is connected; the code can be formed once one is",
          }),
          { status: 201 },
        ),
      ),
    );
    renderPanel();
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Pairing code" }));
    const codeDialog = await screen.findByRole("dialog", { name: /Pairing code — Lobby TV/ });
    expect(
      within(codeDialog).getByText(/no relay is connected/),
    ).toBeInTheDocument();
    expect(within(codeDialog).queryByTestId("pairing-code")).not.toBeInTheDocument();
  });
});

describe("PairingPanel — remove", () => {
  it("deletes the row under its If-Match after the confirm click", async () => {
    let deleted: { path: string; ifMatch: string | null } | null = null;
    let listCalls = 0;
    server.use(
      http.get(`${TEST_BASE}/screens`, () => {
        listCalls += 1;
        return page(listCalls === 1 ? [screenRow({ revision: 3 })] : []);
      }),
      http.delete(`${TEST_BASE}/screens/${SCREEN_ROW_ID}`, ({ request }) => {
        deleted = { path: new URL(request.url).pathname, ifMatch: request.headers.get("If-Match") };
        return new HttpResponse(null, { status: 204, headers: { "Trace-Id": TRACE_ID } });
      }),
    );
    renderPanel();
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "Remove" }));
    const confirm = await screen.findByRole("dialog", { name: /Remove Lobby TV\?/ });
    await user.click(within(confirm).getByRole("button", { name: "Remove" }));

    await waitFor(() => expect(deleted).not.toBeNull());
    expect(deleted!.path).toBe(`/api/v1/screens/${SCREEN_ROW_ID}`);
    // The revision-derived If-Match — never an unconditional delete (API-022).
    expect(deleted!.ifMatch).toBe('"3"');
  });
});
