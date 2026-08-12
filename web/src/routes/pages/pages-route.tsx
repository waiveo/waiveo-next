import { useState, type ReactNode } from "react";
import {
  LayoutDashboard,
  LayoutList,
  SlidersHorizontal,
  Wand2,
  type LucideIcon,
} from "lucide-react";
import {
  KitIcon,
  PageHeader,
  StatCard,
  Tab,
  TabList,
  TabPanel,
  Tabs,
  Toaster,
  toast,
} from "@/components/kit";
import { PageRenderer, type ActionHandler } from "@/renderer";

/**
 * The /pages route — the ui-schema/1 renderer, live. It paints the four valid
 * corpus documents (one per page type) through the REAL renderer over the
 * Horizon kit, inside the responsive app shell, with a nav entry per page type
 * to switch between them. This is the uniformity promise made visible: a
 * pack-declared page document, validated then rendered, looks and behaves the
 * same for core and for every extension because the renderer + kit are the one
 * shared surface. The documents are the frozen conformance corpus itself (loaded
 * directly, no copies), so what renders here is exactly what a conformant pack
 * ships.
 */

// The frozen corpus documents, read straight from the repo tree (single source
// of truth shared with the Go corpus drivers) — bundled by Vite, so the demo
// makes zero external requests.
interface CorpusCase {
  case_id: string;
  input: Record<string, unknown>;
}
const modules = import.meta.glob<CorpusCase>(
  "../../../../conformance/corpora/ui-schema-1/*.json",
  { eager: true, import: "default" },
);
const byId: Record<string, CorpusCase> = Object.fromEntries(
  Object.values(modules).map((c) => [c.case_id, c]),
);
const docFor = (caseId: string): Record<string, unknown> => byId[caseId].input;

interface PageDemo {
  id: string;
  label: string;
  caseId: string;
  icon: LucideIcon;
  blurb: string;
}

const PAGE_DEMOS: PageDemo[] = [
  {
    id: "list-detail",
    label: "List & detail",
    caseId: "UIS-020-valid-list-detail-presets",
    icon: LayoutList,
    blurb: "A list on the left, a detail editor on the right — the master/detail idiom.",
  },
  {
    id: "settings-form",
    label: "Settings form",
    caseId: "UIS-030-valid-settings-form",
    icon: SlidersHorizontal,
    blurb: "Grouped sections of bound fields with a submit-wired save action.",
  },
  {
    id: "dashboard",
    label: "Dashboard",
    caseId: "UIS-040-valid-dashboard",
    icon: LayoutDashboard,
    blurb: "Independently-bound tiles — stat figures and a cross-pack slot.",
  },
  {
    id: "wizard",
    label: "Wizard",
    caseId: "UIS-050-valid-wizard",
    icon: Wand2,
    blurb: "A stepped flow over one shared draft, gated by canAdvanceIf.",
  },
];

// Fixture data each page's root Bindings resolve against (UIS-005). Fixture ULIDs
// only — nothing real.
const presets = [
  { id: "01J8Z3K4N5P6Q7R8S9T0V1W2A1", name: "Morning open" },
  { id: "01J8Z3K4N5P6Q7R8S9T0V1W2A2", name: "Evening wind-down" },
  { id: "01J8Z3K4N5P6Q7R8S9T0V1W2A3", name: "Midday bright" },
];

const DEMO_DATA: Record<string, Record<string, unknown>> = {
  "list-detail": { presets },
  "settings-form": { site: { displayName: "The Hangar", quietHours: true } },
  dashboard: {
    screens: [{}, {}, {}, {}, {}],
    automations: [{}, {}, {}],
  },
  wizard: {},
};

// The demo slot content the dashboard's `slot` tile paints (UIS-185) — a small
// kit StatCard standing in for a cross-pack fragment card.
const DEMO_SLOTS: Record<string, ReactNode> = {
  "dashboard-cards": (
    <StatCard label="Contributed card" value="Live" hint="a slotted pack fragment" />
  ),
};

// The message catalog for the four documents' `msg:` references.
const messages: Record<string, string> = {
  "msg:presets.list.name": "Name",
  "msg:presets.detail.empty": "Select a preset to edit it.",
  "msg:presets.detail.nameLabel": "Preset name",
  "msg:settings.general.title": "General",
  "msg:settings.general.displayName": "Display name",
  "msg:settings.general.quietHours": "Quiet hours",
  "msg:settings.save": "Save",
  "msg:dashboard.onlineScreens": "Online screens",
  "msg:dashboard.activeAutomations": "Active automations",
  "msg:wizard.step.name": "Name",
  "msg:wizard.step.nameLabel": "Automation name",
  "msg:wizard.step.review": "Review",
};

// The api-client seam: this demo has no backend, so the closed action verbs
// surface as toasts instead of hitting a management-API route (UIS-161).
const handler: ActionHandler = {
  submit: (target) => {
    toast.success(`Saved ${target ?? "changes"}`);
  },
  create: (target) => {
    toast(`Created a record in ${target}`);
  },
  remove: (target) => {
    toast(`Removed ${target}`);
  },
  callAction: (action) => {
    toast.success(`Ran action “${action}”`);
  },
  navigate: (to) => {
    toast(`Navigate → ${to}`);
  },
};

export default function PagesRoute() {
  const [active, setActive] = useState<string>(PAGE_DEMOS[0].id);

  return (
    <div className="min-h-screen bg-background text-foreground">
      <div className="mx-auto flex max-w-[1200px] flex-col gap-6 px-4 py-6 lg:px-8">
        <PageHeader
          variant="hero"
          title="Declarative pages"
          description="The four ui-schema/1 page types, painted from the frozen conformance corpus through the real renderer over the Horizon kit — the same path every core and extension page takes."
        />

        {/* Four panels of one subject, sharing one URL — a tab set, and now
            actually built as one. The row of buttons this replaces claimed
            `aria-current="page"` on the selected demo, which told a screen
            reader the URL had changed when it had not, and made all four
            separate tab stops. The kit's Tabs announce "tab 2 of 4, selected",
            tie each tab to the panel it reveals, and put the whole strip on one
            tab stop with the arrow keys moving inside it. */}
        <Tabs value={active} onValueChange={setActive} className="gap-6">
          <TabList aria-label="Page-type demos" className="h-auto flex-wrap">
            {PAGE_DEMOS.map((demo) => (
              <Tab key={demo.id} value={demo.id} className="wv-touch">
                <KitIcon icon={demo.icon} decorative className="size-4 shrink-0" />
                {demo.label}
              </Tab>
            ))}
          </TabList>

          {PAGE_DEMOS.map((demo) => (
            <TabPanel key={demo.id} value={demo.id} className="flex flex-col gap-6">
              <div className="flex flex-col gap-1">
                <h2 className="font-display text-[19px] font-semibold tracking-[-0.01em]">
                  {demo.label}
                </h2>
                <p className="max-w-2xl text-sm text-muted-foreground">{demo.blurb}</p>
              </div>

              <main className="min-w-0">
                {/* A TabPanel unmounts its content when it is not selected, so the
                    renderer is freshly mounted per switch and each page's
                    ephemeral $ui state (UIS-104) starts empty — the same reset
                    navigating away from a page gives it, and the reason the
                    previous strip had to force it with a `key`. */}
                <PageRenderer
                  doc={docFor(demo.caseId)}
                  data={DEMO_DATA[demo.id]}
                  messages={messages}
                  handler={handler}
                  {...(demo.id === "dashboard" ? { slots: DEMO_SLOTS } : {})}
                />
              </main>
            </TabPanel>
          ))}
        </Tabs>
      </div>

      <Toaster />
    </div>
  );
}
