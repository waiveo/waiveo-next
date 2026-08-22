import { useCallback, useEffect, useMemo, useState } from "react";
import { PageHeader } from "@/components/kit";
import { PageRenderer } from "@/renderer";
import { collectPages, createApi, type ScreenStatus, type WaiveoApi } from "@/api";
import { formatAge } from "@/lib/format-age";
import screensDoc from "./screens.uis.json";

/**
 * Screens — what each screen behind a relay is actually doing.
 *
 * # Why this page exists at all
 *
 * It did not, and that was the finding that produced it: `GET /screen-status`
 * served 200, `screenStatus.list()` existed in the API client, the registry held
 * the reports, the wire frame carried them and `relay/1` specified them — and no
 * console route called any of it. An operator could not see a screen anywhere.
 * The whole chain existed except the end an operator looks at.
 *
 * # The distinction this page is FOR (REL-119b)
 *
 * A `screen.status` entry carries two different things and they must never be
 * rendered as one:
 *
 *   - what the platform HANDED the screen — its intent;
 *   - what the screen ACCEPTED — the only program facts the screen itself has
 *     confirmed.
 *
 * A player that refuses a program leaves the intent describing the program it
 * refused. A console reading intent alone therefore reports a wall as showing
 * something it is not, which is exactly what happened for a whole session before
 * the frame was reshaped. So "Showing" binds to the ACKNOWLEDGED revision and
 * says `never accepted a program` when there is none, and the platform's intent
 * gets its own column with its own name. They are never the same cell.
 *
 * # Authored as `ui-schema/1`
 *
 * `screens.uis.json` is the page; this file is the data half. The five renderer
 * capabilities it needs were built over the preceding iterations and this is the
 * surface that most wanted them: `cellWidget` for the three-state Showing cell,
 * `toneFrom` for per-row reachability, `titleMsg` for the sentences that make
 * those states mean anything, and `emptyWidget`/`loadingIf` so an empty table
 * says WHICH emptiness and stays quiet while the first fetch is in flight.
 */

/** The `msg:` catalog the document's references resolve against. */
const MESSAGES: Record<string, string> = {
  "msg:scr.col.name": "Screen",
  "msg:scr.col.reach": "Reachability",
  "msg:scr.col.showing": "Showing",
  "msg:scr.col.handed": "Handed",
  "msg:scr.col.lastSeen": "Last seen",
  "msg:scr.col.relay": "Reported by",
  "msg:scr.filter.reach": "Reachability",
  "msg:scr.filter.relay": "Reported by",
  "msg:scr.search.placeholder": "Screen name, program revision or relay",
  "msg:scr.reach.help":
    "What this box has observed, not what it intends. `live` means the screen pulled and acknowledged recently; " +
    "`fetching` means it is materialising a program it was handed; `stale` means it has stopped checking in.",
  "msg:scr.showing.never": "never accepted a program",
  "msg:scr.showing.neverHelp":
    "This screen has never acknowledged a program. It may be unpaired, or it may be pulling and failing — the " +
    "Handed column says what it was offered, and Reachability says whether it is still asking.",
  "msg:scr.showing.refusedHelp":
    "The screen REFUSED the program it was last handed and is still showing what it accepted before that. A refusal " +
    "is cleared by accepting something and by nothing else, so this stands until it does.",
  "msg:scr.showing.acceptedHelp":
    "The program this screen ACCEPTED — the only program fact the screen itself has confirmed, and the only one " +
    "that may be read as what is on that wall.",
  "msg:scr.col.handedHelp": "",
  "msg:scr.handed.help":
    "What the platform last handed this screen. This is INTENT: a screen that refused it still reports it here, so " +
    "this column never says what a wall is showing.",
  "msg:scr.render.help": "The asset the player last reported actually putting on screen — evidence of playback rather than of intent.",
  "msg:scr.pulls.help":
    "Program pulls served since the last acknowledgement seen. 0 is up to date; 1 is a screen materialising what it " +
    "was just handed; a climbing count is one that keeps asking and never confirms.",
  "msg:scr.empty.none":
    "No screen has been paired to this box yet. Pair one from the player on the screen itself; until then there is " +
    "nothing for a schedule to play on.",
  "msg:scr.empty.unreadable": "The screen list could not be read.",
  "msg:scr.detail.empty": "Select a screen to see what it has confirmed.",
  "msg:scr.detail.title": "What this screen has confirmed",
};

/** How a reachability reads, and what tone it earns.
 *
 * `rejected` is `critical` and `stale` is `warning` on purpose: a refusal is a
 * screen actively saying no, which someone must act on, while a stale screen may
 * simply be a wall someone switched off. Spending the error colour on both would
 * make the one that matters unfindable. */
const REACHABILITY: Record<string, { label: string; tone: string }> = {
  live: { label: "Live", tone: "positive" },
  fetching: { label: "Fetching", tone: "neutral" },
  rejected: { label: "Refused", tone: "critical" },
  stale: { label: "Stale", tone: "warning" },
  never_seen: { label: "Never seen", tone: "warning" },
};

function age(ms: number): string {
  // -1 is the never-observed sentinel (REL-119a) and is NOT a large age.
  if (ms < 0) return "never";
  return `${formatAge(ms)} ago`;
}

/** One status as the document binds it. Total by construction — see the
 * discovered-devices projection for why a partly-populated row goes blank. */
export interface ScreenRow extends Record<string, unknown> {
  screen_id: string;
  name: string;
  reachability: string;
  reachability_label: string;
  reachability_tone: string;
  showing_kind: "never" | "refused" | "accepted";
  showing_display: string;
  showing_sort: string;
  handed_display: string;
  render_display: string;
  pulls_display: string;
  last_seen_display: string;
  relay_display: string;
}

export function toScreenRow(s: ScreenStatus): ScreenRow {
  const reach = REACHABILITY[s.reachability] ?? { label: s.reachability, tone: "neutral" };
  // REL-119b: "Showing" is the ACKNOWLEDGED program and nothing else. An
  // accepted BLANK program is legitimately empty, so the never-accepted case is
  // told by last_ack_age_ms's sentinel rather than by an empty revision.
  const neverAccepted = s.last_ack_age_ms < 0;
  const showingKind: ScreenRow["showing_kind"] = neverAccepted
    ? "never"
    : s.rejected
      ? "refused"
      : "accepted";
  const accepted =
    s.acked_display === "blank"
      ? "blank"
      : (s.acked_program_revision ?? "") || "blank";
  return {
    screen_id: s.screen_id,
    name: s.name ?? s.screen_id,
    reachability: s.reachability,
    reachability_label: reach.label,
    reachability_tone: reach.tone,
    showing_kind: showingKind,
    showing_display: neverAccepted ? "" : accepted,
    showing_sort: neverAccepted ? "" : accepted,
    handed_display: s.program_revision ?? "—",
    render_display: s.render_asset_ref ?? "nothing reported",
    pulls_display: `${s.unacked_pulls} unacknowledged pull${s.unacked_pulls === 1 ? "" : "s"}`,
    last_seen_display: age(s.last_pull_age_ms),
    relay_display: s.relay_id ?? "—",
  };
}

export default function ScreensRoute({ api }: { api?: WaiveoApi }) {
  const client = useMemo(() => api ?? createApi(), [api]);
  const [rows, setRows] = useState<ScreenStatus[] | null>(null);
  const [unreadable, setUnreadable] = useState(false);

  const load = useCallback(async () => {
    try {
      setRows(await collectPages<ScreenStatus>((cursor) => client.screenStatus.list({ cursor })));
      setUnreadable(false);
    } catch {
      // A refused read is a fact about this console, not about the fleet — the
      // document's empty state says which, rather than reporting "no screens".
      setRows([]);
      setUnreadable(true);
    }
  }, [client]);

  useEffect(() => {
    void load();
  }, [load]);

  const data = useMemo(
    () => ({
      screens: (rows ?? []).map(toScreenRow),
      loading: rows === null,
      empty_kind: unreadable ? "unreadable" : "none",
    }),
    [rows, unreadable],
  );

  return (
    <div className="min-h-screen bg-background text-foreground">
      <div className="mx-auto flex max-w-[1200px] flex-col gap-6 px-4 py-6 lg:px-8">
        <PageHeader
          variant="hero"
          title="Screens"
          description="What each screen has actually confirmed — not what this box intended for it."
        />
        <PageRenderer
          doc={screensDoc as unknown as Record<string, unknown>}
          data={data}
          messages={MESSAGES}
        />
      </div>
    </div>
  );
}
