import { describe, it, expect, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { ThemeProvider } from "@/components/theme/theme-provider";
import SchedulesRoute from "./schedules-route";
import scheduleDoc from "./schedule.uis.json";
import daypartDoc from "./daypart.uis.json";
import playlistDoc from "./playlist.uis.json";
import { validatePage } from "@/renderer/validate";
import { TRACE_ID, ULID_ROOT, scopeNode, ok, problem } from "@/api/test-support";

// Schedules authors two DOGFOODED ui-schema collections (schedules + playlists as
// list-detail documents) and a DOGFOODED daypart editor (a settings-form): each
// is validated and rendered through the SAME PageRenderer an extension page takes.
// These tests prove the documents conform, that the list pages by cursor, that the
// create/edit/delete verbs carry the api/1 conventions (Idempotency-Key, If-Match),
// and — the focus of this page — that the daypart editor surfaces the standard
// optimistic-concurrency conflict UX (412 → toast + re-read + review) and maps a
// server field error (422 DAT-073 daypart overlap) onto the field the operator fixes.

// Fixture ULIDs (never real identifiers).
const ULID_SITE = "01J8Z3SITE00000000000000A1";
const ULID_SCHED = "01J8Z3SCHED0000000000000B1";
const ULID_SCHED2 = "01J8Z3SCHED0000000000000B2";
const ULID_DP = "01J8Z3DPART0000000000000C1";
const ULID_DP2 = "01J8Z3DPART0000000000000C2";
const ULID_PL = "01J8Z3PLIST0000000000000D1";

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => {
  server.resetHandlers();
  setViewport(false);
  window.localStorage.clear();
});
afterAll(() => server.close());

function renderSchedules() {
  return render(
    <ThemeProvider>
      <SchedulesRoute />
    </ThemeProvider>,
  );
}

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

function page(items: unknown[], cursor: string | null = null) {
  return HttpResponse.json({ items, cursor }, { headers: { "Trace-Id": TRACE_ID } });
}

const site = () => scopeNode({ id: ULID_SITE, kind: "site", name: "HQ", parent_id: ULID_ROOT });

function schedule(over: Record<string, unknown> = {}) {
  return {
    id: ULID_SCHED,
    scope_node: ULID_SITE,
    name: "Weekday programming",
    revision: 1,
    created_at: 0,
    updated_at: 0,
    ...over,
  };
}

function daypart(over: Record<string, unknown> = {}) {
  return {
    id: ULID_DP,
    schedule_id: ULID_SCHED,
    scope_node: ULID_SITE,
    name: "Morning open",
    days_of_week: [1, 2, 3, 4, 5],
    start_time: "06:00:00",
    end_time: "12:00:00",
    display_power: "on",
    playlist_id: ULID_PL,
    revision: 1,
    created_at: 0,
    updated_at: 0,
    ...over,
  };
}

function playlist(over: Record<string, unknown> = {}) {
  return {
    id: ULID_PL,
    scope_node: ULID_SITE,
    name: "Daytime loop",
    items: [],
    revision: 1,
    created_at: 0,
    updated_at: 0,
    ...over,
  };
}

/** Register the four list GETs the route loads on mount (schedules, playlists,
 * dayparts, scope-nodes). Both sections load playlists + scope-nodes, so one
 * handler per endpoint serves them all. */
function useLists(opts: {
  schedules?: () => unknown[];
  playlists?: () => unknown[];
  dayparts?: () => unknown[];
  scopeNodes?: () => unknown[];
}) {
  server.use(
    http.get("*/api/v1/schedules", () => page(opts.schedules?.() ?? [])),
    http.get("*/api/v1/playlists", () => page(opts.playlists?.() ?? [])),
    http.get("*/api/v1/dayparts", () => page(opts.dayparts?.() ?? [])),
    http.get("*/api/v1/scope-nodes", () => page(opts.scopeNodes?.() ?? [])),
  );
}

/** The schedules section's `<section>`, scoped by its level-2 heading (the hero
 * title is an h1, so level:2 always picks the section). */
function sectionByHeading(name: string): HTMLElement {
  return screen.getByRole("heading", { name, level: 2 }).closest("section") as HTMLElement;
}

// ─────────────────────────────────────────────────────────────────────────────

describe("Schedules — dogfooded ui-schema documents", () => {
  it("schedule.uis.json passes validatePage (the same gate an extension page clears)", () => {
    const result = validatePage(scheduleDoc);
    expect(result).toEqual({ ok: true });
    expect((scheduleDoc as { pageType: string }).pageType).toBe("list-detail");
  });

  it("daypart.uis.json passes validatePage (a dogfooded settings-form)", () => {
    const result = validatePage(daypartDoc);
    expect(result).toEqual({ ok: true });
    expect((daypartDoc as { pageType: string }).pageType).toBe("settings-form");
  });

  it("playlist.uis.json passes validatePage (a dogfooded list-detail)", () => {
    const result = validatePage(playlistDoc);
    expect(result).toEqual({ ok: true });
    expect((playlistDoc as { pageType: string }).pageType).toBe("list-detail");
  });
});

describe("Schedules — the paginated list over api/1", () => {
  it("renders the live schedules through the renderer as a real table", async () => {
    useLists({ schedules: () => [schedule()], scopeNodes: () => [site()] });
    renderSchedules();
    const table = await screen.findByRole("table", { name: "Schedules" });
    expect(within(table).getByText("Weekday programming")).toBeInTheDocument();
  });

  it("walks the keyset cursors to load every schedule (never an offset)", async () => {
    // Page 1 returns a non-null cursor; page 2 returns null — the client walks both.
    server.use(
      http.get("*/api/v1/schedules", ({ request }) => {
        const cursor = new URL(request.url).searchParams.get("cursor");
        return cursor
          ? page([schedule({ id: ULID_SCHED2, name: "Weekend programming" })], null)
          : page([schedule()], "CURSOR-2");
      }),
      http.get("*/api/v1/playlists", () => page([])),
      http.get("*/api/v1/dayparts", () => page([])),
      http.get("*/api/v1/scope-nodes", () => page([site()])),
    );
    renderSchedules();
    const table = await screen.findByRole("table", { name: "Schedules" });
    expect(within(table).getByText("Weekday programming")).toBeInTheDocument();
    expect(within(table).getByText("Weekend programming")).toBeInTheDocument();
  });

  it("creates a schedule carrying an Idempotency-Key and the chosen scope_node", async () => {
    const state = { schedules: [] as unknown[] };
    let idempotencyKey: string | null = null;
    let postedScope: unknown = "unset";
    server.use(
      http.get("*/api/v1/schedules", () => page(state.schedules)),
      http.get("*/api/v1/playlists", () => page([])),
      http.get("*/api/v1/dayparts", () => page([])),
      http.get("*/api/v1/scope-nodes", () => page([site()])),
      http.post("*/api/v1/schedules", async ({ request }) => {
        idempotencyKey = request.headers.get("Idempotency-Key");
        postedScope = ((await request.json()) as { scope_node?: string }).scope_node;
        const created = schedule({ id: ULID_SCHED2, name: "New schedule" });
        state.schedules = [created];
        return ok(created, { status: 201, revision: 1 });
      }),
    );

    const user = userEvent.setup();
    renderSchedules();
    await screen.findByRole("table", { name: "Schedules" });

    // New opens a create draft; Save schedule commits it (the create idiom, UIS-021).
    await user.click(within(sectionByHeading("Schedules")).getByRole("button", { name: "New" }));
    await user.click(await screen.findByRole("button", { name: "Save schedule" }));

    await waitFor(() =>
      expect(
        within(screen.getByRole("table", { name: "Schedules" })).getByText("New schedule"),
      ).toBeInTheDocument(),
    );
    expect(idempotencyKey).toMatch(/^[0-9a-f-]{36}$/i);
    expect(postedScope).toBe(ULID_SITE);
  });

  it("edits a schedule under its If-Match", async () => {
    const state = { schedules: [schedule({ revision: 3 })] };
    let ifMatch: string | null = null;
    server.use(
      http.get("*/api/v1/schedules", () => page(state.schedules)),
      http.get("*/api/v1/playlists", () => page([])),
      http.get("*/api/v1/dayparts", () => page([])),
      http.get("*/api/v1/scope-nodes", () => page([site()])),
      http.patch("*/api/v1/schedules/:id", async ({ request }) => {
        ifMatch = request.headers.get("If-Match");
        const body = (await request.json()) as { name?: string };
        const updated = schedule({ name: body.name ?? "Weekday programming", revision: 4 });
        state.schedules = [updated];
        return ok(updated, { revision: 4 });
      }),
    );

    const user = userEvent.setup();
    renderSchedules();
    await screen.findByRole("table", { name: "Schedules" });
    await user.click(screen.getByText("Weekday programming").closest("tr") as HTMLElement);

    const nameInput = await screen.findByLabelText("Schedule name");
    await user.clear(nameInput);
    await user.type(nameInput, "Holiday hours");
    await user.click(screen.getByRole("button", { name: "Save schedule" }));

    await waitFor(() =>
      expect(within(screen.getByRole("table", { name: "Schedules" })).getByText("Holiday hours")).toBeInTheDocument(),
    );
    expect(ifMatch).toBe('"3"');
  });

  it("deletes a schedule under its If-Match", async () => {
    const state = { schedules: [schedule({ revision: 2 })] };
    let ifMatch: string | null = null;
    server.use(
      http.get("*/api/v1/schedules", () => page(state.schedules)),
      http.get("*/api/v1/playlists", () => page([])),
      http.get("*/api/v1/dayparts", () => page([])),
      http.get("*/api/v1/scope-nodes", () => page([site()])),
      http.delete("*/api/v1/schedules/:id", ({ request }) => {
        ifMatch = request.headers.get("If-Match");
        state.schedules = [];
        return new HttpResponse(null, { status: 204, headers: { "Trace-Id": TRACE_ID } });
      }),
    );

    const user = userEvent.setup();
    renderSchedules();
    await screen.findByRole("table", { name: "Schedules" });
    await user.click(screen.getByText("Weekday programming").closest("tr") as HTMLElement);
    await user.click(await screen.findByRole("button", { name: "Delete schedule" }));

    await waitFor(() => expect(screen.queryByText("Weekday programming")).not.toBeInTheDocument());
    expect(ifMatch).toBe('"2"');
  });
});

describe("Schedules — the daypart editor", () => {
  it("reveals the selected schedule's dayparts, and opens the editor with playlists from the live store", async () => {
    useLists({
      schedules: () => [schedule()],
      dayparts: () => [daypart()],
      playlists: () => [playlist(), playlist({ id: "01J8Z3PLIST0000000000000D2", name: "Evening loop" })],
      scopeNodes: () => [site()],
    });

    const user = userEvent.setup();
    renderSchedules();
    await screen.findByRole("table", { name: "Schedules" });
    await user.click(screen.getByText("Weekday programming").closest("tr") as HTMLElement);

    // The daypart panel is scoped to the selected schedule.
    const daypartTable = await screen.findByRole("table", { name: "Dayparts of Weekday programming" });
    expect(within(daypartTable).getByText("Morning open")).toBeInTheDocument();

    // Open the editor — the playlist select is fed from the live playlists.
    await user.click(screen.getByRole("button", { name: "Edit daypart Morning open" }));
    const dialog = await screen.findByRole("dialog");
    const playlistSelect = within(dialog).getByLabelText("Playlist");
    expect(within(playlistSelect).getByText("Daytime loop")).toBeInTheDocument();
    expect(within(playlistSelect).getByText("Evening loop")).toBeInTheDocument();
  });

  it("creates a daypart carrying an Idempotency-Key and the parent schedule's id + scope_node", async () => {
    const state = { dayparts: [] as unknown[] };
    let idempotencyKey: string | null = null;
    let body: Record<string, unknown> | null = null;
    server.use(
      http.get("*/api/v1/schedules", () => page([schedule()])),
      http.get("*/api/v1/playlists", () => page([playlist()])),
      http.get("*/api/v1/dayparts", () => page(state.dayparts)),
      http.get("*/api/v1/scope-nodes", () => page([site()])),
      http.post("*/api/v1/dayparts", async ({ request }) => {
        idempotencyKey = request.headers.get("Idempotency-Key");
        body = (await request.json()) as Record<string, unknown>;
        const created = daypart({ id: ULID_DP2, name: "Afternoon" });
        state.dayparts = [created];
        return ok(created, { status: 201, revision: 1 });
      }),
    );

    const user = userEvent.setup();
    renderSchedules();
    await screen.findByRole("table", { name: "Schedules" });
    await user.click(screen.getByText("Weekday programming").closest("tr") as HTMLElement);
    await screen.findByRole("table", { name: "Dayparts of Weekday programming" });

    await user.click(screen.getByRole("button", { name: "Add daypart" }));
    const dialog = await screen.findByRole("dialog");
    await user.click(within(dialog).getByRole("button", { name: "Save daypart" }));

    await waitFor(() =>
      expect(
        within(screen.getByRole("table", { name: "Dayparts of Weekday programming" })).getByText("Afternoon"),
      ).toBeInTheDocument(),
    );
    expect(idempotencyKey).toMatch(/^[0-9a-f-]{36}$/i);
    expect(body).toMatchObject({ schedule_id: ULID_SCHED, scope_node: ULID_SITE, display_power: "on" });
  });
});

describe("Schedules — the daypart editor's conflict + validation UX", () => {
  async function openDaypartEditor() {
    const user = userEvent.setup();
    renderSchedules();
    await screen.findByRole("table", { name: "Schedules" });
    await user.click(screen.getByText("Weekday programming").closest("tr") as HTMLElement);
    await screen.findByRole("table", { name: "Dayparts of Weekday programming" });
    await user.click(screen.getByRole("button", { name: "Edit daypart Morning open" }));
    const dialog = await screen.findByRole("dialog");
    return { user, dialog };
  }

  it("on a 412 re-reads the current daypart and holds the editor open for review — never a silent overwrite", async () => {
    const changed = daypart({ name: "Changed elsewhere", start_time: "07:00:00", revision: 9 });
    let patchCount = 0;
    server.use(
      http.get("*/api/v1/schedules", () => page([schedule()])),
      http.get("*/api/v1/playlists", () => page([playlist()])),
      http.get("*/api/v1/dayparts", () => page([daypart({ revision: 3 })])),
      http.get("*/api/v1/scope-nodes", () => page([site()])),
      http.get("*/api/v1/dayparts/:id", () => ok(changed, { revision: 9 })),
      http.patch("*/api/v1/dayparts/:id", () => {
        patchCount += 1;
        return problem(412, "REVISION_CONFLICT", "The resource was modified concurrently.", {
          current_revision: 9,
        });
      }),
    );

    const { user, dialog } = await openDaypartEditor();
    await user.click(within(dialog).getByRole("button", { name: "Save daypart" }));

    // The review state: a distinct banner inside the still-open editor, the editor
    // re-seeded with the CURRENT server values, and exactly one write attempted.
    await waitFor(() =>
      expect(within(dialog).getByRole("status")).toHaveTextContent(/changed elsewhere/i),
    );
    const nameAfter = (await within(dialog).findByLabelText("Daypart name")) as HTMLInputElement;
    await waitFor(() => expect(nameAfter.value).toBe("Changed elsewhere"));
    expect(within(dialog).getByRole("button", { name: "Save daypart" })).toBeInTheDocument();
    expect(patchCount).toBe(1);
  });

  it("maps a 422 DAT-073 daypart-overlap onto the window field the operator adjusts (not only a toast)", async () => {
    let patchCount = 0;
    server.use(
      http.get("*/api/v1/schedules", () => page([schedule()])),
      http.get("*/api/v1/playlists", () => page([playlist()])),
      http.get("*/api/v1/dayparts", () => page([daypart({ revision: 3 })])),
      http.get("*/api/v1/scope-nodes", () => page([site()])),
      http.patch("*/api/v1/dayparts/:id", () => {
        patchCount += 1;
        // The server reports the overlap against the collection field `dayparts`.
        return problem(422, "VALIDATION_FAILED", "The daypart could not be saved.", {
          errors: [
            {
              field: "dayparts",
              code: "DAYPART_OVERLAP",
              message: "This daypart overlaps another in the schedule on Mon–Fri.",
            },
          ],
        });
      }),
    );

    const { user, dialog } = await openDaypartEditor();
    await user.click(within(dialog).getByRole("button", { name: "Save daypart" }));

    // The overlap message lands on the window's start (Starts) as a FormField
    // error (role=alert), the input is flagged aria-invalid, the editor stays open,
    // and exactly one write was attempted.
    const inlineError = await within(dialog).findByText(/overlaps another in the schedule/i);
    expect(inlineError).toHaveAttribute("role", "alert");
    expect(within(dialog).getByLabelText("Starts")).toHaveAttribute("aria-invalid", "true");
    expect(within(dialog).getByRole("button", { name: "Save daypart" })).toBeInTheDocument();
    expect(patchCount).toBe(1);
  });
});

describe("Playlists — dogfooded management over api/1", () => {
  it("renders the live playlists through the renderer and creates one carrying an Idempotency-Key", async () => {
    const state = { playlists: [] as unknown[] };
    let idempotencyKey: string | null = null;
    server.use(
      http.get("*/api/v1/schedules", () => page([])),
      http.get("*/api/v1/playlists", () => page(state.playlists)),
      http.get("*/api/v1/dayparts", () => page([])),
      http.get("*/api/v1/scope-nodes", () => page([site()])),
      http.post("*/api/v1/playlists", async ({ request }) => {
        idempotencyKey = request.headers.get("Idempotency-Key");
        const created = playlist({ name: "New playlist" });
        state.playlists = [created];
        return ok(created, { status: 201, revision: 1 });
      }),
    );

    const user = userEvent.setup();
    renderSchedules();
    await screen.findByRole("table", { name: "Playlists" });
    // New opens a create draft; Save playlist commits it (the create idiom, UIS-021).
    await user.click(within(sectionByHeading("Playlists")).getByRole("button", { name: "New" }));
    await user.click(await screen.findByRole("button", { name: "Save playlist" }));

    await waitFor(() =>
      expect(within(screen.getByRole("table", { name: "Playlists" })).getByText("New playlist")).toBeInTheDocument(),
    );
    expect(idempotencyKey).toMatch(/^[0-9a-f-]{36}$/i);
  });

  it("adds an item to a playlist and saves it under its If-Match", async () => {
    const state = { playlists: [playlist({ revision: 2 })] };
    let ifMatch: string | null = null;
    let savedItemCount = -1;
    server.use(
      http.get("*/api/v1/schedules", () => page([])),
      http.get("*/api/v1/playlists", () => page(state.playlists)),
      http.get("*/api/v1/dayparts", () => page([])),
      http.get("*/api/v1/scope-nodes", () => page([site()])),
      http.patch("*/api/v1/playlists/:id", async ({ request }) => {
        ifMatch = request.headers.get("If-Match");
        const body = (await request.json()) as { items?: unknown[] };
        savedItemCount = body.items?.length ?? 0;
        const updated = playlist({ items: body.items ?? [], revision: 3 });
        state.playlists = [updated];
        return ok(updated, { revision: 3 });
      }),
    );

    const user = userEvent.setup();
    renderSchedules();
    await screen.findByRole("table", { name: "Playlists" });
    await user.click(screen.getByText("Daytime loop").closest("tr") as HTMLElement);

    await user.click(await screen.findByRole("button", { name: "Add item" }));
    // The added item's fields render (the repeat grew by one).
    expect(await screen.findByLabelText("Content reference")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Save playlist" }));
    await waitFor(() => expect(ifMatch).toBe('"2"'));
    expect(savedItemCount).toBe(1);
  });
});

describe("Schedules — responsive at 360px", () => {
  it("keeps the daypart editor usable and the lists as stacked cards (no page-scrolling table)", async () => {
    setViewport(true);
    useLists({
      schedules: () => [schedule()],
      dayparts: () => [daypart()],
      playlists: () => [playlist()],
      scopeNodes: () => [site()],
    });

    const user = userEvent.setup();
    renderSchedules();
    // The schedule list is stacked cards at 360px — no wide <table> to overflow.
    await waitFor(() =>
      expect(document.querySelector('[data-slot="data-table"][data-layout="stacked"]')).not.toBeNull(),
    );
    expect(screen.queryByRole("table")).not.toBeInTheDocument();

    // Select the schedule (its card) and open the daypart editor.
    await user.click(screen.getByText("Weekday programming"));
    await user.click(await screen.findByRole("button", { name: "Edit daypart Morning open" }));

    // The editor's fields render inside the full-screen modal, stacked and labeled.
    const dialog = await screen.findByRole("dialog");
    expect(within(dialog).getByLabelText("Daypart name")).toBeInTheDocument();
    expect(within(dialog).getByLabelText("Starts")).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Save daypart" })).toBeInTheDocument();
    // No wide table inside the editor to force horizontal page scroll.
    expect(within(dialog).queryByRole("table")).not.toBeInTheDocument();
  });
});
