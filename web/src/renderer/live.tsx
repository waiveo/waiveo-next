// ui-schema/1 — the live-updating flag (UIS-109/110).
//
// A LiveBinding `{path, live: true}` re-evaluates for as long as its widget is
// mounted, delivered over `events/1` — the platform's real SSE door at
// `/events/v1` (the same one Vite proxies to the feeder). One EventSource is
// shared by the whole page through a subscription hub; a widget bound live reads
// the latest pushed value, falling back to its once-fetched static value until
// an update arrives. The stream is optional: when EventSource is unavailable the
// static value stands (graceful degrade, UIS-110).
//
// Reconnect posture (the anti-storm contract): a backend restart 502s every open
// SSE connection at once. The browser's OWN EventSource auto-reconnect would then
// hammer the door in a tight loop and flood the console (the "92 errors on a
// stale tab" bug). So the hub does NOT rely on it: on `error` it CLOSES the native
// source (killing the browser's retry) and reconnects itself on capped exponential
// backoff with jitter — a bounded, decorrelated cadence that recovers automatically
// when the backend returns. While reconnecting, every live binding degrades to its
// last value; the connection status is a quiet, observable state (never a console
// flood). Tests inject a fake EventSource and a deterministic clock/jitter.

import {
  createContext,
  useContext,
  useEffect,
  useRef,
  useState,
  useSyncExternalStore,
  type ReactNode,
} from "react";
import { reconnectDelay } from "@/lib/backoff";

/** The minimal surface the hub needs from an EventSource — satisfied by the real
 * `EventSource` and by a test fake. */
export interface EventSourceLike {
  addEventListener(type: string, listener: (ev: MessageEvent) => void): void;
  removeEventListener?(type: string, listener: (ev: MessageEvent) => void): void;
  close(): void;
}

export type EventSourceFactory = (url: string) => EventSourceLike;

/** The hub's connection state — a quiet, observable surface (no console flood).
 * `idle` = no subscribers/closed; `connecting` = a fresh open in flight; `live` =
 * receiving; `reconnecting` = dropped, backing off toward automatic recovery. */
export type LiveStatus = "idle" | "connecting" | "live" | "reconnecting";

/** Reconnect tuning — defaults suit a real browser; tests pin them deterministic. */
export interface LiveHubOptions {
  /** First-retry delay (ms). */
  baseDelayMs?: number;
  /** Backoff ceiling (ms) — the wait never runs away. */
  maxDelayMs?: number;
  /** [0,1) jitter source; defaults to Math.random. */
  random?: () => number;
}

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
type StatusListener = (status: LiveStatus) => void;

const DEFAULT_BASE_DELAY_MS = 1000;
const DEFAULT_MAX_DELAY_MS = 30_000;

/** One EventSource, shared across a page's live bindings. Opens lazily on the
 * first subscriber, reconnects itself on a drop (capped/jittered backoff, never
 * the browser's storming auto-reconnect), and closes when the last unsubscribes. */
export class LiveHub {
  private es: EventSourceLike | null = null;
  private active = false;
  private readonly subs = new Map<string, Set<Listener>>();
  private readonly latest = new Map<string, unknown>();

  private readonly baseDelayMs: number;
  private readonly maxDelayMs: number;
  private readonly random: () => number;

  private status: LiveStatus = "idle";
  private readonly statusListeners = new Set<StatusListener>();
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;
  /** Consecutive reconnect attempts since the last successful open — drives the
   * backoff and is reset the moment the connection recovers. */
  private attempt = 0;

  constructor(
    private readonly factory: EventSourceFactory | null,
    private readonly url: string,
    options: LiveHubOptions = {},
  ) {
    this.baseDelayMs = options.baseDelayMs ?? DEFAULT_BASE_DELAY_MS;
    this.maxDelayMs = options.maxDelayMs ?? DEFAULT_MAX_DELAY_MS;
    this.random = options.random ?? Math.random;
  }

  subscribe(path: string, cb: Listener): () => void {
    let set = this.subs.get(path);
    if (!set) {
      set = new Set();
      this.subs.set(path, set);
    }
    set.add(cb);
    this.ensureActive();
    if (this.latest.has(path)) cb(this.latest.get(path));
    return () => {
      const s = this.subs.get(path);
      if (!s) return;
      s.delete(cb);
      if (s.size === 0) this.subs.delete(path);
      if (this.subs.size === 0) this.shutdown();
    };
  }

  getStatus(): LiveStatus {
    return this.status;
  }

  /** Observe connection-state changes (future changes only — read `getStatus()`
   * for the current value). Returns an unsubscribe. */
  onStatus(cb: StatusListener): () => void {
    this.statusListeners.add(cb);
    return () => this.statusListeners.delete(cb);
  }

  private setStatus(next: LiveStatus): void {
    if (this.status === next) return;
    this.status = next;
    this.statusListeners.forEach((cb) => cb(next));
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

  private ensureActive(): void {
    if (this.active) return;
    this.active = true;
    if (!this.factory) {
      // No EventSource here — degrade permanently to static values, no retry loop.
      this.setStatus("idle");
      return;
    }
    this.attempt = 0;
    this.openConnection();
  }

  private openConnection(): void {
    if (!this.active || !this.factory) return;
    this.setStatus(this.attempt === 0 ? "connecting" : "reconnecting");
    try {
      const es = this.factory(this.url);
      this.es = es;
      es.addEventListener("open", this.onOpen);
      es.addEventListener("message", this.onMessage);
      es.addEventListener("error", this.onError);
    } catch {
      // A blocked/unavailable constructor is treated like a transient failure:
      // back off and try again on the bounded cadence so a flaky start recovers.
      this.es = null;
      this.scheduleReconnect();
    }
  }

  private onOpen = (): void => {
    this.attempt = 0;
    this.setStatus("live");
  };

  private onMessage = (ev: MessageEvent): void => {
    // A delivered frame proves the connection is healthy even if `open` was not
    // observed (some transports/fakes don't fire it): recover and reset backoff.
    this.attempt = 0;
    this.setStatus("live");
    try {
      this.applyUpdate(JSON.parse(String(ev.data)));
    } catch {
      // A non-JSON frame (a keep-alive comment, say) is ignored.
    }
  };

  private onError = (): void => {
    if (!this.active) return;
    // Take reconnection away from the browser's own storming auto-reconnect:
    // close THIS source so it can't hammer, then reconnect on our capped/jittered
    // backoff. Every live binding meanwhile degrades to its last value.
    this.closeEs();
    this.setStatus("reconnecting");
    this.scheduleReconnect();
  };

  private scheduleReconnect(): void {
    if (!this.active || this.subs.size === 0) return;
    this.clearTimer();
    const delay = reconnectDelay(this.attempt, {
      baseMs: this.baseDelayMs,
      capMs: this.maxDelayMs,
      random: this.random,
    });
    this.attempt += 1;
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      this.openConnection();
    }, delay);
  }

  private clearTimer(): void {
    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
  }

  private closeEs(): void {
    if (this.es) {
      this.es.removeEventListener?.("open", this.onOpen);
      this.es.removeEventListener?.("message", this.onMessage);
      this.es.removeEventListener?.("error", this.onError);
      this.es.close();
      this.es = null;
    }
  }

  /** Fully tear the connection down (last unsubscribe) — source closed, pending
   * reconnect cancelled, backoff reset. Retained values stay for a re-subscribe. */
  private shutdown(): void {
    this.active = false;
    this.clearTimer();
    this.closeEs();
    this.attempt = 0;
    this.setStatus("idle");
  }

  closeAll(): void {
    this.shutdown();
    this.subs.clear();
    this.latest.clear();
  }
}

const LiveContext = createContext<LiveHub | null>(null);

export function LiveProvider({
  factory,
  url = "/events/v1",
  options,
  children,
}: {
  factory?: EventSourceFactory | undefined;
  url?: string;
  options?: LiveHubOptions;
  children: ReactNode;
}) {
  const hubRef = useRef<LiveHub | null>(null);
  if (hubRef.current === null) {
    hubRef.current = new LiveHub(factory ?? browserFactory, url, options);
  }
  useEffect(() => {
    const hub = hubRef.current;
    return () => hub?.closeAll();
  }, []);
  return <LiveContext.Provider value={hubRef.current}>{children}</LiveContext.Provider>;
}

/** Subscribe a widget to a live path (when `path` is non-null); returns the
 * latest pushed value, or the once-fetched `staticValue` until one arrives.
 * Across a reconnect the last pushed value stands (degrade to last, UIS-110). */
export function useLive(path: string | null, staticValue: unknown): unknown {
  const hub = useContext(LiveContext);
  const [live, setLive] = useState<{ has: boolean; value: unknown }>({ has: false, value: undefined });
  useEffect(() => {
    if (!hub || !path) return;
    return hub.subscribe(path, (v) => setLive({ has: true, value: v }));
  }, [hub, path]);
  return path && live.has ? live.value : staticValue;
}

/** The shared live-connection status (a quiet "reconnecting…" surface, not a
 * console flood). `idle` where no hub / no EventSource is present. */
export function useLiveConnectionStatus(): LiveStatus {
  const hub = useContext(LiveContext);
  return useSyncExternalStore(
    (onChange) => (hub ? hub.onStatus(onChange) : () => {}),
    () => hub?.getStatus() ?? "idle",
    () => "idle",
  );
}
