import {
  Activity,
  HeartPulse,
  LayoutDashboard,
  Radio,
  Server,
  Settings,
  KeyRound,
  type LucideIcon,
  Puzzle,
} from "lucide-react";

/**
 * NAV_TREE — the console's information architecture, as DATA.
 *
 * # Why this file exists
 *
 * The rail used to be thirteen flat siblings, and the owner named the cost of
 * that exactly: `Casts` sat as a peer of `Backup`, and the product's flagship
 * surface was scattered across the alphabet of unrelated platform pages. So the
 * grouping is a TREE, and the tree is a value — not markup. The shell renders
 * whatever is here; adding a destination is adding a node, and nobody has to
 * touch a component to do it. That matters beyond tidiness: parallel tracks add
 * pages to this console, and a data edit is a one-line merge where a
 * hand-written `<NavLink>` block is a conflict.
 *
 * # What this rail carries, and why it is short (2026-08-19)
 *
 * Almost every product area that used to be here has LEFT core. The owner's rule
 * is that the product ships as packs: of the nine ship targets — automation,
 * marketplace, backups, system, comms, slidecast, roku-integration,
 * device-discovery, device-widgets — only device-discovery is installed on the
 * box, and core stopped carrying the other eight's console surfaces rather than
 * carrying them until each pack was ready. The Slidecast group (Casts, Screens,
 * Schedules, Media, Upload, Widgets), the Automations group (Rules, Variables)
 * and Platform's Backup entry were deleted with their routes.
 *
 * TWO authorities did that, not one, and the difference matters if anyone wants
 * to reverse a piece of it:
 *
 *   - plans/2026-08-11-capability-ownership-map.md attributes Rules, Variables,
 *     Widgets, Backup and the Roku console to packs (§3.1 Bucket A; §2.2 group
 *     E puts the variables UI in `waiveo/system`). For those, the map IS the
 *     authority and the strip is executing a decision already taken.
 *   - Casts, Screens, Schedules, Media and Upload are the map's §3.3 **Bucket C**,
 *     which it deliberately does NOT decide: "This needs an owner decision; I am
 *     not guessing it" (§3.3), "I have not picked a side" (§6.2). Its §2.2 group
 *     A row goes further and says core KEEPS `/casts`, `/playlists`,
 *     `/schedules`, `/screens`. So the map does not sanction these five — it
 *     argues the other way. **The OWNER decided them, on 2026-08-19**, by naming
 *     the Slidecast rail group directly. Do not read the citation above as
 *     covering them.
 *
 * The map's sixth Bucket-C row, `web/src/api/casts.ts` (651 lines), was KEPT on
 * purpose. The owner's instruction was about this rail — the console surface —
 * and the API that module types is still core and live (`/casts`, `/screens`,
 * `/schedules`, `/playlists` in api/openapi.yaml, `internal/app/api/casts.go`
 * and siblings), which is exactly what §2.2 group A says core keeps. It is
 * therefore a live typed client with no console consumer at present, still
 * exported from `api/index.ts` and wired into `api/resources.ts`.
 *
 * What is left divides in three, and the division is the whole IA now:
 *
 *   Overview     the front door, belonging to no area because it summarizes the
 *                box rather than any one resource family
 *   Discovery    device-discovery's surface — the ONE ship target installed
 *   Extensions   the pack lifecycle, which is how Discovery got here
 *   Platform     the box itself: what it is configured as, what it has done,
 *                whether it is healthy
 *
 * # NO PACK GETS ITS OWN RAIL AREA (owner, 2026-08-19)
 *
 * A pack used to contribute rail entries live: the shell resolved an installed
 * pack's declared pages into a second landmark below a divider, one titled
 * section per pack. The owner killed that section outright, for every pack, now
 * and in future. This array is therefore the WHOLE rail — what it declares is
 * what the console shows, and there is nothing beneath it.
 *
 * The trigger was waiveo/discovery putting a second "Discovery" in the rail (a
 * pack section carrying its policy page, directly under the core Discovery
 * group), but the ruling is not about that collision: a pack does not get a nav
 * area whether or not its name is already taken.
 *
 * This removed a RAIL SECTION, not a capability. `/p/{publisher}/{name}/{path}`
 * is still routed and still served; the door is Extensions > Installed, which
 * lists every installed pack with a link to each page it declares. That is the
 * mechanism the eight departed areas come back through — a pack's pages are
 * reached through the pack, not by growing this array or a section beside it.
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
 * A one-child group is not a mistake here. Discovery and Extensions each hold a
 * single leaf today and each is a genuine area with named absent siblings (a
 * Scan page beside All devices; a catalogue browse and a registry-source list
 * beside Installed) — a group gives those somewhere to land instead of forcing
 * another restructure when they arrive. The rail also reserves TOP-LEVEL slots
 * for pages belonging to no area, of which Overview is the only one; promoting
 * either of these to a loose leaf would make "top level" mean "no obvious home",
 * which is the flat rail this tree replaced.
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
  // product area — it summarizes the box.
  { kind: "leaf", to: "/", label: "Overview", icon: LayoutDashboard, end: true },

  // ── Discovery: what is on the network ───────────────────────────────────
  // The device area is named "Discovery" (owner, 2026-08-17): the console's job
  // here is finding what is on the LAN, and the relays report the whole segment,
  // not only the devices this deployment has adopted — "Devices" undersold that
  // to exactly one operator, and it read as the adopted set rather than the
  // network.
  //
  // This is the device-discovery ship target's surface and the one the owner is
  // keeping working while the other eight are stripped. There is no Roku child
  // and no per-device remote here any more: the owner drew that line first
  // ("there should be no such thing as a Roku console when it comes to devices —
  // Roku is its own extension, and Discovery is its own extension"), and the
  // strip finished it by deleting /roku and the remote outright. Adopted Rokus
  // are unclassified until waiveo/roku exists, which the owner accepted.
  //
  // The second child the owner asked for — a "Scan" page beside "All devices" —
  // waits on its own backing (a scan trigger, subnet policy, per-lane engine
  // state), none of which the relay reports yet; it is a deliberate absence, not
  // an oversight.
  {
    kind: "group",
    id: "devices",
    label: "Discovery",
    icon: Radio,
    children: [{ kind: "leaf", to: "/devices", label: "All devices", icon: Radio }],
  },

  // ── Extensions: the pack lifecycle ──────────────────────────────────────
  // Its own GROUP, not a leaf and not a Platform child. The owner calls the
  // extension platform a core pillar — "as much as we can, everything should be
  // in an extension, edited in its extension, and updated through its extension
  // marketplace" — and filing it beside Activity and System would rank it as
  // housekeeping. Legacy gave the marketplace its own section for the same
  // reason.
  //
  // Marketplace is itself one of the nine, so by the strip's own rule this entry
  // should have gone with Slidecast and Automations. It stayed on the ownership
  // map's own grounds (§1.3): an optional pack that is the only way to install
  // packs is a bricking hazard, and this page is how waiveo/discovery — the one
  // pack that IS installed — is installed and updated. Deleting it would leave
  // the CLI and api/1 as the only path, and would point the shell's "extensions
  // need attention" badge at nothing.
  {
    kind: "group",
    id: "extensions",
    label: "Extensions",
    icon: Puzzle,
    children: [{ kind: "leaf", to: "/extensions", label: "Installed", icon: Puzzle }],
  },

  // ── Platform: the box itself, not the product ───────────────────────────
  // These answer "what is this box configured as", "what just happened" and "is
  // it healthy" — questions about the machine. They were interleaved with the
  // product pages once, which was the other half of the owner's complaint; with
  // the product pages gone they are most of what is left, and the grouping still
  // earns itself by keeping the front door and the network view out of the
  // machine's business.
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
  // Backup is NOT here any more: the ownership map attributes it to
  // waiveo/backups, and waiveo/core already ships the schedule/retention half as
  // a settings-form. Export, download and restore left with the route and have
  // no console path until that pack exists.
  //
  // Pages and Design kit are not here either. They are developer reference
  // rather than operator surfaces — the ui-schema/1 conformance documents drawn
  // through the real renderer, and the Horizon kit gallery — so they moved to
  // OFF_RAIL_ROUTES with the door they are reached through, rather than being
  // deleted: the renderer's shop window is worth keeping addressable while packs
  // are being authored against it.
  {
    kind: "group",
    id: "platform",
    label: "Platform",
    icon: Server,
    children: [
      // Settings leads the group, and it is a CHILD of it rather than a
      // top-level leaf.
      //
      // Not top-level: this rail reserves those for pages belonging to no area
      // (Overview), and Settings plainly belongs with the machine — it edits
      // where the box is and what local time it keeps, which is the same
      // subject Activity and System report on.
      //
      // First within the group because it is the only member that CONFIGURES.
      // Activity and System report on a box that this page describes. Legacy
      // buried Settings third in a flat CORE list between Backups and Jobs,
      // which is exactly the interleaving the owner objected to.
      { kind: "leaf", to: "/settings", label: "Settings", icon: Settings },
      // Beside Settings rather than in its own area: an api-key is a property
      // of the box's identity surface, and the rail reserves top-level slots
      // for pages belonging to no area.
      { kind: "leaf", to: "/api-keys", label: "API keys", icon: KeyRound },
      { kind: "leaf", to: "/activity", label: "Activity", icon: Activity },
      { kind: "leaf", to: "/system", label: "System", icon: HeartPulse },
    ],
  },
];

/**
 * Routes that are deliberately NOT on the primary rail — each with the door an
 * operator actually reaches it through.
 *
 * This is not a test fixture; it is the other half of the IA. Some pages are
 * genuinely not rail destinations (a sign-in page behind a sign-in requirement,
 * a reference surface reached by address), and the reachability guard has to
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
    to: "/*",
    reachedVia:
      "The catch-all. Reached by an address that matches no route above — a stale link or a typo — and by definition never by navigation, so it is the one entry here whose absence from the rail is not a decision but a tautology. It renders INSIDE the shell so a dead URL still leaves the operator every route they could go to instead.",
  },
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
    to: "/security",
    reachedVia:
      "The header's account link. It acts on the signed-in principal rather than on a console resource, so it does not belong beside the resource families.",
  },
  {
    to: "/pages",
    reachedVia:
      "By address, as developer reference: the four frozen ui-schema/1 conformance documents drawn through the REAL renderer. It is the reference a pack author checks a page document against, not something an operator has a reason to open, so it left the Platform group when the rail was cut back to what the box actually runs. Kept addressable rather than deleted because it is the renderer's only shop window and packs are being authored against that contract now.",
  },
  {
    to: "/design",
    reachedVia:
      "By address, as developer reference: the Horizon kit gallery — every primitive under both themes, on one page. Off the rail for the same reason /pages is, and kept for the same reason: it is how a widget or page author sees what the kit already provides before reinventing it.",
  },
  {
    to: "/p/:publisher/:name/*",
    reachedVia:
      "An installed pack's own page, reached from Extensions > Installed (/extensions): every installed pack's card links each page the pack declares, at exactly this route. That link IS the door and is pinned as one — routes/extensions/extensions-route.test.tsx clicks it and asserts it lands on this route, so deleting the link fails the build rather than quietly orphaning every pack page. Off the rail because the owner killed the pack-contributed rail section on 2026-08-19: no pack gets a nav area of its own, now or in future. This is also the door the eight stripped product areas come back through as they ship as packs.",
  },
];

/** Every leaf in the tree, depth-first — the rail's destinations in rail order. */
export function navLeaves(tree: NavNode[] = NAV_TREE): NavLeaf[] {
  return tree.flatMap((node) => (node.kind === "leaf" ? [node] : node.children));
}

/** The id of the group that owns `pathname`, or undefined when the active route
 * is a top-level leaf, an off-rail route, or a pack page. Used by the shell to
 * REVEAL the active route's parent: an operator who collapsed Platform and then
 * lands on /system must still be able to see where they are. */
export function groupOwning(pathname: string, tree: NavNode[] = NAV_TREE): string | undefined {
  return tree.find(
    (node): node is NavGroup =>
      node.kind === "group" && node.children.some((child) => child.to === pathname),
  )?.id;
}
