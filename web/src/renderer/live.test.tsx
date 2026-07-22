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

// A `table`'s `source` and a `repeat`'s `bind` are Binding-typed (UIS-070) and so
// MAY be LiveBindings (UIS-066/109). Both must subscribe over events/1 (UIS-110),
// not just read the once-fetched value — otherwise a live-bound collection freezes
// at its first snapshot with no indication liveness was silently dropped.
describe("PageRenderer — a table's LiveBinding source re-renders on a push (UIS-066/109/110)", () => {
  const tableLiveDoc = {
    pageType: "dashboard",
    tiles: [
      {
        size: "large",
        widget: {
          type: "table",
          props: { source: { path: "events", live: true }, columns: [{ headerMsg: "msg:name", cell: "item.name" }] },
        },
      },
    ],
  };

  it("renders the bound rows, then appends a pushed row without a refetch", () => {
    FakeEventSource.instances = [];
    const factory: EventSourceFactory = (url) => new FakeEventSource(url);
    render(
      <PageRenderer
        doc={tableLiveDoc}
        data={{ events: [{ name: "Alpha" }, { name: "Beta" }] }}
        messages={{ "msg:name": "Name" }}
        eventSourceFactory={factory}
      />,
    );
    // The valid document renders its bound content (not the empty state).
    expect(screen.getByText("Alpha")).toBeInTheDocument();
    expect(screen.getByText("Beta")).toBeInTheDocument();
    expect(FakeEventSource.instances).toHaveLength(1);

    act(() =>
      FakeEventSource.instances[0].emit({
        path: "events",
        value: [{ name: "Alpha" }, { name: "Beta" }, { name: "Gamma" }],
      }),
    );
    expect(screen.getByText("Gamma")).toBeInTheDocument();
  });
});

describe("PageRenderer — a repeat's LiveBinding bind grows on a push (UIS-070/109/110)", () => {
  const repeatLiveDoc = {
    pageType: "settings-form",
    source: "doc",
    sections: [
      {
        fields: [
          {
            type: "repeat",
            bind: { path: "tags", live: true },
            props: { itemTemplate: { type: "text", props: { value: "item.label" } } },
          },
        ],
      },
    ],
    actions: [{ type: "button", props: { labelMsg: "msg:save" }, on: { press: { verb: "submit" } } }],
  };

  it("renders the once-fetched item, then a pushed second item appears", () => {
    FakeEventSource.instances = [];
    const factory: EventSourceFactory = (url) => new FakeEventSource(url);
    render(
      <PageRenderer
        doc={repeatLiveDoc}
        data={{ doc: { tags: [{ label: "alpha" }] } }}
        messages={{ "msg:save": "Save" }}
        eventSourceFactory={factory}
      />,
    );
    expect(screen.getByText("alpha")).toBeInTheDocument();
    expect(screen.queryByText("beta")).not.toBeInTheDocument();

    act(() =>
      FakeEventSource.instances[0].emit({ path: "tags", value: [{ label: "alpha" }, { label: "beta" }] }),
    );
    expect(screen.getByText("beta")).toBeInTheDocument();
  });
});
