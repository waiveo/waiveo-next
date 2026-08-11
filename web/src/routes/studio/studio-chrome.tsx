import {
  useCallback,
  useEffect,
  useId,
  useRef,
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
  type PointerEvent as ReactPointerEvent,
  type ReactNode,
} from "react";
import { Check, ChevronDown, ChevronRight, Maximize2, Minus, Plus, Scan } from "lucide-react";
import type { LucideIcon } from "lucide-react";
import { KitIcon } from "@/components/kit";
import { cn } from "@/lib/utils";

/**
 * The Studio's own chrome — the parts of a full-screen editor that no console
 * page has any use for, and which therefore do not belong in the widget kit.
 *
 * ── Why this is not the kit ─────────────────────────────────────────────────
 * `components/kit` is the console's PAGE vocabulary: one PageHeader, one
 * DataTable, one Modal, all of them sized for a document that scrolls inside a
 * nav shell. An editor is the other thing. Its surfaces are dense, fixed-height
 * and non-scrolling, its panels dock rather than stack, and its density is a
 * deliberate departure from the page rhythm rather than a violation of it. Every
 * piece here is built from the SAME Horizon tokens the kit uses (`--wv-surface`,
 * `--wv-border`, `--wv-accent`, …) so the two read as one product — but a
 * MenuBar in the kit would be an idiom with exactly one caller, and a DockPanel
 * there would invite a page to dock something.
 *
 * ── Where the shapes come from ──────────────────────────────────────────────
 * The arrangement, the density and the interaction affordances are the LEGACY
 * Slidecast Studio's, which the owner hand-built: a two-row header (menu bar
 * over document title), a left slide rail, the canvas in a TV frame, a right
 * sidebar of stacked collapsible panels over a resize handle, and a bottom tool
 * rail with a centred zoom cluster. What is NOT carried over is legacy's CSS —
 * `slidecast/static/studio.css` is written against a different token set
 * (`rgb(var(--color-*))`) and a different framework. The design LANGUAGE is
 * reproduced in this codebase's own tokens; nothing is imported.
 */

/* ────────────────────────────────────────────────────────────────────────────
 * The frame
 * ──────────────────────────────────────────────────────────────────────────*/

/**
 * The full-screen editor frame.
 *
 * `fixed inset-0` rather than a tall block, for the reason legacy is
 * `position: fixed; height: 100vh`: an editor must never be the thing that
 * scrolls. Its regions scroll individually (the slide rail, each dock panel, the
 * canvas viewport) and the frame itself holds still, so a drag on the canvas
 * cannot scroll the page out from under the pointer.
 *
 * `h-[100dvh]` alongside `inset-0` because mobile browsers report `100vh` as the
 * tallest the viewport ever gets, not the height it has with the URL bar
 * showing — the tool rail would sit below the fold.
 */
export function StudioShell({ children }: { children: ReactNode }) {
  return (
    <div
      data-slot="studio-shell"
      className="fixed inset-0 z-40 flex h-[100dvh] w-screen flex-col overflow-hidden bg-background text-foreground"
    >
      {children}
    </div>
  );
}

/** The header: two rows, menu bar over document title, on the surface layer. */
export function StudioHeaderBar({ children }: { children: ReactNode }) {
  return (
    <header
      data-slot="studio-header"
      className="flex shrink-0 flex-col border-b border-border bg-[color:var(--wv-surface)]"
    >
      {children}
    </header>
  );
}

/** Row one — the menu bar and the document-level actions. */
export function StudioMenuRow({ left, right }: { left: ReactNode; right: ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-2 border-b border-border/50 px-3 py-1">
      <div className="flex min-w-0 items-center gap-1">{left}</div>
      <div className="flex shrink-0 items-center gap-2">{right}</div>
    </div>
  );
}

/** Row two — what the document is called, and what state it is in. */
export function StudioTitleRow({ left, right }: { left: ReactNode; right: ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-2 px-3 py-1.5">
      <div className="flex min-w-0 flex-1 items-center gap-3">{left}</div>
      <div className="flex shrink-0 items-center gap-2">{right}</div>
    </div>
  );
}

/** The editor's middle band: rail, canvas, sidebar. */
export function StudioMain({ children }: { children: ReactNode }) {
  return (
    <div data-slot="studio-main" className="flex min-h-0 flex-1">
      {children}
    </div>
  );
}

/** The bottom tool rail. A `<footer>`, as legacy has it — the insert tools sit
 * under the canvas rather than beside it so the two vertical panels keep their
 * full height. */
export function StudioToolRail({ children }: { children: ReactNode }) {
  return (
    <footer
      data-slot="studio-toolbar"
      className="relative flex shrink-0 items-center gap-1 border-t border-border bg-[color:var(--wv-surface)] px-3 py-1.5"
    >
      {children}
    </footer>
  );
}

/** A tool-rail button: glyph over a small caption, the way legacy draws one. */
export function ToolButton({
  icon,
  label,
  name,
  onClick,
  disabled = false,
  active = false,
  title,
}: {
  icon: LucideIcon;
  /** The caption under the glyph. Kept short — the button is 58px wide. */
  label: string;
  /** The accessible name, when the caption is too terse to be one on its own
   * ("Widget" under the glyph, "Add widget" to a screen reader). It must CONTAIN
   * the caption (WCAG 2.5.3), so a voice-control user can say what they read. */
  name?: string;
  onClick: () => void;
  disabled?: boolean;
  active?: boolean;
  title?: string;
}) {
  return (
    <button
      type="button"
      data-slot="studio-tool"
      data-active={active}
      disabled={disabled}
      title={title ?? name ?? label}
      aria-label={name}
      onClick={onClick}
      className={cn(
        "flex min-h-[44px] w-[58px] shrink-0 flex-col items-center justify-center gap-0.5 rounded-md px-1 py-1",
        "text-[10px] font-medium leading-tight outline-none transition-colors",
        "focus-visible:ring-2 focus-visible:ring-ring disabled:pointer-events-none disabled:opacity-40",
        active
          ? "bg-[color:var(--wv-nav-active-bg)] text-[color:var(--wv-nav-active-fg)]"
          : "text-muted-foreground hover:bg-accent hover:text-foreground",
      )}
    >
      <KitIcon icon={icon} decorative className="size-[18px]" />
      <span className="w-full truncate text-center">{label}</span>
    </button>
  );
}

/** The 1px rule legacy puts between tool groups. */
export function ToolDivider() {
  return <span aria-hidden="true" className="mx-2 h-9 w-px shrink-0 bg-border" />;
}

/* ────────────────────────────────────────────────────────────────────────────
 * The menu bar
 * ──────────────────────────────────────────────────────────────────────────*/

export interface MenuCommand {
  id: string;
  label: string;
  /** The chord, drawn right-aligned and muted. Display only — the binding lives
   * in the shortcut matcher, and this string is what tells the operator it
   * exists. */
  shortcut?: string | undefined;
  disabled?: boolean | undefined;
  /** Present (true or false) makes the row a toggle and reserves the tick
   * column, so a group of toggles stays aligned whatever is currently on. */
  checked?: boolean | undefined;
  run: () => void;
}

export interface MenuSeparator {
  id: string;
  separator: true;
}

export type MenuEntry = MenuCommand | MenuSeparator;

export interface MenuDef {
  id: string;
  label: string;
  items: MenuEntry[];
}

function isSeparator(entry: MenuEntry): entry is MenuSeparator {
  return "separator" in entry;
}

/**
 * The application menu bar — File / Edit / View / Insert / Arrange / Slide /
 * Help, exactly the legacy header's top row.
 *
 * Built from plain elements rather than the vendored Radix dropdown for two
 * reasons. Routes may not reach `@/components/ui` (the kit boundary), and a
 * menu BAR is not a stack of independent dropdowns: once one menu is open,
 * moving the pointer across the bar has to swap to the next one without a
 * second click, which is the behaviour that makes a menu bar feel like a menu
 * bar and which composing seven separate popups does not give you.
 *
 * It is still a menu to the accessibility tree — `menubar` / `menu` /
 * `menuitem`, arrow keys within an open menu, Escape closes and returns focus
 * to the trigger, and a click anywhere else dismisses.
 */
export function MenuBar({ menus }: { menus: MenuDef[] }) {
  const [open, setOpen] = useState<string | null>(null);
  const barRef = useRef<HTMLDivElement>(null);
  const baseId = useId();

  // Dismiss on a press anywhere outside the bar. `pointerdown` rather than
  // `click`, so a menu never survives long enough to swallow the press that was
  // meant for the canvas underneath it.
  useEffect(() => {
    if (open === null) return;
    const onDown = (e: PointerEvent) => {
      if (barRef.current?.contains(e.target as Node)) return;
      setOpen(null);
    };
    document.addEventListener("pointerdown", onDown);
    return () => document.removeEventListener("pointerdown", onDown);
  }, [open]);

  const close = useCallback((focusTrigger: string | null) => {
    setOpen(null);
    if (focusTrigger === null) return;
    // Focus goes back where the operator left it. Deferred a frame because the
    // menu is still mounted at the moment the handler runs.
    window.requestAnimationFrame(() => {
      const el = barRef.current?.querySelector<HTMLButtonElement>(`[data-menu-trigger="${focusTrigger}"]`);
      el?.focus();
    });
  }, []);

  /**
   * The open menu's keyboard, handled on the BAR rather than on the panel.
   *
   * It was on the panel, and that made Escape conditional on focus being inside
   * it — which it is not when every row is disabled, because there is nothing to
   * focus. Open the Edit menu with no selection (every command needs one) and
   * Escape did nothing: the menu stayed up, and the next click on the trigger
   * read as a toggle and closed it, so the menu appeared to open on the SECOND
   * press. The bar contains the trigger as well as the panel, so a handler here
   * catches the keystroke in both positions.
   */
  const onBarKeyDown = (e: ReactKeyboardEvent) => {
    if (open === null) return;
    if (e.key === "Escape") {
      e.preventDefault();
      // Claimed, so the editor's own Escape (deselect) does not also fire behind
      // a menu the operator was dismissing.
      e.stopPropagation();
      close(open);
      return;
    }
    if (e.key !== "ArrowDown" && e.key !== "ArrowUp") return;
    e.preventDefault();
    const rows = Array.from(
      barRef.current?.querySelectorAll<HTMLButtonElement>("[data-menu-item]:not(:disabled)") ?? [],
    );
    if (rows.length === 0) return;
    // -1 when focus is still on the trigger, which makes ArrowDown land on the
    // first row — the behaviour a menu bar is expected to have.
    const at = rows.indexOf(document.activeElement as HTMLButtonElement);
    const next = e.key === "ArrowDown" ? (at + 1) % rows.length : (at - 1 + rows.length) % rows.length;
    rows[next]?.focus();
  };

  return (
    <div
      ref={barRef}
      role="menubar"
      aria-label="Studio menu"
      className="flex items-center"
      onKeyDown={onBarKeyDown}
    >
      {menus.map((menu) => {
        const isOpen = open === menu.id;
        return (
          <div key={menu.id} className="relative">
            <button
              type="button"
              role="menuitem"
              data-menu-trigger={menu.id}
              aria-haspopup="menu"
              aria-expanded={isOpen}
              onClick={() => setOpen(isOpen ? null : menu.id)}
              // Once ANY menu is open, sliding along the bar opens the next.
              onPointerEnter={() => {
                if (open !== null) setOpen(menu.id);
              }}
              className={cn(
                "rounded px-2.5 py-1.5 text-[13px] font-medium outline-none transition-colors",
                "focus-visible:ring-2 focus-visible:ring-ring",
                isOpen ? "bg-accent text-foreground" : "text-foreground hover:bg-accent",
              )}
            >
              {menu.label}
            </button>

            {isOpen ? (
              <MenuPanel
                id={`${baseId}-${menu.id}`}
                label={menu.label}
                items={menu.items}
                onClose={() => close(menu.id)}
              />
            ) : null}
          </div>
        );
      })}
    </div>
  );
}

/** One open menu. Mounted only while open, so the focus-on-open effect below
 * runs exactly once per opening rather than needing to watch a flag.
 *
 * The KEYBOARD is the bar's, not this component's — see MenuBar.onBarKeyDown for
 * the defect that moved it there. */
function MenuPanel({
  id,
  label,
  items,
  onClose,
}: {
  id: string;
  label: string;
  items: MenuEntry[];
  onClose: () => void;
}) {
  const panelRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    // The first row that can actually be pressed. A menu whose every command
    // needs a selection has none, and focus correctly stays on the trigger.
    panelRef.current?.querySelector<HTMLButtonElement>("[data-menu-item]:not(:disabled)")?.focus();
  }, []);

  return (
    <div
      ref={panelRef}
      id={id}
      role="menu"
      aria-label={label}
      className={cn(
        "absolute left-0 top-full z-50 mt-0.5 min-w-[240px] rounded-lg p-1",
        "border border-border bg-[color:var(--wv-surface)] shadow-[0_8px_28px_-10px_rgba(0,0,0,0.55)]",
      )}
    >
      {items.map((entry) =>
        isSeparator(entry) ? (
          <div key={entry.id} role="separator" className="my-1 h-px bg-border" />
        ) : (
          <button
            key={entry.id}
            type="button"
            role="menuitem"
            data-menu-item={entry.id}
            disabled={entry.disabled === true}
            aria-checked={entry.checked === undefined ? undefined : entry.checked}
            onClick={() => {
              entry.run();
              onClose();
            }}
            className={cn(
              "flex w-full items-center gap-2 rounded px-2 py-1.5 text-left text-[13px] outline-none transition-colors",
              "hover:bg-accent focus-visible:bg-accent focus-visible:ring-2 focus-visible:ring-ring",
              "disabled:pointer-events-none disabled:opacity-40",
            )}
          >
            {/* The tick column is reserved for the WHOLE menu as soon as one row
                is a toggle, so the labels of a mixed menu do not jog sideways
                depending on what is currently on. */}
            {items.some((i) => !isSeparator(i) && i.checked !== undefined) ? (
              <span aria-hidden="true" className="flex w-3.5 shrink-0 justify-center">
                {entry.checked === true ? (
                  <KitIcon icon={Check} decorative className="size-3.5 text-[color:var(--wv-accent-text)]" />
                ) : null}
              </span>
            ) : null}
            <span className="flex-1 truncate">{entry.label}</span>
            {entry.shortcut ? (
              <span aria-hidden="true" className="shrink-0 text-[11px] tabular-nums text-muted-foreground">
                {entry.shortcut}
              </span>
            ) : null}
          </button>
        ),
      )}
    </div>
  );
}

/* ────────────────────────────────────────────────────────────────────────────
 * Docked panels
 * ──────────────────────────────────────────────────────────────────────────*/

/**
 * One docked tool panel: a clickable title bar that collapses the body.
 *
 * A panel is not a page section, and the difference is the header. It is 32px
 * tall, its title is 11px upper-case, and pressing it folds the panel away —
 * which is what makes a column of four of them usable in the height a viewport
 * has, and is exactly how legacy's `.right-panel .panel-header` behaves.
 *
 * `grow` marks the panel that takes the leftover height (Properties). The others
 * are content-sized with their own scroll.
 */
export function DockPanel({
  title,
  count,
  collapsed,
  onToggle,
  grow = false,
  maxBodyClass,
  actions,
  children,
}: {
  title: string;
  /** A muted number after the title — how many layers, how many steps. */
  count?: string | undefined;
  collapsed: boolean;
  onToggle: () => void;
  grow?: boolean;
  /** A Tailwind max-height for the scrolling body, for the panels that must not
   * eat the whole column. */
  maxBodyClass?: string | undefined;
  actions?: ReactNode;
  children: ReactNode;
}) {
  const bodyId = useId();
  return (
    <section
      data-slot="dock-panel"
      data-panel={title.toLowerCase()}
      data-collapsed={collapsed}
      aria-label={title}
      className={cn(
        "flex min-h-0 flex-col border-b border-border last:border-b-0",
        grow && !collapsed ? "flex-1" : "shrink-0",
      )}
    >
      <div className="flex h-8 shrink-0 items-center gap-1 pr-1">
        <button
          type="button"
          onClick={onToggle}
          aria-expanded={!collapsed}
          aria-controls={bodyId}
          className={cn(
            "flex min-w-0 flex-1 items-center gap-1.5 px-2 py-1 text-left outline-none transition-colors",
            "hover:bg-accent focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring",
          )}
        >
          <KitIcon
            icon={collapsed ? ChevronRight : ChevronDown}
            decorative
            className="size-3.5 shrink-0 text-muted-foreground"
          />
          <span className="truncate text-[11px] font-semibold uppercase tracking-[0.06em]">{title}</span>
          {count ? <span className="shrink-0 text-[11px] font-normal text-muted-foreground">{count}</span> : null}
        </button>
        {collapsed ? null : actions}
      </div>
      {collapsed ? null : (
        <div
          id={bodyId}
          data-slot="dock-panel-body"
          className={cn("min-h-0 flex-1 overflow-y-auto overflow-x-hidden px-2 pb-2", maxBodyClass)}
        >
          {children}
        </div>
      )}
    </section>
  );
}

/**
 * The sidebar's drag-to-resize edge.
 *
 * A 4px hit strip that paints accent while it is being used, as legacy's does.
 * The pointer listeners go on the WINDOW during the drag for the same reason
 * the canvas's do: a fast drag outruns a 4px target constantly, and a handler
 * bound to the strip would drop the gesture the moment the pointer left it.
 */
export function SidebarResizer({
  width,
  min,
  max,
  onWidth,
}: {
  width: number;
  min: number;
  max: number;
  /** Called with the new width on every frame of the drag. */
  onWidth: (next: number) => void;
}) {
  const [dragging, setDragging] = useState(false);
  const startRef = useRef<{ x: number; width: number } | null>(null);

  useEffect(() => {
    if (!dragging) return;
    const onMove = (e: PointerEvent) => {
      const start = startRef.current;
      if (!start) return;
      // The sidebar is on the RIGHT, so dragging its left edge leftwards makes
      // it wider — the delta is subtracted, not added.
      onWidth(Math.min(max, Math.max(min, start.width - (e.clientX - start.x))));
    };
    const onUp = () => {
      startRef.current = null;
      setDragging(false);
    };
    window.addEventListener("pointermove", onMove);
    window.addEventListener("pointerup", onUp);
    window.addEventListener("pointercancel", onUp);
    return () => {
      window.removeEventListener("pointermove", onMove);
      window.removeEventListener("pointerup", onUp);
      window.removeEventListener("pointercancel", onUp);
    };
  }, [dragging, min, max, onWidth]);

  const begin = (e: ReactPointerEvent) => {
    e.preventDefault();
    startRef.current = { x: e.clientX, width };
    setDragging(true);
  };

  return (
    <div
      role="separator"
      aria-orientation="vertical"
      aria-label="Resize the panels"
      aria-valuenow={width}
      aria-valuemin={min}
      aria-valuemax={max}
      tabIndex={0}
      data-slot="sidebar-resizer"
      onPointerDown={begin}
      // The keyboard equivalent, because a drag handle nobody can reach without
      // a mouse is a control half the operators do not have.
      onKeyDown={(e) => {
        const step = e.shiftKey ? 48 : 12;
        if (e.key === "ArrowLeft") {
          e.preventDefault();
          onWidth(Math.min(max, width + step));
        } else if (e.key === "ArrowRight") {
          e.preventDefault();
          onWidth(Math.max(min, width - step));
        }
      }}
      className={cn(
        "absolute left-0 top-0 z-10 h-full w-1 cursor-ew-resize outline-none transition-colors",
        "hover:bg-[color:var(--wv-accent)] focus-visible:bg-[color:var(--wv-accent)]",
        dragging ? "bg-[color:var(--wv-accent)]" : "bg-transparent",
      )}
    />
  );
}

/* ────────────────────────────────────────────────────────────────────────────
 * Zoom
 * ──────────────────────────────────────────────────────────────────────────*/

export interface ZoomControlsProps {
  /** The scale actually in force, as a fraction (0.5 = 50%). */
  zoom: number;
  onZoomIn: () => void;
  onZoomOut: () => void;
  onFit: () => void;
  onActualSize: () => void;
  /** True when the zoom is being kept at fit — the button reads as engaged. */
  fitted: boolean;
}

/** The zoom cluster legacy centres in its tool rail. */
export function ZoomControls({ zoom, onZoomIn, onZoomOut, onFit, onActualSize, fitted }: ZoomControlsProps) {
  const pct = Math.round(zoom * 100);
  return (
    <div
      data-slot="zoom-controls"
      className="flex items-center gap-0.5 rounded-md bg-[color:var(--wv-surface-2)] px-1 py-0.5"
    >
      <ZoomButton icon={Minus} label="Zoom out" onClick={onZoomOut} />
      {/* A button, not a readout: pressing the percentage snaps to 100%, which
          is the gesture every editor with a zoom box has. */}
      <button
        type="button"
        data-slot="zoom-level"
        onClick={onActualSize}
        aria-label={`Zoom is ${pct} percent — set to 100 percent`}
        title="Actual size (100%)"
        className="min-w-[52px] rounded px-1.5 py-1 text-center text-[12px] font-medium tabular-nums outline-none hover:bg-[color:var(--wv-surface)] focus-visible:ring-2 focus-visible:ring-ring"
      >
        {pct}%
      </button>
      <ZoomButton icon={Plus} label="Zoom in" onClick={onZoomIn} />
      <span aria-hidden="true" className="mx-0.5 h-4 w-px bg-border" />
      <ZoomButton icon={Scan} label="Fit to window" onClick={onFit} active={fitted} />
      <ZoomButton icon={Maximize2} label="Actual size" onClick={onActualSize} />
    </div>
  );
}

function ZoomButton({
  icon,
  label,
  onClick,
  active = false,
}: {
  icon: LucideIcon;
  label: string;
  onClick: () => void;
  active?: boolean;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      aria-label={label}
      aria-pressed={active}
      title={label}
      className={cn(
        "flex size-7 items-center justify-center rounded outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring",
        active
          ? "bg-[color:var(--wv-nav-active-bg)] text-[color:var(--wv-nav-active-fg)]"
          : "text-muted-foreground hover:bg-[color:var(--wv-surface)] hover:text-foreground",
      )}
    >
      <KitIcon icon={icon} decorative className="size-3.5" />
    </button>
  );
}

/* ────────────────────────────────────────────────────────────────────────────
 * The TV frame
 * ──────────────────────────────────────────────────────────────────────────*/

/**
 * The canvas's TV frame — bezel, brand plate, power LED and stand.
 *
 * This is legacy's, and it is not decoration: the artwork being edited is
 * destined for a television, and a black rectangle floating in a grey field
 * gives an operator no sense of that. Framing it says what the surface IS, and
 * it is the single most recognisable thing about the legacy canvas.
 *
 * Two divergences from legacy, both deliberate.
 *
 * The brand chip carries `--grad-accent`; legacy paints it in a violet gradient
 * that predates HORIZON, and the mark on the bezel should be the same mark as
 * everywhere else in the console.
 *
 * And the bezel is drawn in SCREEN pixels rather than scaled with the artwork.
 * Legacy applies one `transform: scale()` to frame and picture together, which
 * is the natural thing to do when the selection grips are inside the transform
 * too. They are not here — this canvas draws its chrome outside the scale so a
 * grip stays 10px and grabbable at 25% zoom (slide-canvas.tsx says why) — so a
 * scaled bezel would be the one part of the composition that shrank, and at 300%
 * it would be a hand's width of plastic around the picture.
 */
export function TvFrame({ children }: { children: ReactNode }) {
  return (
    <div data-slot="tv-frame" className="flex flex-col items-center">
      <div
        className="relative rounded-[16px] bg-[linear-gradient(145deg,#2d2d2d,#1a1a1a)] px-4 pb-7 pt-4"
        style={{ boxShadow: "0 20px 50px rgba(0,0,0,0.4), inset 0 1px 0 rgba(255,255,255,0.1)" }}
      >
        <div className="relative overflow-hidden rounded-[6px] bg-black shadow-[inset_0_0_20px_rgba(0,0,0,0.6)]">
          {children}
        </div>
        <div
          aria-hidden="true"
          className="absolute bottom-1.5 left-1/2 flex -translate-x-1/2 items-center gap-1.5 text-[11px] font-light lowercase tracking-[2px] text-[#666]"
        >
          <span className="flex size-4 items-center justify-center rounded-[3px] bg-[image:var(--grad-accent)] text-[10px] font-bold tracking-normal text-[color:var(--grad-accent-fg)]">
            W
          </span>
          <span className="opacity-60">waiveo</span>
        </div>
        <span
          aria-hidden="true"
          className="absolute bottom-2.5 right-5 size-[5px] rounded-full bg-[#4ade80]"
          style={{ boxShadow: "0 0 6px #4ade80, 0 0 12px rgba(74,222,128,0.4)" }}
        />
      </div>
      <div aria-hidden="true" className="flex flex-col items-center">
        <div className="h-4 w-10 rounded-b-[2px] bg-[linear-gradient(to_right,#1a1a1a,#333,#1a1a1a)]" />
        <div className="h-1.5 w-24 rounded-[3px] bg-[linear-gradient(145deg,#333,#1a1a1a)]" />
      </div>
    </div>
  );
}
