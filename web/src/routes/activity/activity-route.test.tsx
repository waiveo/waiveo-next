import { describe, it, expect, afterEach } from "vitest";
import { render, screen, within, waitFor, act } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import ActivityRoute from "./activity-route";
import type { ActivityEventSource, ActivityEventSourceFactory } from "./activity-feed";
import { ULID_A, ULID_B, ULID_C, ULID_ROOT } from "@/api/test-support";

// The Activity feed is the console's live window on the platform: an EventSource
// over the real /events/v1 SSE door (events/1). It is one of the three sanctioned
// first-party grammar gaps (a live stream has no ui-schema/1 expression). These
// tests drive the real route against a FAKE EventSource so live rows, the
// Last-Event-ID resume on reconnect, the server gap marker, and pause/resume are
// all exercised without a socket — no network, fixture ULIDs only.

// A fake EventSource satisfying the feed's minimal ActivityEventSource surface:
// it records the URL it was opened with (so a resume reconnect is observable) and
// lets a test drive open/error/event/gap frames as the server would.
class FakeEventSource implements ActivityEventSource {
  readonly url: string;
  closed = false;
  private readonly listeners = new Map<string, Set<(ev: MessageEvent) => void>>();

  constructor(url: string, registry: FakeEventSource[]) {
    this.url = url;
    registry.push(this);
  }

  addEventListener(type: string, listener: (ev: MessageEvent) => void): void {
    let set = this.listeners.get(type);
    if (!set) {
      set = new Set();
      this.listeners.set(type, set);
    }
    set.add(listener);
  }

  removeEventListener(type: string, listener: (ev: MessageEvent) => void): void {
    this.listeners.get(type)?.delete(listener);
  }

  close(): void {
    this.closed = true;
  }

  private dispatch(type: string, ev: MessageEvent): void {
    this.listeners.get(type)?.forEach((cb) => cb(ev));
  }

  emitOpen(): void {
    act(() => this.dispatch("open", new MessageEvent("open")));
  }
  emitError(): void {
    act(() => this.dispatch("error", new MessageEvent("error")));
  }
  emitEvent(env: Record<string, unknown>): void {
    const id = String(env.id ?? "");
    act(() =>
      this.dispatch("event", new MessageEvent("event", { data: JSON.stringify(env), lastEventId: id })),
    );
  }
  emitGap(gap: { from_id: string | null; to_id: string; reason: string }): void {
    act(() =>
      this.dispatch(
        "gap",
        new MessageEvent("gap", { data: JSON.stringify(gap), lastEventId: gap.to_id }),
      ),
    );
  }
}

function fakeFactory(): { factory: ActivityEventSourceFactory; instances: FakeEventSource[] } {
  const instances: FakeEventSource[] = [];
  const factory: ActivityEventSourceFactory = (url) => new FakeEventSource(url, instances);
  return { factory, instances };
}

// Fixture envelopes (events/1 EVT-010 shape) — fixture ULIDs only.
function automationRun(id: string, disposition: string): Record<string, unknown> {
  return {
    id,
    schema: "automation.run",
    ts: 1_700_000_000_000,
    scope_node: ULID_ROOT,
    trace_id: ULID_C,
    cost_class: "telemetry",
    retention_class: "telemetry-standard",
    origin: "relay",
    payload: {
      rule_id: ULID_A,
      rule_revision: 3,
      trigger_snapshot: {},
      condition_results: [],
      action_outcomes: [],
      mode_disposition: disposition,
      misfire_caught: false,
    },
  };
}

function entityStateChanged(id: string): Record<string, unknown> {
  return {
    id,
    schema: "entity.state_changed",
    ts: 1_700_000_000_100,
    scope_node: ULID_ROOT,
    trace_id: ULID_C,
    cost_class: "telemetry",
    retention_class: "telemetry-standard",
    origin: "relay",
    payload: {
      entity_id: ULID_B,
      device_id: ULID_A,
      old_state: "off",
      new_state: "on",
      attribute_change: false,
    },
  };
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

afterEach(() => setViewport(false));

// A ULID for an event id — keeps ids sortable + distinct, no real identifiers.
const EVT1 = "01J8Z3EVENT000000000000001";
const EVT2 = "01J8Z3EVENT000000000000002";
const GAP_TO = "01J8Z3EVENT000000000000009";

describe("ActivityRoute — live stream over /events/v1", () => {
  it("streams live rows with a schema badge, a time, and a per-schema summary", () => {
    const { factory, instances } = fakeFactory();
    render(<ActivityRoute factory={factory} reconnectDelayMs={0} />);

    expect(instances).toHaveLength(1);
    instances[0].emitOpen();
    instances[0].emitEvent(automationRun(EVT1, "ran"));
    instances[0].emitEvent(entityStateChanged(EVT2));

    // Newest-first: two rows, each carrying its schema as a badge.
    const feed = screen.getByRole("log", { name: /activity/i });
    const rows = within(feed).getAllByRole("listitem");
    expect(rows).toHaveLength(2);

    // The automation.run row summarises rule + disposition.
    const autoRow = rows.find((r) => within(r).queryByText("automation.run")) as HTMLElement;
    expect(autoRow).toBeTruthy();
    expect(within(autoRow).getByText(/rule/i)).toBeInTheDocument();
    expect(within(autoRow).getByText("Ran")).toBeInTheDocument();
    // A machine-readable time is present (as a <time> element).
    expect(autoRow.querySelector("time")).not.toBeNull();

    // The entity.state_changed row summarises the transition.
    const entRow = rows.find((r) => within(r).queryByText("entity.state_changed")) as HTMLElement;
    expect(entRow).toBeTruthy();
    expect(within(entRow).getByText(/off\s*→\s*on/)).toBeInTheDocument();
  });

  it("shows the connection as live once the stream opens", () => {
    const { factory, instances } = fakeFactory();
    render(<ActivityRoute factory={factory} reconnectDelayMs={0} />);
    instances[0].emitOpen();
    expect(screen.getByText("Live")).toBeInTheDocument();
  });

  it("resumes from the last event id when the connection drops and reconnects", async () => {
    const { factory, instances } = fakeFactory();
    render(<ActivityRoute factory={factory} reconnectDelayMs={0} />);

    // First connection is fresh (no resume_from).
    expect(instances[0].url).not.toContain("resume_from");
    instances[0].emitOpen();
    instances[0].emitEvent(automationRun(EVT1, "ran"));

    // The stream drops.
    instances[0].emitError();

    // The feed reconnects, carrying the last seen id as resume_from (the
    // Last-Event-ID contract, EVT-102, as the query-param a browser can set).
    await waitFor(() => expect(instances).toHaveLength(2));
    expect(instances[1].url).toContain("resume_from=");
    expect(instances[1].url).toContain(EVT1);
    // The row seen before the drop survives the reconnect.
    expect(screen.getByText("automation.run")).toBeInTheDocument();
  });

  it("shows a visible gap indicator when the server signals a gap", () => {
    const { factory, instances } = fakeFactory();
    render(<ActivityRoute factory={factory} reconnectDelayMs={0} />);
    instances[0].emitOpen();
    instances[0].emitEvent(automationRun(EVT1, "ran"));
    instances[0].emitGap({ from_id: EVT1, to_id: GAP_TO, reason: "retention_expired" });

    const gap = screen.getByTestId("activity-gap");
    expect(gap).toBeInTheDocument();
    expect(gap).toHaveTextContent(/missed/i);
    expect(gap).toHaveTextContent(/retention_expired/i);
  });

  it("pauses and resumes the stream", async () => {
    const user = userEvent.setup();
    const { factory, instances } = fakeFactory();
    render(<ActivityRoute factory={factory} reconnectDelayMs={0} />);
    instances[0].emitOpen();
    instances[0].emitEvent(automationRun(EVT1, "ran"));

    await user.click(screen.getByRole("button", { name: /pause/i }));
    // The live connection is torn down and the state reads paused.
    expect(instances[0].closed).toBe(true);
    expect(screen.getByText("Paused")).toBeInTheDocument();

    // Resume reconnects (from the last id) and offers the pause control again.
    await user.click(screen.getByRole("button", { name: /resume/i }));
    await waitFor(() => expect(instances).toHaveLength(2));
    expect(instances[1].url).toContain(EVT1);
    expect(screen.getByRole("button", { name: /pause/i })).toBeInTheDocument();
  });

  it("shows an empty state while connected but idle", () => {
    const { factory, instances } = fakeFactory();
    render(<ActivityRoute factory={factory} reconnectDelayMs={0} />);
    instances[0].emitOpen();
    // No events yet.
    expect(screen.getByText(/listening for activity/i)).toBeInTheDocument();
    const feed = screen.queryByRole("log", { name: /activity/i });
    if (feed) expect(within(feed).queryAllByRole("listitem")).toHaveLength(0);
  });

  it("surfaces a degraded banner when the live connection is lost", () => {
    const { factory, instances } = fakeFactory();
    // A long reconnect delay so the degraded state is observable (not instantly
    // replaced by the next connecting attempt).
    render(<ActivityRoute factory={factory} reconnectDelayMs={1_000_000} />);
    instances[0].emitOpen();
    instances[0].emitError();
    const banner = screen.getByRole("status");
    expect(banner).toHaveTextContent(/reconnect|lost/i);
  });

  it("degrades gracefully when EventSource is unavailable", () => {
    render(<ActivityRoute factory={null} reconnectDelayMs={0} />);
    // No crash; a plain notice, no live rows. (`.` matches the typographic
    // apostrophe in "aren’t".)
    expect(screen.getByText(/aren.t available/i)).toBeInTheDocument();
  });
});

describe("ActivityRoute — a11y + responsive", () => {
  it("is a labeled live-log region with a labeled, touch-sized pause control", () => {
    const { factory, instances } = fakeFactory();
    render(<ActivityRoute factory={factory} reconnectDelayMs={0} />);
    instances[0].emitOpen();
    const feed = screen.getByRole("log", { name: /activity/i });
    expect(feed).toHaveAttribute("aria-live");
    const pause = screen.getByRole("button", { name: /pause/i });
    expect(pause.className).toContain("wv-touch");
  });

  it("stacks at 360px with no wide table forcing horizontal scroll", () => {
    setViewport(true);
    const { factory, instances } = fakeFactory();
    render(<ActivityRoute factory={factory} reconnectDelayMs={0} />);
    instances[0].emitOpen();
    instances[0].emitEvent(automationRun(EVT1, "ran"));
    // The stream is a stacked list, never a <table> that would blow out 360px.
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
  });
});
