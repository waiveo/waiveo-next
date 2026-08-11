// api/1 — the OPERATOR DIAGNOSTICS surface: the running box's own log and its
// health summary (the legacy stack's `/logs` and `/system` pages).
//
// Two reads, no writes, no ids, no ETags — so this is not a ResourceModule and
// could not be one. Neither answer is authored state: one is a window over what
// the process has logged, the other is a measurement taken at request time.
//
// # Both are `owner`, and a page must be ready for that
//
// The server reserves both to the workspace's owner (a log line names relay
// ids, LAN addresses and file paths; a health summary names every relay's
// advertised address). An admin bound at one site gets 403 from both. A console
// page therefore has to render that refusal as an explanation rather than as a
// blank panel — the module surfaces the ApiError unchanged so it can.
//
// # The log is NOT the audit trail, and the UI must not blur them
//
// `security-model/1` SEC-150 makes `events/1`'s `audit.event` the platform's
// sole audit mechanism; it is durable and scoped, and the console reads it on
// the Activity page. This is volatile operational chatter from ONE process
// lifetime. The two look superficially alike on screen and answer completely
// different questions, so the System page says which one it is showing.

import { ApiClient } from "./client";
import type { components } from "../../../api/gen/ts/api";

/** One captured log line. `level` and `source` are DERIVED from the text by the
 * server, never declared by whoever wrote the line — `raw` always carries the
 * whole line so a reader can judge the classification against it. */
export type PlatformLogRecord = components["schemas"]["PlatformLogRecord"];
/** A bounded window over the newest matching lines, plus what it is NOT
 * showing (`retained_from_ms`, `dropped`, `capacity`). */
export type PlatformLogPage = components["schemas"]["PlatformLogPage"];
/** The health summary: `status` is the worst component grade. */
export type SystemHealth = components["schemas"]["SystemHealth"];
export type ServiceHealth = components["schemas"]["ServiceHealth"];
export type StorageHealth = components["schemas"]["StorageHealth"];
export type RelayHealth = components["schemas"]["RelayHealth"];
export type ScreenHealth = components["schemas"]["ScreenHealth"];

/** The closed level set, in descending severity — the order a filter lists in. */
export const LOG_LEVELS = ["error", "warn", "info"] as const;
export type LogLevel = (typeof LOG_LEVELS)[number];

/** Narrowing for one log read. Every member is optional; omitting all of them
 * asks for the newest lines at any level from any source. */
export interface PlatformLogQuery {
  level?: LogLevel;
  source?: string;
  contains?: string;
  limit?: number;
}

export interface DiagnosticsModule {
  /** The newest matching log lines, newest first.
   *
   * A `level` outside the closed set is refused 400 by the server rather than
   * silently matching nothing — an empty diagnostics page reads as "the box is
   * quiet", which is the opposite of the truth. This module never invents a
   * default level for that reason: no level means every level. */
  logs(query?: PlatformLogQuery): Promise<PlatformLogPage>;
  /** The health summary, measured at request time. */
  health(): Promise<SystemHealth>;
}

export function createDiagnosticsModule(client: ApiClient): DiagnosticsModule {
  return {
    async logs(query = {}) {
      // Only the members the caller actually set are sent. An empty `source` or
      // `contains` is not "match everything" on the wire — it is a filter for
      // the empty string — so a cleared input must drop the parameter, not send
      // it blank.
      const q: Record<string, string | number | undefined> = {};
      if (query.level) q.level = query.level;
      if (query.source) q.source = query.source;
      if (query.contains) q.contains = query.contains;
      if (query.limit !== undefined) q.limit = query.limit;
      const res = await client.list<PlatformLogRecord>("/platform-logs", q);
      // `client.list` types the keyset shape every other list has; this page is
      // deliberately not one (see the server's PlatformLogPage: a cursor into a
      // ring being overwritten would name a record that no longer exists), so
      // the whole body is what matters.
      return res as unknown as PlatformLogPage;
    },
    async health() {
      const { data } = await client.read<SystemHealth>("/system-health");
      return data;
    },
  };
}
