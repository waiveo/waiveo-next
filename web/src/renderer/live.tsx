// ui-schema/1 — the live-updating flag (UIS-109/110).
//
// A LiveBinding `{path, live: true}` re-evaluates for as long as its widget is
// mounted, delivered over `events/1` — the platform's real SSE door at
// `/events/v1` (the same one Vite proxies to the feeder). One EventSource is
// shared by the whole page through a subscription hub; a widget bound live reads
// the latest pushed value, falling back to its once-fetched static value until
// an update arrives. The stream is optional: when EventSource is unavailable or
// errors, the static value stands and the connection retries per EventSource's
// own semantics (graceful degrade, UIS-110). Tests inject a fake EventSource.

import { createContext, useContext, useEffect, useRef, useState, type ReactNode } from "react";

/** The minimal surface the hub needs from an EventSource — satisfied by the real
 * `EventSource` and by a test fake. */
export interface EventSourceLike {
  addEventListener(type: string, listener: (ev: MessageEvent) => void): void;
  removeEventListener?(type: string, listener: (ev: MessageEvent) => void): void;
  close(): void;
}

export type EventSourceFactory = (url: string) => EventSourceLike;

const browserFactory: EventSourceFactory | null =
  typeof EventSource !== "undefined"
    ? (url: string) => new EventSource(url) as unknown as EventSourceLike
    : null;

/** The last dotted segment of a Binding path (its leaf field), stripped of any
 * bracket accessor — the coarse key a live event may address. */
function leafOf(path: string): string {
  const last = path.split(".").pop() ?? path;
  const br = last.indexOf("[");
  return br === -1 ? last : last.slice(0, br);
}

/** True LiveBinding shape (UIS-109): `{path, live: true}`. Its `path` key makes
 * it unambiguous against a Computed / OptionSource / `{lit}` object. */
export function asLiveBinding(v: unknown): { path: string } | null {
  if (v !== null && typeof v === "object" && !Array.isArray(v)) {
    const r = v as Record<string, unknown>;
    if (r.live === true && typeof r.path === "string") return { path: r.path };
  }
  return null;
}

type Listener = (value: unknown) => void;

/** One EventSource, shared across a page's live bindings. Opens lazily on the
 * first subscriber and closes when the last unsubscribes. */
export class LiveHub {
  private es: EventSourceLike | null = null;
  private opened = false;
  private readonly subs = new Map<string, Set<Listener>>();
  private readonly latest = new Map<string, unknown>();

  constructor(
    private readonly factory: EventSourceFactory | null,
    private readonly url: string,
  ) {}

  subscribe(path: string, cb: Listener): () => void {
    let set = this.subs.get(path);
    if (!set) {
      set = new Set();
      this.subs.set(path, set);
    }
    set.add(cb);
    this.ensureOpen();
    if (this.latest.has(path)) cb(this.latest.get(path));
    return () => {
      const s = this.subs.get(path);
      if (!s) return;
      s.delete(cb);
      if (s.size === 0) this.subs.delete(path);
      if (this.subs.size === 0) this.close();
    };
  }

  /** Test/host seam: apply a decoded event as if it arrived over the stream. */
  applyUpdate(data: unknown): void {
    if (data === null || typeof data !== "object") return;
    const rec = data as Record<string, unknown>;
    if (typeof rec.path === "string" && "value" in rec) {
      this.emit(rec.path, rec.value);
      return;
    }
    for (const [key, value] of Object.entries(rec)) this.emit(key, value);
  }

  private emit(eventPath: string, value: unknown): void {
    this.latest.set(eventPath, value);
    for (const [subPath, set] of this.subs) {
      if (subPath === eventPath || leafOf(subPath) === eventPath) {
        this.latest.set(subPath, value);
        set.forEach((cb) => cb(value));
      }
    }
  }

  private ensureOpen(): void {
    if (this.opened) return;
    this.opened = true;
    if (!this.factory) return; // no EventSource here — degrade to static values
    try {
      this.es = this.factory(this.url);
      this.es.addEventListener("message", this.onMessage);
      // Swallow errors: EventSource reconnects on its own; the static value stands.
      this.es.addEventListener("error", () => {});
    } catch {
      this.es = null;
    }
  }

  private onMessage = (ev: MessageEvent): void => {
    try {
      this.applyUpdate(JSON.parse(String(ev.data)));
    } catch {
      // A non-JSON frame (a keep-alive comment, say) is ignored.
    }
  };

  private close(): void {
    if (this.es) this.es.close();
    this.es = null;
    this.opened = false;
  }

  closeAll(): void {
    this.close();
    this.subs.clear();
    this.latest.clear();
  }
}

const LiveContext = createContext<LiveHub | null>(null);

export function LiveProvider({
  factory,
  url = "/events/v1",
  children,
}: {
  factory?: EventSourceFactory | undefined;
  url?: string;
  children: ReactNode;
}) {
  const hubRef = useRef<LiveHub | null>(null);
  if (hubRef.current === null) {
    hubRef.current = new LiveHub(factory ?? browserFactory, url);
  }
  useEffect(() => {
    const hub = hubRef.current;
    return () => hub?.closeAll();
  }, []);
  return <LiveContext.Provider value={hubRef.current}>{children}</LiveContext.Provider>;
}

/** Subscribe a widget to a live path (when `path` is non-null); returns the
 * latest pushed value, or the once-fetched `staticValue` until one arrives. */
export function useLive(path: string | null, staticValue: unknown): unknown {
  const hub = useContext(LiveContext);
  const [live, setLive] = useState<{ has: boolean; value: unknown }>({ has: false, value: undefined });
  useEffect(() => {
    if (!hub || !path) return;
    return hub.subscribe(path, (v) => setLive({ has: true, value: v }));
  }, [hub, path]);
  return path && live.has ? live.value : staticValue;
}
