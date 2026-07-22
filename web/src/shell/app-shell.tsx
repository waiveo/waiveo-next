import { useState, type ReactNode } from "react";
import { Link, NavLink, Outlet } from "react-router";
import {
  AudioWaveform,
  LayoutDashboard,
  Menu,
  Palette,
  PanelLeftClose,
  PanelLeftOpen,
  type LucideIcon,
} from "lucide-react";
import { Button, KitIcon, NavDrawer } from "@/components/kit";
import { ThemeToggle } from "@/components/theme/theme-toggle";
import { cn } from "@/lib/utils";

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

function ShellNav({
  collapsed,
  onNavigate,
}: {
  collapsed?: boolean;
  onNavigate?: () => void;
}) {
  return (
    <nav aria-label="Primary" className="flex flex-col gap-1">
      {NAV_ITEMS.map((item) => (
        <NavLink
          key={item.to}
          to={item.to}
          end={item.end ?? false}
          onClick={onNavigate}
          className={({ isActive }) =>
            cn(
              "wv-touch flex items-center gap-2.5 rounded-input px-3 text-[13px] font-medium outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring",
              isActive
                ? "bg-[color:var(--wv-nav-active-bg)] text-[color:var(--wv-nav-active-fg)]"
                : "text-muted-foreground hover:bg-accent hover:text-foreground",
              collapsed && "justify-center",
            )
          }
        >
          <KitIcon icon={item.icon} decorative className="size-4 shrink-0" />
          <span className={cn(collapsed && "sr-only")}>{item.label}</span>
        </NavLink>
      ))}
    </nav>
  );
}

export function AppShell({ children }: { children?: ReactNode }) {
  const [collapsed, setCollapsed] = useState(false);
  const [drawerOpen, setDrawerOpen] = useState(false);

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
        <ShellNav collapsed={collapsed} />
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
            <ShellNav onNavigate={() => setDrawerOpen(false)} />
          </NavDrawer>
          <Brand className="wv-shell__mobile-brand" />
          <div className="ml-auto flex items-center gap-2">
            <ThemeToggle />
          </div>
        </header>

        <div data-slot="shell-content" className="min-w-0 flex-1">
          {children ?? <Outlet />}
        </div>
      </div>
    </div>
  );
}
