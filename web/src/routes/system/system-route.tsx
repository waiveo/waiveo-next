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
import { PageRenderer, type ActionHandler } from "@/renderer";
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
import restartPanelDoc from "./restart-panel.uis.json";

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
 *
 * ── And one thing an operator can DO ────────────────────────────────────────
 *
 * The Restart panel is the page's only control that changes anything. It is here
 * rather than on a settings page because this is where an operator already is
 * when they have decided the box needs one, and because until it existed the
 * console had no restart control at all while its own copy — the setup route's
 * two 401 messages, the backup route's staged-restore banner — told operators to
 * perform one. Those sentences ended at a dead end.
 *
 * It keeps the same honesty rules as the rest of the page. It never says
 * "restarted": the server answers 202 ACCEPTED, because the process that answers
 * is still running, so the panel says accepted and then WATCHES. It learns the
 * box came back by comparing `started_at_ms` across the outage — not by waiting
 * for the API to answer again, which cannot tell "came back" from "never went".
 * And it renders each published refusal as its own sentence, because
 * "unsupervised deployment", "already restarting" and "a job is running" send an
 * operator to three different places.
 *
 * ── The Restart panel is a ui-schema/1 DOCUMENT, not React ──────────────────
 *
 * `restart-panel.uis.json` is the whole panel — the copy, the destructive
 * button, the confirmation it is gated behind, the pending line, the per-code
 * refusal sentences and the completion line — rendered by the same PageRenderer
 * an extension page goes through. It is the first page in this console that
 * needed all three of the renderer's v1.1 additions at once, and it is here to
 * prove they work end to end rather than in a fixture: the confirm is an
 * ActionRef `confirm` (UIS-165), "working / worked / refused with THIS code" is
 * an ActionOutcome bound at `$ui.restart` (UIS-166), and the control disables
 * itself off that outcome's own `pending` (UIS-076) so a second click cannot
 * ask a box to restart twice.
 *
 * What stays in this file is the SEAM, and only the seam: one `call-action`
 * handler that POSTs the restart and then watches for a different process to
 * answer. That division is the same one the Screens route already draws — the
 * document owns what the operator sees, the host owns the api/1 mechanics (here:
 * a 202 is an acceptance, `started_at_ms` is the only proof of a new process, and
 * a box that never comes back must say so rather than spin). The watcher's whole
 * lifecycle rides back into the page through the outcome: the acceptance is
 * reported as interim progress while `pending`, the return resolves it, and a
 * refusal or a give-up rejects it.
 */

/** How often the page re-reads while it is mounted. Slow enough not to fight the
 * operator's own reading, fast enough that a box degrading while they watch
 * shows it. */
const REFRESH_MS = 10_000;

/** How often the page probes while it is waiting for a restarted box. Much
 * faster than REFRESH_MS: the operator is watching this one, and the whole
 * outage is measured in seconds. */
const RESTART_POLL_MS = 1_500;

/** How long to keep watching before saying so. A restart that has not come back
 * inside this is not "still restarting" — it is a box that needs looking at, and
 * a spinner that never resolves is the worst way to communicate that. */
export const RESTART_GIVEUP_MS = 90_000;

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

/**
 * The zone this page renders log timestamps in, named so the column can say so.
 *
 * The parity diff attributes the unlabelled timestamps here to the absence of a
 * timezone SETTING. They are not caused by one and are not fixed by one. These
 * lines are the feeder PROCESS's own log; local time in this platform comes from
 * a site (DAT-031), a workspace may hold several, and DAT-034 forbids resolution
 * from ever substituting box-local state — so there is no stored zone that is
 * the right one to render a process log in. The person reading it is in the
 * browser, which makes the browser's zone the honest choice.
 *
 * What was actually wrong is that it was unstated: a bare `14:32:07` that could
 * equally be the box's clock or the reader's is a time an operator cannot
 * correlate with anything — a journald line, a screen going dark, a colleague on
 * the phone. Naming the zone costs a header and removes the ambiguity entirely.
 */
export function displayTimeZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "local time";
  } catch {
    return "local time";
  }
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
              : // `refusing` sits first among the not-live counts because it is the
                // only one that is a fault the operator can act on right now: those
                // screens are talking to their relays and not taking what is sent.
                // A roll-up that omitted it would fold them into the arithmetic gap
                // between `live` and `total` with no word for what they are.
                `${h.screens.rejected} refusing their program · ${h.screens.fetching} collecting content · ${h.screens.stale} not heard from · ${h.screens.never_seen} never seen · ${h.screens.overridden} overridden`
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

// ── Restart ──────────────────────────────────────────────────────────────────

/** The message catalog `restart-panel.uis.json`'s own `msg:` references resolve
 * against. Every sentence the panel can say lives here, including the four
 * published refusals — each is its own sentence because "unsupervised
 * deployment", "already restarting" and "a job is running" send an operator to
 * three different places, and the server's own `detail` is interpolated as `{0}`
 * because it carries the specifics this copy cannot know (which job is running). */
const RESTART_MESSAGES: Record<string, string> = {
  "msg:system.restart.leadIn":
    "Restarts the application server — this console and the API — and nothing else. The box is not rebooted, and screens keep playing what their relay already handed them.",
  "msg:system.restart.useIt":
    "Use it to finish a staged restore, to pick up an extension that needs a fresh process, or to have an unclaimed box present a new setup code.",
  "msg:system.restart.button": "Restart this box",
  "msg:system.restart.confirm.title": "Restart this box?",
  "msg:system.restart.confirm.body":
    "The console and the API go away for a few seconds while the application server stops and starts again. Screens keep playing what their relay already handed them, and nothing authored on this box is lost. Anyone else signed in will see the console reconnect.",
  "msg:system.restart.confirm.ok": "Restart now",
  "msg:system.restart.confirm.cancel": "Not now",
  // Two pending sentences, not one: until the 202 lands nothing has been
  // accepted, and this page does not say a word it cannot stand behind.
  "msg:system.restart.asking": "Restarting — asking this box to stop and start again.",
  "msg:system.restart.accepted":
    "Restarting. Accepted — {0} will start it again, and this page is watching for it to come back.",
  "msg:system.restart.back":
    "Back up — this box restarted and is answering again, running since {0}.",
  "msg:system.restart.unsupported":
    "This box cannot restart itself: nothing is configured to start it again if it stops, so restarting would take it down and leave it down. This is a deployment setting, not a missing feature. {0}",
  "msg:system.restart.inProgress": "A restart is already under way — this one was not queued behind it. {0}",
  "msg:system.restart.blocked": "Not now: this box is busy with work a restart would break rather than resume. {0}",
  "msg:system.restart.forbidden":
    "Only the workspace owner can restart this box — it takes the whole deployment's console and API away for a few seconds. Ask an owner.",
  "msg:system.restart.trace": "trace {0}",
};

/** The give-up refusal, as a rejection the outcome can carry (UIS-166).
 *
 * It deliberately has NO `code`: no contract publishes one for "accepted and
 * never came back", and inventing a code the registry does not carry is exactly
 * what api/1 API-011 forbids. The panel's `switch` renders its `detail` through
 * the default arm, which is what that arm is for. */
class RestartNotReturned extends Error {
  readonly detail: string;

  constructor(detail: string) {
    super(detail);
    this.name = "RestartNotReturned";
    this.detail = detail;
  }
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

/** Whether a successful health read proves the box we asked to restart is a NEW
 * process.
 *
 * The identifier comparison is the real answer, and it is exact: `started_at_ms`
 * is captured once at boot, so it changes if and only if the process did. The
 * `sawOutage` fallback is for the one case where no comparison exists — a
 * deployment publishing `-1`, the never-observed sentinel — and it is
 * deliberately weaker: a drop followed by a recovery is consistent with a
 * restart and also with a dropped packet, which is why it is the fallback and
 * not the rule.
 *
 * Never "the API answered": before the process stops it is still answering, so a
 * probe that lands inside the acceptance's grace window would report a restart
 * that had not happened yet. */
export function restartReturned(
  state: { wasStartedAtMs: number; sawOutage: boolean },
  fresh: SystemHealth,
): boolean {
  if (state.wasStartedAtMs >= 0) return fresh.started_at_ms !== state.wasStartedAtMs;
  return state.sawOutage;
}

// ── Logs ─────────────────────────────────────────────────────────────────────

const LOG_COLUMNS: ColumnDef<PlatformLogRecord>[] = [
  {
    id: "ts",
    // The zone rides in the header rather than on every row: it is the same for
    // every line, and repeating it 200 times would push the message column off
    // the screen to say one thing once.
    header: `Time (${displayTimeZone()})`,
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
  // Whether a restart invocation is in flight. The PANEL learns this from its own
  // ActionOutcome (`$ui.restart.status`, UIS-166) and needs nothing from here;
  // this flag exists only so the ordinary health refresh below can stand down
  // while the watcher is probing the same endpoint four times as fast.
  const [restarting, setRestarting] = useState(false);

  // The last successful page is kept so the filter controls (which are built
  // from the WHOLE buffer's source list and level counts) survive a refresh that
  // is in flight, and survive a filter that matches nothing. Without it, the
  // moment a filter emptied the page the controls that could undo it would
  // vanish with the results — the dead end this page must not have.
  const lastPage = useRef<PlatformLogPage | null>(null);
  if (logs.status === "ok") lastPage.current = logs.value;

  // The restart watcher below is a loop inside a promise, not an effect, so it has
  // no cleanup function of its own — this is what stops it. Without it, navigating
  // away mid-restart leaves a poll hammering a box that is already down for the
  // rest of the 90-second bound, with nobody left to tell.
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

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

  // The ordinary refresh is PAUSED while a restart is in flight. Not to hide the
  // outage — the panel below says plainly what is happening — but because the
  // restart watcher is already probing the same endpoint four times as fast, and
  // two pollers racing over one box would leave the health panel flickering
  // between "unreachable" and a stale reading while the operator is trying to
  // read the one line that matters.
  useEffect(() => {
    if (restarting) return;
    load();
    const t = setInterval(load, REFRESH_MS);
    return () => clearInterval(t);
  }, [load, restarting]);

  /**
   * The restart seam — the ONE `call-action` the panel document dispatches.
   *
   * Its promise is the panel's whole state machine, and the mapping is exact:
   *   • in flight → the outcome reads `pending`, so the button disables itself;
   *   • the 202 → reported as interim progress, which is the first moment the
   *     word "accepted" is true and therefore the first moment the panel says it;
   *   • a different process answering → resolve, and the panel says Back up;
   *   • a refusal → reject with the ApiError, whose `code` the panel's `switch`
   *     turns into that refusal's own sentence;
   *   • ninety seconds of nothing → reject with a codeless give-up.
   *
   * It probes health rather than reachability, because the acceptance's
   * `started_at_ms` is the only signal that distinguishes a box that came back
   * from one that never went — a probe landing inside the grace window before the
   * process stops would otherwise report success immediately.
   */
  const restartHandler = useMemo<ActionHandler>(
    () => ({
      callAction: async (_action, _params, report) => {
        setRestarting(true);
        try {
          const acc = await client.diagnostics.restart();
          report?.({ result: { supervisor: acc.supervisor } });
          const since = Date.now();
          let sawOutage = false;
          for (;;) {
            await sleep(RESTART_POLL_MS);
            if (!mounted.current) {
              throw new RestartNotReturned("The System page was closed while this box was restarting.");
            }
            if (Date.now() - since > RESTART_GIVEUP_MS) {
              throw new RestartNotReturned(
                `This box has not come back in ${Math.round(RESTART_GIVEUP_MS / 1000)}s. It accepted the ` +
                  "restart, so it did stop; something is preventing it from starting again. Check the box " +
                  "itself — on an appliance, systemctl status waiveo-feeder and its journal.",
              );
            }
            try {
              const fresh = await client.diagnostics.health();
              if (restartReturned({ wasStartedAtMs: acc.started_at_ms, sawOutage }, fresh)) {
                setHealth({ status: "ok", value: fresh });
                return {
                  supervisor: acc.supervisor,
                  // `-1` is the never-observed sentinel: a box that reports no start
                  // time has not "started in 1969". It reads as unknown, the same
                  // word formatUptime uses for the same sentinel.
                  started_at_iso:
                    fresh.started_at_ms > 0 ? new Date(fresh.started_at_ms).toISOString() : "unknown",
                };
              }
            } catch {
              // A failed probe is the EXPECTED middle of a restart, not an error to
              // surface. It is recorded because for a box publishing no start time it
              // is the only evidence there was an outage at all.
              sawOutage = true;
            }
          }
        } finally {
          setRestarting(false);
        }
      },
    }),
    [client],
  );

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

        <section aria-labelledby="restart-heading" className="flex flex-col gap-3">
          <h2 id="restart-heading" className="text-lg font-semibold">
            Restart
          </h2>
          {/* The panel is a ui-schema/1 document (restart-panel.uis.json) rendered
              through the same PageRenderer an extension page goes through. The
              card chrome stays here — a `section` widget paints no surface — so
              the panel looks exactly as it did when it was TSX. */}
          <div className="flex flex-col gap-3 rounded-md border border-border bg-[color:var(--wv-surface-2)] p-4">
            <PageRenderer doc={restartPanelDoc} messages={RESTART_MESSAGES} handler={restartHandler} />
          </div>
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
              // Paging only, no kit search box. This panel's narrowing is
              // SERVER-side and already above the table — level, source and
              // Contains are query parameters the box applies to its ring
              // buffer, and the counts in the copy are the server's. A second,
              // client-side search here would match only the page the server
              // already returned and quietly disagree with the "showing N of M
              // matching" line beside it. The buffer is thousands of lines,
              // though, so it does need a pager. 50/page: a log is read in runs.
              pagination={{ pageSize: 50, pageSizeOptions: [25, 50, 100, 200] }}
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
