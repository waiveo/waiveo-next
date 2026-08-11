import { describe, it, expect, beforeAll, afterAll, afterEach, vi } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import SystemRoute, { formatBytes, formatUptime } from "./system-route";
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
      stale: 0,
      never_seen: 0,
      paired: 2,
      overridden: 0,
      // Derived server-side from the player's own loop timings; see
      // internal/shared/wire/screencadence.go. Kept current here because a
      // fixture carrying a withdrawn number is a fixture that has stopped
      // describing the system it stands in for.
      live_window_ms: 52_000,
      // The fetching/stale line, republished alongside the fetching COUNT. The
      // roll-up carried the count and not the line for a round, which left a
      // consumer no way to reinterpret it.
      content_transfer_window_ms: 172_000,
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
});
