import { describe, it, expect, afterEach } from "vitest";
import { render, screen, within, waitFor, act } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { ThemeProvider } from "@/components/theme/theme-provider";
import { AppShell } from "./app-shell";
import DesignRoute from "@/routes/design/design-route";

// Drive matchMedia so a test can render at a specific viewport. Default (below)
// restores the "wide desktop" reading (max-width probes false) so a stray render
// never accidentally lands in the narrow branch.
function setViewport(kind: "desktop" | "phone") {
  window.matchMedia = ((query: string) =>
    ({
      matches: /max-width/.test(query) ? kind === "phone" : kind === "desktop",
      media: query,
      onchange: null,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
    }) as unknown as MediaQueryList) as unknown as typeof window.matchMedia;
}

afterEach(() => setViewport("desktop"));

function renderShell(path = "/design", children = <div>route content</div>) {
  return render(
    <ThemeProvider>
      <MemoryRouter initialEntries={[path]}>
        <AppShell>{children}</AppShell>
      </MemoryRouter>
    </ThemeProvider>,
  );
}

describe("AppShell — locked-left responsive shell", () => {
  it("renders the three shell regions (sidebar, header, content)", () => {
    const { container } = renderShell();
    expect(container.querySelector('[data-slot="shell-sidebar"]')).not.toBeNull();
    expect(container.querySelector('[data-slot="shell-header"]')).not.toBeNull();
    const content = container.querySelector('[data-slot="shell-content"]');
    expect(content).not.toBeNull();
    expect(within(content as HTMLElement).getByText("route content")).toBeInTheDocument();
  });

  it("locks the primary nav to a LEFT sidebar rail — never a top-nav", () => {
    const { container } = renderShell();
    const sidebar = container.querySelector('[data-slot="shell-sidebar"]') as HTMLElement;
    // The rail is an <aside> carrying the left-locked breakpoint class.
    expect(sidebar.tagName).toBe("ASIDE");
    expect(sidebar.className).toContain("wv-shell__sidebar");
    // The primary nav lives in the rail...
    const railNav = within(sidebar).getByRole("navigation", { name: /primary/i });
    expect(within(railNav).getByRole("link", { name: "Design kit" })).toBeInTheDocument();
    expect(within(railNav).getByRole("link", { name: "Overview" })).toBeInTheDocument();
    // ...and NOT in the header (that would be a top-nav).
    const header = container.querySelector('[data-slot="shell-header"]') as HTMLElement;
    expect(within(header).queryByRole("link", { name: "Design kit" })).toBeNull();
    // The rail sits before the content in document order (left, not below).
    const content = container.querySelector('[data-slot="shell-content"]') as HTMLElement;
    expect(sidebar.compareDocumentPosition(content) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it("carries the full console navigation in the rail, in order", () => {
    const { container } = renderShell();
    const sidebar = container.querySelector('[data-slot="shell-sidebar"]') as HTMLElement;
    const railNav = within(sidebar).getByRole("navigation", { name: /primary/i });
    const labels = within(railNav)
      .getAllByRole("link")
      .map((a) => a.textContent?.trim());
    expect(labels).toEqual([
      "Overview",
      "Screens",
      "Devices",
      "Schedules",
      "Casts",
      "Content",
      "Media",
      "Automations",
      "Activity",
      "Pages",
      "Design kit",
    ]);
  });

  it("marks the Activity route active when it is the current path", () => {
    const { container } = renderShell("/activity");
    const sidebar = container.querySelector('[data-slot="shell-sidebar"]') as HTMLElement;
    expect(within(sidebar).getByRole("link", { name: "Activity" })).toHaveAttribute(
      "aria-current",
      "page",
    );
  });

  it("carries a brand mark in the rail", () => {
    const { container } = renderShell();
    const sidebar = container.querySelector('[data-slot="shell-sidebar"]') as HTMLElement;
    expect(within(sidebar).getByText(/waiveo/i)).toBeInTheDocument();
  });

  it("marks the active route's nav item with aria-current=page", () => {
    const { container } = renderShell("/design");
    const sidebar = container.querySelector('[data-slot="shell-sidebar"]') as HTMLElement;
    expect(within(sidebar).getByRole("link", { name: "Design kit" })).toHaveAttribute(
      "aria-current",
      "page",
    );
    expect(within(sidebar).getByRole("link", { name: "Overview" })).not.toHaveAttribute(
      "aria-current",
    );
  });

  it("collapses the rail to an icon-only presentation and back", async () => {
    const user = userEvent.setup();
    const { container } = renderShell();
    const sidebar = container.querySelector('[data-slot="shell-sidebar"]') as HTMLElement;
    const collapse = within(sidebar).getByRole("button", { name: /collapse sidebar/i });
    expect(collapse).toHaveAttribute("aria-pressed", "false");
    await user.click(collapse);
    expect(sidebar).toHaveAttribute("data-collapsed", "true");
    const expand = within(sidebar).getByRole("button", { name: /expand sidebar/i });
    expect(expand).toHaveAttribute("aria-pressed", "true");
    // The nav link still exposes an accessible name while collapsed to icons.
    expect(within(sidebar).getByRole("link", { name: "Design kit" })).toBeInTheDocument();
  });

  // ── The mobile drawer: hamburger + overlay + focus trap + esc + aria-expanded ──
  it("opens a left drawer from the hamburger, traps focus, and reflects aria-expanded", async () => {
    const user = userEvent.setup();
    renderShell();
    const hamburger = screen.getByRole("button", { name: /open navigation menu/i });
    expect(hamburger).toHaveAttribute("aria-expanded", "false");

    await user.click(hamburger);
    const drawer = await screen.findByRole("dialog");
    expect(hamburger).toHaveAttribute("aria-expanded", "true");
    // Radix moves focus into the drawer (focus trap).
    expect(drawer.contains(document.activeElement)).toBe(true);
    // The drawer carries the same primary nav.
    expect(within(drawer).getByRole("link", { name: "Design kit" })).toBeInTheDocument();
  });

  it("closes the drawer on Escape and restores aria-expanded", async () => {
    const user = userEvent.setup();
    renderShell();
    const hamburger = screen.getByRole("button", { name: /open navigation menu/i });
    await user.click(hamburger);
    await screen.findByRole("dialog");
    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(hamburger).toHaveAttribute("aria-expanded", "false");
  });

  it("closes the drawer when a nav item is chosen", async () => {
    const user = userEvent.setup();
    renderShell();
    await user.click(screen.getByRole("button", { name: /open navigation menu/i }));
    const drawer = await screen.findByRole("dialog");
    await user.click(within(drawer).getByRole("link", { name: "Design kit" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  it("closes the drawer when the viewport crosses into the locked-left desktop breakpoint", async () => {
    // A drawer left open while the viewport grows past 1024px would keep Radix's
    // modal side-effects (aria-hiding the app, scroll lock) engaged against a
    // CSS-hidden dialog. The shell must drop the open state on the crossing.
    const user = userEvent.setup();

    // Controllable matchMedia: starts narrow, remembers change listeners so the
    // test can drive a real breakpoint crossing (the default stub is inert).
    const listeners = new Set<() => void>();
    let desktop = false;
    window.matchMedia = ((query: string) => {
      const isMinWidth = /min-width/.test(query);
      return {
        get matches() {
          return isMinWidth ? desktop : !desktop;
        },
        media: query,
        onchange: null,
        addEventListener: (_: string, cb: () => void) => listeners.add(cb),
        removeEventListener: (_: string, cb: () => void) => listeners.delete(cb),
        addListener: (cb: () => void) => listeners.add(cb),
        removeListener: (cb: () => void) => listeners.delete(cb),
        dispatchEvent: () => false,
      } as unknown as MediaQueryList;
    }) as unknown as typeof window.matchMedia;

    renderShell();
    await user.click(screen.getByRole("button", { name: /open navigation menu/i }));
    await screen.findByRole("dialog");

    // Grow past the desktop breakpoint and notify subscribers, as the browser would.
    act(() => {
      desktop = true;
      listeners.forEach((cb) => cb());
    });

    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });
});

describe("AppShell — no horizontal PAGE scroll at 360px", () => {
  it("renders the gallery inside the shell at a mocked 360px with no wide table", () => {
    setViewport("phone");
    const { container } = render(
      <ThemeProvider>
        <MemoryRouter initialEntries={["/design"]}>
          <AppShell>
            <DesignRoute />
          </AppShell>
        </MemoryRouter>
      </ThemeProvider>,
    );

    // Every DataTable in the gallery is in its stacked (card) presentation, so no
    // <table> is present to force the page to scroll sideways at 360px.
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
    expect(
      document.querySelectorAll('[data-slot="data-table"][data-layout="stacked"]').length,
    ).toBeGreaterThanOrEqual(2);

    // The content column can shrink (min-w-0) so a flex child never blows out the
    // viewport width — the structural precondition for no horizontal page scroll.
    const content = container.querySelector('[data-slot="shell-content"]') as HTMLElement;
    expect(content.className).toContain("min-w-0");
  });
});
