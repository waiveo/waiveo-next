import { useCallback, useEffect, useMemo, useState } from "react";
import { Layers } from "lucide-react";
import { Button, DataTable, FormField, StatusBadge, toast, type ColumnDef } from "@/components/kit";
import {
  ApiError,
  RevisionConflictError,
  collectPages,
  etagForRevision,
  type Schedule,
  type ScopeNode,
  type Screen,
  type WaiveoApi,
} from "@/api";

/**
 * Bulk assign — "put this content on every screen in that group", the owner's
 * daily action in the legacy system, expressed in this platform's own terms.
 *
 * # What "assigning content to a screen" actually is here
 *
 * There is no field on a screen that names its schedule, and adding one would be
 * a second scheduling model. A screen's program is resolved from WHERE THE
 * SCREEN IS: the feeder hands the screen identity row's `scope_node` to the
 * data-model/1 resolver, and the DAT-051 cascade walks the parent chain to find
 * the schedules that apply (internal/feeder/snapshot/screenprograms.go — "it
 * walks no lists and matches no ids"). A schedule authored at a group therefore
 * governs every screen beneath that group, automatically and with no per-screen
 * write anywhere.
 *
 * So assigning a schedule to a set of screens means MOVING those screens under
 * the node the schedule is authored at. That is not a workaround for a missing
 * field — placement IS the assignment, and DAT-004 explicitly allows a screen
 * identity row to hang off a node of any kind precisely so it can be placed
 * where its content is. The panel says this on screen, because an operator who
 * thinks they are setting an attribute will be surprised by a screen that moved.
 *
 * # No bulk endpoint exists for this, and the panel does not pretend otherwise
 *
 * api/1 has exactly one fleet-mutating operation, `POST /automations/bulk-enable`
 * — selector in, 202 + Job out. Nothing equivalent exists for placement. So this
 * panel does the fan-out CLIENT-SIDE: one conditional `PATCH /screens/{id}` per
 * matched screen, each carrying the If-Match derived from that row's revision
 * (API-022 — there is no unconditional overwrite, not even inside a bulk
 * gesture), and it reports per-screen outcomes rather than a single "done".
 * A partial result is the honest one: seven of nine moved and two were changed
 * elsewhere is a real state, and a bulk action that hid it would leave two
 * screens showing yesterday's content with nothing on screen to say so.
 *
 * # Targeting uses the API's own grammar, not a private one
 *
 * The match is a label-selector string sent to `GET /screens?selector=…` — the
 * same grammar `bulk-enable` takes as its target predicate. Its `scope_node
 * subtree <ulid>` term is what expresses "every screen in this group, however
 * deeply nested", so the group picker below composes that term rather than
 * filtering a fetched list in the browser: the server decides membership, which
 * means the preview and the writes agree by construction.
 */

/** The outcome of one screen's conditional write. */
interface Outcome {
  screen: Screen;
  status: "moved" | "skipped" | "conflict" | "failed";
  detail?: string;
}

function problemMessage(err: unknown): string {
  if (err instanceof ApiError) {
    const fieldMsg = Object.values(err.fieldErrors)[0];
    return fieldMsg ?? err.detail ?? err.code;
  }
  return "the service is unreachable.";
}

/** Compose the api/1 selector from the two controls: a subtree term for the
 * chosen group (server-side membership, including nested groups) ANDed with
 * whatever label terms the operator typed. Empty when neither is set — and an
 * empty selector deliberately matches EVERY screen, which is why the panel
 * previews the match before it will write anything. */
export function composeSelector(subtreeNodeId: string, labelSelector: string): string {
  const terms: string[] = [];
  if (subtreeNodeId !== "") terms.push(`scope_node subtree ${subtreeNodeId}`);
  const labels = labelSelector.trim();
  if (labels !== "") terms.push(labels);
  return terms.join(",");
}

export interface BulkAssignPanelProps {
  api: WaiveoApi;
  /** The site/group nodes offered as a target subtree and rendered by name. */
  nodes: ScopeNode[];
  /** Re-read the screens list after the fan-out lands. */
  onChanged: () => void | Promise<void>;
}

export function BulkAssignPanel({ api, nodes, onChanged }: BulkAssignPanelProps) {
  const [schedules, setSchedules] = useState<Schedule[]>([]);
  const [scheduleId, setScheduleId] = useState("");
  const [subtreeNodeId, setSubtreeNodeId] = useState("");
  const [labelSelector, setLabelSelector] = useState("");
  const [matched, setMatched] = useState<Screen[] | null>(null);
  const [matchError, setMatchError] = useState<string | null>(null);
  const [previewing, setPreviewing] = useState(false);
  const [applying, setApplying] = useState(false);
  const [outcomes, setOutcomes] = useState<Outcome[] | null>(null);

  const nodeNames = useMemo(
    () => new Map(nodes.map((n) => [n.id, n.name] as [string, string])),
    [nodes],
  );

  useEffect(() => {
    let live = true;
    void (async () => {
      try {
        const rows = await collectPages<Schedule>((cursor) => api.schedules.list({ cursor }));
        if (live) setSchedules(rows);
      } catch {
        // A schedule list that will not load is reported by the destination
        // picker being empty; the panel's own error line is reserved for the
        // match, which is the thing an operator is actively driving.
        if (live) setSchedules([]);
      }
    })();
    return () => {
      live = false;
    };
  }, [api]);

  const selector = composeSelector(subtreeNodeId, labelSelector);
  const schedule = schedules.find((s) => s.id === scheduleId) ?? null;
  const destination = schedule?.scope_node ?? null;

  const preview = useCallback(async () => {
    setPreviewing(true);
    setOutcomes(null);
    try {
      const rows = await collectPages<Screen>((cursor) =>
        api.screens.list(selector === "" ? { cursor } : { selector, cursor }),
      );
      setMatched(rows);
      setMatchError(null);
    } catch (err) {
      setMatched(null);
      // A malformed selector answers 400 with the grammar's own complaint —
      // surface it verbatim, since it names the term that will not parse.
      setMatchError(problemMessage(err));
    } finally {
      setPreviewing(false);
    }
  }, [api, selector]);

  const apply = useCallback(async () => {
    if (!matched || destination === null || applying) return;
    setApplying(true);
    const results: Outcome[] = [];
    for (const row of matched) {
      // A screen already under the destination needs no write. Sending the PATCH
      // anyway would bump its revision and re-sign a generation for a change
      // that is not one — churn a fleet of forty screens would feel.
      if (row.scope_node === destination) {
        results.push({ screen: row, status: "skipped" });
        continue;
      }
      try {
        await api.screens.update(row.id, { scope_node: destination }, etagForRevision(row.revision));
        results.push({ screen: row, status: "moved" });
      } catch (err) {
        if (err instanceof RevisionConflictError) {
          results.push({
            screen: row,
            status: "conflict",
            detail: "changed elsewhere while this ran — re-run the preview and apply again",
          });
        } else {
          results.push({ screen: row, status: "failed", detail: problemMessage(err) });
        }
      }
    }
    setOutcomes(results);
    const moved = results.filter((r) => r.status === "moved").length;
    const bad = results.filter((r) => r.status === "conflict" || r.status === "failed").length;
    if (bad === 0) {
      toast.success(`${moved} screen${moved === 1 ? "" : "s"} now follow ${schedule?.name ?? "the schedule"}`);
    } else {
      toast.error(`${moved} moved, ${bad} did not — see the results below.`);
    }
    setApplying(false);
    // The rows the panel holds are now stale (their revisions moved), so the
    // parent re-reads and hands fresh ones down; the preview is cleared with
    // them so a second Apply cannot run against retired ETags.
    setMatched(null);
    await onChanged();
  }, [api, applying, destination, matched, onChanged, schedule]);

  const columns = useMemo<ColumnDef<Screen>[]>(
    () => [
      { id: "name", header: "Screen", accessorFn: (s) => s.name },
      {
        id: "placement",
        header: "Currently under",
        accessorFn: (s) => nodeNames.get(s.scope_node) ?? s.scope_node,
      },
      {
        id: "change",
        header: "After",
        cell: ({ row }) =>
          destination === null ? (
            <span className="text-muted-foreground">choose a schedule</span>
          ) : row.original.scope_node === destination ? (
            <StatusBadge status="off">already there</StatusBadge>
          ) : (
            <StatusBadge status="ok">{nodeNames.get(destination) ?? destination}</StatusBadge>
          ),
      },
    ],
    [destination, nodeNames],
  );

  const outcomeColumns = useMemo<ColumnDef<Outcome>[]>(
    () => [
      { id: "name", header: "Screen", accessorFn: (o) => o.screen.name },
      {
        id: "status",
        header: "Result",
        cell: ({ row }) => {
          const o = row.original;
          if (o.status === "moved") return <StatusBadge status="ok">moved</StatusBadge>;
          if (o.status === "skipped") return <StatusBadge status="off">already there</StatusBadge>;
          if (o.status === "conflict") return <StatusBadge status="warn">conflict</StatusBadge>;
          return <StatusBadge status="error">failed</StatusBadge>;
        },
      },
      { id: "detail", header: "Detail", accessorFn: (o) => o.detail ?? "" },
    ],
    [],
  );

  return (
    <section aria-label="Bulk assign" className="flex flex-col gap-3">
      <div>
        <h2 className="text-lg font-semibold">Bulk assign</h2>
        <p className="text-sm text-muted-foreground">
          Put a schedule on a whole group of screens at once. A schedule applies to everything
          beneath the node it is authored at, so this places the matched screens under that node —
          one conditional write per screen, with the result of each shown below.
        </p>
      </div>

      <div className="grid gap-4 sm:grid-cols-2">
        <FormField label="Assign this schedule">
          {(field) => (
            <select
              {...field}
              className="flex min-h-[44px] w-full min-w-0 rounded-input border border-border bg-transparent px-3 py-1 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
              value={scheduleId}
              onChange={(e) => setScheduleId(e.target.value)}
            >
              <option value="">Choose a schedule…</option>
              {schedules.map((s) => (
                <option key={s.id} value={s.id}>
                  {s.name} — at {nodeNames.get(s.scope_node) ?? s.scope_node}
                </option>
              ))}
            </select>
          )}
        </FormField>
        <FormField label="To screens in">
          {(field) => (
            <select
              {...field}
              className="flex min-h-[44px] w-full min-w-0 rounded-input border border-border bg-transparent px-3 py-1 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
              value={subtreeNodeId}
              onChange={(e) => setSubtreeNodeId(e.target.value)}
            >
              <option value="">Anywhere in the fleet</option>
              {nodes.map((n) => (
                <option key={n.id} value={n.id}>
                  {n.name}
                </option>
              ))}
            </select>
          )}
        </FormField>
        <FormField
          label="…and matching labels"
          help="The api/1 label-selector grammar, e.g. zone=lobby,tier in (a,b). Optional."
          className="sm:col-span-2"
        >
          {(field) => (
            <input
              {...field}
              className="flex min-h-[44px] w-full min-w-0 rounded-input border border-border bg-transparent px-3 py-1 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
              value={labelSelector}
              onChange={(e) => setLabelSelector(e.target.value)}
              placeholder="zone=lobby"
            />
          )}
        </FormField>
      </div>

      <p className="text-xs text-muted-foreground">
        Selector sent to the server:{" "}
        <code data-testid="bulk-selector" className="font-mono">
          {selector === "" ? "(every screen)" : selector}
        </code>
      </p>

      <div className="flex flex-wrap gap-2">
        <Button variant="outline" icon={Layers} onClick={() => void preview()} disabled={previewing}>
          {previewing ? "Matching…" : "Preview matches"}
        </Button>
        <Button
          onClick={() => void apply()}
          disabled={applying || matched === null || matched.length === 0 || destination === null}
        >
          {applying ? "Assigning…" : "Assign to matched screens"}
        </Button>
      </div>

      {matchError ? (
        <p role="alert" className="text-sm text-[color:var(--wv-err)]">
          Couldn't match screens — {matchError}
        </p>
      ) : null}

      {matched ? (
        <>
          <p role="status" className="text-sm text-muted-foreground">
            {matched.length} screen{matched.length === 1 ? "" : "s"} matched.
          </p>
          <DataTable<Screen>
            columns={columns}
            data={matched}
            label="Matched screens"
          />
        </>
      ) : null}

      {outcomes ? (
        <>
          <h3 className="text-sm font-semibold">Results</h3>
          <DataTable<Outcome> columns={outcomeColumns} data={outcomes} label="Bulk assign results" />
        </>
      ) : null}
    </section>
  );
}
