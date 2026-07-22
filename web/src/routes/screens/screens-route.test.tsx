import { describe, it, expect, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { ThemeProvider } from "@/components/theme/theme-provider";
import ScreensRoute from "./screens-route";
import screensPageDoc from "./page.uis.json";
import { validatePage } from "@/renderer/validate";
import { TRACE_ID, ULID_A, ULID_B, ULID_ROOT, scopeNode, ok, problem } from "@/api/test-support";

function renderScreens() {
  return render(
    <ThemeProvider>
      <ScreensRoute />
    </ThemeProvider>,
  );
}

// Screens is a DOGFOODED ui-schema/1 page: its page.uis.json is validated and
// rendered through the SAME PageRenderer path an extension page takes. These
// tests prove the document conforms, that the list/detail renders through the
// renderer, and that the create/edit/delete verbs carry the api/1 conventions
// (Idempotency-Key on create, If-Match on edit/delete, the 412 conflict re-read).

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

describe("Screens — the ui-schema dogfood", () => {
  it("its page.uis.json passes validatePage (the same gate an extension page clears)", () => {
    const result = validatePage(screensPageDoc);
    expect(result.ok).toBe(true);
    expect((screensPageDoc as { pageType: string }).pageType).toBe("list-detail");
  });

  it("renders the live scope-nodes through the renderer as a real table", async () => {
    server.use(
      http.get("*/api/v1/scope-nodes", () =>
        page([
          scopeNode({ id: ULID_A, name: "Lobby display", tz: "America/New_York", revision: 3 }),
          scopeNode({ id: ULID_B, name: "Cafe board", tz: "America/Chicago", revision: 1 }),
        ]),
      ),
    );
    renderScreens();
    // A real DataTable (validate → render), not a mock: the table's accessible
    // name comes from the document's table widget id.
    const table = await screen.findByRole("table", { name: "Screens" });
    expect(within(table).getByText("Lobby display")).toBeInTheDocument();
    expect(within(table).getByText("Cafe board")).toBeInTheDocument();
  });
});

describe("Screens — create / edit / delete over api/1", () => {
  it("creates a screen carrying an Idempotency-Key, then shows the fresh row", async () => {
    const state = { rows: [scopeNode({ id: ULID_A, name: "Lobby display", revision: 1 })] };
    let idempotencyKey: string | null = null;
    server.use(
      http.get("*/api/v1/scope-nodes", () => page(state.rows)),
      http.post("*/api/v1/scope-nodes", async ({ request }) => {
        idempotencyKey = request.headers.get("Idempotency-Key");
        const created = scopeNode({ id: ULID_B, name: "New screen", revision: 1 });
        state.rows = [...state.rows, created];
        return ok(created, { status: 201, revision: 1 });
      }),
    );

    const user = userEvent.setup();
    renderScreens();
    await screen.findByRole("table", { name: "Screens" });

    await user.click(screen.getByRole("button", { name: "New" }));

    await waitFor(() =>
      expect(within(screen.getByRole("table", { name: "Screens" })).getByText("New screen")).toBeInTheDocument(),
    );
    expect(idempotencyKey).toMatch(/^[0-9a-f-]{36}$/i);
  });

  it("edits a screen under its If-Match and persists the change", async () => {
    const state = {
      rows: [scopeNode({ id: ULID_A, name: "Lobby display", tz: "America/New_York", revision: 3 })],
    };
    let ifMatch: string | null = null;
    server.use(
      http.get("*/api/v1/scope-nodes", () => page(state.rows)),
      http.patch("*/api/v1/scope-nodes/:id", async ({ request }) => {
        ifMatch = request.headers.get("If-Match");
        const body = (await request.json()) as { name?: string };
        const updated = scopeNode({ id: ULID_A, name: body.name ?? "Lobby display", revision: 4 });
        state.rows = [updated];
        return ok(updated, { revision: 4 });
      }),
    );

    const user = userEvent.setup();
    renderScreens();
    await screen.findByRole("table", { name: "Screens" });

    // Select the row → the detail form appears.
    const row = screen.getByText("Lobby display").closest("tr");
    await user.click(row as HTMLElement);
    const nameInput = await screen.findByLabelText("Display name");
    await user.clear(nameInput);
    await user.type(nameInput, "Front desk");

    await user.click(screen.getByRole("button", { name: "Save changes" }));

    await waitFor(() =>
      expect(within(screen.getByRole("table", { name: "Screens" })).getByText("Front desk")).toBeInTheDocument(),
    );
    // The edit carried the If-Match derived from the record's revision (API-020),
    // never an unconditional overwrite.
    expect(ifMatch).toBe('"3"');
  });

  it("on a 412 re-reads the current state for review — never a silent overwrite", async () => {
    const changed = scopeNode({ id: ULID_A, name: "Changed elsewhere", revision: 9 });
    const state = {
      rows: [scopeNode({ id: ULID_A, name: "Lobby display", revision: 3 })],
    };
    let patchCount = 0;
    server.use(
      http.get("*/api/v1/scope-nodes", () => page(state.rows)),
      http.get("*/api/v1/scope-nodes/:id", () => ok(changed, { revision: 9 })),
      http.patch("*/api/v1/scope-nodes/:id", () => {
        patchCount += 1;
        // Another writer already advanced it — the concurrent change is now live.
        state.rows = [changed];
        return problem(412, "REVISION_CONFLICT", "The resource was modified concurrently.", {
          current_revision: 9,
        });
      }),
    );

    const user = userEvent.setup();
    renderScreens();
    await screen.findByRole("table", { name: "Screens" });

    const row = screen.getByText("Lobby display").closest("tr");
    await user.click(row as HTMLElement);
    const nameInput = await screen.findByLabelText("Display name");
    await user.clear(nameInput);
    await user.type(nameInput, "My rename");
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    // The review state: the current server value is shown, the user's rename was
    // NOT written, and exactly one write was attempted (no retry-overwrite).
    await waitFor(() =>
      expect(
        within(screen.getByRole("table", { name: "Screens" })).getByText("Changed elsewhere"),
      ).toBeInTheDocument(),
    );
    expect(screen.queryByText("My rename")).not.toBeInTheDocument();
    expect(patchCount).toBe(1);
  });

  it("deletes a screen under its If-Match", async () => {
    const state = { rows: [scopeNode({ id: ULID_A, name: "Lobby display", revision: 2 })] };
    let ifMatch: string | null = null;
    server.use(
      http.get("*/api/v1/scope-nodes", () => page(state.rows)),
      http.delete("*/api/v1/scope-nodes/:id", ({ request }) => {
        ifMatch = request.headers.get("If-Match");
        state.rows = [];
        return new HttpResponse(null, { status: 204, headers: { "Trace-Id": TRACE_ID } });
      }),
    );

    const user = userEvent.setup();
    renderScreens();
    await screen.findByRole("table", { name: "Screens" });
    const row = screen.getByText("Lobby display").closest("tr");
    await user.click(row as HTMLElement);
    await user.click(await screen.findByRole("button", { name: "Delete screen" }));

    await waitFor(() => expect(screen.queryByText("Lobby display")).not.toBeInTheDocument());
    expect(ifMatch).toBe('"2"');
  });
});

describe("Screens — responsive at 360px", () => {
  it("switches the list to stacked cards (no wide table to overflow the page)", async () => {
    setViewport(true);
    server.use(
      http.get("*/api/v1/scope-nodes", () =>
        page([scopeNode({ id: ULID_A, name: "Lobby display", revision: 1, parent_id: ULID_ROOT })]),
      ),
    );
    renderScreens();
    await waitFor(() =>
      expect(document.querySelector('[data-slot="data-table"][data-layout="stacked"]')).not.toBeNull(),
    );
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
    expect(screen.getByText("Lobby display")).toBeInTheDocument();
  });
});
