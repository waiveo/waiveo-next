import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Activity, HardDrive, Radio, MonitorPlay } from "lucide-react";
import {
  Button,
  DataTable,
  EmptyState,
  FormField,
  PageHeader,
  StatCard,
  StatusBadge,
  type ColumnDef,
  type Status,
} from "@/components/kit";
import {
  ApiError,
  createApi,
  LOG_LEVELS,
  type LogLevel,
  type PlatformLogPage,
  type PlatformLogRecord,
  type SystemHealth,
  type WaiveoApi,
} from "@/api";

/**
 * The System route ("/system") — parity row 7.4, the legacy stack's `/logs` and
 * `/system` pages in one place. It exists to answer "what is wrong with this
 * box" WITHOUT an SSH session, which is a daily job the console previously had
 * no surface for at all: the only health signal a deployment published was
 * `/healthz`, and if that were failing this page would not have loaded.
 *
 * ── The honesty rules this page is written to ───────────────────────────────
 *
 * It shows what it is NOT showing. The log is an in-process ring buffer that
 * starts empty at every boot and overwrites its oldest end. So the header states
 * how far back it can see and how many lines have already scrolled away, and it
 * names journald as the place to go when they have. A page that let an operator
 * conclude "no errors" from a buffer that had already discarded them would be
 * worse than no page.
 *
 * It says the level is a GUESS. The server reads a level and a source out of
 * each plain log line, because the process's log is lines and not structured
 * events. Every row therefore also carries the raw text, and the page says so
 * once, in the header, rather than presenting a derived classification as fact.
 *
 * It never conflates itself with the audit trail. `security-model/1` SEC-150
 * makes `audit.event` the platform's sole audit mechanism — durable, scoped, and
 * read on the Activity page. This is volatile operational chatter from one
 * process lifetime; the header links the reader to the other one.
 *
 * A filter that matches nothing says "nothing matched THIS FILTER", never
 * nothing. The level and source controls are populated from the WHOLE buffer
 * (the server publishes the full source list and per-level counts regardless of
 * the filter), so narrowing can always be undone from the page itself.
 */

/** How often the page re-reads while it is mounted. Slow enough not to fight the
 * operator's own reading, fast enough that a box degrading while they watch
 * shows it. */
const REFRESH_MS = 10_000;

/** Health grades → the kit's status vocabulary. `unknown` is `pending`, never
 * `ok`: "we could not look" and "we looked and it is fine" send an operator to
 * different places, and spending the ok/green lane on the first is how a status
 * column stops being believed. */
const HEALTH_STATUS: Record<string, Status> = {
  ok: "ok",
  degraded: "warn",
  down: "error",
  unknown: "pending",
};

const HEALTH_LABEL: Record<string, string> = {
  ok: "OK",
  degraded: "Degraded",
  down: "Down",
  unknown: "Unknown",
};

/** Disk grades → the same vocabulary. `low` is a warning and `critical` is an
 * error, because below the critical line an upload, a snapshot or an export can
 * actually fail. */
const STORAGE_STATUS: Record<string, Status> = {
  ok: "ok",
  low: "warn",
  critical: "error",
  unknown: "pending",
};

const LEVEL_STATUS: Record<string, Status> = {
  error: "error",
  warn: "warn",
  info: "pending",
};

/** Render a byte count in the largest unit that keeps it readable. */
export function formatBytes(n: number): string {
  if (!Number.isFinite(n) || n < 0) return "—";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i += 1;
  }
  return `${v >= 10 || i === 0 ? Math.round(v) : v.toFixed(1)} ${units[i]}`;
}

/** Render a duration in milliseconds as a short human string. `-1` is the
 * never-observed sentinel and renders as "unknown", never as "0s" — a box that
 * did not report its start time has not just restarted. */
export function formatUptime(ms: number): string {
  if (ms < 0) return "unknown";
  const s = Math.floor(ms / 1000);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ${m % 60}m`;
  return `${Math.floor(h / 24)}d ${h % 24}h`;
}

/** Render an absolute instant as a local time-of-day, for a log row. */
export function formatClock(ms: number): string {
  if (!ms) return "—";
  return new Date(ms).toLocaleTimeString();
}

type Load<T> =
  | { status: "loading" }
  | { status: "ok"; value: T }
  | { status: "error"; detail: string; traceId: string | null; code: string | null };

function errorState<T>(err: unknown): Load<T> {
  if (err instanceof ApiError) {
    return {
      status: "error",
      detail: err.detail ?? err.title ?? err.code,
      traceId: err.traceId,
      code: err.code,
    };
  }
  return { status: "error", detail: "The service is unreachable.", traceId: null, code: null };
}

/** The one place a refusal is turned into a sentence an operator can act on.
 * Both reads are owner-only, and "Forbidden" alone would leave an admin
 * wondering whether the page is broken. */
function failureMessage(state: { detail: string; code: string | null }): string {
  if (state.code === "FORBIDDEN") {
    return "Only the workspace owner can read this box's diagnostics — a log line or a relay address describes the whole deployment. Ask an owner to look.";
  }
  return state.detail;
}

function LoadError({ state }: { state: { detail: string; traceId: string | null; code: string | null } }) {
  return (
    <p className="text-sm text-[color:var(--wv-err)]">
      {failureMessage(state)}
      {state.traceId ? (
        <span className="mt-0.5 block font-mono text-[11px] text-muted-foreground">
          trace {state.traceId}
        </span>
      ) : null}
    </p>
  );
}

// ── Health ───────────────────────────────────────────────────────────────────

function HealthPanel({ state }: { state: Load<SystemHealth> }) {
  if (state.status === "loading") {
    return <p className="text-sm text-muted-foreground">Checking this box…</p>;
  }
  if (state.status === "error") return <LoadError state={state} />;
  const h = state.value;
  const storage = h.storage;

  return (
    <div className="flex flex-col gap-4">
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <StatCard
          label="Overall"
          icon={Activity}
          value={
            <StatusBadge status={HEALTH_STATUS[h.status] ?? "pending"}>
              {HEALTH_LABEL[h.status] ?? h.status}
            </StatusBadge>
          }
          hint={`${h.version} · up ${formatUptime(h.uptime_ms)}`}
        />
        <StatCard
          label="Disk"
          icon={HardDrive}
          value={
            <StatusBadge status={STORAGE_STATUS[storage.status] ?? "pending"}>
              {storage.free_bytes === undefined ? "Unknown" : formatBytes(storage.free_bytes)}
            </StatusBadge>
          }
          hint={storage.detail}
        />
        <StatCard
          label="Relays connected"
          icon={Radio}
          value={h.relays.length.toLocaleString()}
          hint={
            h.relays.length === 0
              ? "Nothing this console does can reach a screen or a device."
              : h.relays.map((r) => `${r.relay_id} (${r.screen_count})`).join(", ")
          }
        />
        <StatCard
          label="Screens live"
          icon={MonitorPlay}
          value={`${h.screens.live} / ${h.screens.total}`}
          hint={
            h.screens.total === 0
              ? "No screens are authored yet."
              : `${h.screens.fetching} collecting content · ${h.screens.stale} not heard from · ${h.screens.never_seen} never seen · ${h.screens.overridden} overridden`
          }
        />
      </div>

      <ul className="flex flex-col gap-2" aria-label="Service checks">
        {h.services.map((svc) => (
          <li
            key={svc.name}
            className="flex flex-col gap-1 rounded-md border border-border bg-[color:var(--wv-surface-2)] p-3 sm:flex-row sm:items-center sm:gap-3"
          >
            <StatusBadge status={HEALTH_STATUS[svc.status] ?? "pending"}>
              {HEALTH_LABEL[svc.status] ?? svc.status}
            </StatusBadge>
            <span className="font-medium">{svc.name}</span>
            <span className="text-sm text-muted-foreground">{svc.detail}</span>
          </li>
        ))}
      </ul>
    </div>
  );
}

// ── Logs ─────────────────────────────────────────────────────────────────────

const LOG_COLUMNS: ColumnDef<PlatformLogRecord>[] = [
  {
    id: "ts",
    header: "Time",
    accessorFn: (r) => r.ts_ms,
    cell: ({ row }) => (
      <span className="font-mono text-xs text-muted-foreground">{formatClock(row.original.ts_ms)}</span>
    ),
  },
  {
    id: "level",
    header: "Level",
    accessorFn: (r) => r.level,
    cell: ({ row }) => (
      <StatusBadge status={LEVEL_STATUS[row.original.level] ?? "pending"}>{row.original.level}</StatusBadge>
    ),
  },
  {
    id: "source",
    header: "Source",
    accessorFn: (r) => r.source,
    cell: ({ row }) => <span className="font-mono text-xs">{row.original.source}</span>,
  },
  {
    id: "message",
    header: "Message",
    accessorFn: (r) => r.message,
    cell: ({ row }) => (
      <span className="font-mono text-xs break-words whitespace-pre-wrap">{row.original.message}</span>
    ),
  },
];

export default function SystemRoute({ api }: { api?: WaiveoApi }) {
  const client = useMemo(() => api ?? createApi(), [api]);

  const [health, setHealth] = useState<Load<SystemHealth>>({ status: "loading" });
  const [logs, setLogs] = useState<Load<PlatformLogPage>>({ status: "loading" });
  const [level, setLevel] = useState<LogLevel | "">("");
  const [source, setSource] = useState("");
  const [contains, setContains] = useState("");

  // The last successful page is kept so the filter controls (which are built
  // from the WHOLE buffer's source list and level counts) survive a refresh that
  // is in flight, and survive a filter that matches nothing. Without it, the
  // moment a filter emptied the page the controls that could undo it would
  // vanish with the results — the dead end this page must not have.
  const lastPage = useRef<PlatformLogPage | null>(null);
  if (logs.status === "ok") lastPage.current = logs.value;

  const load = useCallback(() => {
    client.diagnostics
      .health()
      .then((value) => setHealth({ status: "ok", value }))
      .catch((err: unknown) => setHealth(errorState(err)));
    client.diagnostics
      .logs({
        ...(level ? { level } : {}),
        ...(source ? { source } : {}),
        ...(contains ? { contains } : {}),
      })
      .then((value) => setLogs({ status: "ok", value }))
      .catch((err: unknown) => setLogs(errorState(err)));
  }, [client, level, source, contains]);

  useEffect(() => {
    load();
    const t = setInterval(load, REFRESH_MS);
    return () => clearInterval(t);
  }, [load]);

  const page = logs.status === "ok" ? logs.value : lastPage.current;
  const sources = page?.sources ?? [];
  const counts = page?.level_counts ?? {};
  const filtered = Boolean(level || source || contains);

  return (
    <div className="min-h-screen bg-background text-foreground">
      <div className="mx-auto flex max-w-[1200px] flex-col gap-6 px-4 py-6 lg:px-8">
        <PageHeader
          variant="hero"
          title="System"
          description="What this box is doing right now — services, disk headroom, relays and the fleet — and the log lines behind them. Diagnose without an SSH session."
        />

        <section aria-labelledby="health-heading" className="flex flex-col gap-3">
          <h2 id="health-heading" className="text-lg font-semibold">
            Health
          </h2>
          <HealthPanel state={health} />
        </section>

        <section aria-labelledby="logs-heading" className="flex flex-col gap-3">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <h2 id="logs-heading" className="text-lg font-semibold">
              Platform log
            </h2>
            <Button variant="secondary" onClick={load} aria-label="Refresh the platform log">
              Refresh
            </Button>
          </div>

          {/* Said once, plainly, rather than implied: what this list is, what it
              cannot see, and where the durable record lives. */}
          <p className="text-sm text-muted-foreground">
            Lines this process has written since it started, newest first.{" "}
            <strong>Level and source are read out of each line</strong>, not declared by it — the
            raw text is shown so you can judge. This is not the audit trail: who did what is on
            the Activity page, which is durable and survives a restart.
            {page ? (
              <>
                {" "}
                Showing {page.items.length} of {page.matched} matching; {page.retained} line(s)
                retained of a {page.capacity} line buffer
                {page.retained_from_ms ? `, back to ${formatClock(page.retained_from_ms)}` : ""}.
                {page.dropped > 0 ? (
                  <>
                    {" "}
                    <strong>{page.dropped} older line(s) have already been overwritten</strong> —
                    read journald on the box for anything older, and for previous boots.
                  </>
                ) : null}
              </>
            ) : null}
          </p>

          <div className="flex flex-wrap items-end gap-3">
            <FormField label="Level" help="Exactly this level — not this level and above.">
              {(control) => (
                <select
                  {...control}
                  className="h-9 rounded-md border border-border bg-[color:var(--wv-surface-2)] px-2 text-sm"
                  value={level}
                  onChange={(e) => setLevel(e.target.value as LogLevel | "")}
                >
                  <option value="">All levels</option>
                  {LOG_LEVELS.map((l) => (
                    <option key={l} value={l}>
                      {l} ({counts[l] ?? 0})
                    </option>
                  ))}
                </select>
              )}
            </FormField>
            <FormField label="Source">
              {(control) => (
                <select
                  {...control}
                  className="h-9 rounded-md border border-border bg-[color:var(--wv-surface-2)] px-2 text-sm"
                  value={source}
                  onChange={(e) => setSource(e.target.value)}
                >
                  <option value="">All sources</option>
                  {sources.map((s) => (
                    <option key={s} value={s}>
                      {s}
                    </option>
                  ))}
                </select>
              )}
            </FormField>
            <FormField label="Contains">
              {(control) => (
                <input
                  {...control}
                  type="search"
                  className="h-9 rounded-md border border-border bg-[color:var(--wv-surface-2)] px-2 text-sm"
                  value={contains}
                  onChange={(e) => setContains(e.target.value)}
                  placeholder="e.g. ECP"
                />
              )}
            </FormField>
            {filtered ? (
              <Button
                variant="ghost"
                aria-label="Clear the log filters"
                onClick={() => {
                  setLevel("");
                  setSource("");
                  setContains("");
                }}
              >
                Clear filters
              </Button>
            ) : null}
          </div>

          {logs.status === "error" ? (
            <LoadError state={logs} />
          ) : (
            <DataTable
              label="Platform log"
              columns={LOG_COLUMNS}
              data={logs.status === "ok" ? logs.value.items : []}
              loading={logs.status === "loading"}
              emptyState={
                <EmptyState
                  title={filtered ? "Nothing matched this filter" : "No log lines yet"}
                  description={
                    filtered
                      ? "The box may still be busy — clear the filters to see everything the buffer holds."
                      : "This process has not logged anything since it started."
                  }
                />
              }
            />
          )}
        </section>
      </div>
    </div>
  );
}
