import { describe, expect, it, afterEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ThemeProvider } from "@/components/theme/theme-provider";
import PagesRoute from "./pages-route";

// The /pages demo renders the four valid ui-schema/1 corpus documents — one per
// page type — through the REAL renderer, with a nav entry per page type to
// switch between them. It proves the uniformity promise executable end to end:
// a pack-declared document, validated then painted through the one Horizon kit.

afterEach(() => window.localStorage.clear());

function renderRoute() {
  return render(
    <ThemeProvider>
      <PagesRoute />
    </ThemeProvider>,
  );
}

describe("PagesRoute — the /pages demo inside the shell", () => {
  it("offers a nav entry for each of the four page types", () => {
    renderRoute();
    const nav = screen.getByRole("navigation", { name: /page-type demos/i });
    for (const label of ["List & detail", "Settings form", "Dashboard", "Wizard"]) {
      expect(within(nav).getByRole("button", { name: label })).toBeInTheDocument();
    }
  });

  it("renders the list-detail corpus document through the renderer by default", () => {
    renderRoute();
    // The list-detail document's presets list paints as a real table (through the
    // kit DataTable) — proof it went through validate → render, not a mock.
    const table = screen.getByRole("table", { name: "presets" });
    expect(within(table).getByText("Morning open")).toBeInTheDocument();
    // Its nav entry is marked current.
    const nav = screen.getByRole("navigation", { name: /page-type demos/i });
    expect(within(nav).getByRole("button", { name: "List & detail" })).toHaveAttribute(
      "aria-current",
      "page",
    );
  });

  it("switches page-type demos from the nav and repaints through the renderer", async () => {
    const user = userEvent.setup();
    renderRoute();

    // Dashboard → the stat-tile document paints its computed labels.
    await user.click(screen.getByRole("button", { name: "Dashboard" }));
    expect(screen.getByText("Online screens")).toBeInTheDocument();
    expect(screen.queryByRole("table", { name: "presets" })).not.toBeInTheDocument();

    // Wizard → the wizard document paints its stepper (Next affordance present).
    await user.click(screen.getByRole("button", { name: "Wizard" }));
    expect(screen.getByRole("button", { name: "Next" })).toBeInTheDocument();

    // Settings form → the settings document paints its section heading.
    await user.click(screen.getByRole("button", { name: "Settings form" }));
    expect(screen.getByRole("heading", { name: "General" })).toBeInTheDocument();
  });

  it("marks the active page-type demo with aria-current=page", async () => {
    const user = userEvent.setup();
    renderRoute();
    const nav = screen.getByRole("navigation", { name: /page-type demos/i });
    const dashboard = within(nav).getByRole("button", { name: "Dashboard" });
    expect(dashboard).not.toHaveAttribute("aria-current");
    await user.click(dashboard);
    expect(dashboard).toHaveAttribute("aria-current", "page");
    // Switching moves the marker off the previously-active entry.
    expect(within(nav).getByRole("button", { name: "List & detail" })).not.toHaveAttribute(
      "aria-current",
    );
  });
});
