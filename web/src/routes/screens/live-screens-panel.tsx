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
/** `rejected` is the one that DOES get the error colour, and it is the only
 * reachability that earns it. The others are all statements about what has or
 * has not been heard; this one is a screen telling us, or showing us, that it
 * will not take what the platform is sending. Something is broken, it is
 * broken now, and an operator can act on it — which is the whole test for
 * spending the error colour. */
const REACHABILITY_STATUS: Record<string, Status> = {
  live: "ok",
  fetching: "pending",
  rejected: "error",
  stale: "warn",
  never_seen: "pending",
};

const REACHABILITY_LABEL: Record<string, string> = {
  live: "Live",
  fetching: "Collecting content",
  rejected: "Refusing its program",
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

/** What a row's Status cell says UNDER the chip when a screen is refusing its
 * program: the player's own reason, which is the actionable half of the state.
 *
 * The relay carries PLY-091's `reason` verbatim (truncated, never reworded)
 * because "content fetch failed: HTTP 403" and "signature invalid" send an
 * operator to completely different places, and a console that substituted its
 * own phrasing for the player's would be inventing the diagnosis it is meant to
 * be relaying. When the refusal was inferred from repeated unacknowledged pulls
 * rather than stated, there IS no reason and this says what was actually
 * observed instead — never a guess at why. */
export function rejectionDetail(row: ScreenStatus): string | null {
  if (row.reachability !== "rejected") return null;
  if (row.reject_reason) return `The screen refused it: ${row.reject_reason}`;
  return `Handed ${row.unacked_pulls} program${row.unacked_pulls === 1 ? "" : "s"} it never confirmed`;
}

/** ScreenStatus fields NO SHIPPED PRODUCER POPULATES, and therefore fields this
 * panel must not draw a conclusion from.
 *
 * Both have exactly one producer in the whole system: the relay's `noteRenderStart`,
 * reached only from `POST /player/v1/render/start`. player-v3 never calls it. Its
 * only three HTTP calls are `/player/v1/pair`, `/player/v1/program` and
 * `/player/v1/lease/ack` (Pairing.brs, Program.brs). PLY-110 is a MUST the shipped
 * player does not implement, and until it does these arrive as the never sentinel
 * (-1) and an absent string on every row from real hardware.
 *
 * That is not a reason to delete the fields — the contract requires them and
 * another track owns the player — but it IS the reason a branch keyed on them
 * cannot be treated as reachable. A guard that reads "has this screen ever
 * rendered?" off a field nobody sends does not answer "no, never"; it answers
 * "nobody said", and phrasing the second as the first is how the cell ended up
 * telling an operator a week-old wall was blank.
 *
 * live-screens-panel.test.tsx pins the consequence and is where this list is
 * re-armed if the player ever ships PLY-110. */
export const SCREEN_STATUS_FIELDS_WITH_NO_SHIPPED_PRODUCER = [
  "last_render_start_age_ms",
  "render_asset_ref",
] as const;

/** What a row's "Now playing" cell should say, from the facts the server gives
 * about what the screen ACCEPTED — never from what it was handed.
 *
 * Exported and pure so it is testable on its own: the phrasing IS the feature
 * here, and a cell that says "Showing content" for a screen showing nothing is
 * the failure mode.
 *
 * # It reads acked_*, and that is the correction this cell most needed
 *
 * It used to read `display` and `content_count` — the program the relay HANDED
 * the screen. Those are the platform's intent, and a screen that refuses a
 * program leaves them describing the program it refused. On The Hanger on
 * 2026-08-11 this cell reported a two-item program on a wall that was rejecting
 * exactly that program and drawing an hour-old slide, for a whole session; and
 * earlier the same day it reported "Blank (scheduled off)" about a TV visibly
 * showing content, because a blank program had been sent and never taken.
 *
 * `acked_*` is what the screen confirmed (PLY-091). The shipped player fetches
 * and verifies every asset BEFORE it acknowledges, and never-wipe keeps an
 * accepted program on the wall until another is accepted — so an acceptance is
 * the strongest evidence about the glass this platform has short of PLY-110,
 * which nothing ships (see SCREEN_STATUS_FIELDS_WITH_NO_SHIPPED_PRODUCER).
 *
 * # The clause ORDER is load-bearing and has been wrong twice
 *
 * `fetching` used to come before the blank check, so a screen scheduled off was
 * described as downloading content. It is now not a clause here at all: whether
 * a transfer is in progress is what the Status column says, and this column
 * answers a different question — what the screen last took — which a transfer
 * does not change. A screen mid-update therefore keeps reporting its previous
 * program here, which is precisely what never-wipe guarantees is on the wall.
 *
 * # What it will not claim
 *
 * "Nothing confirmed yet" for a screen that has never acknowledged anything is a
 * statement about this platform's evidence, not about the wall — deliberately,
 * because the earlier attempt at that distinction ("Collecting its first content
 * (nothing on screen yet)") made a positive claim about a physical screen from a
 * field no player sends, and got it wrong on every update. */
export function nowPlayingLabel(row: ScreenStatus): string {
  if (row.reachability === "never_seen") {
    return row.program_revision ? "Waiting to collect its program" : "Nothing assigned";
  }
  // Nothing has ever been acknowledged, so nothing is confirmed. `-1` is the
  // never sentinel and is what separates this from an accepted BLANK program,
  // which is legitimately empty in every acked_* field.
  if (row.last_ack_age_ms < 0) return "Nothing confirmed yet";
  // Scheduled off, as the screen itself confirmed it. This outranks the item
  // count for the same reason it always did: a blank program has nothing to
  // show, and "Blank (scheduled off)" is the answer to the question the operator
  // is actually asking.
  if (row.acked_display === "blank") return "Blank (scheduled off)";
  if (row.render_asset_ref) {
    return `Rendering ${row.acked_content_count} item${row.acked_content_count === 1 ? "" : "s"}`;
  }
  if (row.acked_content_count > 0) {
    return `Accepted ${row.acked_content_count} item${row.acked_content_count === 1 ? "" : "s"}`;
  }
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

  /** Clear the override on every selected screen that HAS one, and report the
   * result per screen rather than as one cheerful summary.
   *
   * The loop is sequential and deliberately so: this is a handful of screens, and
   * a burst of parallel writes against the same relay set buys nothing but a
   * harder failure to describe. Screens with no override are skipped rather than
   * "cleared" — claiming to have cleared what was never set is the same class of
   * dishonesty as legacy's fire-and-forget bulk assign, which swallowed every
   * failure into `console.error`. */
  const clearPushMany = useCallback(
    async (chosen: ScreenStatus[]) => {
      if (busy) return;
      const targets = chosen.filter((row) => row.now);
      if (targets.length === 0) {
        toast.error("None of the selected screens has an override to clear.");
        return;
      }
      setBusy(true);
      const failed: string[] = [];
      try {
        for (const row of targets) {
          try {
            await api.screens.clearNow(row.screen_id);
          } catch (err) {
            failed.push(`${row.name ?? row.screen_id} (${problemMessage(err)})`);
          }
        }
        const cleared = targets.length - failed.length;
        if (failed.length === 0) {
          toast.success(
            cleared === 1
              ? "1 screen returns to its schedule on its next check-in."
              : `${cleared} screens return to their schedules on their next check-in.`,
          );
        } else {
          toast.error(
            `Cleared ${cleared} of ${targets.length}. Still overridden: ${failed.join("; ")}`,
          );
        }
        await load();
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
        // Accessors as well as cells throughout: the cell is a stack of chips and
        // sub-lines, but a fleet page has to be searchable and sortable, and a
        // render-only column offers a table no value to work from.
        id: "screen",
        header: "Screen",
        accessorFn: (r) => r.name ?? r.screen_id,
        meta: { searchable: true },
        cell: ({ row }) => (
          <span className="font-medium">{row.original.name ?? row.original.screen_id}</span>
        ),
      },
      {
        id: "reachability",
        header: "Status",
        // The LABEL, not the wire word: the filter's options are what the column
        // shows, so "Not heard from" is what an operator picks — and the option
        // set is faceted from the rows, so a reachability the server starts
        // reporting appears in the filter with no list to update.
        accessorFn: (r) => REACHABILITY_LABEL[r.reachability] ?? r.reachability,
        meta: { filter: "enum", filterLabel: "Reachability" },
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
              {/* Why it is refusing, in the player's own words. A chip that said
                  only "Refusing its program" would move the operator from "no
                  idea what is wrong" to "no idea why", which is most of the same
                  trip. */}
              {rejectionDetail(r) ? (
                <span className="text-xs text-[color:var(--wv-err)]">{rejectionDetail(r)}</span>
              ) : null}
            </div>
          );
        },
      },
      {
        id: "now",
        header: "Now playing",
        accessorFn: (r) => nowPlayingLabel(r),
        meta: { searchable: true },
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
        search={{ label: "Search screens", placeholder: "Screen name or what it is showing" }}
        filters
        pagination
        selection={{
          rowId: (row) => row.screen_id,
          rowLabel: (row) => `Select ${row.name ?? row.screen_id}`,
          // Only the non-destructive fleet gesture is offered in bulk. "Show now"
          // is not: it needs a cast chosen per push and reports intent, not
          // delivery, so a bulk version would be a second, differently-shaped
          // dialog rather than the same act repeated.
          bulkActions: (chosen) => (
            <Button
              variant="outline"
              size="sm"
              disabled={busy}
              onClick={() => void clearPushMany(chosen)}
            >
              {/* Not "Back to schedule": that is the per-ROW button's name, and
                  two identically-named buttons on one page is an ambiguity a
                  screen-reader user cannot resolve. */}
              Return selected to schedule
            </Button>
          ),
        }}
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
