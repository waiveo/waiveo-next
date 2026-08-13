import { useCallback, useEffect, useMemo, useState } from "react";
import { ImageOff } from "lucide-react";
import { EmptyState } from "@/components/kit";
import { PageRenderer } from "@/renderer";
import { ApiError, type DerivePendingLayer, type WaiveoApi } from "@/api";
import pendingRendersDoc from "./pending-renders.uis.json";

/**
 * Renders waiting to happen — the library-wide view of `GET /derive/pending`.
 *
 * # Authored as `ui-schema/1`, not as React
 *
 * `pending-renders.uis.json` is the whole panel, rendered through the same
 * PageRenderer an extension page goes through. This file is the DATA half only:
 * fetch, shape, hand over.
 *
 * The first version of this panel was hand-written TSX, which the loop's own
 * standing rule (parity-loop runbook, step 1.5) says not to do — a console page
 * belongs in a `.uis.json` document unless a named renderer limit forces
 * otherwise, because 81% of the register's rows were extension-owned in legacy
 * and a page in this format is PORTABLE INTO A PACK while the same page in TSX
 * is thrown away. Slidecast is one of the nine extensions this programme exists
 * to extract, so this panel is squarely on that list. It was converted rather
 * than left as redo bill.
 *
 * Nothing in the catalog had to be stretched to do it. Two limits were worked
 * WITH rather than around, and they are worth recording because the list of such
 * limits is the specification for renderer work:
 *
 *   - `table` has no `emptyMsg` prop, so the empty state is a sibling `switch`
 *     on `isEmpty(jobs)` rather than a property of the table. That is arguably
 *     the better shape anyway — the empty sentence is a different statement from
 *     the table, not a decoration on it.
 *   - There is no `gt` among the Computed functions (UIS-140), so "some are
 *     stale" is `isEmpty(stale_jobs)` over a pre-filtered array rather than a
 *     count comparison. The filtering happens here, where the fetch already is.
 *
 * # What was missing before any of this
 *
 * The endpoint had NO consumer anywhere in the console. Per-layer status is
 * legible in the Studio, but only for the cast already open — so finding every
 * unrendered or stale layer across the library meant opening every cast one at a
 * time, or curling the endpoint.
 *
 * # `pending` and `stale` are different facts
 *
 * `pending` — no PNG has ever been produced. The projection omits the layer and
 * the rest of the slide still draws, so a screen shows the design MINUS this
 * element. `stale` — a PNG exists but the spec or geometry changed since; the
 * OLD picture keeps being served, deliberately, because an edit nobody has
 * rendered must never blank a screen. So a stale row is a screen showing
 * something CORRECT-LOOKING and out of date, which is the more dangerous of the
 * two and the harder to notice. It gets its own line.
 *
 * # This panel cannot run the render, and says so once
 *
 * `waiveo-derive` is a separate binary; api/1 exposes no endpoint that starts a
 * rasterization. Rather than offer a button that cannot work, or stay silent and
 * let the queue look like something the console is about to handle, the copy
 * names the tool. Adding a trigger means adding an endpoint AND deciding whether
 * the appliance runs Chromium — a deployment question, not a console one.
 */

/** The `msg:` catalog `pending-renders.uis.json`'s references resolve against.
 *
 * Held here rather than in the document because the document is the LAYOUT and
 * this is the copy — the same split every other core ui-schema page uses, and
 * the one that makes the document portable into a pack whose own locale catalog
 * would supply these. */
const MESSAGES: Record<string, string> = {
  "msg:renders.title": "Waiting to be rendered",
  "msg:renders.lead":
    "Layers whose picture has not been produced yet, or was produced before the design changed. A pending layer is left out of the slide; a stale one keeps showing its old picture, so a screen looks right and is out of date.",
  // Rendered only in the non-empty branch: naming the tool on an empty queue is
  // a nag about nothing, and the original panel was careful about that.
  "msg:renders.runTool":
    "Run waiveo-derive against this box to clear them — the console cannot start a render.",
  "msg:renders.empty": "Everything is rendered — every derive layer in the library has a current picture.",
  "msg:renders.stale": "{0} layer(s) are showing an out-of-date picture right now.",
  "msg:renders.col.in": "In",
  "msg:renders.col.kind": "Kind",
  "msg:renders.col.where": "Where",
  "msg:renders.col.state": "State",
  "msg:renders.col.size": "Size",
};

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

/** One job as the document binds it.
 *
 * The display strings are computed HERE rather than in the document for the
 * reason every other core ui-schema page computes its own: the catalog has no
 * string concatenation, and a `cell` is a Binding at a row — so "slide s1, layer
 * 0" has to arrive already assembled. */
export interface RenderJobRow {
  where_resource: string;
  source: string;
  where_layer: string;
  state_label: string;
  size_display: string;
}

export function toRow(job: DerivePendingLayer): RenderJobRow {
  return {
    // The row's NAME when it has one — a job carrying only an id is a job an
    // operator cannot find. The id is the honest fallback, not the default.
    where_resource: job.resource_name ?? job.resource_id,
    source: job.source,
    where_layer: locate(job),
    state_label: job.state,
    size_display: `${job.w}×${job.h}`,
  };
}

export function PendingRenders({ api }: { api: WaiveoApi }) {
  const [jobs, setJobs] = useState<DerivePendingLayer[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  // Bumped on every load so PageRenderer REMOUNTS against the fetched rows. The
  // renderer seeds its store once from the initial resource, so a panel that
  // fetches after first paint — which is every panel — shows the empty state
  // forever without this. The pack page route carries the same counter for the
  // same reason; this one was written without it and every row-bearing test
  // failed, which is the cheapest possible way to find out.
  const [generation, setGeneration] = useState(0);

  const load = useCallback(async () => {
    try {
      setJobs(await api.casts.pendingDerives());
      setLoadError(null);
    } catch (err) {
      setJobs([]);
      setLoadError(problemMessage(err));
    } finally {
      setGeneration((g) => g + 1);
    }
  }, [api]);

  useEffect(() => {
    void load();
  }, [load]);

  // `stale_jobs` is a pre-filtered array rather than a count, because UIS-140
  // has no `gt` — see the header. The document asks `isEmpty` of it.
  const data = useMemo(() => {
    const all = jobs ?? [];
    return {
      jobs: all.map(toRow),
      stale_jobs: all.filter((j) => j.state === "stale").map(toRow),
    };
  }, [jobs]);

  if (loadError) {
    return (
      <section aria-label="Waiting to be rendered" className="flex flex-col gap-3">
        <EmptyState
          title="The render queue could not be read"
          description={loadError}
          icon={ImageOff}
        />
      </section>
    );
  }

  return (
    <section aria-label="Waiting to be rendered" className="flex flex-col gap-3">
      <PageRenderer key={generation} doc={pendingRendersDoc} data={data} messages={MESSAGES} />
    </section>
  );
}
