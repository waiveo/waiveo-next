import { describe, it, expect, beforeAll, afterAll, afterEach, vi } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import SystemRoute, {
  displayTimeZone,
  formatBytes,
  formatUptime,
  RESTART_GIVEUP_MS,
} from "./system-route";
import restartPanelDoc from "./restart-panel.uis.json";
import { validatePage } from "@/renderer";
import { TRACE_ID, problem } from "@/api/test-support";
import type { PlatformLogPage, PlatformLogRecord, SystemHealth } from "@/api";

// The System route is parity row 7.4 — "diagnose a problem without SSH". These
// tests drive the REAL route against a mocked feeder (msw) over the same-origin
// client the shipped app uses, and they hold the page to the honesty rules its
// header states: it shows what it is NOT showing, it says the level is derived,
// it never pretends to be the audit trail, and a filter that matches nothing can
// always be undone from the page itself.

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

function record(over: Partial<PlatformLogRecord> = {}): PlatformLogRecord {
  return {
    seq: 1,
    ts_ms: 1_752_537_600_000,
    level: "info",
    source: "waiveo-feeder",
    message: "listening on :7420",
    raw: "waiveo-feeder: listening on :7420",
    ...over,
  };
}

function logPage(over: Partial<PlatformLogPage> = {}): PlatformLogPage {
  const items = over.items ?? [record()];
  return {
    items,
    matched: items.length,
    retained: items.length,
    capacity: 4000,
    dropped: 0,
    retained_from_ms: items[0]?.ts_ms ?? 0,
    sources: ["http", "waiveo-feeder", "waiveo-relay"],
    level_counts: { error: 0, warn: 0, info: items.length },
    ...over,
  };
}

function healthBody(over: Partial<SystemHealth> = {}): SystemHealth {
  return {
    status: "ok",
    checked_at_ms: 1_752_537_600_000,
    started_at_ms: 1_752_537_600_000 - 3_723_000,
    uptime_ms: 3_723_000,
    version: "1.2.3 (stable)",
    services: [
      { name: "store", status: "ok", detail: "Readable; 2 authored screen row(s)." },
      { name: "content-origin", status: "ok", detail: "4 asset(s), 128 KiB." },
      { name: "relay-plane", status: "ok", detail: "1 relay(s) connected." },
      { name: "screens", status: "ok", detail: "All 2 screen(s) are live." },
    ],
    storage: {
      path: "/var/lib/waiveo",
      status: "ok",
      total_bytes: 40 * 1024 ** 3,
      free_bytes: 20 * 1024 ** 3,
      used_percent: 50,
      detail: "20480 MiB available of 40960 MiB.",
    },
    relays: [{ relay_id: "relay-a", address: "192.0.2.40:7443", screen_count: 2 }],
    screens: {
      total: 2,
      live: 2,
      fetching: 0,
    rejected: 0,
      stale: 0,
      never_seen: 0,
      paired: 2,
      overridden: 0,
      // Derived server-side from the player's own loop timings; see
      // internal/shared/wire/screencadence.go. Kept current here because a
      // fixture carrying a withdrawn number is a fixture that has stopped
      // describing the system it stands in for.
      live_window_ms: 52_000,
      // The two fetching/stale lines, republished alongside the fetching COUNT.
      // The roll-up carried the count and neither line for a round, then the age
      // line and not the progress one — and two of three thresholds is the same
      // defect, because a consumer that has two believes it has the rule.
      content_transfer_window_ms: 172_000,
      fetching_max_unacked_pulls: 2,
    },
    ...over,
  };
}

/** Serve both reads. `onLogs` sees the request so a test can assert what the
 * filters actually sent — the difference between a control that filters and one
 * that only looks like it does. */
function serve(opts: {
  health?: SystemHealth;
  logs?: PlatformLogPage | ((url: URL) => PlatformLogPage);
  onLogs?: (url: URL) => void;
}) {
  server.use(
    http.get("*/api/v1/system-health", () =>
      HttpResponse.json(opts.health ?? healthBody(), { headers: { "Trace-Id": TRACE_ID } }),
    ),
    http.get("*/api/v1/platform-logs", ({ request }) => {
      const url = new URL(request.url);
      opts.onLogs?.(url);
      const body = typeof opts.logs === "function" ? opts.logs(url) : (opts.logs ?? logPage());
      return HttpResponse.json(body, { headers: { "Trace-Id": TRACE_ID } });
    }),
  );
}

/** The stat card whose label matches. */
function card(label: string): HTMLElement {
  const c = screen.getByText(label).closest('[data-slot="stat-card"]');
  if (!c) throw new Error(`no stat-card for label ${label}`);
  return c as HTMLElement;
}

describe("SystemRoute — health", () => {
  it("renders the derived summary, the disk headroom and every service check", async () => {
    serve({});
    render(<SystemRoute />);

    await waitFor(() => expect(within(card("Overall")).getByText("OK")).toBeInTheDocument());
    expect(within(card("Overall")).getByText(/1\.2\.3 \(stable\)/)).toBeInTheDocument();
    expect(within(card("Overall")).getByText(/up 1h 2m/)).toBeInTheDocument();
    expect(within(card("Disk")).getByText("20 GiB")).toBeInTheDocument();
    expect(within(card("Relays connected")).getByText("1")).toBeInTheDocument();
    expect(within(card("Screens live")).getByText("2 / 2")).toBeInTheDocument();

    const checks = screen.getByRole("list", { name: /service checks/i });
    for (const name of ["store", "content-origin", "relay-plane", "screens"]) {
      expect(within(checks).getByText(name)).toBeInTheDocument();
    }
  });

  it("shows a down component and lets the summary say so", async () => {
    serve({
      health: healthBody({
        status: "down",
        relays: [],
        services: [
          { name: "store", status: "ok", detail: "Readable; 2 authored screen row(s)." },
          {
            name: "relay-plane",
            status: "down",
            detail: "No relay is connected; nothing this console does can reach a screen or a device.",
          },
        ],
      }),
    });
    render(<SystemRoute />);

    await waitFor(() => expect(within(card("Overall")).getByText("Down")).toBeInTheDocument());
    expect(
      within(card("Relays connected")).getByText(/Nothing this console does can reach/),
    ).toBeInTheDocument();
  });

  it("renders an UNMEASURED disk as unknown rather than as an empty one", async () => {
    // A `free_bytes` of 0 on this card would read as a full disk. The server
    // omits the member instead; the page must render that as Unknown.
    serve({
      health: healthBody({
        storage: {
          path: "",
          status: "unknown",
          detail: "This deployment does not publish a data directory, so disk headroom cannot be measured.",
        },
      }),
    });
    render(<SystemRoute />);

    await waitFor(() => expect(within(card("Disk")).getByText("Unknown")).toBeInTheDocument());
    expect(within(card("Disk")).queryByText("0 B")).not.toBeInTheDocument();
  });
});

describe("SystemRoute — the platform log", () => {
  it("renders the lines and says what it is NOT showing", async () => {
    serve({
      logs: logPage({
        items: [
          record({ seq: 9, level: "error", source: "waiveo-relay", message: "ECP command failed after 12s" }),
        ],
        matched: 40,
        retained: 4000,
        dropped: 137,
        level_counts: { error: 3, warn: 1, info: 3996 },
      }),
    });
    render(<SystemRoute />);

    await waitFor(() => expect(screen.getByText("ECP command failed after 12s")).toBeInTheDocument());
    // Scoped to the table: "waiveo-relay" is also an option in the source
    // filter, and an unscoped query would match either and prove neither.
    const table = screen.getByRole("table", { name: /platform log/i });
    expect(within(table).getByText("waiveo-relay")).toBeInTheDocument();

    // The three honesty statements, each one a thing an operator could
    // otherwise wrongly conclude.
    expect(screen.getByText(/Level and source are read out of each line/)).toBeInTheDocument();
    expect(screen.getByText(/not the audit trail/i)).toBeInTheDocument();
    expect(screen.getByText(/137 older line\(s\) have already been overwritten/)).toBeInTheDocument();
    expect(screen.getByText(/read journald on the box/i)).toBeInTheDocument();
    expect(screen.getByText(/Showing 1 of 40 matching/)).toBeInTheDocument();
  });

  it("names the zone its timestamps are in", async () => {
    serve({ logs: logPage({ items: [record({ seq: 1 })] }) });
    render(<SystemRoute />);
    const table = await screen.findByRole("table", { name: /platform log/i });
    // A bare clock time that could be the box's or the reader's is a time that
    // correlates with nothing. The zone is stated once, in the header.
    expect(within(table).getByText(`Time (${displayTimeZone()})`)).toBeInTheDocument();
    expect(within(table).queryByText("Time")).toBeNull();
  });

  it("sends the level, source and text filters to the server", async () => {
    const seen: URL[] = [];
    serve({ onLogs: (url) => seen.push(url) });
    render(<SystemRoute />);
    await waitFor(() => expect(seen.length).toBeGreaterThan(0));

    await userEvent.selectOptions(screen.getByLabelText(/^Level/), "error");
    await waitFor(() => expect(seen.at(-1)?.searchParams.get("level")).toBe("error"));

    await userEvent.selectOptions(screen.getByLabelText(/^Source/), "waiveo-relay");
    await waitFor(() => expect(seen.at(-1)?.searchParams.get("source")).toBe("waiveo-relay"));

    await userEvent.type(screen.getByLabelText(/^Contains/), "ecp");
    await waitFor(() => expect(seen.at(-1)?.searchParams.get("contains")).toBe("ecp"));
    // All three ride together — a control that replaced the others rather than
    // narrowing with them would answer a question nobody asked.
    expect(seen.at(-1)?.searchParams.get("level")).toBe("error");
    expect(seen.at(-1)?.searchParams.get("source")).toBe("waiveo-relay");
  });

  it("never sends an EMPTY filter value, which the server would read as a filter for the empty string", async () => {
    const seen: URL[] = [];
    serve({ onLogs: (url) => seen.push(url) });
    render(<SystemRoute />);
    await waitFor(() => expect(seen.length).toBeGreaterThan(0));
    const first = seen[0]!;
    expect(first.searchParams.has("level")).toBe(false);
    expect(first.searchParams.has("source")).toBe(false);
    expect(first.searchParams.has("contains")).toBe(false);
  });

  it("keeps the filter controls populated when a filter matches nothing, so it can be undone", async () => {
    // The dead end this guards against: the source list is built from results,
    // a filter empties the results, and the control that could clear it
    // disappears with them. The server publishes the WHOLE buffer's sources and
    // counts for exactly this reason, and the page must use them.
    const seen: URL[] = [];
    serve({
      onLogs: (url) => seen.push(url),
      logs: (url) =>
        url.searchParams.get("level") === "error"
          ? logPage({ items: [], matched: 0, retained: 12, level_counts: { error: 0, warn: 2, info: 10 } })
          : logPage(),
    });
    render(<SystemRoute />);
    await waitFor(() => expect(seen.length).toBeGreaterThan(0));

    await userEvent.selectOptions(screen.getByLabelText(/^Level/), "error");
    await waitFor(() => expect(screen.getByText(/Nothing matched this filter/)).toBeInTheDocument());

    // Every source is still offered, and the clear affordance is there.
    const sourceSelect = screen.getByLabelText(/^Source/) as HTMLSelectElement;
    expect(Array.from(sourceSelect.options).map((o) => o.value)).toEqual([
      "",
      "http",
      "waiveo-feeder",
      "waiveo-relay",
    ]);
    await userEvent.click(screen.getByRole("button", { name: /clear the log filters/i }));
    await waitFor(() => expect(seen.at(-1)?.searchParams.has("level")).toBe(false));
  });

  it("explains an owner-only refusal instead of rendering a blank panel", async () => {
    server.use(
      http.get("*/api/v1/system-health", () =>
        problem(403, "FORBIDDEN", "This principal is not an owner of this workspace."),
      ),
      http.get("*/api/v1/platform-logs", () =>
        problem(403, "FORBIDDEN", "This principal is not an owner of this workspace."),
      ),
    );
    render(<SystemRoute />);
    await waitFor(() =>
      expect(screen.getAllByText(/Only the workspace owner can read this box's diagnostics/).length).toBe(2),
    );
    expect(screen.getAllByText(new RegExp(TRACE_ID)).length).toBeGreaterThan(0);
  });

  it("re-reads on the refresh button without waiting for the timer", async () => {
    const seen: URL[] = [];
    serve({ onLogs: (url) => seen.push(url) });
    render(<SystemRoute />);
    await waitFor(() => expect(seen.length).toBe(1));
    await userEvent.click(screen.getByRole("button", { name: /refresh the platform log/i }));
    await waitFor(() => expect(seen.length).toBe(2));
  });

  it("polls while mounted and stops when it unmounts", async () => {
    vi.useFakeTimers();
    try {
      const seen: URL[] = [];
      serve({ onLogs: (url) => seen.push(url) });
      const view = render(<SystemRoute />);
      await vi.advanceTimersByTimeAsync(0);
      const first = seen.length;
      await vi.advanceTimersByTimeAsync(10_000);
      expect(seen.length).toBeGreaterThan(first);
      const beforeUnmount = seen.length;
      view.unmount();
      await vi.advanceTimersByTimeAsync(60_000);
      expect(seen.length).toBe(beforeUnmount);
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("SystemRoute — formatters", () => {
  it("formatBytes reads in the largest unit that stays legible", () => {
    expect(formatBytes(0)).toBe("0 B");
    expect(formatBytes(1023)).toBe("1023 B");
    expect(formatBytes(1024)).toBe("1.0 KiB");
    expect(formatBytes(20 * 1024 ** 3)).toBe("20 GiB");
    expect(formatBytes(-1)).toBe("—");
  });

  it("formatUptime renders the never sentinel as unknown, never as a just-restarted box", () => {
    expect(formatUptime(-1)).toBe("unknown");
    expect(formatUptime(0)).toBe("0s");
    expect(formatUptime(3_723_000)).toBe("1h 2m");
    expect(formatUptime(90_000_000)).toBe("1d 1h");
  });

  it("displayTimeZone names a zone rather than returning an empty string", () => {
    // A blank would render the column as "Time ()", which is worse than the
    // unlabelled column it replaces.
    expect(displayTimeZone()).not.toBe("");
    expect(typeof displayTimeZone()).toBe("string");
  });
});

// ── Restart ──────────────────────────────────────────────────────────────────
//
// These cases DRIVE the control rather than assert it rendered. The console has
// shipped a dead button before — one that painted correctly and did nothing —
// past tests that only checked the markup, so every case here clicks through the
// confirm dialog to the request the server actually receives.
//
// The property they are built around is the one the panel exists to get right:
// a 202 is an ACCEPTANCE, and the page must not report a restart until it has
// evidence of a different process answering.

/** Serve the two reads plus a restart endpoint, recording each POST and each
 * health probe. The probe COUNT is load-bearing: "the page kept watching" can
 * only be asserted against evidence that it went on probing, and a negative
 * assertion taken immediately after the click passes before the first poll has
 * even run — which is exactly how the too-eager version of this page slipped
 * through a mutation. */
function serveRestart(opts: {
  health?: SystemHealth | (() => SystemHealth | Response);
  onRestart: () => Response;
  restarts?: number[];
  probes?: { n: number };
}) {
  server.use(
    http.get("*/api/v1/system-health", () => {
      if (opts.probes) opts.probes.n += 1;
      const h = typeof opts.health === "function" ? opts.health() : (opts.health ?? healthBody());
      if (h instanceof Response) return h;
      return HttpResponse.json(h, { headers: { "Trace-Id": TRACE_ID } });
    }),
    http.get("*/api/v1/platform-logs", () =>
      HttpResponse.json(logPage(), { headers: { "Trace-Id": TRACE_ID } }),
    ),
    http.post("*/api/v1/system/restart", () => {
      opts.restarts?.push(1);
      return opts.onRestart();
    }),
  );
}

/** The acceptance body the server answers a fresh restart with. */
function acceptance(startedAtMs: number) {
  return HttpResponse.json(
    {
      accepted_at_ms: 1_752_537_600_000,
      stopping_in_ms: 250,
      drain_budget_ms: 5_000,
      started_at_ms: startedAtMs,
      supervisor: "systemd",
    },
    { status: 202, headers: { "Trace-Id": TRACE_ID } },
  );
}

/** Click Restart and confirm in the dialog. Deliberately two steps: the point of
 * the dialog is that the first click does NOT restart anything. */
async function clickRestartAndConfirm() {
  await userEvent.click(screen.getByRole("button", { name: /restart this box/i }));
  await userEvent.click(await screen.findByRole("button", { name: /restart now/i }));
}

describe("SystemRoute — restart", () => {
  it("does not restart until the confirm dialog is confirmed", async () => {
    const restarts: number[] = [];
    serveRestart({ onRestart: () => acceptance(1), restarts });
    render(<SystemRoute />);

    await userEvent.click(await screen.findByRole("button", { name: /restart this box/i }));
    // The dialog is open and NOTHING has been sent.
    expect(await screen.findByRole("button", { name: /restart now/i })).toBeInTheDocument();
    expect(restarts).toHaveLength(0);

    // Backing out sends nothing either. A confirm an operator can dismiss and
    // still have acted is not a confirm.
    await userEvent.click(screen.getByRole("button", { name: /not now/i }));
    expect(restarts).toHaveLength(0);
  });

  it("says ACCEPTED and keeps watching — it does not claim the box restarted", async () => {
    const started = 1_752_537_600_000 - 3_723_000;
    const probes = { n: 0 };
    // Health keeps answering with the SAME instance: the process has not stopped
    // yet, which is exactly the state inside the acceptance's grace window.
    serveRestart({ health: () => healthBody({ started_at_ms: started }), onRestart: () => acceptance(started), probes });
    render(<SystemRoute />);
    await screen.findByRole("button", { name: /restart this box/i });

    const before = probes.n;
    await clickRestartAndConfirm();

    // The panel says "asking" until the 202 lands and "accepted" after it —
    // because until the 202 nothing HAS been accepted, and this page does not say
    // a word it cannot stand behind. They are different nodes, so this re-queries
    // rather than holding the first one it saw.
    expect(await screen.findByRole("status")).toHaveTextContent(/Restarting/);
    await waitFor(() =>
      expect(screen.getByRole("status")).toHaveTextContent(/systemd will start it again/i),
    );
    expect(screen.getByRole("status")).toHaveTextContent(/Restarting/);

    // Wait for EVIDENCE that the page went on watching — two probes answered by
    // the same instance — and only then assert it has not claimed a restart.
    // A page that treats a reachable API as proof stops probing at the first
    // one, so this waits out rather than passing vacuously.
    await waitFor(() => expect(probes.n).toBeGreaterThanOrEqual(before + 2), { timeout: 8_000 });
    expect(screen.queryByText(/Back up/)).not.toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent(/Restarting/);
  }, 15_000);

  it("reports BACK UP only once a DIFFERENT process instance answers", async () => {
    const before = 1_752_537_600_000 - 3_723_000;
    const after = 1_752_537_600_000 - 2_000;
    let restarted = false;
    serveRestart({
      health: () => healthBody({ started_at_ms: restarted ? after : before }),
      onRestart: () => {
        restarted = true;
        return acceptance(before);
      },
    });
    render(<SystemRoute />);
    await screen.findByRole("button", { name: /restart this box/i });

    await clickRestartAndConfirm();

    expect(await screen.findByText(/Back up/, {}, { timeout: 5_000 })).toBeInTheDocument();
    expect(screen.getByText(/restarted and is answering again/i)).toBeInTheDocument();
  }, 10_000);

  it("renders each published refusal as its own sentence, and stays idle", async () => {
    for (const [code, status, needle] of [
      ["RESTART_UNSUPPORTED", 501, /nothing is configured to start it again/i],
      ["RESTART_IN_PROGRESS", 409, /already under way/i],
      ["RESTART_BLOCKED", 409, /busy with work a restart would break/i],
      ["FORBIDDEN", 403, /only the workspace owner can restart/i],
    ] as const) {
      serveRestart({
        onRestart: () => problem(status, code, `server detail for ${code}`),
      });
      const view = render(<SystemRoute />);
      await screen.findByRole("button", { name: /restart this box/i });

      await clickRestartAndConfirm();

      const alert = await screen.findByRole("alert");
      expect(alert, code).toHaveTextContent(needle);
      // Never left mid-flight: a refusal returns the control to the operator.
      expect(screen.getByRole("button", { name: /restart this box/i })).toBeEnabled();
      view.unmount();
      server.resetHandlers();
    }
  }, 20_000);

  it("gives up out loud rather than spinning forever", async () => {
    const started = 1_752_537_600_000 - 3_723_000;
    // The box accepted and never came back: every later probe fails.
    serveRestart({
      health: () => HttpResponse.error(),
      onRestart: () => acceptance(started),
    });
    render(<SystemRoute />);
    await screen.findByRole("button", { name: /restart this box/i });
    await clickRestartAndConfirm();
    await screen.findByRole("status");

    // The give-up bound is 90s of WALL time, which no test should wait out. The
    // clock is moved instead of the test — `Date.now` is what the watcher
    // measures the wait against, and the poll interval keeps running for real,
    // so the very next probe crosses the bound. Faking the whole timer set
    // instead would also stop msw's own fetch machinery, which is how the
    // straightforward version of this test hung.
    const realNow = Date.now;
    try {
      const jumped = realNow() + RESTART_GIVEUP_MS + 1_000;
      vi.spyOn(Date, "now").mockImplementation(() => jumped);
      const alert = await screen.findByRole("alert", {}, { timeout: 5_000 });
      expect(alert).toHaveTextContent(/has not come back in 90s/i);
      expect(alert).toHaveTextContent(/systemctl status waiveo-feeder/);
    } finally {
      vi.restoreAllMocks();
    }
  }, 20_000);
});

// ── The panel as a ui-schema/1 document ──────────────────────────────────────
//
// The restart panel is authored as `restart-panel.uis.json` and painted by the
// same PageRenderer an extension page goes through. The cases above already
// prove the BEHAVIOUR survived that move — they were not changed for it, beyond
// re-querying one live region across a transition. These add what only the new
// authoring makes checkable.

describe("SystemRoute — the restart panel is a ui-schema/1 document", () => {
  it("the document is conformant, so a broken one fails HERE and not as a mystery blank panel", () => {
    // Without this the renderer would paint its rejection panel, and the restart
    // cases above would fail with "no button named /restart this box/" — true,
    // unhelpful, and several inferences away from the actual mistake.
    const result = validatePage(restartPanelDoc);
    expect(result.ok ? [] : result.errors).toEqual([]);
  });

  it("says ASKING until the 202 lands, and only then says accepted", async () => {
    // The page's honesty rule, now visible as two distinct sentences: before the
    // acceptance nothing has been accepted, so it does not use the word.
    const started = 1_752_537_600_000 - 3_723_000;
    let release: () => void = () => {};
    const held = new Promise<void>((resolve) => {
      release = resolve;
    });
    server.use(
      http.get("*/api/v1/system-health", () =>
        HttpResponse.json(healthBody({ started_at_ms: started }), { headers: { "Trace-Id": TRACE_ID } }),
      ),
      http.get("*/api/v1/platform-logs", () =>
        HttpResponse.json(logPage(), { headers: { "Trace-Id": TRACE_ID } }),
      ),
      http.post("*/api/v1/system/restart", async () => {
        await held;
        return acceptance(started);
      }),
    );
    render(<SystemRoute />);
    await screen.findByRole("button", { name: /restart this box/i });
    await clickRestartAndConfirm();

    // Pending, and the control is out of reach — a restart cannot be asked twice.
    expect(await screen.findByRole("status")).toHaveTextContent(/asking this box to stop and start again/i);
    expect(screen.getByRole("button", { name: /restart this box/i })).toBeDisabled();

    release();
    await waitFor(() =>
      expect(screen.getByRole("status")).toHaveTextContent(/Accepted — systemd will start it again/i),
    );
  }, 15_000);

  it("renders the refusal's trace id, so a support conversation has the one identifier that matters", async () => {
    serveRestart({ onRestart: () => problem(409, "RESTART_BLOCKED", "a media import is running") });
    render(<SystemRoute />);
    await screen.findByRole("button", { name: /restart this box/i });
    await clickRestartAndConfirm();

    expect(await screen.findByRole("alert")).toHaveTextContent(/a media import is running/);
    expect(screen.getByText(`trace ${TRACE_ID}`)).toBeInTheDocument();
  }, 15_000);

  it("stops watching when the page unmounts, rather than polling a downed box to nobody", async () => {
    // The watcher is a loop inside a promise, not an effect, so it has no cleanup
    // of its own. This is the case that proves it got one anyway.
    const started = 1_752_537_600_000 - 3_723_000;
    const probes = { n: 0 };
    serveRestart({
      health: () => healthBody({ started_at_ms: started }),
      onRestart: () => acceptance(started),
      probes,
    });
    const view = render(<SystemRoute />);
    await screen.findByRole("button", { name: /restart this box/i });
    await clickRestartAndConfirm();
    await waitFor(() =>
      expect(screen.getByRole("status")).toHaveTextContent(/systemd will start it again/i),
    );

    // Wait out one watcher probe so the loop is demonstrably running…
    const running = probes.n;
    await waitFor(() => expect(probes.n).toBeGreaterThan(running), { timeout: 8_000 });
    view.unmount();

    // …then give it two more poll intervals to prove it has stopped.
    const atUnmount = probes.n;
    await new Promise((r) => setTimeout(r, 3_500));
    expect(probes.n).toBe(atUnmount);
  }, 20_000);
});
