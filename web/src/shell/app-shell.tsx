import { useEffect, useState, type ReactNode } from "react";
import { Link, NavLink, Outlet } from "react-router";
import {
  Activity,
  AudioWaveform,
  CalendarClock,
  DatabaseBackup,
  FileText,
  Film,
  HeartPulse,
  LayoutDashboard,
  LayoutTemplate,
  Menu,
  MonitorPlay,
  Palette,
  PanelLeftClose,
  PanelLeftOpen,
  Puzzle,
  Radio,
  Tv,
  Upload,
  Workflow,
  type LucideIcon,
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
 */
interface NavItem {
  to: string;
  label: string;
  icon: LucideIcon;
  end?: boolean;
}

const NAV_ITEMS: NavItem[] = [
  { to: "/", label: "Overview", icon: LayoutDashboard, end: true },
  { to: "/screens", label: "Screens", icon: MonitorPlay },
  { to: "/devices", label: "Devices", icon: Radio },
  { to: "/roku", label: "Roku", icon: Tv },
  { to: "/schedules", label: "Schedules", icon: CalendarClock },
  { to: "/casts", label: "Casts", icon: LayoutTemplate },
  { to: "/upload", label: "Upload", icon: Upload },
  { to: "/media", label: "Media", icon: Film },
  { to: "/automations", label: "Automations", icon: Workflow },
  { to: "/activity", label: "Activity", icon: Activity },
  { to: "/pages", label: "Pages", icon: LayoutTemplate },
  { to: "/system", label: "System", icon: HeartPulse },
  { to: "/backup", label: "Backup", icon: DatabaseBackup },
  { to: "/design", label: "Design kit", icon: Palette },
];

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

function ShellNav({
  collapsed,
  onNavigate,
  packNav,
}: {
  collapsed?: boolean;
  onNavigate?: () => void;
  packNav?: PackNavGroup[];
}) {
  return (
    <div className="flex flex-col gap-4">
      <nav aria-label="Primary" className="flex flex-col gap-1">
        {NAV_ITEMS.map((item) => (
          <NavLink
            key={item.to}
            to={item.to}
            end={item.end ?? false}
            onClick={onNavigate}
            className={navLinkClass(collapsed)}
          >
            <KitIcon icon={item.icon} decorative className="size-4 shrink-0" />
            <span className={cn(collapsed && "sr-only")}>{item.label}</span>
          </NavLink>
        ))}
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
    <nav aria-label="Extensions" className="flex flex-col gap-2">
      <div className={cn("flex items-center gap-2 px-3", collapsed && "justify-center")}>
        <KitIcon icon={Puzzle} decorative className="size-4 shrink-0 text-muted-foreground" />
        <span
          className={cn(
            "text-[11px] font-semibold uppercase tracking-wide text-muted-foreground",
            collapsed && "sr-only",
          )}
        >
          Extensions
        </span>
      </div>
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
        <ShellNav collapsed={collapsed} packNav={packNav} />
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
            <ShellNav onNavigate={() => setDrawerOpen(false)} packNav={packNav} />
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
