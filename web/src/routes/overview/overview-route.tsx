import { useEffect, useMemo, useState, type ReactNode } from "react";
import { Link } from "react-router";
import { Cpu, Puzzle, Radio, ShieldCheck, type LucideIcon } from "lucide-react";
import { PageHeader, StatCard } from "@/components/kit";
import { ApiError, collectPages, createApi, type Pack, type WaiveoApi } from "@/api";

/**
 * The Overview route ("/") — the console's home.
 *
 * # What it reports, and why it changed
 *
 * It used to count screens, schedules, automations and the assets in play, and
 * open with three first-run steps into Extensions, Screens and Casts. Every one
 * of those areas left core on 2026-08-19: the product ships as packs, and of the
 * nine ship targets only device-discovery is installed on this box. A home page
 * whose four figures and three links all point at deleted routes is worse than
 * no home page — it reports a product the box is not running.
 *
 * So the figures are now the ones a Discovery-only box can actually answer, and
 * each is a live read of the store rather than a remembered number:
 *
 *   Devices found      what the relays have reported seeing on their networks
 *   Adopted            what THIS deployment decided to put under its control
 *   Entities           the controllable things those devices expose
 *   Extensions         installed packs — where every other capability comes from
 *
 * The first two are deliberately separate cards rather than one "12 of 30"
 * figure. Discovery and adoption are different facts with different owners
 * (relay-owned read model vs. this deployment's durable record), and the devices
 * page exists to keep them apart; collapsing them here would re-blur exactly what
 * that page unblurs. A discovered-but-unadopted fleet and an adopted fleet whose
 * relay is offline are both normal, and the two cards read differently in each.
 *
 * There is still no server-side "counts" endpoint (api/1 lists are keyset-
 * paginated, no totals, API-030), so each figure is a real walk of that
 * resource's cursors. The four run independently, so a slow or failing family
 * degrades only its own card, never the page. Each carries a graceful zero-state
 * (nothing yet → a calm next step) and a graceful error-state (couldn't load →
 * the Problem detail), so an empty or partly-degraded backend still renders a
 * coherent home.
 */

type MetricKey = "devices" | "adopted" | "entities" | "extensions";

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
    key: "devices",
    label: "Devices found",
    icon: Radio,
    emptyHint: "Nothing on the network yet — check a relay is connected.",
    count: (api) => countAll(api.devices.pages()),
  },
  {
    key: "adopted",
    label: "Adopted",
    icon: ShieldCheck,
    emptyHint: "Nothing adopted yet — adopt a device to put it under control.",
    count: (api) => countAll(api.adoptedDevices.pages()),
  },
  {
    key: "entities",
    label: "Entities",
    icon: Cpu,
    emptyHint: "No entities — a device reports these with itself.",
    count: (api) => countAll(api.entities.pages()),
  },
  {
    key: "extensions",
    label: "Extensions",
    icon: Puzzle,
    emptyHint: "No packs installed — this box can do almost nothing until one is.",
    count: async (api) => {
      // packs has no `pages()` generator (one keyset page per call), so the walk
      // is the eager collector every other pack surface uses.
      const packs = await collectPages<Pack>((cursor) => api.packs.list({ cursor }));
      return packs.length;
    },
  },
];

/** The first-run steps, in the order they unblock each other.
 *
 * Extensions leads because a freshly claimed box can do almost nothing until one
 * is installed — that was true when the register wrote "a freshly claimed box
 * arrives with no way to install anything", and it is MORE true now that the
 * eight other capability areas ship as packs rather than as core routes. The
 * remaining two are the only other things an operator can usefully do on a
 * Discovery-only box: see what the relays found, and tell the box where it is
 * (a site's timezone and coordinates are what every schedule and sun rule
 * eventually resolves against). */
const FIRST_RUN_STEPS: { to: string; label: string; detail: string }[] = [
  {
    to: "/extensions",
    label: "Install an extension",
    detail: "Almost every capability arrives as a pack. Start here.",
  },
  {
    to: "/devices",
    label: "See what is on the network",
    detail: "The relays report the whole segment; adopt the ones this box should control.",
  },
  {
    to: "/settings",
    label: "Tell the box where it is",
    detail: "A site's timezone and coordinates are what local time resolves against.",
  },
];

/** Whether this looks like a box nobody has set up yet.
 *
 * Every metric must have LOADED and be zero. A metric still loading, or one
 * that failed, does not count as empty — a box whose store is unreachable would
 * otherwise be greeted with "welcome, start here" while its real content sits
 * behind the error, which is the same absence-of-evidence mistake the update
 * badge and the catalog both had to avoid. Four zeros are only four zeros when
 * four answers arrived. */
function looksUnconfigured(metrics: Record<MetricKey, MetricState>): boolean {
  return METRICS.every((spec) => {
    const state = metrics[spec.key];
    return state.status === "ok" && state.value === 0;
  });
}

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
    devices: { status: "loading" },
    adopted: { status: "loading" },
    entities: { status: "loading" },
    extensions: { status: "loading" },
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
          description="This box at a glance — what the relays have found, what it has adopted, and what it has installed."
        />
        {looksUnconfigured(metrics) ? (
          <section
            aria-labelledby="first-run-heading"
            className="flex flex-col gap-3 rounded-input border border-border p-4"
          >
            <h2 id="first-run-heading" className="text-lg font-semibold">
              Start here
            </h2>
            <p className="text-sm text-muted-foreground">
              This box is claimed and empty. Three steps make it a working deployment.
            </p>
            <ol className="flex flex-col gap-2">
              {FIRST_RUN_STEPS.map((step, i) => (
                <li key={step.to} className="flex flex-wrap items-baseline gap-2">
                  <span className="font-mono text-xs text-muted-foreground">{i + 1}</span>
                  <Link to={step.to} className="text-sm font-medium underline underline-offset-4">
                    {step.label}
                  </Link>
                  <span className="text-sm text-muted-foreground">{step.detail}</span>
                </li>
              ))}
            </ol>
          </section>
        ) : null}

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
