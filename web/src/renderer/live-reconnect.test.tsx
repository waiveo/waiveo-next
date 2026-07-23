import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, render, screen } from "@testing-library/react";
import { PageRenderer } from "./PageRenderer";
import {
  LiveHub,
  LiveProvider,
  useLive,
  useLiveConnectionStatus,
  type EventSourceFactory,
  type EventSourceLike,
} from "./live";

// A backend restart (the feeder bounces; every open SSE connection 502s at once)
// must NOT storm the door with a tight reconnect loop or flood the console. These
// tests drive the LiveHub directly over a controllable fake EventSource — asserting
// capped/jittered backoff between reconnects, that the native EventSource is CLOSED
// on error (so the browser's own auto-reconnect can't hammer), clean teardown with
// no leaked source, and automatic recovery when the backend returns.
class FakeES implements EventSourceLike {
  static all: FakeES[] = [];
  closed = false;
  private readonly listeners = new Map<string, Set<(ev: MessageEvent) => void>>();
  constructor(public readonly url: string) {
    FakeES.all.push(this);
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
  emit(type: string, data?: unknown): void {
    const ev = { data: data === undefined ? "" : JSON.stringify(data) } as MessageEvent;
    this.listeners.get(type)?.forEach((cb) => cb(ev));
  }
}

const factory: EventSourceFactory = (url) => new FakeES(url);

beforeEach(() => {
  FakeES.all = [];
  vi.useFakeTimers();
});
afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("LiveHub — reconnect backoff, teardown, recovery (UIS-110)", () => {
  it("backs off exponentially (capped, jittered) between reconnects — no tight loop", () => {
    const setTimeoutSpy = vi.spyOn(globalThis, "setTimeout");
    // random pinned to the top of the jitter window → the delay equals the capped
    // ceiling, so the back-off envelope is a deterministic, assertable sequence.
    const hub = new LiveHub(factory, "/events/v1", { baseDelayMs: 100, maxDelayMs: 2000, random: () => 1 });
    const unsub = hub.subscribe("x", () => {});
    expect(FakeES.all).toHaveLength(1); // opened exactly one connection

    const delays: number[] = [];
    for (let i = 0; i < 6; i++) {
      const current = FakeES.all[FakeES.all.length - 1];
      setTimeoutSpy.mockClear();
      current.emit("error");
      // The native EventSource is CLOSED on error — we take reconnection away from
      // the browser's own storming auto-reconnect.
      expect(current.closed).toBe(true);
      // The reconnect is SCHEDULED on a timer (not run synchronously → no hot loop).
      const call = setTimeoutSpy.mock.calls.at(-1);
      expect(call).toBeTruthy();
      delays.push(call![1] as number);
      act(() => vi.advanceTimersByTime(call![1] as number));
    }

    // Increasing then capped at maxDelayMs — a bounded, decorrelated cadence.
    expect(delays).toEqual([100, 200, 400, 800, 1600, 2000]);
    // Each reconnect made a fresh connection (it keeps trying to recover).
    expect(FakeES.all).toHaveLength(7);
    unsub();
  });

  it("resets the backoff after a successful reconnect", () => {
    const setTimeoutSpy = vi.spyOn(globalThis, "setTimeout");
    const hub = new LiveHub(factory, "/events/v1", { baseDelayMs: 100, maxDelayMs: 2000, random: () => 1 });
    hub.subscribe("x", () => {});

    // Two failures grow the delay: 100 → 200.
    FakeES.all[0].emit("error");
    act(() => vi.advanceTimersByTime(100));
    FakeES.all[1].emit("error");
    setTimeoutSpy.mockClear();
    act(() => vi.advanceTimersByTime(200));
    // The 3rd connection opens successfully → backoff is reset.
    FakeES.all[2].emit("open");
    expect(hub.getStatus()).toBe("live");

    // A fresh drop now schedules from the BASE delay again (not the grown 400).
    setTimeoutSpy.mockClear();
    FakeES.all[2].emit("error");
    expect(setTimeoutSpy.mock.calls.at(-1)![1]).toBe(100);
  });

  it("surfaces connecting → reconnecting → live status transitions", () => {
    const seen: string[] = [];
    const hub = new LiveHub(factory, "/events/v1", { baseDelayMs: 100, maxDelayMs: 2000, random: () => 1 });
    hub.onStatus((s) => seen.push(s));
    hub.subscribe("x", () => {});
    expect(hub.getStatus()).toBe("connecting");

    FakeES.all[0].emit("error");
    expect(hub.getStatus()).toBe("reconnecting");
    // The retry stays in the "reconnecting" surface (not a flip back to
    // "connecting") until the backend actually returns.
    act(() => vi.advanceTimersByTime(100));
    expect(hub.getStatus()).toBe("reconnecting");
    FakeES.all[1].emit("open");
    expect(hub.getStatus()).toBe("live");

    expect(seen).toEqual(["connecting", "reconnecting", "live"]);
  });

  it("closes the source and cancels a pending reconnect on teardown — no leak", () => {
    const hub = new LiveHub(factory, "/events/v1", { baseDelayMs: 100, maxDelayMs: 2000, random: () => 1 });
    hub.subscribe("x", () => {});
    const es0 = FakeES.all[0];
    es0.emit("error"); // schedules a reconnect
    const openedCount = FakeES.all.length;

    hub.closeAll();
    expect(es0.closed).toBe(true);
    // The scheduled reconnect must NOT fire after teardown — no leaked EventSource.
    act(() => vi.advanceTimersByTime(60_000));
    expect(FakeES.all).toHaveLength(openedCount);
    expect(hub.getStatus()).toBe("idle");
  });

  it("closes the live source when the last subscriber unsubscribes", () => {
    const hub = new LiveHub(factory, "/events/v1", { baseDelayMs: 100, maxDelayMs: 2000, random: () => 1 });
    const unsub = hub.subscribe("x", () => {});
    const es = FakeES.all[0];
    unsub();
    expect(es.closed).toBe(true);
  });

  it("does not flood the console on a reconnect storm (bounded error surface)", () => {
    const err = vi.spyOn(console, "error").mockImplementation(() => {});
    const hub = new LiveHub(factory, "/events/v1", { baseDelayMs: 100, maxDelayMs: 2000, random: () => 1 });
    hub.subscribe("x", () => {});
    for (let i = 0; i < 8; i++) {
      const current = FakeES.all[FakeES.all.length - 1];
      current.emit("error");
      act(() => vi.advanceTimersByTime(2000));
    }
    // Our engine never logs per-error — the reconnecting STATE is the surface.
    expect(err).not.toHaveBeenCalled();
  });
});

const liveDoc = {
  pageType: "dashboard",
  tiles: [
    {
      size: "small",
      widget: {
        type: "stat-tile",
        props: { labelMsg: "msg:dashboard.currentTemp", value: { path: "current_temperature", live: true } },
      },
    },
  ],
};

describe("PageRenderer live binding — degrades to the last value across a restart, then recovers", () => {
  it("holds the last pushed value while reconnecting, then updates when the backend returns", () => {
    render(<PageRenderer doc={liveDoc} data={{ current_temperature: 68 }} eventSourceFactory={factory} />);
    expect(screen.getByText("68")).toBeInTheDocument();

    // A push updates the live value.
    act(() => FakeES.all[0].emit("message", { path: "current_temperature", value: 72 }));
    expect(screen.getByText("72")).toBeInTheDocument();

    // Backend restart: the stream errors. The binding degrades to the LAST value
    // (72) — it does not blank out or crash.
    act(() => FakeES.all[0].emit("error"));
    expect(screen.getByText("72")).toBeInTheDocument();
    expect(FakeES.all[0].closed).toBe(true);

    // Automatic recovery: the reconnect fires and a fresh push updates the value.
    act(() => vi.advanceTimersByTime(2000));
    const reconnected = FakeES.all[FakeES.all.length - 1];
    act(() => reconnected.emit("message", { path: "current_temperature", value: 75 }));
    expect(screen.getByText("75")).toBeInTheDocument();
  });
});

function StatusProbe() {
  const value = useLive("x", "init");
  const status = useLiveConnectionStatus();
  return (
    <div>
      <span data-testid="v">{String(value)}</span>
      <span data-testid="s">{status}</span>
    </div>
  );
}

describe("useLiveConnectionStatus — the quiet reconnecting surface under a LiveProvider", () => {
  it("reads connecting → live → reconnecting → live while the binding holds its last value", () => {
    render(
      <LiveProvider factory={factory} options={{ baseDelayMs: 100, maxDelayMs: 2000, random: () => 1 }}>
        <StatusProbe />
      </LiveProvider>,
    );
    // A live binding is mounted → the hub is connecting.
    expect(screen.getByTestId("s")).toHaveTextContent("connecting");

    act(() => FakeES.all[0].emit("message", { path: "x", value: "hello" }));
    expect(screen.getByTestId("s")).toHaveTextContent("live");
    expect(screen.getByTestId("v")).toHaveTextContent("hello");

    // Backend restart: reconnecting, but the binding keeps its last value.
    act(() => FakeES.all[0].emit("error"));
    expect(screen.getByTestId("s")).toHaveTextContent("reconnecting");
    expect(screen.getByTestId("v")).toHaveTextContent("hello");

    // Recovery.
    act(() => vi.advanceTimersByTime(100));
    act(() => FakeES.all[FakeES.all.length - 1].emit("open"));
    expect(screen.getByTestId("s")).toHaveTextContent("live");
  });
});
