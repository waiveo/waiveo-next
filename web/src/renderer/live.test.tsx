import { describe, expect, it } from "vitest";
import { act, render, screen } from "@testing-library/react";
import { PageRenderer } from "./PageRenderer";
import type { EventSourceFactory, EventSourceLike } from "./live";

// A fake EventSource: it records itself so a test can push a frame as the real
// `/events/v1` stream would (UIS-110), driving a LiveBinding re-evaluation
// without a network. This is the only injected seam the live path needs.
class FakeEventSource implements EventSourceLike {
  static instances: FakeEventSource[] = [];
  private listeners = new Map<string, Set<(ev: MessageEvent) => void>>();
  closed = false;
  constructor(public url: string) {
    FakeEventSource.instances.push(this);
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
  emit(data: unknown): void {
    const ev = { data: JSON.stringify(data) } as MessageEvent;
    this.listeners.get("message")?.forEach((cb) => cb(ev));
  }
}

const liveDoc = {
  pageType: "dashboard",
  tiles: [
    {
      size: "small",
      widget: {
        type: "stat-tile",
        // A LiveBinding value (UIS-109): re-evaluates over events/1 rather than
        // being fetched once.
        props: { labelMsg: "msg:dashboard.currentTemp", value: { path: "current_temperature", live: true } },
      },
    },
  ],
};

describe("PageRenderer — LiveBinding over a fake EventSource (UIS-109/110)", () => {
  it("shows the once-fetched value, then updates when an event mutates the bound value", () => {
    FakeEventSource.instances = [];
    const factory: EventSourceFactory = (url) => new FakeEventSource(url);
    render(<PageRenderer doc={liveDoc} data={{ current_temperature: 68 }} eventSourceFactory={factory} />);

    // Once-fetched static value first.
    expect(screen.getByText("68")).toBeInTheDocument();
    // The hub opened exactly one connection to the real events door.
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(FakeEventSource.instances[0].url).toBe("/events/v1");

    // A pushed frame naming the bound path mutates the displayed value.
    act(() => FakeEventSource.instances[0].emit({ path: "current_temperature", value: 72 }));
    expect(screen.getByText("72")).toBeInTheDocument();
    expect(screen.queryByText("68")).not.toBeInTheDocument();
  });

  it("degrades gracefully to the static value when the stream cannot be opened", () => {
    const throwingFactory: EventSourceFactory = () => {
      throw new Error("no stream here");
    };
    // The connection failure is swallowed; the once-fetched value stands, no crash.
    render(<PageRenderer doc={liveDoc} data={{ current_temperature: 55 }} eventSourceFactory={throwingFactory} />);
    expect(screen.getByText("55")).toBeInTheDocument();
  });
});
