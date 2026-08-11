import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import {
  Button,
  DataTable,
  EmptyState,
  FormField,
  Modal,
  StatusBadge,
  toast,
  type ColumnDef,
  type Status,
} from "@/components/kit";
import {
  ApiError,
  collectPages,
  type Cast,
  type ScreenStatus,
  type WaiveoApi,
} from "@/api";

/**
 * The fleet-operations panel: what every screen is actually doing, and the one
 * action an operator takes about it out of band — "show this here, now."
 *
 * It is deliberately ONE panel for two parity rows (5.8 live status, 5.7
 * push-now) because they are one job. The reason to push content to a screen is
 * almost always something you just learned from looking at it, and the way you
 * find out whether a push worked is to watch the same row change. Splitting them
 * across two surfaces would mean an operator alt-tabbing between "what is it
 * doing" and "make it do something else".
 *
 * ── The honesty rules this panel is written to ──────────────────────────────
 *
 * It never says OFFLINE. The platform genuinely cannot tell a screen that is
 * switched off from one whose network dropped, from one whose player crashed,
 * from one that was never switched on — and an operator told "offline" goes and
 * checks the wrong thing. The server reports `live | fetching | stale |
 * never_seen` plus raw ages, and this panel renders exactly that, with the age
 * always visible beside the word so the judgement can always be second-guessed.
 * `fetching` is the one that saves a wasted trip: a screen downloading a newly
 * assigned video is silent for as long as the transfer takes while the previous
 * program keeps playing, and calling that "not heard from" sends somebody to
 * look at a wall that is working.
 *
 * It distinguishes "this screen went quiet" from "this RELAY went quiet". Those
 * are different failures with different remedies and every other column renders
 * them identically; `report_age_ms` is the field that separates them, and a row
 * whose report is itself stale says so in as many words rather than blaming the
 * screen.
 *
 * A push reports INTENT, not delivery. The server persists the override and
 * nudges the relays; the screen adopts it on its next poll. So the toast says
 * the push was sent, the row shows the override immediately (it is a fact about
 * the platform), and `Now playing` keeps reporting what the fleet last OBSERVED
 * until the screen actually reports the swap. Claiming otherwise is the exact
 * "surface that accepts work it never performs" failure this codebase keeps
 * having to remove.
 */

/** How often the panel re-reads status while it is mounted. Matched to the
 * relay's own report cadence (10s) — polling faster only re-renders the same
 * numbers, and slower makes the ages visibly lag their own labels. */
const REFRESH_MS = 10_000;

/** Which chip a reachability reads as. `stale` is `warn`, never `error`: the
 * platform has not established that anything is broken, only that it has not
 * heard recently, and spending the error colour on an uncertainty is how a
 * status column stops being believed.
 *
 * `fetching` is `pending` for the same discipline pointed the other way. The
 * screen was handed a program and has not confirmed it, which the server reads
 * as a content transfer in progress — never-wipe means the PREVIOUS program is
 * still on the wall throughout, so this is not a fault and must not wear the
 * warning colour. It is not `ok` either: nothing has been heard back. */
const REACHABILITY_STATUS: Record<string, Status> = {
  live: "ok",
  fetching: "pending",
  stale: "warn",
  never_seen: "pending",
};

const REACHABILITY_LABEL: Record<string, string> = {
  live: "Live",
  fetching: "Collecting content",
  stale: "Not heard from",
  never_seen: "Never seen",
};

/** Render a millisecond age as a short human duration, or an em dash for the
 * never sentinel (-1). Never "0s ago" for never — that is the single most
 * misleading thing this panel could say. */
export function formatAge(ms: number): string {
  if (ms < 0) return "—";
  if (ms < 1_000) return "just now";
  const s = Math.floor(ms / 1_000);
  if (s < 60) return `${s}s ago`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  return `${Math.floor(h / 24)}d ago`;
}

/** Whether the server has EVIDENCE that this screen has something on it.
 *
 * `last_render_start_age_ms` is the only field that is evidence rather than
 * intent: the player reports a render start when it actually begins presenting
 * an item (PLY-110), and `-1` means it never has. `render_asset_ref` is checked
 * alongside it because it is the same report's payload, and a row carrying one
 * without the other is a report this UI should still read generously.
 *
 * Deliberately NOT `content_count > 0`. That is how many items the LAST LEASE
 * carried — intent, and specifically intent about the program the screen is
 * collecting right now, so it is at its most positive exactly when the screen is
 * fetching its first-ever content and the wall is blank. */
function hasEverRendered(row: ScreenStatus): boolean {
  return row.last_render_start_age_ms >= 0 || Boolean(row.render_asset_ref);
}

/** What a row's "Now playing" cell should say, from the facts the server gives:
 * what the relay last handed the screen, whether the screen reported actually
 * rendering something, and whether it is currently collecting new content.
 *
 * Exported and pure so it is testable on its own: the phrasing IS the feature
 * here, and a cell that says "Showing content" for a screen showing nothing is
 * the failure mode.
 *
 * The clause ORDER is load-bearing and was wrong. `fetching` came first and
 * therefore pre-empted `display === "blank"`, and it claimed "still showing the
 * last" unconditionally — including for a screen collecting its first-ever
 * program, whose wall is blank and which has no last to be showing. Both are the
 * same mistake: reporting a transfer state as though it implied something about
 * what is on the screen, which it does not. */
export function nowPlayingLabel(row: ScreenStatus): string {
  if (row.reachability === "never_seen") {
    return row.program_revision ? "Waiting to collect its program" : "Nothing assigned";
  }
  // Scheduled off is a fact about the program the screen was HANDED, and it
  // outranks the transfer state: a blank program has nothing to fetch, so
  // "downloading new content" about one is describing work that is not
  // happening, and "Blank (scheduled off)" is the answer to the question the
  // operator is actually asking.
  if (row.display === "blank") return "Blank (scheduled off)";
  // A transferring screen is still showing whatever it had; saying so is the
  // difference between an operator waiting and an operator driving to the site.
  // But only if it HAD something — never-wipe keeps the outgoing program on the
  // wall, and a screen with no outgoing program keeps nothing.
  if (row.reachability === "fetching") {
    return hasEverRendered(row)
      ? "Downloading new content (still showing the last)"
      : "Collecting its first content (nothing on screen yet)";
  }
  if (row.render_asset_ref) {
    return `Rendering ${row.content_count} item${row.content_count === 1 ? "" : "s"}`;
  }
  if (row.content_count > 0) return `Sent ${row.content_count} item${row.content_count === 1 ? "" : "s"}`;
  return "Nothing to show";
}

function problemMessage(err: unknown): string {
  if (err instanceof ApiError) return err.detail ?? err.code;
  return "the service is unreachable.";
}

/** The push dialog's occupancy. */
type PushDialog = { kind: "closed" } | { kind: "open"; screen: ScreenStatus };

export interface LiveScreensPanelProps {
  api: WaiveoApi;
  /** Disables the auto-refresh timer. Tests set it so a case observes exactly
   * the reads it drives, rather than racing a background poll. */
  autoRefresh?: boolean;
}

export function LiveScreensPanel({ api, autoRefresh = true }: LiveScreensPanelProps) {
  const [rows, setRows] = useState<ScreenStatus[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [casts, setCasts] = useState<Cast[]>([]);
  const [dialog, setDialog] = useState<PushDialog>({ kind: "closed" });
  const [castId, setCastId] = useState<string>("");
  const [busy, setBusy] = useState(false);
  // Held in a ref as well as in state so the refresh timer's callback does not
  // have to be re-created (and the interval re-armed) on every load.
  const loadingRef = useRef(false);

  const load = useCallback(async () => {
    if (loadingRef.current) return;
    loadingRef.current = true;
    try {
      const list = await collectPages<ScreenStatus>((cursor) => api.screenStatus.list({ cursor }));
      setRows(list);
      setLoadError(null);
    } catch (err) {
      setRows([]);
      setLoadError(problemMessage(err));
    } finally {
      loadingRef.current = false;
    }
  }, [api]);

  useEffect(() => {
    void load();
  }, [load]);

  useEffect(() => {
    if (!autoRefresh) return;
    const id = setInterval(() => void load(), REFRESH_MS);
    return () => clearInterval(id);
  }, [autoRefresh, load]);

  // The casts an operator can push. Loaded once when the dialog first opens
  // rather than with the page: a fleet page that fetched the whole cast library
  // on every mount would pay for it on every operator's every visit, and the
  // list is only ever read inside the dialog.
  const loadCasts = useCallback(async () => {
    if (casts.length > 0) return;
    try {
      const list = await collectPages<Cast>((cursor) => api.casts.list({ cursor }));
      setCasts(list);
      setCastId((current) => current || (list[0]?.id ?? ""));
    } catch (err) {
      toast.error(`Couldn't load the cast library: ${problemMessage(err)}`);
    }
  }, [api, casts.length]);

  const openPush = useCallback(
    (screen: ScreenStatus) => {
      setDialog({ kind: "open", screen });
      void loadCasts();
    },
    [loadCasts],
  );

  const push = useCallback(async () => {
    if (dialog.kind !== "open" || busy) return;
    if (!castId) {
      toast.error("Choose a cast to show.");
      return;
    }
    setBusy(true);
    try {
      // `play`, not `alert`: this dialog is the everyday "show this here"
      // gesture, and `alert` is the takeover that interrupts whatever a screen
      // is mid-way through (PLY-108). Defaulting the everyday act to the
      // takeover would make every push a preempt and leave the console with no
      // way to express the ordinary one.
      await api.screens.pushNow(dialog.screen.screen_id, { mode: "play", cast_id: castId });
      // "Sent", not "showing": the screen adopts it on its next poll, and this
      // console has no evidence it has yet. The row's own Now-playing cell
      // reports what the fleet actually observed, and will change on its own.
      toast.success(
        `Sent to ${dialog.screen.name ?? dialog.screen.screen_id} — it swaps on its next check-in (about 10 seconds).`,
      );
      setDialog({ kind: "closed" });
      await load();
    } catch (err) {
      toast.error(`Couldn't push to this screen: ${problemMessage(err)}`);
    } finally {
      setBusy(false);
    }
  }, [api, busy, castId, dialog, load]);

  const clearPush = useCallback(
    async (row: ScreenStatus) => {
      if (busy) return;
      setBusy(true);
      try {
        await api.screens.clearNow(row.screen_id);
        toast.success(`${row.name ?? row.screen_id} returns to its schedule on its next check-in.`);
        await load();
      } catch (err) {
        toast.error(`Couldn't clear the override: ${problemMessage(err)}`);
      } finally {
        setBusy(false);
      }
    },
    [api, busy, load],
  );

  const castName = useMemo(() => {
    const byID = new Map(casts.map((c) => [c.id, c.name]));
    return (id: string | undefined) => (id ? (byID.get(id) ?? id) : "");
  }, [casts]);

  const columns = useMemo<ColumnDef<ScreenStatus>[]>(
    () => [
      {
        id: "screen",
        header: "Screen",
        cell: ({ row }) => (
          <span className="font-medium">{row.original.name ?? row.original.screen_id}</span>
        ),
      },
      {
        id: "reachability",
        header: "Status",
        cell: ({ row }) => {
          const r = row.original;
          return (
            <div className="flex flex-col gap-1">
              <StatusBadge status={REACHABILITY_STATUS[r.reachability] ?? "pending"}>
                {REACHABILITY_LABEL[r.reachability] ?? r.reachability}
              </StatusBadge>
              <span className="text-xs text-muted-foreground">
                Last check-in {formatAge(r.last_pull_age_ms)}
              </span>
            </div>
          );
        },
      },
      {
        id: "now",
        header: "Now playing",
        cell: ({ row }) => {
          const r = row.original;
          return (
            <div className="flex flex-col gap-1">
              <span>{nowPlayingLabel(r)}</span>
              {r.now ? (
                <span className="text-xs text-[color:var(--wv-warn)]">
                  Pushed by an operator{r.now.cast_id ? `: ${castName(r.now.cast_id)}` : ""}
                </span>
              ) : null}
              {/* A row whose REPORT is stale is describing the relay, not the
                  screen. Said explicitly, because every other cell in this row
                  renders the two failures identically. */}
              {r.report_age_ms > REFRESH_MS * 3 ? (
                <span className="text-xs text-muted-foreground">
                  Relay last reported {formatAge(r.report_age_ms)} — this row may be out of date
                </span>
              ) : null}
            </div>
          );
        },
      },
    ],
    [castName],
  );

  return (
    <section aria-label="Live screens" className="flex flex-col gap-3">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold">Live screens</h2>
          <p className="text-sm text-muted-foreground">
            What each screen is doing right now, and what the relays last heard from it. Push a cast
            to override the schedule until you clear it.
          </p>
        </div>
        <Button variant="outline" onClick={() => void load()}>
          Refresh
        </Button>
      </div>

      {loadError ? (
        <p role="alert" className="text-sm text-[color:var(--wv-err)]">
          Couldn't load screen status — {loadError}
        </p>
      ) : null}

      <DataTable<ScreenStatus>
        columns={columns}
        data={rows ?? []}
        label="Live screens"
        loading={rows === null}
        emptyState={
          <EmptyState
            title="No screens yet"
            description="Pair a display below to give it an identity content can be scheduled against."
          />
        }
        rowActions={(row) => (
          <div className="flex justify-end gap-2">
            <Button size="sm" variant="outline" onClick={() => openPush(row)} disabled={busy}>
              Show now
            </Button>
            {row.now ? (
              <Button size="sm" variant="ghost" onClick={() => void clearPush(row)} disabled={busy}>
                Back to schedule
              </Button>
            ) : null}
          </div>
        )}
      />

      <Modal
        title="Show now"
        description={
          dialog.kind === "open"
            ? `Override ${dialog.screen.name ?? dialog.screen.screen_id} until you clear it. It swaps on its next check-in, about 10 seconds.`
            : ""
        }
        open={dialog.kind === "open"}
        onOpenChange={(open) => {
          if (!open) setDialog({ kind: "closed" });
        }}
        footer={
          <>
            <Button variant="ghost" onClick={() => setDialog({ kind: "closed" })}>
              Cancel
            </Button>
            <Button onClick={() => void push()} disabled={busy || casts.length === 0}>
              {busy ? "Sending…" : "Show it"}
            </Button>
          </>
        }
      >
        {casts.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            There are no casts to show yet — author one in Studio first.
          </p>
        ) : (
          <FormField label="Cast">
            {(field) => (
              <select
                {...field}
                className="flex min-h-[44px] w-full min-w-0 rounded-input border border-border bg-transparent px-3 py-1 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
                value={castId}
                onChange={(e) => setCastId(e.target.value)}
              >
                {casts.map((c) => (
                  <option key={c.id} value={c.id}>
                    {c.name}
                  </option>
                ))}
              </select>
            )}
          </FormField>
        )}
      </Modal>
    </section>
  );
}
