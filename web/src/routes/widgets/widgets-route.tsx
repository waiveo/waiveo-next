import { useEffect, useMemo, useState } from "react";
import { Link } from "react-router";
import { CloudSun, LayoutGrid, MousePointerClick, Shapes, Sparkles } from "lucide-react";
import type { LucideIcon } from "lucide-react";
import { Button, KitIcon, PageHeader } from "@/components/kit";
import {
  ApiError,
  DERIVE_KINDS,
  collectPages,
  createApi,
  type Cast,
  type DeriveKind,
  type LayerKind,
  type WaiveoApi,
} from "@/api";
import {
  DERIVE_BLURB,
  DERIVE_LABEL,
  INTERACTIVE_BLURB,
  KIND_ICON,
  KIND_LABEL,
  STATIC_BLURB,
  STATIC_LAYER_KINDS,
  WIDGET_BLURB,
} from "@/routes/studio/layer-catalog";

/**
 * The /widgets route — what a slide can carry, browsable.
 *
 * # Why this page exists
 *
 * Legacy had a Widgets area under Slidecast (`/ext/slidecast/widgets`, "Widget
 * Library") and waiveo-next had none: the kinds existed only as an insert menu
 * INSIDE the Studio, reachable only by first opening a cast. That is parity row
 * 8.4, and it is a real gap rather than a cosmetic one — an operator deciding
 * whether this platform can put the weather on a wall had to open an editor on
 * a document they had not made yet to find out.
 *
 * # What it deliberately is NOT
 *
 * Legacy's page was an INSTANCE manager: saved, named, reusable widget records
 * with categories, thumbnails, a published/draft state, and edit/duplicate/
 * export/delete on each. waiveo-next has no such resource — a widget here is a
 * LAYER KIND, authored inline on a slide, with no standalone identity to
 * manage. Building a management UI over records that do not exist would mean
 * inventing a server model, which is a data-model decision and not an
 * information-architecture one. So this page browses the catalog and is honest
 * about it; the reusable-instance half is named as a follow-up rather than
 * faked.
 *
 * # The one live thing on it
 *
 * "Used in N casts", counted from the cast list. That is what turns a reference
 * page into an operational one — it answers "which of these am I actually
 * running?" and "is anything still using the rasterized path?" — and it costs a
 * single list call, because a cast carries its slides inline. When that call
 * fails the catalog still renders; only the counts go quiet.
 */

/** One entry in the catalog: everything the card needs, already resolved. */
interface CatalogEntry {
  /** The key usage is counted against — a layer kind, or `derive:<kind>` for the
   * three rasterized specs, which all share the single `derive` layer kind. */
  usageKey: string;
  label: string;
  blurb: string;
  icon: LucideIcon;
}

interface CatalogSection {
  id: string;
  title: string;
  icon: LucideIcon;
  /** Why this family exists — the distinction that makes it a family. */
  blurb: string;
  entries: CatalogEntry[];
}

const entry = (kind: LayerKind, blurb: string): CatalogEntry => ({
  usageKey: kind,
  label: KIND_LABEL[kind],
  blurb,
  icon: KIND_ICON[kind],
});

/**
 * The catalog, in the order an operator meets these things.
 *
 * Live data first, because that is the family the page is named for and the one
 * that needs explaining. Rasterized last, because it is the family with a real
 * cost attached (a render an operator has to go and run) and the one to reach
 * for only when nothing above will do.
 *
 * The Clock sits with the live widgets even though the codebase does not count
 * it as one — `WIDGET_LAYER_KINDS` excludes it because it predates the concept,
 * which is a history fact, not an operator-facing one. A clock draws itself from
 * the screen's own clock and never stops changing; that is what "live" means on
 * this page, so filing it anywhere else would be filing by implementation date.
 */
const SECTIONS: CatalogSection[] = [
  {
    id: "live",
    title: "Live data",
    icon: CloudSun,
    blurb:
      "These draw themselves. Nothing is typed in — the value comes from the screen's own clock, from the box, or from a device, and it keeps changing after the slide is published.",
    entries: [
      entry(
        "clock",
        "The current time, drawn by the screen from its own clock. Keeps ticking whether or not the box is reachable.",
      ),
      entry("date", WIDGET_BLURB.date),
      entry("countdown", WIDGET_BLURB.countdown),
      entry("weather", WIDGET_BLURB.weather),
      entry("entity", WIDGET_BLURB.entity),
    ],
  },
  {
    id: "interactive",
    title: "Interactive",
    icon: MousePointerClick,
    blurb:
      "Placed like any other layer, but the remote can land on them. These are what turn a cast from something a viewer watches into something a viewer steers.",
    entries: [entry("ping", INTERACTIVE_BLURB.ping), entry("nav", INTERACTIVE_BLURB.nav)],
  },
  {
    id: "static",
    title: "Static",
    icon: Shapes,
    blurb:
      "What you type or choose. The screen draws these itself with nothing to resolve, which makes them the cheapest thing a slide can carry.",
    entries: STATIC_LAYER_KINDS.map((kind) => entry(kind, STATIC_BLURB[kind])),
  },
  {
    id: "rasterized",
    title: "Rasterized",
    icon: Sparkles,
    blurb:
      "Things no screen can draw for itself. waiveo-derive renders these to a picture off the appliance, and the layer stays marked NEEDS RENDER until it has been run — so reach for one only when nothing above will do.",
    entries: DERIVE_KINDS.map((kind: DeriveKind) => ({
      usageKey: `derive:${kind}`,
      label: DERIVE_LABEL[kind],
      blurb: DERIVE_BLURB[kind],
      icon: Sparkles,
    })),
  },
];

/** Every usage key the catalog describes, in section order. Exported for the
 * test that pins the catalog against the wire's kind lists. */
export const CATALOG_USAGE_KEYS: string[] = SECTIONS.flatMap((s) =>
  s.entries.map((e) => e.usageKey),
);

/**
 * How many casts carry at least one layer of each kind.
 *
 * Per CAST, not per layer: "used in 3 casts" is the number that decides whether
 * changing something matters, where "47 layers" is trivia. A `derive` layer is
 * counted under its spec kind (`derive:qr`) rather than under `derive`, because
 * those three are what an operator picked and what the page shows.
 */
export function countCastUsage(casts: Cast[]): Record<string, number> {
  const counts: Record<string, number> = {};
  for (const cast of casts) {
    const kinds = new Set<string>();
    for (const slide of cast.slides ?? []) {
      for (const layer of slide.layers ?? []) {
        kinds.add(layer.kind === "derive" ? `derive:${layer.derive?.kind ?? "unknown"}` : layer.kind);
      }
    }
    for (const kind of kinds) counts[kind] = (counts[kind] ?? 0) + 1;
  }
  return counts;
}

function useCastUsage(api: WaiveoApi): {
  usage: Record<string, number> | null;
  error: string | null;
} {
  const [usage, setUsage] = useState<Record<string, number> | null>(null);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const casts = await collectPages<Cast>((cursor) => api.casts.list({ cursor }));
        if (!cancelled) {
          setUsage(countCastUsage(casts));
          setError(null);
        }
      } catch (err) {
        if (cancelled) return;
        // The catalog is the page; the counts are an enrichment. A failure here
        // must never blank it — it reports itself and the cards render on.
        setUsage(null);
        setError(
          err instanceof ApiError ? (err.detail ?? err.code) : "The cast library is unreachable.",
        );
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [api]);

  return { usage, error };
}

/** Usage is deliberately NOT a StatusBadge. That component's ok/green lane is
 * reserved by the brand for live/ok, and "used in 3 casts" is a fact about the
 * library, not a health signal — dressing it as one would put a green chip on a
 * page where nothing is up or down. Plain text, and nothing at all while the
 * count is still unknown (a card that flashes "Not used yet" before the list
 * lands is worse than a card that says nothing yet). */
function UsageNote({ count }: { count: number | undefined }) {
  if (count === undefined) return null;
  return (
    <p data-slot="widget-usage" className="text-[12px] font-medium text-muted-foreground">
      {count === 0 ? "Not used yet" : count === 1 ? "Used in 1 cast" : `Used in ${count} casts`}
    </p>
  );
}

export default function WidgetsRoute({ api }: { api?: WaiveoApi } = {}) {
  const client = useMemo(() => api ?? createApi(), [api]);
  const { usage, error } = useCastUsage(client);

  return (
    <div className="flex flex-col gap-8 p-4 sm:p-6 lg:p-8">
      <PageHeader
        variant="hero"
        title="Widgets"
        description="Everything a slide can carry, and where each one gets its value from. A widget lives on a slide rather than on its own — open a cast in the Studio to place one."
        actions={
          <Button asChild>
            <Link to="/casts">
              <KitIcon icon={LayoutGrid} decorative className="size-4" />
              Open the cast library
            </Link>
          </Button>
        }
      />

      {error ? (
        <p
          data-slot="usage-error"
          role="status"
          className="text-sm text-muted-foreground"
        >
          Usage counts are unavailable — {error} The catalog below is unaffected.
        </p>
      ) : null}

      {SECTIONS.map((section) => (
        <section key={section.id} aria-labelledby={`widgets-${section.id}`} className="flex flex-col gap-4">
          <div className="flex flex-col gap-1.5">
            <h2
              id={`widgets-${section.id}`}
              className="flex items-center gap-2 font-display text-[19px] font-semibold"
            >
              <KitIcon icon={section.icon} decorative className="size-4.5 shrink-0 text-[color:var(--wv-accent-text)]" />
              {section.title}
            </h2>
            <p className="max-w-2xl text-sm text-muted-foreground">{section.blurb}</p>
          </div>
          <ul className="grid grid-cols-1 gap-3 sm:grid-cols-2 xl:grid-cols-3">
            {section.entries.map((item) => (
              <li
                key={item.usageKey}
                data-slot="widget-card"
                data-usage-key={item.usageKey}
                className="flex min-w-0 flex-col gap-2 rounded-card border border-border bg-card p-4"
              >
                <div className="flex items-start gap-3">
                  <KitIcon
                    icon={item.icon}
                    decorative
                    className="mt-0.5 size-5 shrink-0 text-[color:var(--wv-accent-text)]"
                  />
                  <div className="flex min-w-0 flex-col gap-1">
                    <h3 className="text-sm font-semibold">{item.label}</h3>
                    <p className="text-[13px] text-muted-foreground">{item.blurb}</p>
                  </div>
                </div>
                <div className="mt-auto pt-1">
                  {/* `usage` is null while the list is in flight and after it
                      fails; both mean "unknown", and the card says nothing. Once
                      it HAS loaded, a key the counter never saw means zero — the
                      map only records kinds it found, so reading it raw would
                      leave every unused widget silent instead of saying so. */}
                  <UsageNote
                    count={usage === null ? undefined : (usage[item.usageKey] ?? 0)}
                  />
                </div>
              </li>
            ))}
          </ul>
        </section>
      ))}
    </div>
  );
}
