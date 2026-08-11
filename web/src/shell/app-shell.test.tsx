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

afterEach(() => {
  setViewport("desktop");
  // The rail's expand/collapse memory is persisted, so a test that collapses a
  // group would otherwise hand that state to the next one.
  window.localStorage.clear();
});

function renderShell(path = "/design", children = <div>route content</div>) {
  return render(
    <ThemeProvider>
      <MemoryRouter initialEntries={[path]}>
        <AppShell>{children}</AppShell>
      </MemoryRouter>
    </ThemeProvider>,
  );
}

/**
 * Read the rail back as the TREE it renders, from the DOM.
 *
 * Reading structure rather than a flat list of labels is the point: the defect
 * being fixed was not "the wrong links" — every link was there — it was that
 * they were all siblings. A test that collects `getAllByRole("link")` passes
 * just as happily on thirteen flat items as on a grouped tree, which is exactly
 * how the flat rail survived this long.
 */
function readRailTree(sidebar: HTMLElement) {
  const nav = within(sidebar).getByRole("navigation", { name: /primary/i });
  const top = nav.querySelector(":scope > ul") as HTMLElement;
  return Array.from(top.children).map((li) => {
    const toggle = li.querySelector(':scope > [data-slot="nav-group-toggle"]');
    if (toggle) {
      const panel = li.querySelector(':scope > [data-slot="nav-group-panel"]') as HTMLElement;
      return {
        group: toggle.textContent?.trim(),
        expanded: toggle.getAttribute("aria-expanded") === "true",
        children: Array.from(panel.querySelectorAll("a")).map((a) => a.textContent?.trim()),
      };
    }
    return { leaf: (li.querySelector(":scope > a") as HTMLElement | null)?.textContent?.trim() };
  });
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

  // ── The information architecture ──────────────────────────────────────────
  // The rail used to be thirteen flat siblings. The owner's complaint was
  // structural, not cosmetic: "you've got CASTS over here, but CASTS should be
  // under slide casts... Screens should be under slide cast, just like CASTS."
  // These assertions ARE the IA — moving a page between areas has to be a
  // deliberate edit here, not something that happens to still pass.

  it("groups the rail by product area, with Casts and Screens under Slidecast", () => {
    const { container } = renderShell();
    const sidebar = container.querySelector('[data-slot="shell-sidebar"]') as HTMLElement;
    expect(readRailTree(sidebar)).toEqual([
      { leaf: "Overview" },
      {
        group: "Slidecast",
        expanded: true,
        // Legacy's own order (Casts · Screens · Media · Widgets), with the two
        // members legacy never had slotted where they belong rather than
        // appended: Schedules programs Screens, Upload is Media's write half.
        children: ["Casts", "Screens", "Schedules", "Media", "Upload", "Widgets"],
      },
      { group: "Devices", expanded: true, children: ["All devices", "Roku"] },
      { leaf: "Automations" },
      { group: "Extensions", expanded: true, children: ["Installed"] },
      {
        group: "Platform",
        expanded: true,
        children: ["Activity", "System", "Backup", "Pages", "Design kit"],
      },
    ]);
  });

  it("puts NO product page at the top level beside the platform pages", () => {
    // The other half of the same property, stated as an absence. The tree
    // assertion above would still pass if a duplicate `Casts` were ALSO left at
    // the top level; this is what says the page moved rather than multiplied.
    const { container } = renderShell();
    const sidebar = container.querySelector('[data-slot="shell-sidebar"]') as HTMLElement;
    const topLevelLeaves = readRailTree(sidebar)
      .map((n) => ("leaf" in n ? n.leaf : undefined))
      .filter((l): l is string => l !== undefined);
    expect(topLevelLeaves).toEqual(["Overview", "Automations"]);
    for (const scattered of ["Casts", "Screens", "Media", "Upload", "System", "Backup"]) {
      expect(topLevelLeaves).not.toContain(scattered);
    }
  });

  it("still reaches every destination the flat rail had", () => {
    // Grouping is exactly the operation that loses a page, and a lost page is a
    // worse outcome than the flat rail. (The build-level version of this is
    // cmd/waiveo-feeder/consoleroutes_test.go, which derives BOTH the route
    // table and the tree; this is the rendered check.)
    const { container } = renderShell();
    const sidebar = container.querySelector('[data-slot="shell-sidebar"]') as HTMLElement;
    const railNav = within(sidebar).getByRole("navigation", { name: /primary/i });
    const labels = within(railNav)
      .getAllByRole("link")
      .map((a) => a.textContent?.trim());
    for (const had of [
      "Overview",
      "Screens",
      "Devices",
      // Reachable as a link — this list checks nothing disappeared, not where it
      // sits. Roku is a child of Devices; see the placement note in nav-tree.ts.
      "Roku",
      "Schedules",
      "Casts",
      "Upload",
      "Media",
      "Automations",
      "Activity",
      "Pages",
      "Extensions",
      "System",
      "Backup",
      "Design kit",
    ]) {
      // Two destinations kept their route and changed their LABEL, because both
      // became groups with siblings coming: "Devices" now reads "All devices"
      // (the legacy label, and the one that reads correctly beside the Discovery
      // and Roku entries joining it), and "Extensions" now reads "Installed"
      // under an Extensions group — a catalogue browse and a registry-source
      // list are both recorded ABSENT and will land beside it.
      const renamed: Record<string, string> = {
        Devices: "All devices",
        Extensions: "Installed",
      };
      const expected = renamed[had] ?? had;
      expect(labels).toContain(expected);
    }
    // ...plus the destination the flat rail never had at all (parity row 8.4).
    expect(labels).toContain("Widgets");
  });

  it("exposes each group as a real disclosure — button, aria-expanded, aria-controls", () => {
    // The semantics are the accessibility story: a <button> is reachable by Tab
    // and worked by Space/Enter with no key handling of our own, aria-expanded
    // is what a screen reader announces as collapsed/expanded, and
    // aria-controls is what ties the announcement to the region it opens.
    const { container } = renderShell();
    const sidebar = container.querySelector('[data-slot="shell-sidebar"]') as HTMLElement;
    const railNav = within(sidebar).getByRole("navigation", { name: /primary/i });
    expect(railNav.tagName).toBe("NAV");

    const toggles = within(railNav).getAllByRole("button");
    expect(toggles.map((b) => b.textContent?.trim())).toEqual(["Slidecast", "Devices", "Extensions", "Platform"]);
    for (const toggle of toggles) {
      expect(toggle).toHaveAttribute("type", "button");
      expect(toggle).toHaveAttribute("aria-expanded", "true");
      const controls = toggle.getAttribute("aria-controls");
      expect(controls).toBeTruthy();
      // The controlled region must actually exist, or the announcement points
      // at nothing.
      expect(railNav.querySelector(`#${controls}`)).not.toBeNull();
    }
  });

  it("collapses a group, and takes its destinations out of the accessibility tree", async () => {
    const user = userEvent.setup();
    const { container } = renderShell();
    const sidebar = container.querySelector('[data-slot="shell-sidebar"]') as HTMLElement;
    const slidecast = within(sidebar).getByRole("button", { name: "Slidecast" });

    expect(within(sidebar).getByRole("link", { name: "Casts" })).toBeInTheDocument();
    await user.click(slidecast);

    expect(slidecast).toHaveAttribute("aria-expanded", "false");
    // Not merely invisible: gone from the a11y tree, so a screen-reader user is
    // not offered six destinations a sighted user cannot see. (A Tailwind
    // display class would look identical here and fail this.)
    expect(within(sidebar).queryByRole("link", { name: "Casts" })).toBeNull();
    expect(within(sidebar).queryByRole("link", { name: "Widgets" })).toBeNull();
    // A sibling group is untouched.
    expect(within(sidebar).getByRole("link", { name: "Activity" })).toBeInTheDocument();

    await user.click(slidecast);
    expect(slidecast).toHaveAttribute("aria-expanded", "true");
    expect(within(sidebar).getByRole("link", { name: "Casts" })).toBeInTheDocument();
  });

  it("remembers which groups are collapsed across a remount", async () => {
    // Collapse SLIDECAST while standing on /design, which lives in Platform.
    // Collapsing the group that owns the current route would not test memory at
    // all — the reveal rule below re-opens it on the next mount, correctly.
    const user = userEvent.setup();
    const first = renderShell("/design");
    const sidebar1 = first.container.querySelector('[data-slot="shell-sidebar"]') as HTMLElement;
    await user.click(within(sidebar1).getByRole("button", { name: "Slidecast" }));
    expect(within(sidebar1).queryByRole("link", { name: "Casts" })).toBeNull();
    first.unmount();

    const second = renderShell("/design");
    const sidebar2 = second.container.querySelector('[data-slot="shell-sidebar"]') as HTMLElement;
    expect(within(sidebar2).getByRole("button", { name: "Slidecast" })).toHaveAttribute(
      "aria-expanded",
      "false",
    );
    expect(within(sidebar2).queryByRole("link", { name: "Casts" })).toBeNull();
    // Only the group that was collapsed — the memory records deviations, not a
    // whole snapshot, so an area nobody touched still ships open.
    expect(within(sidebar2).getByRole("button", { name: "Devices" })).toHaveAttribute(
      "aria-expanded",
      "true",
    );
  });

  it("survives a corrupt stored preference rather than taking the console with it", () => {
    window.localStorage.setItem("waiveo.nav.groups", "{not json");
    const { container } = renderShell();
    const sidebar = container.querySelector('[data-slot="shell-sidebar"]') as HTMLElement;
    expect(within(sidebar).getByRole("button", { name: "Slidecast" })).toHaveAttribute(
      "aria-expanded",
      "true",
    );
  });

  it("REVEALS the active route's group even when the operator had collapsed it", async () => {
    // The failure this prevents: an operator collapses Slidecast, later follows
    // a link to /casts, and the rail shows no trace of where they landed. The
    // reveal is a real state change, so the group stays open and its toggle goes
    // on working normally.
    const user = userEvent.setup();
    const first = renderShell("/design");
    const sidebar1 = first.container.querySelector('[data-slot="shell-sidebar"]') as HTMLElement;
    await user.click(within(sidebar1).getByRole("button", { name: "Slidecast" }));
    expect(within(sidebar1).queryByRole("link", { name: "Casts" })).toBeNull();
    first.unmount();

    // Arrive on a page INSIDE the collapsed group.
    const second = renderShell("/casts");
    const sidebar2 = second.container.querySelector('[data-slot="shell-sidebar"]') as HTMLElement;
    expect(within(sidebar2).getByRole("button", { name: "Slidecast" })).toHaveAttribute(
      "aria-expanded",
      "true",
    );
    expect(within(sidebar2).getByRole("link", { name: "Casts" })).toHaveAttribute(
      "aria-current",
      "page",
    );
  });

  it("keeps the drawer's tree in step with the rail's — one memory, not two", async () => {
    const user = userEvent.setup();
    renderShell();
    // Collapse in the rail...
    const sidebar = document.querySelector('[data-slot="shell-sidebar"]') as HTMLElement;
    await user.click(within(sidebar).getByRole("button", { name: "Devices" }));
    // ...and the drawer opens showing the same thing. Two independent copies of
    // this state would drift the moment a phone and a desktop shared a session.
    await user.click(screen.getByRole("button", { name: /open navigation menu/i }));
    const drawer = await screen.findByRole("dialog");
    expect(within(drawer).getByRole("button", { name: "Devices" })).toHaveAttribute(
      "aria-expanded",
      "false",
    );
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
