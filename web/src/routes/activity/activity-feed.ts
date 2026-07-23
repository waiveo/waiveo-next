// The Activity feed's connection engine + per-schema summariser.
//
// grammar-gap: live stream view. ui-schema/1 renders bound + live-bound values
// (UIS-109) but has no widget for an OPEN-ENDED stream of heterogeneous events —
// a scrolling log where each frame is a different platform schema, resume tokens
// flow on reconnect, and the server can mark a discontinuity. A live activity
// stream is one of the three sanctioned first-party gaps (alongside file upload
// and the raw code editor). This module is the framework-thin engine behind the
// first-party <ActivityRoute>; everything it consumes is the real events/1 wire
// shape (the EVT-010 envelope, the EVT-104 gap marker), so the day ui-schema/1
// grows a `stream` surface this maps straight onto it. Future path: a `stream`
// pageType/widget once the event-row projection is specced.
//
// The transport is the platform's real SSE door at /events/v1 (events/1):
//   - Named SSE events (EVT-103/104): an `event` frame carries one EVT-010
//     envelope as JSON; a `gap` frame carries the {from_id,to_id,reason} loss
//     marker. The browser reflects each frame's `id:` into MessageEvent.lastEventId.
//   - Resume (EVT-102): the id last seen is carried on a client-driven reconnect
//     as the `resume_from` query parameter — the query-param equivalent of the
//     Last-Event-ID request header a browser EventSource cannot set, which the
//     server treats identically (a fresh connect omits it).
//   - Degrade (EVT-110 spirit): on a dropped connection the engine marks the feed
//     degraded and reconnects on a backoff; where EventSource itself is absent it
//     stays gracefully unavailable rather than throwing.
// The engine is transport-injectable (an ActivityEventSourceFactory) so a test
// drives it with a fake EventSource — no socket, no network.

import { useCallback, useEffect, useRef, useState } from "react";
import type { Status } from "@/components/kit";
import { reconnectDelay } from "@/lib/backoff";

/** The minimal EventSource surface the feed needs — satisfied by the browser
 * `EventSource` and by a test fake. `open`/`error`/`event`/`gap` all arrive as
 * MessageEvents through addEventListener. */
export interface ActivityEventSource {
  addEventListener(type: string, listener: (ev: MessageEvent) => void): void;
  removeEventListener?(type: string, listener: (ev: MessageEvent) => void): void;
  close(): void;
}

/** Opens a connection to `url` and returns the stream handle. */
export type ActivityEventSourceFactory = (url: string) => ActivityEventSource;

/** The default browser transport — `null` where EventSource is unavailable (e.g.
 * a jsdom test with no fake injected), so the feed degrades rather than throws. */
export const browserEventSourceFactory: ActivityEventSourceFactory | null =
  typeof EventSource !== "undefined"
    ? (url: string) => new EventSource(url) as unknown as ActivityEventSource
    : null;

/** The events/1 door this feed watches (same-origin; the vite dev proxy covers
 * `web-dev`, the feeder serves it in production). */
export const EVENTS_URL = "/events/v1";

export type ActivityStatus = "connecting" | "live" | "degraded" | "paused" | "unavailable";

/** How many consecutive failed connects that never opened we tolerate while
 * carrying a resume cursor before giving up on it and reconnecting fresh. A
 * rejected resume_from (400 RESUME_FROM_INVALID — e.g. the feeder restarted and
 * never recorded the id) surfaces to a browser EventSource as a plain `error`
 * with no `open`, indistinguishable from a transient drop. We retry a few times
 * (a transient failure often clears, and resuming avoids a silent gap), but a
 * cursor the server keeps refusing is dead: after this many failures we clear it
 * and fall back to a fresh subscribe so the feed recovers instead of looping on
 * an id the server will never accept again. */
const RESUME_GIVEUP_ATTEMPTS = 3;

/** One rendered stream entry — either a delivered event or a server gap marker. */
export type ActivityRow = EventRow | GapRow;

export interface EventRow {
  kind: "event";
  /** Stable list key (id + a monotonic seq so duplicate ids never collide). */
  key: string;
  schema: string;
  /** epoch-ms (EVT-010 ts). */
  ts: number;
  /** The human one-line summary for this schema. */
  summary: string;
  /** A status chip for schemas that carry a disposition (automation.run). */
  badge?: { status: Status; label: string };
}

export interface GapRow {
  kind: "gap";
  key: string;
  reason: string;
  fromId: string | null;
  toId: string;
}

/** Shorten a ULID / long ref to a legible tail so a summary stays scannable. */
export function shortId(v: unknown): string {
  const s = typeof v === "string" ? v : v == null ? "" : String(v);
  return s.length > 10 ? "…" + s.slice(-6) : s;
}

/** A scalar (variable.changed old/new value) rendered compactly. */
function scalar(v: unknown): string {
  if (v === null || v === undefined) return "∅";
  if (typeof v === "string") return v;
  return String(v);
}

/** automation.run's mode_disposition → a StatusBadge tone + label. The ok/green
 * lane is reserved for the live/ran outcome (HORIZON); restarted warns, skipped
 * reads as off, anything else is pending. */
export function dispositionBadge(disposition: unknown): { status: Status; label: string } {
  switch (disposition) {
    case "ran":
      return { status: "ok", label: "Ran" };
    case "restarted":
      return { status: "warn", label: "Restarted" };
    case "skipped":
      return { status: "off", label: "Skipped" };
    default:
      return { status: "pending", label: typeof disposition === "string" ? disposition : "—" };
  }
}

function asObject(v: unknown): Record<string, unknown> {
  return v !== null && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : {};
}

/** The one-line human summary for a delivered event's payload, per registered
 * schema (EVT-020). An unregistered / pack-namespaced schema has no projection
 * here, so it renders as its schema badge + time with an empty summary. */
export function schemaSummary(schema: string, payload: unknown): string {
  const p = asObject(payload);
  switch (schema) {
    case "automation.run":
      return `rule ${shortId(p.rule_id)}`;
    case "entity.state_changed":
      return `${shortId(p.entity_id)} · ${scalar(p.old_state)} → ${scalar(p.new_state)}`;
    case "content.played":
      return `${shortId(p.asset_ref)} · ${scalar(p.completion)}`;
    case "device.heartbeat":
      return `${scalar(p.power_state)} · ${scalar(p.app_state)}`;
    case "box.vitals":
      return `CPU ${scalar(p.cpu_temp)}°C${p.undervoltage ? " · undervoltage" : ""}`;
    case "audit.event":
      return `${scalar(p.action)} ${scalar(p.target)} · ${scalar(p.result)}`;
    case "variable.changed":
      return `${scalar(p.variable)}: ${scalar(p.old_value)} → ${scalar(p.new_value)}`;
    default:
      return "";
  }
}

/** Build an EventRow from a decoded /events/v1 `event` frame. Returns null when
 * the frame is not a JSON object envelope (a keep-alive or corrupt line). */
export function eventRowFrom(data: string, lastEventId: string, seq: number): { row: EventRow; id: string } | null {
  let env: unknown;
  try {
    env = JSON.parse(data);
  } catch {
    return null;
  }
  const e = asObject(env);
  if (Object.keys(e).length === 0) return null;
  const schema = typeof e.schema === "string" ? e.schema : "event";
  const ts = typeof e.ts === "number" ? e.ts : Date.now();
  const id = lastEventId || (typeof e.id === "string" ? e.id : "");
  const summary = schemaSummary(schema, e.payload);
  const row: EventRow = { kind: "event", key: `${id || "evt"}-${seq}`, schema, ts, summary };
  if (schema === "automation.run") {
    row.badge = dispositionBadge(asObject(e.payload).mode_disposition);
  }
  return { row, id };
}

/** Build a GapRow from a decoded /events/v1 `gap` frame. */
export function gapRowFrom(data: string, lastEventId: string, seq: number): { row: GapRow; id: string } | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(data);
  } catch {
    return null;
  }
  const g = asObject(parsed);
  const toId = lastEventId || (typeof g.to_id === "string" ? g.to_id : "");
  const row: GapRow = {
    kind: "gap",
    key: `gap-${toId || "?"}-${seq}`,
    reason: typeof g.reason === "string" ? g.reason : "unknown",
    fromId: typeof g.from_id === "string" ? g.from_id : null,
    toId,
  };
  return { row, id: toId };
}

export interface ActivityFeed {
  status: ActivityStatus;
  rows: ActivityRow[];
  paused: boolean;
  pause: () => void;
  resume: () => void;
  clear: () => void;
}

export interface UseActivityFeedOptions {
  url: string;
  factory: ActivityEventSourceFactory | null;
  /** First-retry backoff delay (ms); `0` reconnects instantly (test mode). */
  reconnectDelayMs: number;
  /** Backoff ceiling (ms) — the reconnect wait never runs away. Default 30s. */
  maxReconnectDelayMs?: number;
  /** [0,1) jitter source; injectable so tests are deterministic. */
  random?: () => number;
  /** Cap the retained rows so a long-lived feed never grows unbounded. */
  maxRows?: number;
}

/**
 * The live-feed engine as a hook: owns one EventSource at a time, prepends each
 * delivered event / gap as a row (newest first, capped), tracks the last id for a
 * resume, reconnects on a drop with capped exponential backoff + jitter (so a
 * backend restart can't be met by a tight reconnect loop), and supports
 * pause/resume. A missing transport (no EventSource, no fake) reports
 * `unavailable` and connects nothing.
 */
export function useActivityFeed({
  url,
  factory,
  reconnectDelayMs,
  maxReconnectDelayMs = 30_000,
  random = Math.random,
  maxRows = 200,
}: UseActivityFeedOptions): ActivityFeed {
  const [status, setStatus] = useState<ActivityStatus>(factory ? "connecting" : "unavailable");
  const [rows, setRows] = useState<ActivityRow[]>([]);
  const [paused, setPaused] = useState(false);

  const esRef = useRef<ActivityEventSource | null>(null);
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const lastIdRef = useRef<string>("");
  const pausedRef = useRef(false);
  const seqRef = useRef(0);
  const connectRef = useRef<(() => void) | null>(null);
  // Whether the CURRENT connection ever fired `open` — an errored connect that
  // never opened is a failed handshake (e.g. a rejected resume_from), not a
  // mid-stream drop of a healthy connection.
  const openedRef = useRef(false);
  // Consecutive failed connects (errored without opening) since the last
  // successful open. Drives the resume-cursor give-up above.
  const failedConnectsRef = useRef(0);
  // Consecutive reconnect attempts since the last successful open — drives the
  // capped exponential backoff and is reset the moment the stream recovers.
  const backoffAttemptRef = useRef(0);

  const closeEs = useCallback(() => {
    if (esRef.current) {
      esRef.current.close();
      esRef.current = null;
    }
  }, []);

  const clearTimer = useCallback(() => {
    if (timerRef.current !== null) {
      clearTimeout(timerRef.current);
      timerRef.current = null;
    }
  }, []);

  const pushRow = useCallback(
    (row: ActivityRow) => {
      setRows((prev) => {
        const next = [row, ...prev];
        return next.length > maxRows ? next.slice(0, maxRows) : next;
      });
    },
    [maxRows],
  );

  const onEvent = useCallback(
    (ev: MessageEvent) => {
      const built = eventRowFrom(String(ev.data), ev.lastEventId ?? "", seqRef.current++);
      if (!built) return;
      if (built.id) lastIdRef.current = built.id;
      pushRow(built.row);
    },
    [pushRow],
  );

  const onGap = useCallback(
    (ev: MessageEvent) => {
      const built = gapRowFrom(String(ev.data), ev.lastEventId ?? "", seqRef.current++);
      if (!built) return;
      if (built.id) lastIdRef.current = built.id;
      pushRow(built.row);
    },
    [pushRow],
  );

  useEffect(() => {
    if (!factory) {
      setStatus("unavailable");
      return;
    }
    let disposed = false;

    const scheduleReconnect = () => {
      clearTimer();
      const delay = reconnectDelay(backoffAttemptRef.current, {
        baseMs: reconnectDelayMs,
        capMs: maxReconnectDelayMs,
        random,
      });
      backoffAttemptRef.current += 1;
      timerRef.current = setTimeout(() => {
        timerRef.current = null;
        if (disposed || pausedRef.current) return;
        connect();
      }, delay);
    };

    const connect = () => {
      if (disposed || pausedRef.current) return;
      closeEs();
      openedRef.current = false;
      setStatus("connecting");
      const suffix = lastIdRef.current ? `?resume_from=${encodeURIComponent(lastIdRef.current)}` : "";
      const es = factory(url + suffix);
      esRef.current = es;
      es.addEventListener("open", () => {
        if (disposed || pausedRef.current) return;
        // A live handshake: the resume (or fresh) connect was accepted, so any
        // prior failed-connect streak and the backoff are cleared — the next drop
        // starts from the base delay again.
        openedRef.current = true;
        failedConnectsRef.current = 0;
        backoffAttemptRef.current = 0;
        setStatus("live");
      });
      es.addEventListener("error", () => {
        if (disposed || pausedRef.current) return;
        // The connection dropped: mark degraded, tear down THIS stream (so the
        // browser's own auto-reconnect can't race our controlled one), and
        // reconnect on the backoff — carrying resume_from so no event is lost.
        //
        // But an error on a connect that never opened is a failed handshake,
        // not a mid-stream drop. When we're carrying a resume cursor the server
        // keeps refusing (a rejected resume_from returns a 400 before any SSE
        // frame — indistinguishable from a network blip to EventSource), naively
        // reconnecting resends the same dead id and loops forever. Count the
        // consecutive failures and, once the cursor is clearly never going to be
        // accepted, drop it and reconnect fresh so the feed actually recovers.
        if (!openedRef.current) {
          failedConnectsRef.current += 1;
          if (failedConnectsRef.current >= RESUME_GIVEUP_ATTEMPTS && lastIdRef.current) {
            lastIdRef.current = "";
            failedConnectsRef.current = 0;
          }
        }
        setStatus("degraded");
        closeEs();
        scheduleReconnect();
      });
      es.addEventListener("event", onEvent);
      es.addEventListener("gap", onGap);
    };

    connectRef.current = connect;
    connect();

    return () => {
      disposed = true;
      connectRef.current = null;
      clearTimer();
      closeEs();
    };
  }, [url, factory, reconnectDelayMs, maxReconnectDelayMs, random, onEvent, onGap, closeEs, clearTimer]);

  const pause = useCallback(() => {
    pausedRef.current = true;
    setPaused(true);
    clearTimer();
    closeEs();
    setStatus("paused");
  }, [clearTimer, closeEs]);

  const resume = useCallback(() => {
    pausedRef.current = false;
    setPaused(false);
    // A manual resume is a fresh intent to reconnect — start the backoff over.
    backoffAttemptRef.current = 0;
    connectRef.current?.();
  }, []);

  const clear = useCallback(() => setRows([]), []);

  return { status, rows, paused, pause, resume, clear };
}
