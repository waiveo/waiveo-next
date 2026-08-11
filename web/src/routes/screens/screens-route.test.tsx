import { describe, it, expect, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { ThemeProvider } from "@/components/theme/theme-provider";
import ScreensRoute from "./screens-route";
import screensPageDoc from "./page.uis.json";
import { validatePage } from "@/renderer/validate";
import { TRACE_ID, ULID_A, ULID_B, ULID_C, ULID_ROOT, scopeNode, ok, problem } from "@/api/test-support";

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

/** A row in the dogfooded Screens table, addressed by the screen's name.
 *
 * Scoped to that table rather than to the document, because the page's scope
 * tree above it names the same nodes: the tree renders the WHOLE hierarchy
 * (org → site → group → screen) so an operator can build it, while this table
 * renders the screen-kind nodes it manages. Both showing "Lobby display" is
 * correct; a document-wide lookup for it is what would be ambiguous. */
function screenRow(name: string): HTMLElement {
  const table = screen.getByRole("table", { name: "Screens" });
  return within(table).getByText(name).closest("tr") as HTMLElement;
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
  it("creates a screen carrying an Idempotency-Key AND the seeded name, then shows the fresh row", async () => {
    const state = { rows: [scopeNode({ id: ULID_A, name: "Lobby display", revision: 1 })] };
    let idempotencyKey: string | null = null;
    let postedName: unknown = "unset";
    server.use(
      http.get("*/api/v1/scope-nodes", () => page(state.rows)),
      http.post("*/api/v1/scope-nodes", async ({ request }) => {
        idempotencyKey = request.headers.get("Idempotency-Key");
        // Reflect the ACTUAL submitted name — a canned response would let a blank
        // body sail through (the server does not enforce minLength, so a nameless
        // POST would persist a nameless row).
        const body = (await request.json()) as { name?: unknown };
        postedName = body.name;
        const created = scopeNode({ id: ULID_B, name: String(body.name ?? ""), revision: 1 });
        state.rows = [...state.rows, created];
        return ok(created, { status: 201, revision: 1 });
      }),
    );

    const user = userEvent.setup();
    renderScreens();
    await screen.findByRole("table", { name: "Screens" });

    // New opens a create draft (a blank detail form); Save commits it with no typing.
    // The page's newAction seeds a real starter name, so a no-type Save produces an
    // identifiable row — never a silent blank (UIS-021).
    await user.click(screen.getByRole("button", { name: "New" }));
    await user.click(await screen.findByRole("button", { name: "Save changes" }));

    await waitFor(() =>
      expect(within(screen.getByRole("table", { name: "Screens" })).getByText("New screen")).toBeInTheDocument(),
    );
    expect(idempotencyKey).toMatch(/^[0-9a-f-]{36}$/i);
    expect(postedName).toBe("New screen");
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
    const row = screenRow("Lobby display");
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

    const row = screenRow("Lobby display");
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

  it("on a 412, keeps the detail form open on the current values with a distinct review affordance (not the collapsed empty state a successful save leaves)", async () => {
    const changed = scopeNode({ id: ULID_A, name: "Changed elsewhere", revision: 9 });
    const state = { rows: [scopeNode({ id: ULID_A, name: "Lobby display", revision: 3 })] };
    server.use(
      http.get("*/api/v1/scope-nodes", () => page(state.rows)),
      http.get("*/api/v1/scope-nodes/:id", () => ok(changed, { revision: 9 })),
      http.patch("*/api/v1/scope-nodes/:id", () => {
        state.rows = [changed];
        return problem(412, "REVISION_CONFLICT", "The resource was modified concurrently.", {
          current_revision: 9,
        });
      }),
    );

    const user = userEvent.setup();
    renderScreens();
    await screen.findByRole("table", { name: "Screens" });

    const row = screenRow("Lobby display");
    await user.click(row as HTMLElement);
    const nameInput = await screen.findByLabelText("Display name");
    await user.clear(nameInput);
    await user.type(nameInput, "My rename");
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    // A 412 must produce a REVIEW state, not the same collapse a successful save
    // leaves: the detail form stays mounted, now seeded with the CURRENT server
    // values, and a distinct review banner (role=status) tells the operator to
    // reconcile — the empty "select a screen" prompt must NOT be showing.
    const banner = await screen.findByRole("status");
    expect(banner).toHaveTextContent(/changed elsewhere/i);
    const reviewed = (await screen.findByLabelText("Display name")) as HTMLInputElement;
    await waitFor(() => expect(reviewed.value).toBe("Changed elsewhere"));
    expect(screen.queryByText("Select a screen to edit it, or add a new one.")).not.toBeInTheDocument();
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
    const row = screenRow("Lobby display");
    await user.click(row as HTMLElement);
    await user.click(await screen.findByRole("button", { name: "Delete screen" }));

    await waitFor(() => expect(screen.queryByText("Lobby display")).not.toBeInTheDocument());
    expect(ifMatch).toBe('"2"');
  });

  it("on a delete refused 409, tells the operator the server's own reason and keeps the row", async () => {
    const state = { rows: [scopeNode({ id: ULID_A, name: "Lobby display", revision: 2 })] };
    let deleteCount = 0;
    server.use(
      http.get("*/api/v1/scope-nodes", () => page(state.rows)),
      // The refusal is spelled as the wire bytes the Go handler actually sends.
      // That began as a workaround — `SCOPE_NODE_IN_USE` is data-model/1's code
      // and api/1's generated `ErrorCode` union did not enumerate it, so the typed
      // `problem()` helper could not express it. The enum now carries all three
      // scope-node delete refusals, so a cast is no longer needed; the hand-built
      // body stays because a fixture of the real response bytes is worth more here
      // than one assembled from the same types the code under test uses. The delete
      // leaves the row in place, exactly as the server's `delete_executed: false`
      // says.
      http.delete("*/api/v1/scope-nodes/:id", () => {
        deleteCount += 1;
        return HttpResponse.json(
          {
            type: "about:blank",
            title: "Conflict",
            status: 409,
            detail:
              "This scope node is still named as the scope_node of at least one other row (a screen row). " +
              "A row placed at a node is a separate record from the node itself — delete or re-place every such row first.",
            code: "SCOPE_NODE_IN_USE",
            trace_id: TRACE_ID,
          },
          { status: 409, headers: { "Content-Type": "application/problem+json", "Trace-Id": TRACE_ID } },
        );
      }),
    );

    const user = userEvent.setup();
    renderScreens();
    await screen.findByRole("table", { name: "Screens" });
    await user.click(screenRow("Lobby display"));
    await user.click(await screen.findByRole("button", { name: "Delete screen" }));

    // The operator is told WHY, in the server's own sentence — not "409", and not a
    // bare machine code they would have to look up.
    const notice = await screen.findByText(/still named as the scope_node/i);
    const shown = notice.textContent ?? "";
    expect(shown).toContain("(a screen row)");
    expect(shown).not.toMatch(/\b409\b/);

    // And the two nouns stay apart: what is being deleted is a screen PLACEMENT (a
    // screen-kind scope node), what blocks it is a screen ROW (a screen identity
    // row). DAT-004a makes those distinct records with distinct ids, so a message
    // that called both "the screen" would send the operator after the wrong one.
    expect(shown).toMatch(/screen placement/i);
    expect(shown).not.toMatch(/delete the screen\b/i);

    // Nothing was removed, and the refusal was not retried behind the operator's back.
    expect(screenRow("Lobby display")).toBeInTheDocument();
    expect(deleteCount).toBe(1);
  });
});

// A fourth fixture ULID (test-support exports A/B/C/ROOT) for the created row in
// the multi-site case, so it stays distinct from the two sites and the seed screen.
const ULID_D = "01J8Z3K4N5P6Q7R8S9T0V1W2X9";

// Route the scope-nodes list by its selector: the page loads the screens
// (`kind=screen`) for the table AND the candidate parents (`kind in (site,group)`)
// a new screen must attach under. A real server filters by selector; the mock
// branches so the two lists are distinct, as they are in production.
function scopeNodesBySelector(screens: unknown[], parents: unknown[]) {
  return http.get("*/api/v1/scope-nodes", ({ request }) => {
    const sel = new URL(request.url).searchParams.get("selector") ?? "";
    return page(sel.includes("site") ? parents : screens);
  });
}

describe("Screens — a 422 maps its field errors onto the FormField", () => {
  it("on an edit rejected 422 VALIDATION_FAILED, shows the per-field message inline (not only a toast) and keeps the edit", async () => {
    const state = {
      rows: [scopeNode({ id: ULID_A, name: "Lobby display", tz: "America/New_York", revision: 3 })],
    };
    let patchCount = 0;
    server.use(
      scopeNodesBySelector(state.rows, []),
      http.patch("*/api/v1/scope-nodes/:id", () => {
        patchCount += 1;
        return problem(422, "VALIDATION_FAILED", "The screen could not be saved.", {
          errors: [
            { field: "name", code: "ALREADY_EXISTS", message: "A screen with this name already exists." },
          ],
        });
      }),
    );

    const user = userEvent.setup();
    renderScreens();
    await screen.findByRole("table", { name: "Screens" });

    const row = screenRow("Lobby display");
    await user.click(row as HTMLElement);
    const nameInput = await screen.findByLabelText("Display name");
    await user.clear(nameInput);
    await user.type(nameInput, "Cafe board");
    await user.click(screen.getByRole("button", { name: "Save changes" }));

    // The server's per-field message lands on the offending field as a FormField
    // error (role="alert"), the input is flagged aria-invalid, and — because a
    // validation failure is NOT a conflict — the form stays put with the operator's
    // edit intact so they can fix and resubmit. Exactly one write was attempted.
    const inlineError = await screen.findByText("A screen with this name already exists.");
    expect(inlineError).toHaveAttribute("role", "alert");
    const fieldAfter = screen.getByLabelText("Display name") as HTMLInputElement;
    expect(fieldAfter).toHaveAttribute("aria-invalid", "true");
    expect(fieldAfter.value).toBe("Cafe board");
    expect(patchCount).toBe(1);
  });

  it("does not bleed one screen's 422 field error onto a different, untouched screen after switching rows", async () => {
    const state = {
      rows: [
        scopeNode({ id: ULID_A, name: "Lobby display", tz: "America/New_York", revision: 3 }),
        scopeNode({ id: ULID_B, name: "Cafe board", tz: "America/Chicago", revision: 1 }),
      ],
    };
    server.use(
      scopeNodesBySelector(state.rows, []),
      http.patch("*/api/v1/scope-nodes/:id", () =>
        problem(422, "VALIDATION_FAILED", "The screen could not be saved.", {
          errors: [
            { field: "name", code: "ALREADY_EXISTS", message: "A screen with this name already exists." },
          ],
        }),
      ),
    );

    const user = userEvent.setup();
    renderScreens();
    await screen.findByRole("table", { name: "Screens" });

    // Screen A's edit is rejected 422 → its per-field error lands on A's field.
    await user.click(screenRow("Lobby display"));
    const nameA = await screen.findByLabelText("Display name");
    await user.clear(nameA);
    await user.type(nameA, "Renamed lobby");
    await user.click(screen.getByRole("button", { name: "Save changes" }));
    expect(await screen.findByText("A screen with this name already exists.")).toBeInTheDocument();

    // Switch to a DIFFERENT, untouched screen B (never submitted) — its
    // identically-named field must NOT inherit A's stale error; the error is keyed
    // by bind-path, so without a clear on selection change it would bleed across.
    await user.click(screenRow("Cafe board"));
    const tzB = (await screen.findByLabelText("Time zone")) as HTMLInputElement;
    await waitFor(() => expect(tzB.value).toBe("America/Chicago")); // we are on B
    const nameB = screen.getByLabelText("Display name") as HTMLInputElement;
    expect(screen.queryByText("A screen with this name already exists.")).not.toBeInTheDocument();
    expect(nameB).not.toHaveAttribute("aria-invalid", "true");
  });
});

describe("Screens — a new screen is placed under a real parent site", () => {
  it("adds the first screen under the loaded site even when the screen list is empty (scenario A)", async () => {
    const site = scopeNode({ id: ULID_C, kind: "site", name: "HQ", parent_id: ULID_ROOT });
    const state = { rows: [] as unknown[] };
    let postedParent: string | null | undefined = "unset";
    server.use(
      scopeNodesBySelector(state.rows, [site]),
      http.post("*/api/v1/scope-nodes", async ({ request }) => {
        const body = (await request.json()) as { parent_id?: string };
        postedParent = body.parent_id;
        const created = scopeNode({ id: ULID_B, name: "New screen", parent_id: ULID_C, revision: 1 });
        state.rows = [created];
        return ok(created, { status: 201, revision: 1 });
      }),
    );

    const user = userEvent.setup();
    renderScreens();
    // No sibling screen exists to copy a parent from — the parent comes from the
    // loaded site, so the very first screen can still be created.
    await user.click(await screen.findByRole("button", { name: "New" }));
    await user.click(await screen.findByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(postedParent).toBe(ULID_C));
  });

  it("lets the operator pick which site a new screen lands under when the org spans several (scenario B)", async () => {
    const siteA = scopeNode({ id: ULID_A, kind: "site", name: "North Campus", parent_id: ULID_ROOT });
    const siteB = scopeNode({ id: ULID_B, kind: "site", name: "South Campus", parent_id: ULID_ROOT });
    const state = { rows: [scopeNode({ id: ULID_C, name: "Lobby display", parent_id: ULID_A, revision: 1 })] };
    let postedParent: string | null | undefined = "unset";
    server.use(
      scopeNodesBySelector(state.rows, [siteA, siteB]),
      http.post("*/api/v1/scope-nodes", async ({ request }) => {
        const body = (await request.json()) as { parent_id?: string };
        postedParent = body.parent_id;
        const created = scopeNode({ id: ULID_D, name: "New screen", parent_id: body.parent_id, revision: 1 });
        state.rows = [...state.rows, created];
        return ok(created, { status: 201, revision: 1 });
      }),
    );

    const user = userEvent.setup();
    renderScreens();
    await screen.findByRole("table", { name: "Screens" });

    // Two sites → the target-site picker appears; the fresh screen goes under the
    // one chosen, not under whichever site the first-loaded screen happened to be in.
    const picker = await screen.findByLabelText("Add new screens under");
    await user.selectOptions(picker, ULID_B);
    await user.click(screen.getByRole("button", { name: "New" }));
    await user.click(await screen.findByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(postedParent).toBe(ULID_B));
  });

  it("refuses to add a screen when there is no site to place it under, instead of silently failing (scenario A, no site)", async () => {
    let posted = false;
    server.use(
      scopeNodesBySelector([], []),
      http.post("*/api/v1/scope-nodes", () => {
        posted = true;
        return ok(scopeNode({}), { status: 201, revision: 1 });
      }),
    );

    const user = userEvent.setup();
    renderScreens();
    // New opens the draft; committing it (Save) is where the guard fires — a clear
    // message, and — crucially — no write attempted (never a POST the server would
    // reject with SCOPE_NODE_PARENT_INVALID).
    await user.click(await screen.findByRole("button", { name: "New" }));
    await user.click(await screen.findByRole("button", { name: "Save changes" }));
    expect(await screen.findByText(/add a site/i)).toBeInTheDocument();
    expect(posted).toBe(false);
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
    // The stacked grid keeps the table's accessible name on a role="group", so
    // the row is still addressable — and still distinguishable from the same
    // node's entry in the scope tree above.
    const stacked = screen.getByRole("group", { name: "Screens" });
    expect(within(stacked).getByText("Lobby display")).toBeInTheDocument();
  });
});
