import { describe, it, expect, beforeAll, afterAll, afterEach, vi } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { ThemeProvider } from "@/components/theme/theme-provider";
import { Toaster } from "@/components/kit/toaster";
import {
  LiveScreensPanel,
  formatAge,
  nowPlayingLabel,
  SCREEN_STATUS_FIELDS_WITH_NO_SHIPPED_PRODUCER,
} from "./live-screens-panel";
import { createApi, type Cast, type ScreenStatus } from "@/api";
import { TEST_BASE, TRACE_ID, problem } from "@/api/test-support";

// The fleet-operations panel, CLICKED THROUGH. A rendering-only test would have
// passed against a "Show now" button wired to nothing, which is this project's
// signature defect — so every case here drives a real interaction and asserts on
// the REQUEST the panel made, not on what it drew afterwards.
//
// The other thing these hold is the honesty rules: the panel must never say
// "offline", must never render the never-observed sentinel as a time, and must
// not claim a push is on screen when all it did was state an intent.

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => {
  server.resetHandlers();
  window.localStorage.clear();
});
afterAll(() => server.close());

const SCREEN_1 = "01J8ZSCREEN000000000000001";
const SCREEN_2 = "01J8ZSCREEN000000000000002";
const CAST_A = "01J8ZCAST00000000000000A1";

function statusRow(over: Partial<ScreenStatus> = {}): ScreenStatus {
  return {
    screen_id: SCREEN_1,
    name: "Lobby TV",
    scope_node: "01J8ZS1TE0000000000000001",
    relay_id: "relay-1",
    reachability: "live",
    // The windows the server actually publishes, DERIVED from the player's own
    // loop timings (poll wait + program-request timeout + lease-ack timeout).
    // These are fixtures, not assertions — but a fixture carrying a number the
    // server stopped producing (this one said 180_000 for a whole round after
    // that window was withdrawn) is a fixture that stops describing the system.
    live_window_ms: 52_000,
    content_transfer_window_ms: 172_000,
    // The third line the server draws `fetching` at: at most this many pulls may
    // be outstanding. Without it a screen that answers every pull and never
    // acknowledges reads `fetching` forever, which is what it did.
    fetching_max_unacked_pulls: 2,
    paired: true,
    last_pull_age_ms: 4_000,
    // The ack ANSWERS the pull, so it is the fresher of the two. An ack older
    // than the pull is what the server reads as a transfer in progress.
    last_ack_age_ms: 3_900,
    // Nothing outstanding: this row's ack answered its pull.
    unacked_pulls: 0,
    last_render_start_age_ms: 4_500,
    report_age_ms: 2_000,
    program_revision: "rev-1",
    priority: "scheduled",
    display: "content",
    content_count: 3,
    render_asset_ref: "sha256:aa",
    ...over,
  };
}

function castRow(over: Partial<Cast> = {}): Cast {
  return {
    id: CAST_A,
    name: "Fire Drill",
    scope_node: "01J8ZS1TE0000000000000001",
    labels: {},
    slides: [],
    revision: 1,
    created_at: 1752537000000,
    updated_at: 1752537000000,
    ...over,
  } as Cast;
}

/** A status row with NO `render_asset_ref` — a screen that has been sent content
 * but has never reported putting any of it on screen. The key is DELETED rather
 * than set to undefined, because `exactOptionalPropertyTypes` distinguishes the
 * two and only the absent form is what the server actually sends (the Go field
 * is `omitempty`). */
function withoutRender(over: Partial<ScreenStatus> = {}): ScreenStatus {
  const row = statusRow(over);
  delete row.render_asset_ref;
  return row;
}

/** A status row as REAL HARDWARE produces it: every render-evidence field pinned
 * to its never value, because no shipped producer populates them.
 *
 * `statusRow` is a generous fixture — it carries a `render_asset_ref` and a
 * five-second-old render start, neither of which any deployed screen has ever
 * sent. That is fine for cases about a field's formatting, and fatal for cases
 * about a BRANCH: two of round 3's tests proved a clause worked without proving
 * it was reachable, and the clause was in fact dead on every real row.
 *
 * The pins are driven off the exported list rather than written out, so a player
 * that implements PLY-110 shortens one list and this fixture stops lying in the
 * other direction. */
function asShippedPlayerSends(over: Partial<ScreenStatus> = {}): ScreenStatus {
  const row = statusRow(over);
  for (const field of SCREEN_STATUS_FIELDS_WITH_NO_SHIPPED_PRODUCER) {
    if (field === "render_asset_ref") delete row.render_asset_ref;
    else row.last_render_start_age_ms = -1;
  }
  return row;
}

function page(items: unknown[]) {
  return HttpResponse.json({ items, cursor: null }, { headers: { "Trace-Id": TRACE_ID } });
}

/** Mounts the panel over an msw-backed API, recording every push/clear request
 * the panel actually makes — the assertion surface for "the button is wired". */
function renderPanel(rows: ScreenStatus[] = [statusRow()]) {
  const pushes: { path: string; body: unknown }[] = [];
  const clears: string[] = [];
  server.use(
    http.get(`${TEST_BASE}/screen-status`, () => page(rows)),
    http.get(`${TEST_BASE}/casts`, () => page([castRow()])),
    http.put(`${TEST_BASE}/screens/:id/now`, async ({ params, request }) => {
      const body = await request.json();
      pushes.push({ path: String(params.id), body });
      return HttpResponse.json(
        { screen_id: String(params.id), source: "cast", cast_id: CAST_A, pushed_at: 1752537000000 },
        { headers: { "Trace-Id": TRACE_ID } },
      );
    }),
    http.delete(`${TEST_BASE}/screens/:id/now`, ({ params }) => {
      clears.push(String(params.id));
      return new HttpResponse(null, { status: 204, headers: { "Trace-Id": TRACE_ID } });
    }),
  );
  const api = createApi({ baseUrl: TEST_BASE });
  render(
    <ThemeProvider>
      {/* autoRefresh off: a background poll would race every assertion below
          against a re-render, and the timer's own behaviour is not what any of
          these cases are about. */}
      <LiveScreensPanel api={api} autoRefresh={false} />
      <Toaster />
    </ThemeProvider>,
  );
  return { user: userEvent.setup(), pushes, clears };
}

describe("formatAge", () => {
  it("renders the never sentinel as a dash, NOT as a time", () => {
    // The bug this forbids: -1 formatted arithmetically reads as "just now" or
    // as a negative duration, either of which tells an operator a screen that
    // has never checked in is healthy.
    expect(formatAge(-1)).toBe("—");
  });

  it("scales through the units an operator actually reads", () => {
    expect(formatAge(0)).toBe("just now");
    expect(formatAge(4_000)).toBe("4s ago");
    expect(formatAge(90_000)).toBe("1m ago");
    expect(formatAge(3 * 3_600_000)).toBe("3h ago");
    expect(formatAge(50 * 3_600_000)).toBe("2d ago");
  });
});

describe("nowPlayingLabel", () => {
  it("separates rendered from merely sent — only one of them is evidence", () => {
    expect(nowPlayingLabel(statusRow({ render_asset_ref: "sha256:aa", content_count: 3 }))).toBe(
      "Rendering 3 items",
    );
    expect(nowPlayingLabel(withoutRender({ content_count: 3 }))).toBe("Sent 3 items");
  });

  it("names an intentional blank rather than reporting it as a fault", () => {
    expect(nowPlayingLabel(statusRow({ display: "blank", content_count: 0 }))).toBe(
      "Blank (scheduled off)",
    );
  });

  it("says a never-seen screen is waiting, not that it is showing something", () => {
    expect(
      nowPlayingLabel(statusRow({ reachability: "never_seen", program_revision: "rev-1" })),
    ).toBe("Waiting to collect its program");
  });

  it("says a transferring screen is downloading, and says nothing about the wall", () => {
    // Both of these are rows the shipped player can actually produce: the render
    // fields are at their never values, because player-v3 does not implement
    // PLY-110 and so never posts /player/v1/render/start.
    //
    // A screen collecting its first-ever program and a screen picking up an
    // update to one it has shown all week are INDISTINGUISHABLE in that row, and
    // the label must therefore describe the transfer rather than the glass. It
    // said "Collecting its first content (nothing on screen yet)" for both, which
    // is a false claim about a physical wall on every ordinary update.
    //
    // `content_count` is 3 in the first case deliberately: it is the count of the
    // Lease being collected RIGHT NOW, so it is at its most positive exactly when
    // the wall may be blank. It is not evidence either.
    expect(
      nowPlayingLabel(
        asShippedPlayerSends({ reachability: "fetching", unacked_pulls: 1, content_count: 3 }),
      ),
    ).toBe("Downloading new content");
    expect(
      nowPlayingLabel(
        asShippedPlayerSends({
          reachability: "fetching",
          unacked_pulls: 1,
          content_count: 3,
          program_revision: "rev-week-old",
        }),
      ),
    ).toBe("Downloading new content");
  });

  it("never claims wall state on a row the shipped player can actually produce", () => {
    // The defect class this whole case exists for: a UI branch keyed on a field
    // that has no producer on real hardware. `last_render_start_age_ms` and
    // `render_asset_ref` have exactly one producer between them — the relay's
    // POST /player/v1/render/start — and player-v3 never calls it, so on every
    // real row the "has it rendered?" question answers "nobody said", not "no".
    //
    // So: sweep the whole field space the shipped player CAN produce, and fail if
    // any label asserts what is or is not on the glass. The two forbidden claims
    // are the two the round-3 branch made.
    //
    // If the player ever implements PLY-110, re-arm this by shortening
    // SCREEN_STATUS_FIELDS_WITH_NO_SHIPPED_PRODUCER — the assertion below reads
    // that list, so the guard relaxes deliberately rather than by being deleted.
    const wallClaims = ["still showing", "nothing on screen"];
    const reachabilities: ScreenStatus["reachability"][] = [
      "live",
      "fetching",
      "stale",
      "never_seen",
    ];
    for (const reachability of reachabilities) {
      for (const display of ["content", "blank"] as const) {
        for (const content_count of [0, 1, 3]) {
          for (const program_revision of ["", "rev-1"]) {
            const row = asShippedPlayerSends({
              reachability,
              display,
              content_count,
              program_revision,
              unacked_pulls: reachability === "fetching" ? 1 : 0,
            });
            const label = nowPlayingLabel(row);
            for (const claim of wallClaims) {
              expect(
                label.toLowerCase().includes(claim),
                `nowPlayingLabel said ${JSON.stringify(label)} for a row the shipped player can produce ` +
                  `(${reachability}/${display}/${content_count} items). That asserts what is on the physical ` +
                  `wall, and the only fields that could substantiate it — ` +
                  `${SCREEN_STATUS_FIELDS_WITH_NO_SHIPPED_PRODUCER.join(", ")} — have no producer on real ` +
                  `hardware (player-v3 does not implement PLY-110). A cell that guesses at the glass is worse ` +
                  `than one that only reports the transfer.`,
              ).toBe(false);
            }
          }
        }
      }
    }
  });

  it("lets a scheduled blank outrank the transfer state", () => {
    // `fetching` was checked FIRST and pre-empted this, so a screen whose
    // program is display:blank was described as downloading content — work a
    // blank program does not involve.
    expect(
      nowPlayingLabel(
        statusRow({ reachability: "fetching", display: "blank", content_count: 0, unacked_pulls: 1 }),
      ),
    ).toBe("Blank (scheduled off)");
  });
});

describe("LiveScreensPanel — status", () => {
  it("shows each screen's reachability and how long since it checked in", async () => {
    renderPanel([
      statusRow(),
      statusRow({
        screen_id: SCREEN_2,
        name: "Cafe board",
        reachability: "stale",
        last_pull_age_ms: 180_000,
        report_age_ms: 1_000,
      }),
    ]);
    const table = await screen.findByRole("table", { name: "Live screens" });
    expect(within(table).getByText("Lobby TV")).toBeInTheDocument();
    expect(within(table).getByText("Live")).toBeInTheDocument();
    expect(within(table).getByText("Last check-in 4s ago")).toBeInTheDocument();
    expect(within(table).getByText("Not heard from")).toBeInTheDocument();
    expect(within(table).getByText("Last check-in 3m ago")).toBeInTheDocument();
  });

  it("names a screen that is downloading content, and does not dress it as a fault", async () => {
    // The server's third reachability state. A screen mid content transfer is
    // still showing its previous program (never-wipe), so the row must not read
    // as a problem — and must not read as confirmed-live either, because nothing
    // has been heard back. Before the panel learned the word it fell through to
    // the raw enum and an undefined chip variant, which is the half-of-a-pair
    // this case exists to prevent.
    renderPanel([
      asShippedPlayerSends({
        reachability: "fetching",
        last_pull_age_ms: 72_000,
        last_ack_age_ms: 82_000,
        unacked_pulls: 1,
      }),
    ]);
    const table = await screen.findByRole("table", { name: "Live screens" });
    expect(within(table).getByText("Collecting content")).toBeInTheDocument();
    expect(within(table).queryByText("fetching")).not.toBeInTheDocument();
    expect(within(table).queryByText("Not heard from")).not.toBeInTheDocument();
    // Just the transfer, with no claim attached about what is on the glass —
    // the row is exactly what real hardware sends, and on one of those the box
    // has no render evidence in either direction.
    expect(within(table).getByText("Downloading new content")).toBeInTheDocument();
  });

  it("never says OFFLINE — the platform cannot tell which failure it is", async () => {
    renderPanel([statusRow({ reachability: "stale", last_pull_age_ms: 600_000 })]);
    await screen.findByRole("table", { name: "Live screens" });
    expect(screen.queryByText(/offline/i)).not.toBeInTheDocument();
  });

  it("renders a never-seen screen with a dash, not with a plausible time", async () => {
    renderPanel([
      withoutRender({
        reachability: "never_seen",
        paired: false,
        last_pull_age_ms: -1,
        last_ack_age_ms: -1,
        last_render_start_age_ms: -1,
        report_age_ms: -1,
      }),
    ]);
    const table = await screen.findByRole("table", { name: "Live screens" });
    expect(within(table).getByText("Never seen")).toBeInTheDocument();
    expect(within(table).getByText("Last check-in —")).toBeInTheDocument();
  });

  it("blames the RELAY, not the screen, when the report itself is stale", async () => {
    renderPanel([statusRow({ report_age_ms: 300_000 })]);
    const table = await screen.findByRole("table", { name: "Live screens" });
    expect(within(table).getByText(/Relay last reported 5m ago/)).toBeInTheDocument();
  });

  it("surfaces a failed read instead of showing an empty fleet", async () => {
    server.use(
      http.get(`${TEST_BASE}/screen-status`, () => problem(500, "INTERNAL", "boom")),
      http.get(`${TEST_BASE}/casts`, () => page([])),
    );
    const api = createApi({ baseUrl: TEST_BASE });
    render(
      <ThemeProvider>
        <LiveScreensPanel api={api} autoRefresh={false} />
        <Toaster />
      </ThemeProvider>,
    );
    expect(await screen.findByRole("alert")).toHaveTextContent(/Couldn't load screen status/);
  });
});

describe("LiveScreensPanel — push now", () => {
  it("pushes the chosen cast to the chosen screen and says it was SENT, not shown", async () => {
    const { user, pushes } = renderPanel();
    const table = await screen.findByRole("table", { name: "Live screens" });

    await user.click(within(table).getByRole("button", { name: "Show now" }));
    // The cast library loads only when the dialog opens.
    await screen.findByRole("dialog", { name: "Show now" });
    await waitFor(() => expect(screen.getByLabelText("Cast")).toBeInTheDocument());
    await user.click(screen.getByRole("button", { name: "Show it" }));

    // THE assertion: a real PUT reached the server, at the right screen, naming
    // the right cast. A button wired to nothing passes every other check here.
    await waitFor(() => expect(pushes).toHaveLength(1));
    expect(pushes[0]).toEqual({ path: SCREEN_1, body: { mode: "play", cast_id: CAST_A } });

    // And the claim made to the operator is INTENT, never delivery.
    const toast = await screen.findByText(/Sent to Lobby TV/);
    expect(toast).toHaveTextContent(/next check-in/);
    expect(screen.queryByText(/now showing/i)).not.toBeInTheDocument();
  });

  it("offers Back to schedule only for a screen that is actually overridden, and clears it", async () => {
    const { user, clears } = renderPanel([
      statusRow(),
      statusRow({
        screen_id: SCREEN_2,
        name: "Cafe board",
        now: { screen_id: SCREEN_2, mode: "play", source: "cast", cast_id: CAST_A, pushed_at: 1752537000000 },
      }),
    ]);
    const table = await screen.findByRole("table", { name: "Live screens" });

    // Exactly one row offers the clear — the overridden one.
    const clearButtons = within(table).getAllByRole("button", { name: "Back to schedule" });
    expect(clearButtons).toHaveLength(1);

    await user.click(clearButtons[0]);
    await waitFor(() => expect(clears).toEqual([SCREEN_2]));
    expect(await screen.findByText(/Cafe board returns to its schedule/)).toBeInTheDocument();
  });

  it("marks an overridden row as pushed by an operator", async () => {
    renderPanel([
      statusRow({
        now: { screen_id: SCREEN_1, mode: "play", source: "cast", cast_id: CAST_A, pushed_at: 1752537000000 },
      }),
    ]);
    const table = await screen.findByRole("table", { name: "Live screens" });
    expect(within(table).getByText(/Pushed by an operator/)).toBeInTheDocument();
  });

  it("reports a refused push instead of leaving the operator thinking it worked", async () => {
    renderPanel();
    server.use(
      http.put(`${TEST_BASE}/screens/:id/now`, () =>
        problem(422, "VALIDATION_FAILED", "No cast or playlist exists with the identifier this push names."),
      ),
    );
    const user = userEvent.setup();
    const table = await screen.findByRole("table", { name: "Live screens" });
    await user.click(within(table).getByRole("button", { name: "Show now" }));
    await screen.findByRole("dialog", { name: "Show now" });
    await waitFor(() => expect(screen.getByLabelText("Cast")).toBeInTheDocument());
    await user.click(screen.getByRole("button", { name: "Show it" }));

    expect(await screen.findByText(/Couldn't push to this screen/)).toBeInTheDocument();
  });

  it("says so rather than offering an empty picker when there are no casts to push", async () => {
    server.use(
      http.get(`${TEST_BASE}/screen-status`, () => page([statusRow()])),
      http.get(`${TEST_BASE}/casts`, () => page([])),
    );
    const api = createApi({ baseUrl: TEST_BASE });
    render(
      <ThemeProvider>
        <LiveScreensPanel api={api} autoRefresh={false} />
        <Toaster />
      </ThemeProvider>,
    );
    const user = userEvent.setup();
    const table = await screen.findByRole("table", { name: "Live screens" });
    await user.click(within(table).getByRole("button", { name: "Show now" }));
    expect(await screen.findByText(/no casts to show yet/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Show it" })).toBeDisabled();
  });
});

describe("LiveScreensPanel — refresh", () => {
  it("re-reads on demand so an operator can watch a push land", async () => {
    let reads = 0;
    server.use(
      http.get(`${TEST_BASE}/screen-status`, () => {
        reads += 1;
        return page([statusRow()]);
      }),
      http.get(`${TEST_BASE}/casts`, () => page([castRow()])),
    );
    const api = createApi({ baseUrl: TEST_BASE });
    render(
      <ThemeProvider>
        <LiveScreensPanel api={api} autoRefresh={false} />
        <Toaster />
      </ThemeProvider>,
    );
    const user = userEvent.setup();
    await screen.findByRole("table", { name: "Live screens" });
    await waitFor(() => expect(reads).toBe(1));
    await user.click(screen.getByRole("button", { name: "Refresh" }));
    await waitFor(() => expect(reads).toBe(2));
  });

  it("polls on its own while mounted, so a wall going dark is noticed unattended", async () => {
    vi.useFakeTimers();
    try {
      let reads = 0;
      server.use(
        http.get(`${TEST_BASE}/screen-status`, () => {
          reads += 1;
          return page([statusRow()]);
        }),
        http.get(`${TEST_BASE}/casts`, () => page([castRow()])),
      );
      const api = createApi({ baseUrl: TEST_BASE });
      render(
        <ThemeProvider>
          <LiveScreensPanel api={api} />
          <Toaster />
        </ThemeProvider>,
      );
      await vi.waitFor(() => expect(reads).toBe(1));
      await vi.advanceTimersByTimeAsync(10_000);
      await vi.waitFor(() => expect(reads).toBeGreaterThanOrEqual(2));
    } finally {
      vi.useRealTimers();
    }
  });
});
