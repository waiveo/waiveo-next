import { describe, expect, it } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { afterAll, afterEach, beforeAll } from "vitest";
import ScreensRoute, { toScreenRow } from "./screens-route";
import { createApi } from "@/api";

/**
 * The Screens page — and above all the ONE distinction it exists to draw.
 *
 * `screen.status` carries what the platform HANDED a screen and what the screen
 * ACCEPTED. A player that refuses a program leaves the intent describing the
 * program it refused, so a console reading intent alone reports a wall as
 * showing something it is not (relay/1 REL-119b). Every test below that names
 * "Showing" is guarding that, because it is the failure that already happened
 * once and is invisible while every screen is behaving.
 */

const TEST_BASE = "https://box.test/api/v1";
const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

function status(over: Record<string, unknown> = {}) {
  return {
    screen_id: "01J8Z9SCREEN0000000000000A",
    name: "Lobby wall",
    reachability: "live",
    content_transfer_window_ms: 60_000,
    fetching_max_unacked_pulls: 3,
    live_window_ms: 30_000,
    paired: true,
    last_pull_age_ms: 2_000,
    last_ack_age_ms: 1_500,
    last_render_start_age_ms: 1_000,
    unacked_pulls: 0,
    report_age_ms: 500,
    program_revision: "rev-handed",
    priority: "scheduled",
    display: "content",
    content_count: 3,
    acked_program_revision: "rev-accepted",
    acked_display: "content",
    acked_content_count: 3,
    rejected: false,
    ...over,
  };
}

function seed(items: unknown[]) {
  server.use(
    http.get(`${TEST_BASE}/screen-status`, () => HttpResponse.json({ items, cursor: null })),
  );
}

function renderRoute() {
  return render(<ScreensRoute api={createApi({ baseUrl: TEST_BASE })} />);
}

describe("Screens — what a screen CONFIRMED, never what it was handed", () => {
  it("shows the ACCEPTED revision under Showing, and the handed one separately", async () => {
    // The whole point. If these ever render the same value, a refusing screen
    // reads as compliant and the page is worse than nothing.
    seed([status()]);
    renderRoute();
    const table = await screen.findByRole("table", { name: "Screens" });
    await waitFor(() => expect(within(table).getByText("rev-accepted")).toBeInTheDocument());
    expect(within(table).getByText("rev-handed")).toBeInTheDocument();
  });

  it("does NOT claim a refused program is showing", async () => {
    // A screen that refused `rev-handed` is still showing what it accepted
    // before. Reading intent here would put the refused revision on the wall.
    seed([
      status({
        rejected: true,
        reachability: "rejected",
        program_revision: "rev-refused",
        acked_program_revision: "rev-older",
      }),
    ]);
    renderRoute();
    await screen.findByRole("table", { name: "Screens" });
    await waitFor(() => expect(screen.getByText("rev-older")).toBeInTheDocument());
    // Asserted through the CELL each value lands in, rather than by row index:
    // what matters is that the two revisions never share a cell, and locating
    // them from their own text is robust to how the table lays rows out.
    const showingCell = screen.getByText("rev-older").closest("td");
    const handedCell = screen.getByText("rev-refused").closest("td");
    expect(showingCell).not.toBeNull();
    expect(handedCell).not.toBeNull();
    expect(showingCell).not.toBe(handedCell);
    // The refused revision must not appear in the Showing cell under any
    // rendering — that is the whole rule (REL-119b).
    expect(showingCell).not.toHaveTextContent("rev-refused");
  });

  it("says a screen has NEVER accepted a program rather than showing a blank", async () => {
    // Told by the ack sentinel, not by an empty revision: an accepted BLANK
    // program is legitimately empty and must not read as "never".
    seed([status({ last_ack_age_ms: -1, acked_program_revision: undefined, acked_display: undefined })]);
    renderRoute();
    const table = await screen.findByRole("table", { name: "Screens" });
    await waitFor(() =>
      expect(within(table).getByText("never accepted a program")).toBeInTheDocument(),
    );
  });

  it("tells an accepted BLANK apart from never having accepted anything", async () => {
    // THE shape the ack sentinel exists for, and the one a revision-based check
    // gets wrong: the wire doc is explicit that an accepted blank is
    // "legitimately empty here too", so a screen can have acknowledged a program
    // and carry NO revision. Telling never-accepted by an empty revision would
    // report a screen that is deliberately showing nothing as one that has never
    // answered — a working wall read as a broken one.
    seed([
      status({
        acked_display: "blank",
        acked_program_revision: undefined,
        acked_content_count: 0,
        last_ack_age_ms: 1_500,
      }),
    ]);
    renderRoute();
    const table = await screen.findByRole("table", { name: "Screens" });
    await waitFor(() => expect(within(table).getByText("blank")).toBeInTheDocument());
    expect(within(table).queryByText("never accepted a program")).toBeNull();
  });
});

describe("Screens — the states an operator acts on differently", () => {
  it("gives each reachability its own tone", async () => {
    seed([
      status({ screen_id: "01J8Z9SCREEN0000000000000A", name: "A", reachability: "live" }),
      status({ screen_id: "01J8Z9SCREEN0000000000000B", name: "B", reachability: "rejected", rejected: true }),
      status({ screen_id: "01J8Z9SCREEN0000000000000C", name: "C", reachability: "stale" }),
    ]);
    renderRoute();
    const table = await screen.findByRole("table", { name: "Screens" });
    await waitFor(() => expect(within(table).getByText("Live")).toBeInTheDocument());
    const tone = (label: string) =>
      within(table).getByText(label).closest("[data-slot='status-badge']")?.getAttribute("data-status");
    expect(tone("Live")).toBe("ok");
    // A refusal is someone actively saying no; a stale screen may just be a wall
    // switched off. Spending the error colour on both hides the one that matters.
    expect(tone("Refused")).toBe("error");
    expect(tone("Stale")).toBe("warn");
  });

  it("renders the never-observed sentinel as 'never', not as a huge age", async () => {
    seed([status({ last_pull_age_ms: -1 })]);
    renderRoute();
    const table = await screen.findByRole("table", { name: "Screens" });
    await waitFor(() => expect(within(table).getByText("never")).toBeInTheDocument());
  });
});

describe("Screens — an empty table says WHICH emptiness", () => {
  it("explains that no screen is paired, rather than showing a bare no-rows card", async () => {
    seed([]);
    renderRoute();
    await waitFor(() =>
      expect(screen.getByText(/No screen has been paired to this box yet/)).toBeInTheDocument(),
    );
  });

  it("does not report a refused READ as an absence of screens", async () => {
    // A read this console was refused is a fact about the console. Saying "no
    // screens are paired" would be a claim about the fleet nobody verified.
    server.use(
      http.get(`${TEST_BASE}/screen-status`, () =>
        HttpResponse.json({ type: "about:blank", title: "Forbidden", status: 403 }, { status: 403 }),
      ),
    );
    renderRoute();
    await waitFor(() =>
      expect(screen.getByText(/The screen list could not be read/)).toBeInTheDocument(),
    );
    expect(screen.queryByText(/No screen has been paired/)).toBeNull();
  });
});

describe("toScreenRow — the projection, at its edges", () => {
  it("never lets the handed revision leak into the showing field", () => {
    const row = toScreenRow(
      status({ rejected: true, program_revision: "rev-handed", acked_program_revision: "rev-older" }) as never,
    );
    expect(row.showing_display).toBe("rev-older");
    expect(row.handed_display).toBe("rev-handed");
  });

  it("falls back to the screen id when a screen has no name", () => {
    const row = toScreenRow(status({ name: undefined }) as never);
    expect(row.name).toBe("01J8Z9SCREEN0000000000000A");
  });
});
