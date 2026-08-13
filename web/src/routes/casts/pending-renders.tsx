import { useCallback, useEffect, useMemo, useState } from "react";
import { ImageOff } from "lucide-react";
import { DataTable, EmptyState, StatusBadge, type ColumnDef } from "@/components/kit";
import { ApiError, type DerivePendingLayer, type WaiveoApi } from "@/api";

/**
 * Renders waiting to happen — the library-wide view of `GET /derive/pending`.
 *
 * # What was missing
 *
 * The endpoint has existed with NO consumer anywhere in the console. Per-layer
 * status is legible in the Studio (a layer says NEEDS RENDER / BYTES MISSING),
 * but only for the cast you already have open — so to find every unrendered or
 * stale layer across the library an operator opened every cast one at a time, or
 * curled the endpoint.
 *
 * # `pending` and `stale` are different facts and are shown as such
 *
 * `pending` — no PNG has ever been produced. The projection omits the layer and
 * the rest of the slide still draws, so a screen is showing the design MINUS
 * this element.
 *
 * `stale` — a PNG exists but the spec or geometry changed since. The OLD picture
 * keeps being served, deliberately: an edit nobody has rendered yet must never
 * blank a screen. So a stale row is a screen showing something CORRECT-LOOKING
 * and out of date, which is the more dangerous of the two to leave alone and the
 * harder one to notice.
 *
 * # This panel cannot run the render, and says so once
 *
 * `waiveo-derive` is a separate binary an operator runs against the box; api/1
 * exposes no endpoint that starts a rasterization. Rather than offer a button
 * that cannot work, or stay silent and let the queue look like something the
 * console is about to handle, the panel names the tool. Adding a trigger means
 * adding an endpoint AND deciding whether the appliance runs Chromium — a
 * deployment question, not a console one.
 */

function problemMessage(err: unknown): string {
  if (err instanceof ApiError) return err.detail ?? err.code;
  return "the service is unreachable.";
}

/** Where a job's layer is authored, in the words the operator's own screen uses.
 *
 * A cast job is located by its slide's document-local id; a playlist job by the
 * index of the item whose INLINE slide carries the layer, because an inline
 * slide has no id of its own. Exactly one of the two is present (the schema says
 * so), and the fallback exists only so a malformed row renders as text rather
 * than as "undefined". */
export function locate(job: DerivePendingLayer): string {
  const where =
    job.source === "cast"
      ? job.slide_id !== undefined
        ? `slide ${job.slide_id}`
        : "slide (unidentified)"
      : job.item_index !== undefined
        ? `item ${job.item_index}`
        : "item (unidentified)";
  return `${where}, layer ${job.layer_index}`;
}

export function PendingRenders({ api }: { api: WaiveoApi }) {
  const [jobs, setJobs] = useState<DerivePendingLayer[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);

  const load = useCallback(async () => {
    try {
      setJobs(await api.casts.pendingDerives());
      setLoadError(null);
    } catch (err) {
      setJobs([]);
      setLoadError(problemMessage(err));
    }
  }, [api]);

  useEffect(() => {
    void load();
  }, [load]);

  const columns = useMemo<ColumnDef<DerivePendingLayer>[]>(
    () => [
      {
        id: "resource",
        header: "In",
        // The row's NAME when it has one — a job carrying only an id is a job an
        // operator cannot find. The id is the honest fallback, not the default.
        accessorFn: (j) => j.resource_name ?? j.resource_id,
        meta: { searchable: true },
      },
      {
        id: "source",
        header: "Kind",
        accessorFn: (j) => j.source,
        meta: { filter: "enum", filterLabel: "Kind" },
      },
      { id: "where", header: "Where", accessorFn: locate, meta: { searchable: true } },
      {
        id: "state",
        header: "State",
        accessorFn: (j) => j.state,
        meta: { filter: "enum", filterLabel: "State" },
        cell: ({ row }) =>
          row.original.state === "stale" ? (
            // Warn, not error: something IS on the screen. It is just the old
            // something, which is the point of the distinction.
            <StatusBadge status="warn">stale</StatusBadge>
          ) : (
            <StatusBadge status="pending">pending</StatusBadge>
          ),
      },
      {
        id: "size",
        header: "Size",
        accessorFn: (j) => `${j.w}×${j.h}`,
      },
    ],
    [],
  );

  const stale = (jobs ?? []).filter((j) => j.state === "stale").length;

  return (
    <section aria-label="Waiting to be rendered" className="flex flex-col gap-3">
      <div>
        <h2 className="text-base font-semibold">Waiting to be rendered</h2>
        <p className="text-sm text-muted-foreground">
          Layers whose picture has not been produced yet, or was produced before the design changed.
          A pending layer is left out of the slide; a stale one keeps showing its old picture, so a
          screen looks right and is out of date.{" "}
          {jobs !== null && jobs.length > 0
            ? "Run waiveo-derive against this box to clear them — the console cannot start a render."
            : null}
        </p>
      </div>
      {loadError ? (
        <EmptyState
          title="The render queue could not be read"
          description={loadError}
          icon={ImageOff}
        />
      ) : (
        <DataTable<DerivePendingLayer>
          label="Waiting to be rendered"
          columns={columns}
          data={jobs ?? []}
          loading={jobs === null}
          search={{ label: "Search pending renders", placeholder: "Cast, playlist or slide" }}
          filters
          pagination
          emptyState={
            <EmptyState
              icon={ImageOff}
              title="Everything is rendered"
              description="Every derive layer in the library has a current picture."
            />
          }
        />
      )}
      {stale > 0 ? (
        <p role="status" className="text-[13px] text-[color:var(--wv-warn)]">
          {stale} layer{stale === 1 ? " is" : "s are"} showing an out-of-date picture right now.
        </p>
      ) : null}
    </section>
  );
}
