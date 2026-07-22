import { useEffect, useMemo, useState, type ReactNode } from "react";
import { CalendarClock, Images, MonitorPlay, Wand2, type LucideIcon } from "lucide-react";
import { PageHeader, StatCard } from "@/components/kit";
import { ApiError, createApi, type WaiveoApi } from "@/api";

/**
 * The Overview route ("/") — the console's home. It reports live counts of the
 * things the operator manages — screens, schedules, automations, and the media
 * assets in play — read straight from the api/1 store. There is no server-side
 * "counts" endpoint (api/1 lists are keyset-paginated, no totals, API-030), so
 * each figure is a real walk of that resource's cursors; the four run
 * independently, so a slow or failing family degrades only its own card, never
 * the page. Each card carries a graceful zero-state (nothing yet → a calm next
 * step) and a graceful error-state (couldn't load → the Problem detail), so an
 * empty or partly-degraded backend still renders a coherent home.
 *
 * "Assets" has no resource of its own — content is content-addressed and
 * write-only in api/1 (POST /content), with no inventory to list — so the figure
 * is derived live from the playlists that reference assets: the count of distinct
 * asset_refs in play. That keeps it a true reading of the store (and naturally
 * zero when nothing is scheduled) rather than an invented number.
 */

type MetricKey = "screens" | "schedules" | "automations" | "assets";

type MetricState =
  | { status: "loading" }
  | { status: "ok"; value: number }
  | { status: "error"; detail: string; traceId: string | null };

interface MetricSpec {
  key: MetricKey;
  label: string;
  icon: LucideIcon;
  /** The zero-state nudge shown when the live count is exactly 0. */
  emptyHint: string;
  count: (api: WaiveoApi) => Promise<number>;
}

async function countAll<T>(pages: AsyncGenerator<T, void, void>): Promise<number> {
  let n = 0;
  for await (const _ of pages) {
    void _;
    n += 1;
  }
  return n;
}

const METRICS: MetricSpec[] = [
  {
    key: "screens",
    label: "Screens",
    icon: MonitorPlay,
    emptyHint: "No screens yet — add one to start casting.",
    count: (api) => countAll(api.scopeNodes.pages({ selector: "kind=screen" })),
  },
  {
    key: "schedules",
    label: "Schedules",
    icon: CalendarClock,
    emptyHint: "No schedules yet — program a daypart to light a screen up.",
    count: (api) => countAll(api.schedules.pages()),
  },
  {
    key: "automations",
    label: "Automations",
    icon: Wand2,
    emptyHint: "No automations yet — a rule reacts to the fleet for you.",
    count: (api) => countAll(api.automations.pages()),
  },
  {
    key: "assets",
    label: "Assets in play",
    icon: Images,
    emptyHint: "No assets in play — upload content and add it to a playlist.",
    count: async (api) => {
      const refs = new Set<string>();
      for await (const pl of api.playlists.pages()) {
        for (const item of pl.items ?? []) {
          if (item.source === "asset" && item.asset_ref) refs.add(item.asset_ref);
        }
      }
      return refs.size;
    },
  },
];

function errorState(err: unknown): { status: "error"; detail: string; traceId: string | null } {
  if (err instanceof ApiError) {
    return { status: "error", detail: err.detail ?? err.title ?? err.code, traceId: err.traceId };
  }
  return { status: "error", detail: "The service is unreachable.", traceId: null };
}

function MetricValue({ state }: { state: MetricState }): ReactNode {
  if (state.status === "ok") return state.value.toLocaleString();
  // Loading and error both read as a placeholder dash in the mono figure slot;
  // the hint below distinguishes them.
  return <span className="text-muted-foreground">—</span>;
}

function metricHint(spec: MetricSpec, state: MetricState): ReactNode {
  if (state.status === "loading") return "Loading…";
  if (state.status === "error") {
    return (
      <span className="text-[color:var(--wv-err)]">
        Couldn't load — {state.detail}
        {state.traceId ? (
          <span className="mt-0.5 block font-mono text-[11px] text-muted-foreground">
            trace {state.traceId}
          </span>
        ) : null}
      </span>
    );
  }
  if (state.value === 0) return spec.emptyHint;
  return null;
}

export default function OverviewRoute({ api }: { api?: WaiveoApi }) {
  const client = useMemo(() => api ?? createApi(), [api]);
  const [metrics, setMetrics] = useState<Record<MetricKey, MetricState>>({
    screens: { status: "loading" },
    schedules: { status: "loading" },
    automations: { status: "loading" },
    assets: { status: "loading" },
  });

  useEffect(() => {
    let live = true;
    // Each metric resolves on its own so one failing family never blanks the page.
    for (const spec of METRICS) {
      spec
        .count(client)
        .then((value) => {
          if (live) setMetrics((m) => ({ ...m, [spec.key]: { status: "ok", value } }));
        })
        .catch((err: unknown) => {
          if (live) setMetrics((m) => ({ ...m, [spec.key]: errorState(err) }));
        });
    }
    return () => {
      live = false;
    };
  }, [client]);

  return (
    <div className="min-h-screen bg-background text-foreground">
      <div className="mx-auto flex max-w-[1200px] flex-col gap-6 px-4 py-6 lg:px-8">
        <PageHeader
          variant="hero"
          title="Overview"
          description="Your fleet at a glance — screens, schedules, automations, and the assets in play, live from the store."
        />
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
          {METRICS.map((spec) => {
            const state = metrics[spec.key];
            const hint = metricHint(spec, state);
            return (
              <StatCard
                key={spec.key}
                label={spec.label}
                icon={spec.icon}
                value={<MetricValue state={state} />}
                {...(hint ? { hint } : {})}
              />
            );
          })}
        </div>
      </div>
    </div>
  );
}
