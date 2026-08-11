import { describe, expect, it, afterEach } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ThemeProvider } from "@/components/theme/theme-provider";
import { STORAGE_KEY, type Theme } from "@/components/theme/theme-context";
import DesignRoute from "./design-route";

// The gallery is the design system's living reference: it must render every
// widget-kit primitive, and it must do so faithfully in BOTH skies. This walk
// mounts the whole route under each theme (seeded via the persisted preference
// the ThemeProvider restores) and asserts each kit primitive is present.

const THEMES: Theme[] = ["dark", "light"];

function renderGallery(theme: Theme) {
  window.localStorage.setItem(STORAGE_KEY, theme);
  return render(
    <ThemeProvider>
      <DesignRoute />
    </ThemeProvider>,
  );
}

afterEach(() => {
  window.localStorage.clear();
});

describe.each(THEMES)("the design gallery under the %s theme", (theme) => {
  it("reflects the theme on the document element", () => {
    renderGallery(theme);
    expect(document.documentElement.getAttribute("data-theme")).toBe(theme);
  });

  it("renders the PageHeader hero (scrimmed, never raw gradient)", () => {
    const { container } = renderGallery(theme);
    const hero = container.querySelector('[data-slot="page-header"][data-variant="hero"]');
    expect(hero).not.toBeNull();
    expect(hero!.className).toContain("wv-hero");
    expect(
      screen.getByRole("heading", { level: 1, name: "Waiveo design kit" }),
    ).toBeInTheDocument();
  });

  it("visualizes both palettes and the full gradient set", () => {
    const { container } = renderGallery(theme);
    expect(screen.getByTestId("palette-dusk")).toBeInTheDocument();
    expect(screen.getByTestId("palette-daybreak")).toBeInTheDocument();
    // gradHero + gradAccent + the four daypart decks.
    expect(container.querySelectorAll('[data-testid="gradient-band"]').length).toBe(6);
  });

  it("renders the type specimen across all three families", () => {
    renderGallery(theme);
    expect(screen.getAllByText(/Bricolage Grotesque/).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/JetBrains Mono/).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/Inter/).length).toBeGreaterThan(0);
  });

  it("renders StatCards", () => {
    const { container } = renderGallery(theme);
    expect(container.querySelectorAll('[data-slot="stat-card"]').length).toBeGreaterThanOrEqual(4);
    expect(screen.getByText("Screens on air")).toBeInTheDocument();
  });

  it("renders every StatusBadge state", () => {
    const { container } = renderGallery(theme);
    // Five specimen states in the status section (more appear inside the tables).
    expect(container.querySelectorAll('[data-slot="status-badge"]').length).toBeGreaterThanOrEqual(5);
    for (const status of ["ok", "warn", "error", "off", "pending"]) {
      expect(container.querySelector(`[data-slot="status-badge"][data-status="${status}"]`)).not.toBeNull();
    }
  });

  it("renders the DataTables with their required accessible names", () => {
    const { container } = renderGallery(theme);
    expect(screen.getByRole("table", { name: "Screens" })).toBeInTheDocument();
    expect(screen.getByRole("table", { name: "Automations" })).toBeInTheDocument();
    expect(container.querySelectorAll('[data-slot="data-table"]').length).toBeGreaterThanOrEqual(2);
    // A sortable header is a real button carrying aria-sort on its column.
    const screensTable = screen.getByRole("table", { name: "Screens" });
    expect(within(screensTable).getByRole("button", { name: "Screen" })).toBeInTheDocument();
  });

  it("renders the FormField idiom with a labelled control", () => {
    const { container } = renderGallery(theme);
    expect(container.querySelectorAll('[data-slot="form-field"]').length).toBeGreaterThanOrEqual(4);
    expect(screen.getByLabelText(/screen name/i)).toBeInTheDocument();
    // The error field surfaces its message via role=alert.
    expect(screen.getByRole("alert")).toHaveTextContent("Pick a daypart to light up.");
  });

  it("renders the EmptyState idiom", () => {
    const { container } = renderGallery(theme);
    expect(container.querySelector('[data-slot="empty-state"]')).not.toBeNull();
    expect(screen.getByText("Nothing scheduled for this daypart yet")).toBeInTheDocument();
  });

  it("mounts the Toaster notifications region", () => {
    renderGallery(theme);
    expect(screen.getByRole("region", { name: /notifications/i })).toBeInTheDocument();
  });

  it("renders a labelled KitIcon", () => {
    renderGallery(theme);
    expect(screen.getByRole("img", { name: "Waiveo" })).toBeInTheDocument();
  });

  it("opens the Modal from its trigger and traps focus", async () => {
    const user = userEvent.setup();
    const { container } = renderGallery(theme);
    const overlays = container.querySelector("#overlays") as HTMLElement;
    await user.click(within(overlays).getByRole("button", { name: "Publish schedule" }));
    const dialog = await screen.findByRole("dialog", { name: "Publish schedule" });
    expect(dialog).toBeInTheDocument();
    expect(dialog.contains(document.activeElement)).toBe(true);
  });

  it("gives the per-row action a >=44px touch target (wv-touch)", () => {
    // Regression: the gallery is the living reference every console/extension page
    // renders through, so its own per-row action button must honour the >=44px
    // touch-target contract. A bare `size-8` (32px) button fails it; the guard is
    // the `wv-touch` class (min-width/height 44px), asserted here since jsdom does
    // not compute box sizes.
    renderGallery(theme);
    const action = screen.getByRole("button", { name: "Actions for The Hangar TV" });
    expect(action.className).toContain("wv-touch");
  });

  // The gallery is the reference an author copies from, so a specimen that does
  // nothing is worse here than anywhere else: it teaches the affordance and then
  // proves it is decorative. Each of the table capabilities it advertises is
  // therefore DRIVEN, not merely found in the DOM.
  it("drives the gallery table's search, filter, paging and bulk selection", async () => {
    const user = userEvent.setup();
    const { container } = renderGallery(theme);
    // Scoped to the data section: the Forms section below has its own "Daypart"
    // field, and an unscoped label query would resolve to the wrong control.
    const data = within(container.querySelector("#data") as HTMLElement);

    // Paging: 5 fixture screens at 3/page.
    const range = container.querySelector('[data-slot="table-range"]')!;
    expect(range).toHaveTextContent("Showing 1–3 of 5 rows");
    expect(data.queryByText("Drive-Thru Menu")).not.toBeInTheDocument();
    await user.click(data.getByRole("button", { name: "Next" }));
    expect(data.getByText("Drive-Thru Menu")).toBeInTheDocument();

    // Filter: the options are the dayparts the FIXTURES carry.
    await user.selectOptions(data.getByLabelText("Daypart"), "Sunrise");
    expect(data.getByText("Lobby Wall")).toBeInTheDocument();
    expect(data.queryByText("Cafe Board")).not.toBeInTheDocument();
    await user.selectOptions(data.getByLabelText("Daypart"), "");

    // Search.
    await user.type(data.getByLabelText("Search screens"), "Patio");
    await waitFor(() => expect(data.queryByText("Lobby Wall")).not.toBeInTheDocument());
    expect(data.getByText("Patio Screen")).toBeInTheDocument();

    // Selection + the bulk-action slot, which must reach the caller's handler.
    await user.click(data.getByRole("checkbox", { name: "Select Patio Screen" }));
    await user.click(data.getByRole("button", { name: "Publish to selected" }));
    expect(await screen.findByText("Published to 1 screen")).toBeInTheDocument();
  });

  it("renders the kit Checkbox as its own specimen, accessibly named", () => {
    renderGallery(theme);
    expect(
      screen.getByRole("checkbox", { name: "Restart the player if it stops reporting" }),
    ).toBeInTheDocument();
  });

  it("highlights a nav item as the current section", async () => {
    const user = userEvent.setup();
    renderGallery(theme);
    const nav = screen.getByRole("navigation", { name: /design gallery sections/i });
    const colorLink = within(nav).getByRole("link", { name: /color/i });
    await user.click(colorLink);
    expect(colorLink).toHaveAttribute("aria-current", "page");
  });
});
