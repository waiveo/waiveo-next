import { describe, expect, it, afterEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ThemeProvider } from "@/components/theme/theme-provider";
import PagesRoute from "./pages-route";

// The /pages demo renders the four valid ui-schema/1 corpus documents — one per
// page type — through the REAL renderer, with a TAB per page type to switch
// between them. It proves the uniformity promise executable end to end: a
// pack-declared document, validated then painted through the one Horizon kit.
//
// The switcher used to be a row of buttons wearing `aria-current="page"`, which
// is a claim about navigation on a page whose URL never changes. These tests now
// assert the tab CONTRACT instead — roles, selected state, the panel each tab
// controls, and roving arrow-key focus — because that contract is the whole
// reason the button row was replaced.

afterEach(() => window.localStorage.clear());

function renderRoute() {
  return render(
    <ThemeProvider>
      <PagesRoute />
    </ThemeProvider>,
  );
}

const TAB_LABELS = ["List & detail", "Settings form", "Dashboard", "Wizard"];

describe("PagesRoute — the /pages demo inside the shell", () => {
  it("offers a named tablist with one tab per page type", () => {
    renderRoute();
    const tablist = screen.getByRole("tablist", { name: /page-type demos/i });
    for (const label of TAB_LABELS) {
      expect(within(tablist).getByRole("tab", { name: label })).toBeInTheDocument();
    }
    expect(within(tablist).getAllByRole("tab")).toHaveLength(4);
  });

  it("renders the list-detail corpus document through the renderer by default", () => {
    renderRoute();
    // The list-detail document's presets list paints as a real table (through the
    // kit DataTable) — proof it went through validate → render, not a mock.
    const table = screen.getByRole("table", { name: "presets" });
    expect(within(table).getByText("Morning open")).toBeInTheDocument();
    // Its tab is the selected one, and it is the only selected one.
    const tabs = screen.getAllByRole("tab");
    expect(screen.getByRole("tab", { name: "List & detail" })).toHaveAttribute(
      "aria-selected",
      "true",
    );
    expect(tabs.filter((t) => t.getAttribute("aria-selected") === "true")).toHaveLength(1);
  });

  it("switches page-type demos from the tabs and repaints through the renderer", async () => {
    const user = userEvent.setup();
    renderRoute();

    // Dashboard → the stat-tile document paints its computed labels.
    await user.click(screen.getByRole("tab", { name: "Dashboard" }));
    expect(screen.getByText("Online screens")).toBeInTheDocument();
    expect(screen.queryByRole("table", { name: "presets" })).not.toBeInTheDocument();

    // Wizard → the wizard document paints its stepper (Next affordance present).
    await user.click(screen.getByRole("tab", { name: "Wizard" }));
    expect(screen.getByRole("button", { name: "Next" })).toBeInTheDocument();

    // Settings form → the settings document paints its section heading.
    await user.click(screen.getByRole("tab", { name: "Settings form" }));
    expect(screen.getByRole("heading", { name: "General" })).toBeInTheDocument();
  });

  it("moves the selected state, and shows exactly one panel at a time", async () => {
    const user = userEvent.setup();
    renderRoute();
    const dashboard = screen.getByRole("tab", { name: "Dashboard" });
    expect(dashboard).toHaveAttribute("aria-selected", "false");

    await user.click(dashboard);
    expect(dashboard).toHaveAttribute("aria-selected", "true");
    expect(screen.getByRole("tab", { name: "List & detail" })).toHaveAttribute(
      "aria-selected",
      "false",
    );
    // One panel is showing, and it is the one this tab controls.
    const panels = screen.getAllByRole("tabpanel");
    expect(panels).toHaveLength(1);
    expect(dashboard).toHaveAttribute("aria-controls", panels[0].id);
  });

  it("is ONE tab stop: the arrow keys move between tabs, Tab does not", async () => {
    // The substantive win over the button row it replaced. On four buttons a
    // keyboard user pressed Tab four times to reach the last option and Tab
    // again to leave; on a tablist the strip is one stop and Left/Right/Home/End
    // move inside it.
    const user = userEvent.setup();
    renderRoute();

    const [first, second, , last] = screen.getAllByRole("tab");
    first.focus();
    expect(first).toHaveFocus();

    // Inside the strip, the arrow keys move — and moving also selects, so one
    // keypress both moves and switches the panel.
    await user.keyboard("{ArrowRight}");
    expect(second).toHaveFocus();
    expect(second).toHaveAttribute("aria-selected", "true");

    await user.keyboard("{End}");
    expect(last).toHaveFocus();

    await user.keyboard("{Home}");
    expect(first).toHaveFocus();

    // …and Tab LEAVES the strip rather than stepping to the next option, which
    // is the whole difference from the four separate buttons this replaced.
    await user.tab();
    expect(screen.getAllByRole("tab").some((t) => t === document.activeElement)).toBe(false);
  });
});
