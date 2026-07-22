import { useMemo } from "react";
import { Activity, Pause, Play, Trash2, TriangleAlert, WifiOff } from "lucide-react";
import { Button, EmptyState, KitIcon, PageHeader, StatusBadge, type Status } from "@/components/kit";
import { cn } from "@/lib/utils";
import {
  browserEventSourceFactory,
  EVENTS_URL,
  useActivityFeed,
  type ActivityEventSourceFactory,
  type ActivityStatus,
  type EventRow,
  type GapRow,
} from "./activity-feed";

/**
 * The Activity route — the console's live window on the platform, a scrolling
 * feed of the events/1 stream over the real /events/v1 SSE door.
 *
 * // grammar-gap: live stream view. ui-schema/1 binds and live-binds discrete
 * values (UIS-109) but cannot express an OPEN-ENDED stream of heterogeneous
 * events — a growing log where each frame is a different platform schema, a resume
 * token flows on reconnect, and the server can mark a discontinuity. A live
 * activity stream is one of the three sanctioned first-party gaps (alongside file
 * upload and the raw code editor); the connection engine + per-schema summariser
 * live in ./activity-feed, and this file is its on-brand view. Everything it
 * consumes is the real events/1 wire shape (the EVT-010 envelope, the EVT-104 gap
 * marker), so the day ui-schema/1 grows a `stream` surface this maps onto it.
 *
 * Live behaviours: each `event` frame prepends a row (schema badge, local time,
 * a per-schema summary — automation.run reads rule + disposition); a `gap` frame
 * renders a visible discontinuity marker; a dropped connection degrades the feed
 * and reconnects carrying the last id as resume_from (the Last-Event-ID contract,
 * EVT-102); pause/resume tear down and re-open the stream; and where EventSource
 * is unavailable the page degrades to a plain notice rather than throwing.
 */

/** The feed's connection state → a StatusBadge tone + label. The ok/green lane is
 * reserved for the live/connected state (HORIZON). */
const CONNECTION: Record<ActivityStatus, { status: Status; label: string }> = {
  connecting: { status: "pending", label: "Connecting" },
  live: { status: "ok", label: "Live" },
  degraded: { status: "warn", label: "Reconnecting" },
  paused: { status: "off", label: "Paused" },
  unavailable: { status: "error", label: "Offline" },
};

function formatTime(ts: number): { iso: string; label: string } {
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return { iso: "", label: "—" };
  return { iso: d.toISOString(), label: d.toLocaleTimeString() };
}

function EventRowView({ row }: { row: EventRow }) {
  const time = formatTime(row.ts);
  return (
    <li
      data-slot="activity-row"
      data-schema={row.schema}
      className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1 border-b border-border px-3 py-2.5 last:border-b-0"
    >
      <span className="rounded-pill bg-[color:var(--wv-surface-2)] px-2 py-0.5 font-mono text-[11px] text-muted-foreground">
        {row.schema}
      </span>
      {row.badge ? <StatusBadge status={row.badge.status}>{row.badge.label}</StatusBadge> : null}
      <span className="min-w-0 flex-1 truncate text-[13px] text-foreground">{row.summary}</span>
      <time dateTime={time.iso} title={time.iso} className="shrink-0 font-mono text-[11px] tabular-nums text-muted-foreground">
        {time.label}
      </time>
    </li>
  );
}

function GapRowView({ row }: { row: GapRow }) {
  return (
    <li
      data-slot="activity-gap"
      data-testid="activity-gap"
      role="note"
      className="flex min-w-0 items-center gap-2 border-b border-[color:var(--wv-warn)] bg-[color:var(--wv-warn-bg)] px-3 py-2.5 text-[13px] text-[color:var(--wv-warn)] last:border-b-0"
    >
      <KitIcon icon={TriangleAlert} decorative className="size-4 shrink-0" />
      <span className="min-w-0">
        Some activity was missed ({row.reason}). The stream resumed at the next available event.
      </span>
    </li>
  );
}

export default function ActivityRoute({
  factory,
  reconnectDelayMs = 3000,
  url = EVENTS_URL,
}: {
  /** Injectable transport — a test passes a fake EventSource; `null` forces the
   * unavailable/degraded path. Defaults to the browser EventSource. */
  factory?: ActivityEventSourceFactory | null;
  reconnectDelayMs?: number;
  url?: string;
}) {
  const transport = factory === undefined ? browserEventSourceFactory : factory;
  const feed = useActivityFeed({ url, factory: transport, reconnectDelayMs });
  const conn = CONNECTION[feed.status];
  const unavailable = feed.status === "unavailable";
  const eventCount = useMemo(() => feed.rows.filter((r) => r.kind === "event").length, [feed.rows]);

  return (
    <div className="min-h-screen bg-background text-foreground">
      <div className="mx-auto flex max-w-[1000px] flex-col gap-6 px-4 py-6 lg:px-8">
        <PageHeader
          variant="hero"
          title="Activity"
          description="Watch the platform live — automation runs, state changes, playback and audit events stream here as the feeder records them."
        />

        {unavailable ? (
          <div className="flex flex-col items-center gap-3 rounded-card border border-border bg-card px-6 py-12 text-center wv-elevation">
            <div className="flex size-12 items-center justify-center rounded-panel bg-[color:var(--wv-surface-2)]">
              <KitIcon icon={WifiOff} decorative className="size-6 text-muted-foreground" />
            </div>
            <p className="font-display text-[17px] font-semibold">Live updates aren&rsquo;t available here</p>
            <p className="max-w-sm text-sm text-muted-foreground">
              This browser has no live-event support, so the activity stream can&rsquo;t connect. The rest of the
              console works normally.
            </p>
          </div>
        ) : (
          <>
            <div className="flex flex-wrap items-center gap-3">
              <span data-testid="activity-connection" className="inline-flex items-center">
                <StatusBadge status={conn.status}>{conn.label}</StatusBadge>
              </span>
              <span className="text-[13px] text-muted-foreground">
                {eventCount === 0 ? "No events yet" : `${eventCount} event${eventCount === 1 ? "" : "s"}`}
              </span>
              <div className="ml-auto flex items-center gap-2">
                <Button
                  variant="secondary"
                  size="sm"
                  icon={Trash2}
                  className="wv-touch"
                  disabled={feed.rows.length === 0}
                  onClick={feed.clear}
                >
                  Clear
                </Button>
                <Button
                  variant="secondary"
                  size="sm"
                  icon={feed.paused ? Play : Pause}
                  className="wv-touch"
                  onClick={feed.paused ? feed.resume : feed.pause}
                >
                  {feed.paused ? "Resume" : "Pause"}
                </Button>
              </div>
            </div>

            {feed.status === "degraded" ? (
              <p
                role="status"
                className="rounded-card border border-[color:var(--wv-warn)] bg-[color:var(--wv-warn-bg)] p-3 text-sm text-[color:var(--wv-warn)]"
              >
                The live connection dropped — reconnecting&hellip; New activity will resume automatically from where
                it left off.
              </p>
            ) : null}

            <section
              role="log"
              aria-label="Activity feed"
              aria-live="polite"
              aria-relevant="additions"
              className={cn(
                "min-w-0 overflow-hidden rounded-card border border-border bg-card wv-elevation",
                feed.rows.length === 0 && "border-dashed",
              )}
            >
              {feed.rows.length === 0 ? (
                feed.paused ? (
                  <EmptyState
                    icon={Pause}
                    title="Paused"
                    description="The stream is paused. Resume to watch new activity as it happens."
                  />
                ) : (
                  <EmptyState
                    icon={Activity}
                    title="Listening for activity"
                    description="New events appear here as the platform records them — automation runs, state changes, playback and audits."
                  />
                )
              ) : (
                <ul className="flex min-w-0 flex-col">
                  {feed.rows.map((row) =>
                    row.kind === "gap" ? (
                      <GapRowView key={row.key} row={row} />
                    ) : (
                      <EventRowView key={row.key} row={row} />
                    ),
                  )}
                </ul>
              )}
            </section>
          </>
        )}
      </div>
    </div>
  );
}
