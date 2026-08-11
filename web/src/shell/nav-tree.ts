import {
  Activity,
  CalendarClock,
  DatabaseBackup,
  Film,
  HeartPulse,
  LayoutDashboard,
  LayoutGrid,
  LayoutTemplate,
  MonitorPlay,
  Palette,
  Presentation,
  Radio,
  Server,
  Tv,
  Upload,
  Workflow,
  type LucideIcon,
  Puzzle,
} from "lucide-react";

/**
 * NAV_TREE — the console's information architecture, as DATA.
 *
 * # Why this file exists
 *
 * The rail used to be thirteen flat siblings, and the owner named the cost of
 * that exactly: *"you've got CASTS over here, but CASTS should be under slide
 * casts... Screens should be under slide cast, just like CASTS."* A flat list
 * of thirteen tells an operator nothing about what belongs to what — `Casts`
 * sat as a peer of `Backup`, and the product's flagship surface (Slidecast: the
 * casts, the screens they play on, the media they draw from, the widgets they
 * carry) was scattered across the alphabet of unrelated platform pages.
 *
 * So the grouping is a TREE, and the tree is a value — not markup. The shell
 * renders whatever is here; adding a destination is adding a node, and nobody
 * has to touch a component to do it. That matters beyond tidiness: parallel
 * tracks add pages to this console (an Extensions/Marketplace surface, a Roku
 * operator page, a discovery page), and a data edit is a one-line merge where a
 * hand-written `<NavLink>` block is a conflict.
 *
 * # The shape of the model
 *
 * Two node kinds, and deliberately only two. A LEAF is a destination. A GROUP is
 * a product area that owns destinations — it is NOT itself a page, which is why
 * it carries no `to`: a group heading that also navigates gives an operator two
 * different meanings for one click, and makes "which page am I on" ambiguous
 * when a group and its first child would both be current. Groups do not nest
 * further; a third level is a menu, not a rail, and this console has no product
 * area deep enough to earn one.
 *
 * # Every route is accounted for, or the build fails
 *
 * A restructure that orphans a page is worse than the flat nav it replaced, so
 * `cmd/waiveo-feeder/consoleroutes_test.go` derives BOTH sides — every
 * `<Route path=…>` in web/src/App.tsx, and every `to:` here — and requires each
 * declared route to be reachable from one of them. Routes that are deliberately
 * NOT on the rail are declared in `OFF_RAIL_ROUTES` below WITH the door that
 * reaches them, so "not in the nav" is always a recorded decision and never an
 * oversight. Adding a route to App.tsx and forgetting the nav fails the build.
 */

/** A destination: one page, one rail entry. */
export interface NavLeaf {
  kind: "leaf";
  to: string;
  label: string;
  icon: LucideIcon;
  /** Match the path exactly (react-router `end`) — only "/" needs it, since
   * every other path would otherwise mark "/" active on every page. */
  end?: boolean;
}

/** A product area that owns destinations. Not a page: no `to`. */
export interface NavGroup {
  kind: "group";
  /** Stable key for the expand/collapse memory. Never a label — a label is copy
   * and copy changes; a persisted preference keyed on copy silently resets. */
  id: string;
  label: string;
  icon: LucideIcon;
  children: NavLeaf[];
}

export type NavNode = NavLeaf | NavGroup;

export const NAV_TREE: NavNode[] = [
  // The console's front door stays a top-level destination. It belongs to no
  // product area — it summarizes all of them.
  { kind: "leaf", to: "/", label: "Overview", icon: LayoutDashboard, end: true },

  // ── Slidecast: the flagship product area ────────────────────────────────
  // Everything an operator touches to answer "what is on the walls, and when".
  //
  // This IS the legacy grouping, restored. Legacy slidecast declared exactly one
  // section with four entries (slidecast/index.js `nav:`, order 70–73):
  //
  //     SLIDECAST → Slidecast · Screens · Media · Widgets
  //
  // and that first entry, confusingly labelled with the section's own name, was
  // the CASTS list (its page reads "Your Casts"). The stutter came from a real
  // legacy bug — the section title fell back to the first item's label because
  // no extension ever declared a title — so the group label here is explicit and
  // the entry is called what the owner calls it: "Casts".
  //
  // The relative order of the four legacy members is preserved. The two members
  // legacy did not have are slotted where they belong rather than appended:
  // Schedules follows Screens (it programs them; legacy had no scheduling at
  // all), and Upload follows Media (they are the write and read halves of one
  // content origin — legacy had only the one page).
  //
  //   Casts     the slide documents themselves (the Studio edits one)
  //   Screens   the displays a cast is assigned to — not "devices": a screen is
  //             a content destination, a device is a box on the LAN
  //   Schedules the week those casts play on, daypart by daypart
  //   Media     the read half of the content origin, addressed by digest
  //   Upload    the write half — bytes onto the box
  //   Widgets   what a slide can carry that draws itself from live data
  {
    kind: "group",
    id: "slidecast",
    label: "Slidecast",
    icon: Presentation,
    children: [
      { kind: "leaf", to: "/casts", label: "Casts", icon: LayoutTemplate },
      { kind: "leaf", to: "/screens", label: "Screens", icon: MonitorPlay },
      { kind: "leaf", to: "/schedules", label: "Schedules", icon: CalendarClock },
      { kind: "leaf", to: "/media", label: "Media", icon: Film },
      { kind: "leaf", to: "/upload", label: "Upload", icon: Upload },
      { kind: "leaf", to: "/widgets", label: "Widgets", icon: LayoutGrid },
    ],
  },

  // ── Devices: the hardware plane ─────────────────────────────────────────
  // What the relays found on the LAN and what has been adopted — a different
  // resource with a different owner (the relay) from the screens above.
  //
  // Legacy never had this group, and the absence was an accident rather than a
  // choice: device-discovery and roku-integration were two separate sections
  // whose `order` values (25/26 and 60/61) were plainly reaching for adjacency,
  // and the discovery section's heading was the nonsense string "NETWORK SCAN"
  // — the same missing-title fallback that produced the Slidecast stutter. Its
  // child label, "All Devices", is legacy's own and is kept.
  //
  // It is a GROUP rather than a leaf because that is the shape it takes the
  // moment the discovery and Roku operator surfaces land beside it (parity rows
  // 8.5/8.6, in flight on another track): they are siblings of this entry,
  // added as two more objects in this array and nothing else.
  {
    kind: "group",
    id: "devices",
    label: "Devices",
    icon: Radio,
    children: [
      { kind: "leaf", to: "/devices", label: "All devices", icon: Radio },
      // Roku is a SIBLING of All devices, not a top-level entry. The track that
      // built it argued for top-level, "beside Devices, not inside it: discovering
      // and OPERATING are different jobs" — true of the two pages, but it was
      // reasoning against the flat rail, where "inside" did not exist. Under the
      // area model the rule is the one this file already states: a top-level leaf
      // is for a page belonging to no area, and a per-driver control surface
      // plainly belongs to the device area. Filing it loose is the exact thing
      // Matt objected to ("Screens should be under slide cast, just like CAS").
      { kind: "leaf", to: "/roku", label: "Roku", icon: Tv },
    ],
  },

  // Automations sit at the top level on purpose. A rule's trigger is usually a
  // device and its action is usually a screen, so filing it under either one
  // hides it from the other half of its own job.
  { kind: "leaf", to: "/automations", label: "Automations", icon: Workflow },

  // Extensions is its own GROUP, not a leaf and not a Platform child.
  //
  // Not a Platform child: the owner calls the extension platform a core pillar —
  // "as much as we can, everything should be in an extension, edited in its
  // extension, and updated through its extension marketplace". Filing it beside
  // Backup and Design kit would rank it as housekeeping, and legacy gave the
  // marketplace its own section for the same reason.
  //
  // Not a top-level leaf: this rail reserves those for pages belonging to no
  // area (Overview, Automations). A group keeps that rule intact and gives the
  // two known-missing siblings — a catalogue browse and a registry-source list,
  // both recorded as ABSENT with no route today — a home to land in rather than
  // forcing another restructure when they arrive.
  {
    kind: "group",
    id: "extensions",
    label: "Extensions",
    icon: Puzzle,
    children: [{ kind: "leaf", to: "/extensions", label: "Installed", icon: Puzzle }],
  },

  // ── Platform: the box itself, not the product ───────────────────────────
  // These answer "is the box healthy", "what just happened", "is my work safe"
  // — questions about the machine, asked on a different day and in a different
  // mood from "what is on the walls". Grouping them apart is the other half of
  // the owner's complaint: they were interleaved with the product pages.
  //
  // Legacy kept these flat in a "CORE" section (Logs, Backups, Settings, Jobs,
  // Isolation). One thing to respect from that history: core nav DID try two
  // levels once and reverted it — commit 4712e76f added a System parent with a
  // Jobs child, and 65d4ebb7 flattened it straight back out. Nothing recorded
  // why, but the legacy sublist had no collapse affordance at all (it rendered
  // children unconditionally), so nesting cost a permanent indent and bought
  // nothing. That is the specific failure this tree must not repeat, and it is
  // why every group here collapses, remembers, and reveals the active route.
  //
  // Pages and Design kit are the platform's own reference surfaces (the
  // ui-schema/1 renderer and the widget kit gallery) — they belong with the
  // machine, not with the signage.
  {
    kind: "group",
    id: "platform",
    label: "Platform",
    icon: Server,
    children: [
      { kind: "leaf", to: "/activity", label: "Activity", icon: Activity },
      { kind: "leaf", to: "/system", label: "System", icon: HeartPulse },
      { kind: "leaf", to: "/backup", label: "Backup", icon: DatabaseBackup },
      { kind: "leaf", to: "/pages", label: "Pages", icon: LayoutTemplate },
      { kind: "leaf", to: "/design", label: "Design kit", icon: Palette },
    ],
  },
];

/**
 * Routes that are deliberately NOT on the primary rail — each with the door an
 * operator actually reaches it through.
 *
 * This is not a test fixture; it is the other half of the IA. Some pages are
 * genuinely not rail destinations (a sign-in page behind a sign-in requirement,
 * an editor opened on the thing it edits), and the reachability guard has to
 * tell those apart from a page somebody forgot. Declaring the door here is what
 * makes that difference machine-checkable: a new route is either a node in
 * NAV_TREE or an entry here, and there is no third option that compiles.
 */
export interface OffRailRoute {
  to: string;
  /** How an operator gets here. Prose, because the point is the justification. */
  reachedVia: string;
}

export const OFF_RAIL_ROUTES: OffRailRoute[] = [
  {
    to: "/login",
    reachedVia:
      "The SessionGate redirects here when no session exists. It sits OUTSIDE the shell — a rail entry to the sign-in page would only ever be visible to someone already signed in.",
  },
  {
    to: "/setup",
    reachedVia:
      "First-boot claim (SEC-120). Reached from the setup code the feeder prints on an unclaimed boot; outside the shell for the same reason /login is.",
  },
  {
    to: "/studio",
    reachedVia:
      "Opened on a cast from the Casts library (`/studio?id=…`). It is a tool applied to a cast, not a destination — a rail entry would open it on nothing.",
  },
  {
    to: "/security",
    reachedVia:
      "The header's account link. It acts on the signed-in principal rather than on a console resource, so it does not belong beside the resource families.",
  },
  {
    to: "/p/:publisher/:name/*",
    reachedVia:
      "An installed pack's own page. The shell resolves these live from the installed-pack list into the separate 'Extensions' nav landmark, so they are navigable without being declared here.",
  },
];

/** Every leaf in the tree, depth-first — the rail's destinations in rail order. */
export function navLeaves(tree: NavNode[] = NAV_TREE): NavLeaf[] {
  return tree.flatMap((node) => (node.kind === "leaf" ? [node] : node.children));
}

/** The id of the group that owns `pathname`, or undefined when the active route
 * is a top-level leaf, an off-rail route, or a pack page. Used by the shell to
 * REVEAL the active route's parent: an operator who collapsed Slidecast and then
 * lands on /casts must still be able to see where they are. */
export function groupOwning(pathname: string, tree: NavNode[] = NAV_TREE): string | undefined {
  return tree.find(
    (node): node is NavGroup =>
      node.kind === "group" && node.children.some((child) => child.to === pathname),
  )?.id;
}
