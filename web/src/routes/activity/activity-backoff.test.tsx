import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, screen, cleanup } from "@testing-library/react";
import ActivityRoute from "./activity-route";
import type { ActivityEventSource, ActivityEventSourceFactory } from "./activity-feed";

// A backend restart 502s every open SSE connection at once. The Activity feed must
// meet that with a bounded, decorrelated reconnect cadence — capped exponential
// backoff with jitter — not a fixed-interval tight loop, while showing a calm
// "Reconnecting" indicator that returns to "Live" the moment the backend does.
// Driven entirely over a fake EventSource with a deterministic clock + jitter.
class FakeES implements ActivityEventSource {
  readonly url: string;
  closed = false;
  private readonly listeners = new Map<string, Set<(ev: MessageEvent) => void>>();
  constructor(url: string, registry: FakeES[]) {
    this.url = url;
    registry.push(this);
  }
  addEventListener(type: string, cb: (ev: MessageEvent) => void): void {
    let set = this.listeners.get(type);
    if (!set) {
      set = new Set();
      this.listeners.set(type, set);
    }
    set.add(cb);
  }
  removeEventListener(type: string, cb: (ev: MessageEvent) => void): void {
    this.listeners.get(type)?.delete(cb);
  }
  close(): void {
    this.closed = true;
  }
  dispatch(type: string): void {
    this.listeners.get(type)?.forEach((cb) => cb(new MessageEvent(type)));
  }
}

function fakeFactory(): { factory: ActivityEventSourceFactory; instances: FakeES[] } {
  const instances: FakeES[] = [];
  return { factory: (url) => new FakeES(url, instances), instances };
}

beforeEach(() => vi.useFakeTimers());
afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
});

/** setTimeout delays scheduled during `fn`, ignoring React's own zero-delay
 * scheduler timers — the reconnect backoff is the only positive-delay timer the
 * feed sets. */
function delaysDuring(spy: ReturnType<typeof vi.spyOn>, fn: () => void): number[] {
  spy.mockClear();
  fn();
  return (spy.mock.calls as unknown[][])
    .map((c) => c[1] as number)
    .filter((d) => typeof d === "number" && d >= 1);
}

describe("ActivityRoute — reconnect backoff (no storm on a backend restart)", () => {
  it("backs off exponentially and caps between repeated failed reconnects", () => {
    const setTimeoutSpy = vi.spyOn(globalThis, "setTimeout");
    const { factory, instances } = fakeFactory();
    // random pinned to the top of the jitter window → delay == the capped ceiling,
    // so the back-off envelope is assertable; base 100ms, cap 2000ms.
    render(
      <ActivityRoute factory={factory} reconnectDelayMs={100} maxReconnectDelayMs={2000} random={() => 1} />,
    );
    act(() => instances[0].dispatch("open"));

    const delays: number[] = [];
    for (let i = 0; i < 6; i++) {
      const current = instances[instances.length - 1];
      const seen = delaysDuring(setTimeoutSpy, () => act(() => current.dispatch("error")));
      // Exactly one reconnect timer is scheduled per drop (no tight loop).
      expect(current.closed).toBe(true);
      delays.push(seen[seen.length - 1]);
      act(() => vi.advanceTimersByTime(delays[delays.length - 1]));
    }

    expect(delays).toEqual([100, 200, 400, 800, 1600, 2000]);
    // Increasing until the cap, then flat — a bounded cadence, never runaway.
    for (let i = 1; i < 5; i++) expect(delays[i]).toBeGreaterThan(delays[i - 1]);
    expect(Math.max(...delays)).toBe(2000);
  });

  it("shows Reconnecting during the backoff, then Live when the backend returns", () => {
    const { factory, instances } = fakeFactory();
    render(
      <ActivityRoute factory={factory} reconnectDelayMs={100} maxReconnectDelayMs={2000} random={() => 1} />,
    );
    act(() => instances[0].dispatch("open"));
    expect(screen.getByText("Live")).toBeInTheDocument();

    // Backend restart: the stream drops. A calm "Reconnecting" indicator + banner
    // appear — not a console flood.
    act(() => instances[0].dispatch("error"));
    expect(screen.getByText("Reconnecting")).toBeInTheDocument();
    expect(screen.getByRole("status")).toHaveTextContent(/reconnect/i);

    // Automatic recovery: the backed-off reconnect fires and the backend returns.
    act(() => vi.advanceTimersByTime(100));
    const reconnected = instances[instances.length - 1];
    act(() => reconnected.dispatch("open"));
    expect(screen.getByText("Live")).toBeInTheDocument();
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("resets the backoff after a successful reconnect", () => {
    const setTimeoutSpy = vi.spyOn(globalThis, "setTimeout");
    const { factory, instances } = fakeFactory();
    render(
      <ActivityRoute factory={factory} reconnectDelayMs={100} maxReconnectDelayMs={2000} random={() => 1} />,
    );
    act(() => instances[0].dispatch("open"));

    // Two failures grow the delay 100 → 200.
    let d = delaysDuring(setTimeoutSpy, () => act(() => instances[0].dispatch("error")));
    expect(d[d.length - 1]).toBe(100);
    act(() => vi.advanceTimersByTime(100));
    d = delaysDuring(setTimeoutSpy, () => act(() => instances[1].dispatch("error")));
    expect(d[d.length - 1]).toBe(200);
    act(() => vi.advanceTimersByTime(200));

    // A successful open resets the backoff; the next drop starts from the base again.
    act(() => instances[2].dispatch("open"));
    d = delaysDuring(setTimeoutSpy, () => act(() => instances[2].dispatch("error")));
    expect(d[d.length - 1]).toBe(100);
  });

  it("closes the source and cancels the pending reconnect on unmount — no leak", () => {
    const { factory, instances } = fakeFactory();
    const { unmount } = render(
      <ActivityRoute factory={factory} reconnectDelayMs={100} maxReconnectDelayMs={2000} random={() => 1} />,
    );
    act(() => instances[0].dispatch("open"));
    act(() => instances[0].dispatch("error")); // schedules a reconnect
    const openedCount = instances.length;

    unmount();
    expect(instances[0].closed).toBe(true);
    // The pending reconnect must NOT open a new source after unmount.
    act(() => vi.advanceTimersByTime(60_000));
    expect(instances).toHaveLength(openedCount);
  });
});
