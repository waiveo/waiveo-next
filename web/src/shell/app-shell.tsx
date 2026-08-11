import { useCallback, useEffect, useState, type ReactNode } from "react";
import { Link, NavLink, Outlet, useLocation } from "react-router";
import {
  AudioWaveform,
  ChevronDown,
  ChevronRight,
  FileText,
  Menu,
  PanelLeftClose,
  PanelLeftOpen,
} from "lucide-react";
import { Button, KitIcon, NavDrawer } from "@/components/kit";
import { ThemeToggle } from "@/components/theme/theme-toggle";
import { SignOutButton } from "@/auth/sign-out-button";
import { SecurityLink } from "@/auth/security-link";
import { useMediaQuery } from "@/lib/use-media-query";
import { cn } from "@/lib/utils";
import type { WaiveoApi } from "@/api";
import { useInstalledPackNav, type PackNavGroup } from "@/routes/packs/use-installed-packs";
import { resolvePackIcon } from "./pack-icon";
import { NAV_TREE, groupOwning, type NavGroup, type NavLeaf } from "./nav-tree";

/**
 * AppShell — the console's responsive frame. The primary navigation is LOCKED
 * LEFT (Matt, 2026-07-22): a permanent rail from 1024px up, and a left slide-in
 * drawer with a hamburger below it — never a top-nav. The shell is three regions:
 * the sidebar rail, a header, and the content region (a route's page, via
 * `children` when used directly or `<Outlet/>` when used as a layout route).
 *
 * Visibility is CSS-driven through the authored breakpoint classes in
 * responsive.css (`wv-shell__sidebar`, `wv-shell__hamburger`, `wv-shell__drawer`)
 * so the rail lock is robust and flash-free; the drawer's overlay/focus-trap/
 * Escape/`aria-expanded` come from the kit `NavDrawer` (Radix Dialog).
 *
 * # What the rail CONTAINS lives elsewhere
 *
 * The information architecture — which destinations exist and which product
 * area owns each one — is DATA, in `./nav-tree.ts`. This file knows how to
 * render a tree; it does not know that Casts belong under Slidecast. That split
 * is deliberate: adding a page is a node in an array, which is a one-line merge
 * between parallel tracks rather than a conflict inside a component.
 *
 * There are two nav landmarks, never merged: "Primary" (this tree, the core
 * console) and "Extensions" (installed packs' own pages, resolved live), so
 * third-party destinations stay visibly demarcated from first-party ones.
 */

/** Which groups the operator has left OPEN, keyed by the group's stable id.
 * Absent means "as shipped", which is open — a rail that hides five of its six
 * product areas on first run reintroduces the problem the tree exists to fix. */
type OpenGroups = Record<string, boolean>;

const NAV_STORAGE_KEY = "waiveo.nav.groups";

function readOpenGroups(): OpenGroups {
  if (typeof window === "undefined") return {};
  try {
    const raw = window.localStorage.getItem(NAV_STORAGE_KEY);
    if (!raw) return {};
    const parsed: unknown = JSON.parse(raw);
    if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) return {};
    // Keep only the booleans. Anything else in there is corruption or a stale
    // shape, and a nav that throws on it would take the whole console with it.
    return Object.fromEntries(
      Object.entries(parsed as Record<string, unknown>).filter(([, v]) => typeof v === "boolean"),
    ) as OpenGroups;
  } catch {
    return {};
  }
}

function persistOpenGroups(state: OpenGroups): void {
  try {
    window.localStorage.setItem(NAV_STORAGE_KEY, JSON.stringify(state));
  } catch {
    // A blocked/full localStorage must never break navigation.
  }
}

/** Groups ship OPEN; the stored map only ever records a deviation. */
function isGroupOpen(state: OpenGroups, id: string): boolean {
  return state[id] !== false;
}

/**
 * The expand/collapse memory, plus the REVEAL rule.
 *
 * Two things a collapsible rail has to get right, and legacy got neither (its
 * one nesting attempt was reverted within days): the state has to survive a
 * reload, and the group holding the page you are ON has to be open — otherwise
 * an operator who collapsed Slidecast last week clicks a link to /casts and the
 * rail shows no trace of where they landed. The reveal is a real state change,
 * not a render-time override, so the group stays open and its toggle keeps
 * working normally afterwards.
 */
function useNavGroups() {
  const [open, setOpen] = useState<OpenGroups>(readOpenGroups);
  const { pathname } = useLocation();

  useEffect(() => {
    const owner = groupOwning(pathname);
    if (owner === undefined) return;
    setOpen((prev) => {
      if (isGroupOpen(prev, owner)) return prev;
      const next = { ...prev, [owner]: true };
      persistOpenGroups(next);
      return next;
    });
  }, [pathname]);

  const toggleGroup = useCallback((id: string) => {
    setOpen((prev) => {
      const next = { ...prev, [id]: !isGroupOpen(prev, id) };
      persistOpenGroups(next);
      return next;
    });
  }, []);

  return { open, toggleGroup };
}

function Brand({ collapsed, className }: { collapsed?: boolean; className?: string }) {
  return (
    <Link
      to="/"
      className={cn(
        "flex items-center gap-2 rounded-input px-2 py-1 outline-none focus-visible:ring-2 focus-visible:ring-ring",
        className,
      )}
    >
      <KitIcon
        icon={AudioWaveform}
        decorative
        className="size-5 shrink-0 text-[color:var(--wv-accent-text)]"
      />
      <span className={cn("font-display text-[17px] font-semibold", collapsed && "sr-only")}>
        waiveo
      </span>
    </Link>
  );
}

/** The shared active/hover styling every nav link (core or extension) wears. */
function navLinkClass(collapsed?: boolean) {
  return ({ isActive }: { isActive: boolean }) =>
    cn(
      "wv-touch flex items-center gap-2.5 rounded-input px-3 text-[13px] font-medium outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring",
      isActive
        ? "bg-[color:var(--wv-nav-active-bg)] text-[color:var(--wv-nav-active-fg)]"
        : "text-muted-foreground hover:bg-accent hover:text-foreground",
      collapsed && "justify-center",
    );
}

/** One destination. Shared by the top level and by every group's children, so a
 * leaf looks and behaves identically wherever it sits in the tree. */
function NavLeafLink({
  item,
  collapsed,
  onNavigate,
}: {
  item: NavLeaf;
  collapsed?: boolean | undefined;
  onNavigate?: (() => void) | undefined;
}) {
  return (
    <NavLink
      to={item.to}
      end={item.end ?? false}
      onClick={onNavigate}
      className={navLinkClass(collapsed)}
    >
      <KitIcon icon={item.icon} decorative className="size-4 shrink-0" />
      <span className={cn(collapsed && "sr-only")}>{item.label}</span>
    </NavLink>
  );
}

/**
 * A product area and its destinations — a DISCLOSURE, built out of the two
 * things the pattern actually requires and nothing else: a real `<button>` that
 * carries `aria-expanded` and points at the region it controls, and a `<ul>`
 * that is genuinely hidden (the `hidden` attribute, not a CSS class) when
 * closed. That combination is what makes the tree keyboard-navigable with no
 * key handling of our own — Tab reaches the toggle, Space/Enter works it because
 * it is a button, and a screen reader announces "collapsed"/"expanded" and skips
 * the closed region instead of reading destinations the operator cannot see.
 *
 * The heading does NOT navigate. A group is a product area, not a page: giving
 * it a `to` as well as a toggle would mean one click had two meanings, and it
 * would compete with its own first child for "which page am I on".
 */
function NavGroupSection({
  group,
  open,
  onToggle,
  collapsed,
  onNavigate,
}: {
  group: NavGroup;
  open: boolean;
  onToggle: (id: string) => void;
  collapsed?: boolean | undefined;
  onNavigate?: (() => void) | undefined;
}) {
  const panelId = `nav-group-${group.id}`;
  return (
    <>
      <button
        type="button"
        data-slot="nav-group-toggle"
        data-nav-group={group.id}
        aria-expanded={open}
        aria-controls={panelId}
        onClick={() => onToggle(group.id)}
        className={cn(
          "wv-touch flex w-full items-center gap-2.5 rounded-input px-3 text-[11px] font-semibold uppercase tracking-wide text-muted-foreground outline-none transition-colors hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring",
          collapsed && "justify-center",
        )}
      >
        <KitIcon icon={group.icon} decorative className="size-4 shrink-0" />
        <span className={cn(collapsed && "sr-only")}>{group.label}</span>
        {collapsed ? null : (
          <KitIcon
            icon={open ? ChevronDown : ChevronRight}
            decorative
            className="ml-auto size-3.5 shrink-0"
          />
        )}
      </button>
      {/* The `hidden` attribute is load-bearing: it is what removes the closed
          region from the accessibility tree. A Tailwind display class would look
          identical and leave every closed destination focusable and announced. */}
      <ul
        id={panelId}
        hidden={!open}
        data-slot="nav-group-panel"
        className={cn(
          "flex flex-col gap-1",
          // Indent under the heading, with a hairline that makes the containment
          // visible rather than merely implied. Collapsed to icons there is no
          // room for either, and the group icon above is the only affordance.
          collapsed ? "mt-1" : "mt-1 ml-4 border-l border-border pl-3",
        )}
      >
        {group.children.map((child) => (
          <li key={child.to}>
            <NavLeafLink item={child} collapsed={collapsed} onNavigate={onNavigate} />
          </li>
        ))}
      </ul>
    </>
  );
}

function ShellNav({
  collapsed,
  onNavigate,
  packNav,
  openGroups,
  onToggleGroup,
}: {
  collapsed?: boolean;
  onNavigate?: () => void;
  packNav?: PackNavGroup[];
  openGroups: OpenGroups;
  onToggleGroup: (id: string) => void;
}) {
  return (
    <div className="flex flex-col gap-4">
      {/* A real list inside the landmark: a screen reader announces "6 items"
          and the group/child relationship is structural, not a matter of
          indentation an assistive technology cannot see. */}
      <nav aria-label="Primary">
        <ul className="flex flex-col gap-1">
          {NAV_TREE.map((node) => (
            <li key={node.kind === "leaf" ? node.to : node.id}>
              {node.kind === "leaf" ? (
                <NavLeafLink item={node} collapsed={collapsed} onNavigate={onNavigate} />
              ) : (
                <NavGroupSection
                  group={node}
                  open={isGroupOpen(openGroups, node.id)}
                  onToggle={onToggleGroup}
                  collapsed={collapsed}
                  onNavigate={onNavigate}
                />
              )}
            </li>
          ))}
        </ul>
      </nav>
      {packNav && packNav.length > 0 ? (
        <ExtensionsNav groups={packNav} collapsed={collapsed} onNavigate={onNavigate} />
      ) : null}
    </div>
  );
}

/** The Extensions section — installed packs' pages, grouped by pack. A second,
 * distinctly-labelled nav landmark (never merged into Primary) so third-party
 * content is clearly demarcated from the core console destinations. */
function ExtensionsNav({
  groups,
  collapsed,
  onNavigate,
}: {
  groups: PackNavGroup[];
  collapsed?: boolean | undefined;
  onNavigate?: (() => void) | undefined;
}) {
  return (
    // The landmark keeps its accessible name ("Extensions") and its own <nav>, so
    // assistive tech still hears third-party content announced as a separate
    // region — the demarcation above is intact where it does the work.
    //
    // What it no longer renders is a VISIBLE heading reading "Extensions", which
    // collided head-on with the core Extensions group in Primary: the rail showed
    // two sections under that one word, one meaning "manage packs" and one meaning
    // "pages packs contributed". Legacy never had this meta-heading — each
    // extension declared its own titled section and they stacked as peers of the
    // core ones — and every pack group here already carries the pack's own name
    // and icon, so the word was labelling a set its members label better.
    //
    // The rule is separated by a border instead. That keeps the boundary visible
    // (a pack may title a page "Backup"; an operator must be able to see that it
    // is a pack's page) without spending a section name on it.
    <nav
      aria-label="Extensions"
      className="flex flex-col gap-2 border-t border-border pt-3 mt-1"
    >
      {groups.map((group) => (
        <div key={group.packId} className="flex flex-col gap-1">
          {/* The group heading wears the pack's icon: its manifest-declared glyph
              when it names a host-allowed one, else the default extension glyph.
              resolvePackIcon ALWAYS returns a real component — the nav never shows
              a broken/missing icon, even for an unknown or non-string name. */}
          <div
            data-slot="pack-group-heading"
            className={cn("flex items-center gap-2 px-3", collapsed && "justify-center")}
          >
            <KitIcon
              icon={resolvePackIcon(group.icon)}
              decorative
              className="size-4 shrink-0 text-muted-foreground"
            />
            <span
              className={cn(
                "text-[11px] font-medium text-muted-foreground",
                collapsed && "sr-only",
              )}
            >
              {group.title}
            </span>
          </div>
          {group.pages.map((pg) => (
            <NavLink key={pg.to} to={pg.to} onClick={onNavigate} className={navLinkClass(collapsed)}>
              <KitIcon icon={FileText} decorative className="size-4 shrink-0" />
              <span className={cn(collapsed && "sr-only")}>{pg.label}</span>
            </NavLink>
          ))}
        </div>
      ))}
    </nav>
  );
}

export function AppShell({ children, api }: { children?: ReactNode; api?: WaiveoApi }) {
  const [collapsed, setCollapsed] = useState(false);
  const [drawerOpen, setDrawerOpen] = useState(false);
  // The Extensions nav: installed packs' pages, resolved over the api/1 client.
  // Degrades to nothing (no section) when no packs are installed or unreachable.
  const packNav = useInstalledPackNav(api);
  // ONE expand/collapse state for the rail and the drawer. They are never both
  // visible (the breakpoint decides), but they are both mounted — two
  // independent copies would drift, so an operator who collapsed a group on a
  // phone would find it open again on the desktop rail of the same session.
  const { open: openGroups, toggleGroup } = useNavGroups();

  // The desktop/mobile split is CSS-only: at >=1024px responsive.css hides the
  // drawer (`wv-shell__drawer`) and shows the permanent rail. But Radix Dialog's
  // modal side-effects (aria-hiding the rest of the app, scroll lock, focus trap)
  // key off `open`, not CSS visibility — so a drawer left open while the viewport
  // grows past the breakpoint would keep the now-visible rail + content
  // aria-hidden and body scroll locked against a display:none dialog. Reset the
  // open state on the crossing so the drawer's modality can never outlive it.
  const isDesktop = useMediaQuery("(min-width: 1024px)");
  useEffect(() => {
    if (isDesktop) setDrawerOpen(false);
  }, [isDesktop]);

  return (
    <div
      data-slot="app-shell"
      className="flex min-h-screen bg-background text-foreground"
    >
      {/* Locked-left rail — permanent from 1024px up (wv-shell__sidebar). */}
      <aside
        data-slot="shell-sidebar"
        data-collapsed={collapsed}
        className={cn(
          "wv-shell__sidebar sticky top-0 h-screen shrink-0 flex-col gap-3 overflow-y-auto border-r border-border bg-card p-4",
          collapsed ? "w-[4.5rem] items-center" : "w-64",
        )}
      >
        <Brand collapsed={collapsed} />
        <ShellNav
          collapsed={collapsed}
          packNav={packNav}
          openGroups={openGroups}
          onToggleGroup={toggleGroup}
        />
        <div className="mt-auto flex flex-col gap-1">
          <button
            type="button"
            aria-pressed={collapsed}
            aria-label={collapsed ? "Expand sidebar" : "Collapse sidebar"}
            onClick={() => setCollapsed((v) => !v)}
            className={cn(
              "wv-touch flex items-center gap-2.5 rounded-input px-3 text-[13px] font-medium text-muted-foreground outline-none transition-colors hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring",
              collapsed && "justify-center",
            )}
          >
            <KitIcon
              icon={collapsed ? PanelLeftOpen : PanelLeftClose}
              decorative
              className="size-4 shrink-0"
            />
            <span className={cn(collapsed && "sr-only")}>Collapse</span>
          </button>
        </div>
      </aside>

      {/* Content column: header + routed content. min-w-0 lets it shrink so a wide
          child (a table) never forces the page to scroll horizontally at 360px. */}
      <div className="flex min-w-0 flex-1 flex-col">
        <header
          data-slot="shell-header"
          className="sticky top-0 z-30 flex h-14 items-center gap-2 border-b border-border bg-background/95 px-4 backdrop-blur"
        >
          <NavDrawer
            open={drawerOpen}
            onOpenChange={setDrawerOpen}
            title="Navigation"
            trigger={
              <Button
                variant="ghost"
                size="icon"
                icon={Menu}
                aria-label="Open navigation menu"
                className="wv-shell__hamburger wv-touch"
              />
            }
          >
            <Brand />
            <ShellNav
              onNavigate={() => setDrawerOpen(false)}
              packNav={packNav}
              openGroups={openGroups}
              onToggleGroup={toggleGroup}
            />
          </NavDrawer>
          <Brand className="wv-shell__mobile-brand" />
          <div className="ml-auto flex items-center gap-2">
            <ThemeToggle />
            <SecurityLink />
            <SignOutButton />
          </div>
        </header>

        <div data-slot="shell-content" className="min-w-0 flex-1">
          {children ?? <Outlet />}
        </div>
      </div>
    </div>
  );
}
