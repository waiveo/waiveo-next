import { useState, type ReactNode } from "react";
import {
  Activity,
  AudioWaveform,
  Bell,
  CalendarClock,
  LayoutDashboard,
  Megaphone,
  MessageSquare,
  MonitorPlay,
  MoreHorizontal,
  Palette,
  Plus,
  Radio,
  Shapes,
  SquarePen,
  Table as TableIcon,
  Trash2,
  Type as TypeIcon,
  Upload,
  Zap,
  type LucideIcon,
} from "lucide-react";
import {
  Button,
  DataTable,
  EmptyState,
  FormField,
  KitIcon,
  Checkbox,
  Modal,
  ConfirmModal,
  PageHeader,
  StatCard,
  StatusBadge,
  Toaster,
  toast,
  type ColumnDef,
  type Status,
} from "@/components/kit";
import { useTheme } from "@/components/theme/theme-context";
import { cn } from "@/lib/utils";
import {
  daybreakPalette,
  duskPalette,
  gradients,
  radii,
  spacing,
  typeScale,
  type Swatch,
} from "./tokens";
import {
  automations,
  dayparts,
  screens,
  type AutomationRow,
  type ScreenRow,
} from "./fixtures";

/**
 * The /design route — the living design gallery. It renders every widget-kit
 * primitive with realistic signage fixtures (screens, schedules, automations)
 * and visualizes the HORIZON token scales — both palettes, the gradient set as
 * labeled bands, the type specimen — in whichever sky (Dusk / Daybreak) the
 * toggle selects. It is the design system's reference: what a component looks
 * like here is what it looks like everywhere, because the kit is the only
 * sanctioned surface.
 *
 * The whole page obeys the brand build-rules it documents: gradients live only
 * on the hero band, the primary action, and the content-poster decks; every
 * word sits on the flat content layer; text never renders on a raw gradient (a
 * scrim or a flat chip always intervenes); and green is reserved for live/ok.
 */

interface Section {
  id: string;
  label: string;
  icon: LucideIcon;
}

const SECTIONS: Section[] = [
  { id: "overview", label: "Overview", icon: AudioWaveform },
  { id: "color", label: "Color", icon: Palette },
  { id: "gradients", label: "Gradients", icon: MonitorPlay },
  { id: "type", label: "Typography", icon: TypeIcon },
  { id: "shape", label: "Shape & space", icon: Shapes },
  { id: "actions", label: "Buttons", icon: SquarePen },
  { id: "status", label: "Status", icon: Radio },
  { id: "stats", label: "Stat cards", icon: LayoutDashboard },
  { id: "data", label: "Data tables", icon: TableIcon },
  { id: "forms", label: "Forms", icon: SquarePen },
  { id: "overlays", label: "Modals & toasts", icon: MessageSquare },
  { id: "empty", label: "Empty states", icon: CalendarClock },
];

// ── Gallery-local presentation ──────────────────────────────────────────────
// Buttons come from the kit (@/components/kit) like every other primitive — the
// route never re-implements the vendored control, so the gallery's `default`
// button is byte-for-byte the one every page renders.

// Native form control styling, straight from the tokens (radius 8, hairline
// border, focus ring) — used inside FormField, whose render-prop wires the a11y.
const fieldClass =
  "flex h-9 w-full rounded-input border border-border bg-transparent px-3 py-1 text-sm text-foreground outline-none transition-[box-shadow,border-color] focus-visible:ring-2 focus-visible:ring-ring aria-invalid:border-[color:var(--wv-err)] placeholder:text-muted-foreground";

// A framed, titled section with a stable anchor id.
function SectionShell({
  id,
  title,
  intro,
  children,
}: {
  id: string;
  title: string;
  intro?: string;
  children: ReactNode;
}) {
  return (
    <section id={id} aria-labelledby={`${id}-title`} className="flex scroll-mt-8 flex-col gap-5">
      <div className="flex flex-col gap-1.5 border-b border-border pb-3">
        <h2 id={`${id}-title`} className="font-display text-[21px] font-semibold tracking-[-0.01em]">
          {title}
        </h2>
        {intro ? <p className="max-w-2xl text-sm text-muted-foreground">{intro}</p> : null}
      </div>
      {children}
    </section>
  );
}

// ── Color ───────────────────────────────────────────────────────────────────
function PaletteColumn({ name, palette, testId }: { name: string; palette: Swatch[]; testId: string }) {
  return (
    <div data-testid={testId} className="flex flex-1 flex-col gap-3 rounded-card border border-border bg-card p-5 wv-elevation">
      <h3 className="font-display text-[17px] font-semibold">{name}</h3>
      <ul className="flex flex-col gap-2">
        {palette.map((s) => (
          <li key={s.token} className="flex items-center gap-3">
            <span
              className="size-9 shrink-0 rounded-input border border-border"
              style={{ background: s.value }}
              aria-hidden="true"
            />
            <span className="flex min-w-0 flex-col">
              <span className="font-mono text-[13px]">
                --wv-{s.token} <span className="text-muted-foreground">{s.value}</span>
              </span>
              <span className="text-[13px] text-muted-foreground">{s.role}</span>
            </span>
          </li>
        ))}
      </ul>
    </div>
  );
}

function ColorSection() {
  return (
    <SectionShell
      id="color"
      title="Color — two skies, one ramp"
      intro="Hue and chroma are held across the themes; only tone swaps. Both palettes are shown at once — the flat content layer where every word, table and form renders, AA-provable in both skies."
    >
      <div className="flex flex-col gap-4 lg:flex-row">
        <PaletteColumn name="Dusk (dark)" palette={duskPalette} testId="palette-dusk" />
        <PaletteColumn name="Daybreak (light)" palette={daybreakPalette} testId="palette-daybreak" />
      </div>
    </SectionShell>
  );
}

// ── Gradients ─────────────────────────────────────────────────────────────
function GradientsSection() {
  const [hero, accent] = gradients;
  const bands = [hero, accent];
  return (
    <SectionShell
      id="gradients"
      title="Gradients — the atmosphere layer"
      intro="Gradient paints the stage, never the page: the hero band, the one primary action, and the content-poster decks. Each re-tones with the theme (interpolated in oklch), grained to kill banding. Text never sits on the raw ramp — a scrim or a flat chip always intervenes."
    >
      <div className="grid gap-4 sm:grid-cols-2">
        {bands.map((g) => (
          <figure key={g.cssVar} className="flex flex-col gap-2">
            <div
              data-testid="gradient-band"
              className="wv-grain h-24 rounded-card border border-border"
              style={{ backgroundImage: `var(${g.cssVar})` }}
              aria-hidden="true"
            />
            <figcaption className="flex flex-col">
              <span className="font-mono text-[13px]">{g.cssVar}</span>
              <span className="text-[13px] text-muted-foreground">{g.where}</span>
            </figcaption>
          </figure>
        ))}
      </div>

      <div className="flex flex-col gap-2">
        <p className="text-[11px] font-semibold tracking-[0.10em] text-muted-foreground uppercase">
          The decks — the sky turning across the day
        </p>
        <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
          {dayparts.map((d) => (
            <div
              key={d.cssVar}
              data-testid="gradient-band"
              className="wv-grain relative h-40 overflow-hidden rounded-card border border-border"
              style={{ backgroundImage: `var(${d.cssVar})` }}
            >
              {/* Deck fg is a flat text-color chip, never the raw ramp. */}
              <div className="absolute inset-x-3 bottom-3 z-10 flex flex-col gap-0.5 rounded-input border border-border bg-card p-3">
                <span className="font-display text-[15px] font-semibold">{d.name}</span>
                <span className="font-mono text-[12px] text-muted-foreground">{d.window}</span>
                <span className="text-[12px] text-muted-foreground">{d.program}</span>
              </div>
            </div>
          ))}
        </div>
      </div>
    </SectionShell>
  );
}

// ── Typography ────────────────────────────────────────────────────────────
function TypeSection() {
  return (
    <SectionShell
      id="type"
      title="Typography"
      intro="Bricolage Grotesque carries the display voice, Inter is the console workhorse, JetBrains Mono renders every measured value. All self-hosted (OFL) — the built bundle makes zero font-CDN requests."
    >
      <div className="flex flex-col divide-y divide-border rounded-card border border-border bg-card">
        {typeScale.map((t) => (
          <div key={t.role} className="flex flex-col gap-2 p-5 sm:flex-row sm:items-baseline sm:gap-6">
            <div className="flex w-52 shrink-0 flex-col">
              <span className="text-[13px] font-medium">{t.role}</span>
              <span className="text-[12px] text-muted-foreground">
                {t.family} · {t.size}/{t.weight} · {t.lineHeight}
              </span>
            </div>
            <div
              className={cn("min-w-0", t.fontClass, t.tabular && "wv-tnum")}
              style={{
                fontSize: `${t.size}px`,
                fontWeight: t.weight,
                lineHeight: t.lineHeight,
                letterSpacing: `${t.tracking}em`,
                textTransform: t.uppercase ? "uppercase" : "none",
              }}
            >
              {t.sample}
            </div>
          </div>
        ))}
      </div>
    </SectionShell>
  );
}

// ── Shape & space ───────────────────────────────────────────────────────────
function ShapeSection() {
  return (
    <SectionShell
      id="shape"
      title="Shape & space"
      intro="Softer radii for the daybreak warmth, still engineered. A 4px base space scale — compact for the console, breathing for the studio."
    >
      <div className="grid gap-4 lg:grid-cols-2">
        <div className="flex flex-col gap-3 rounded-card border border-border bg-card p-5 wv-elevation">
          <h3 className="font-display text-[17px] font-semibold">Radii</h3>
          <div className="flex flex-wrap gap-4">
            {radii.map((r) => (
              <div key={r.role} className="flex flex-col items-center gap-1.5">
                <div className={cn("size-14 border border-border bg-[color:var(--wv-surface-2)]", r.radiusClass)} />
                <span className="text-[12px] font-medium">{r.value}</span>
                <span className="max-w-20 text-center text-[11px] text-muted-foreground">{r.role}</span>
              </div>
            ))}
          </div>
        </div>
        <div className="flex flex-col gap-3 rounded-card border border-border bg-card p-5 wv-elevation">
          <h3 className="font-display text-[17px] font-semibold">Space</h3>
          <ul className="flex flex-col gap-2">
            {spacing.map((s) => (
              <li key={s} className="flex items-center gap-3">
                <span className="w-8 font-mono text-[13px] wv-tnum text-muted-foreground">{s}</span>
                <span
                  className="h-3 rounded-[3px] bg-[color:var(--wv-accent)]"
                  style={{ width: `${s}px` }}
                  aria-hidden="true"
                />
              </li>
            ))}
          </ul>
        </div>
      </div>
    </SectionShell>
  );
}

// ── Buttons ─────────────────────────────────────────────────────────────────
function ActionsSection() {
  return (
    <SectionShell
      id="actions"
      title="Buttons & actions"
      intro="One primary action per view carries --grad-accent (the hotter Afterglow ramp, ink holding AA across the whole gradient). Every other variant stays flat."
    >
      <div className="flex flex-wrap items-center gap-3 rounded-card border border-border bg-card p-5 wv-elevation">
        <Button variant="default" icon={Upload}>
          Publish schedule
        </Button>
        <Button variant="secondary">Save draft</Button>
        <Button variant="outline">Preview</Button>
        <Button variant="ghost">Cancel</Button>
        <Button variant="destructive" icon={Trash2}>
          Remove screen
        </Button>
        <Button variant="link">Learn more</Button>
      </div>
    </SectionShell>
  );
}

// ── Status ──────────────────────────────────────────────────────────────────
const STATUS_SPECIMENS: { status: Status; label: string; note: string }[] = [
  { status: "ok", label: "On air", note: "Live — the reserved green" },
  { status: "warn", label: "Degraded", note: "Amber, hue-distant from magenta" },
  { status: "error", label: "Dropped", note: "Error red" },
  { status: "off", label: "Offline", note: "Disabled / offline" },
  { status: "pending", label: "Syncing", note: "In progress" },
];

function StatusSection() {
  return (
    <SectionShell
      id="status"
      title="Status"
      intro="Status is flat fill + icon + label — legible in greyscale, never a gradient wash. Green means live; nothing decorative borrows it."
    >
      <div className="flex flex-wrap gap-6 rounded-card border border-border bg-card p-5 wv-elevation">
        {STATUS_SPECIMENS.map((s) => (
          <div key={s.status} className="flex flex-col items-start gap-1.5">
            <StatusBadge status={s.status}>{s.label}</StatusBadge>
            <span className="text-[12px] text-muted-foreground">{s.note}</span>
          </div>
        ))}
      </div>
    </SectionShell>
  );
}

// ── Stat cards ────────────────────────────────────────────────────────────
function StatsSection() {
  const online = screens.filter((s) => s.status === "ok").length;
  const devices = screens.reduce((n, s) => n + s.devices, 0);
  return (
    <SectionShell
      id="stats"
      title="Stat cards"
      intro="A single measured figure on the flat content layer, set in the mono face with tabular numerals so a row lines up digit-for-digit."
    >
      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <StatCard label="Screens on air" value={online} icon={MonitorPlay} hint="of 5 governed" />
        <StatCard label="Devices online" value={devices} icon={Radio} hint="across 2 sites" />
        <StatCard label="Automations" value={automations.length} icon={Zap} hint="1 fired today" />
        <StatCard label="Uptime" value="99.98%" icon={Activity} hint="last 30 days" />
      </div>
    </SectionShell>
  );
}

// ── Data tables ─────────────────────────────────────────────────────────────
// `meta.searchable` and `meta.filter` are how a column opts INTO the toolbar —
// declared on the column, so the searched set and the filter's options are always
// derived from the columns and rows that exist rather than from a second list.
const screenColumns: ColumnDef<ScreenRow>[] = [
  { accessorKey: "name", header: "Screen", meta: { searchable: true } },
  { accessorKey: "site", header: "Site", meta: { searchable: true } },
  { accessorKey: "address", header: "Address", meta: { numeric: true, align: "left", searchable: true } },
  { accessorKey: "devices", header: "Devices", meta: { numeric: true } },
  { accessorKey: "daypart", header: "Daypart", meta: { filter: "enum" } },
  {
    id: "status",
    header: "Status",
    // An accessor as well as a cell: the badge is the PRESENTATION, the label is
    // the value — a filter (or a sort) with nothing to read cannot work.
    accessorFn: (row) => row.statusLabel,
    meta: { filter: "enum", filterLabel: "Status" },
    cell: ({ row }) => (
      <StatusBadge status={row.original.status}>{row.original.statusLabel}</StatusBadge>
    ),
  },
];

const automationColumns: ColumnDef<AutomationRow>[] = [
  { accessorKey: "name", header: "Automation" },
  { accessorKey: "trigger", header: "Trigger" },
  { accessorKey: "lastRun", header: "Last run", meta: { align: "right" } },
  {
    id: "status",
    header: "Status",
    enableSorting: false,
    cell: ({ row }) => (
      <StatusBadge status={row.original.status}>{row.original.statusLabel}</StatusBadge>
    ),
  },
];

function DataSection() {
  const [loading, setLoading] = useState(false);
  return (
    <SectionShell
      id="data"
      title="Data tables"
      intro="One table idiom over @tanstack/react-table: sortable header buttons with aria-sort, mono-tabular numerics, a loading skeleton, an empty state, per-row actions — and, opt-in per call site, a debounced search, per-column filters faceted from the data, paging with an announced range, and row selection whose select-all means the page and says so. A gradient never touches it."
    >
      <div className="flex flex-col gap-3">
        <div className="flex items-center gap-3">
          <Button variant="outline" onClick={() => setLoading((v) => !v)}>
            {loading ? "Show data" : "Show loading"}
          </Button>
        </div>
        {/* The gallery turns everything on at once — it is the reference for what
            each affordance looks like. A real page opts in to what its list is
            long enough to need; the Automations table below has none of it.
            `pageSize` is 3 against 5 fixture rows so the pager is actually
            exercised rather than sitting inert. */}
        <DataTable
          columns={screenColumns}
          data={screens}
          label="Screens"
          loading={loading}
          search={{ label: "Search screens", placeholder: "Name, site or address" }}
          filters
          pagination={{ pageSize: 3, pageSizeOptions: [3, 5, 10] }}
          selection={{
            rowId: (row) => row.id,
            rowLabel: (row) => `Select ${row.name}`,
            bulkActions: (chosen) => (
              <Button
                variant="outline"
                onClick={() => toast(`Published to ${chosen.length} screen${chosen.length === 1 ? "" : "s"}`)}
              >
                Publish to selected
              </Button>
            ),
          }}
          rowActions={(row) => (
            <button
              type="button"
              aria-label={`Actions for ${row.name}`}
              onClick={() => toast(`Opened actions for ${row.name}`)}
              className="wv-touch inline-flex items-center justify-center rounded-input text-muted-foreground outline-none hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring"
            >
              <KitIcon icon={MoreHorizontal} decorative className="size-4" />
            </button>
          )}
        />
      </div>

      <div className="flex flex-col gap-2">
        <p className="text-[11px] font-semibold tracking-[0.10em] text-muted-foreground uppercase">
          Automations
        </p>
        <DataTable columns={automationColumns} data={automations} label="Automations" />
      </div>
    </SectionShell>
  );
}

// ── Forms ─────────────────────────────────────────────────────────────────
function FormsSection() {
  return (
    <SectionShell
      id="forms"
      title="Forms"
      intro="FormField is the one form idiom — label, control, help and error composed with the a11y wired in one place. Its render-prop hands the control the id, describedby, invalid and required plumbing; passing them on is the whole contract."
    >
      <form
        onSubmit={(e) => {
          e.preventDefault();
          toast.success("Screen saved");
        }}
        className="grid max-w-2xl gap-5 rounded-card border border-border bg-card p-6 wv-elevation"
      >
        <FormField label="Screen name" help="Shown on the device list." required>
          {(field) => <input {...field} className={fieldClass} placeholder="e.g. The Hangar TV" />}
        </FormField>

        <FormField label="Site">
          {(field) => (
            <select {...field} className={fieldClass} defaultValue="riverside">
              <option value="riverside">Riverside Roastery</option>
              <option value="depot">Depot Nine</option>
            </select>
          )}
        </FormField>

        <FormField label="Daypart" error="Pick a daypart to light up.">
          {(field) => (
            <select {...field} className={fieldClass} defaultValue="">
              <option value="" disabled>
                Choose a daypart
              </option>
              {dayparts.map((d) => (
                <option key={d.name} value={d.name}>
                  {d.name} · {d.window}
                </option>
              ))}
            </select>
          )}
        </FormField>

        <FormField label="Notes" help="Optional context for the fleet.">
          {(field) => (
            <textarea {...field} rows={3} className={cn(fieldClass, "h-auto py-2")} placeholder="Add a note" />
          )}
        </FormField>

        {/* The kit's Checkbox, which is also what a DataTable's row-selection
            column ticks. It is a kit wrapper rather than a raw shadcn import
            because a route may not reach `@/components/ui` at all — and it
            demands an accessible name at the TYPE level, so a bare box in a
            table cell is never nameless. */}
        <div className="flex items-center gap-2">
          {/* Named by the visible text via aria-labelledby rather than a
              duplicate aria-label — a Radix checkbox renders as a <button>, which
              a `<label for>` does not associate with at all. */}
          <Checkbox aria-labelledby="design-watchdog-label" />
          <span id="design-watchdog-label" className="text-[13px] text-foreground">
            Restart the player if it stops reporting
          </span>
        </div>

        <div className="flex gap-3">
          <Button variant="default" type="submit">
            Save screen
          </Button>
          <Button variant="ghost">Cancel</Button>
        </div>
      </form>
    </SectionShell>
  );
}

// ── Modals & toasts ─────────────────────────────────────────────────────────
function OverlaysSection() {
  return (
    <SectionShell
      id="overlays"
      title="Modals & toasts"
      intro="One dialog idiom (title / body / footer, sizes, a confirm variant) with the Radix focus trap, and one toast idiom on the reserved ok-green for success."
    >
      <div className="flex flex-wrap items-center gap-3 rounded-card border border-border bg-card p-5 wv-elevation">
        <Modal
          title="Publish schedule"
          description="This goes live on every governed screen."
          trigger={<Button variant="default" icon={Upload}>Publish schedule</Button>}
          footer={<Button variant="default">Publish now</Button>}
        >
          <p className="text-sm text-muted-foreground">
            Review the changes before publishing. The sky turns on the screens the moment you
            confirm.
          </p>
        </Modal>

        <ConfirmModal
          title="Remove The Hangar TV?"
          description="This removes the screen from the Riverside Roastery fleet. You can re-add it later."
          confirmLabel="Remove screen"
          destructive
          onConfirm={() => toast.success("The Hangar TV removed")}
          trigger={<Button variant="destructive" icon={Trash2}>Remove screen</Button>}
        />

        <Button variant="secondary" icon={Bell} onClick={() => toast("The Hangar TV dropped off 2m ago. Retrying every 30s.")}>
          Notify
        </Button>
        <Button
          variant="outline"
          icon={Megaphone}
          onClick={() => toast.success("Sunrise daypart is on air")}
        >
          Success toast
        </Button>
      </div>
    </SectionShell>
  );
}

// ── Empty states ────────────────────────────────────────────────────────────
function EmptySection() {
  return (
    <SectionShell
      id="empty"
      title="Empty states"
      intro="Calm and plain per the brand voice — state the fact, name the next step."
    >
      <div className="rounded-card border border-border bg-card wv-elevation">
        <EmptyState
          icon={CalendarClock}
          title="Nothing scheduled for this daypart yet"
          description="Add content to light it up. The screen stays on the flat idle frame until then."
          action={
            <Button variant="default" icon={Plus}>
              Add content
            </Button>
          }
        />
      </div>
    </SectionShell>
  );
}

// ── The gallery shell ───────────────────────────────────────────────────────
function Sidebar({ active, onSelect }: { active: string; onSelect: (id: string) => void }) {
  const { theme } = useTheme();
  return (
    <aside className="sticky top-6 hidden h-fit w-56 shrink-0 lg:block">
      <nav aria-label="Design gallery sections" className="flex flex-col gap-1">
        <div className="mb-2 flex items-center gap-2 px-3">
          <KitIcon icon={AudioWaveform} label="Waiveo" className="size-5 text-[color:var(--wv-accent-text)]" />
          <span className="font-display text-[17px] font-semibold">waiveo</span>
        </div>
        {SECTIONS.map((s) => {
          const isActive = active === s.id;
          return (
            <a
              key={s.id}
              href={`#${s.id}`}
              aria-current={isActive ? "page" : undefined}
              onClick={() => onSelect(s.id)}
              className={cn(
                "flex items-center gap-2.5 rounded-input px-3 py-1.5 text-[13px] font-medium outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring",
                isActive
                  ? "bg-[color:var(--wv-nav-active-bg)] text-[color:var(--wv-nav-active-fg)]"
                  : "text-muted-foreground hover:bg-accent hover:text-foreground",
              )}
            >
              <KitIcon icon={s.icon} decorative className="size-4" />
              {s.label}
            </a>
          );
        })}
        <div className="mt-3 px-3 text-[12px] text-muted-foreground">
          Current sky: <span className="font-medium text-foreground">{theme === "dark" ? "Dusk" : "Daybreak"}</span>
        </div>
      </nav>
    </aside>
  );
}

export default function DesignRoute() {
  const [active, setActive] = useState<string>(SECTIONS[0].id);

  return (
    <div className="min-h-screen bg-background text-foreground">
      <div className="mx-auto flex max-w-[1200px] gap-8 px-4 py-6 lg:px-8">
        <Sidebar active={active} onSelect={setActive} />

        <main className="flex min-w-0 flex-1 flex-col gap-14 pb-24">
          <div id="overview" className="scroll-mt-8">
            <PageHeader
              variant="hero"
              title="Waiveo design kit"
              description="Every widget-kit primitive and every HORIZON token, in Dusk and Daybreak — the living reference the console and every extension page render through."
              actions={
                <Button variant="default" icon={Upload}>
                  Publish
                </Button>
              }
            />
          </div>

          <ColorSection />
          <GradientsSection />
          <TypeSection />
          <ShapeSection />
          <ActionsSection />
          <StatusSection />
          <StatsSection />
          <DataSection />
          <FormsSection />
          <OverlaysSection />
          <EmptySection />
        </main>
      </div>

      <Toaster />
    </div>
  );
}
