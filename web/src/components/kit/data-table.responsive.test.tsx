import { describe, it, expect, afterEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import type { ColumnDef } from "@tanstack/react-table";
import { DataTable } from "./data-table";

// Responsive contract (Matt): a DataTable must never make the PAGE scroll
// horizontally at 360px. Below the narrow breakpoint the kit switches the table
// to a stacked/card presentation (label + value per cell), so there is no wide
// grid to overflow; at/above it, the ordinary table renders (and, between the
// two, it scrolls INSIDE its own region). The switch is driven by matchMedia,
// mocked here to prove the behaviour flips.

type Screen = { id: string; name: string; devices: number };
const rows: Screen[] = [
  { id: "01JQZ0R0000000000000000001", name: "The Hangar TV", devices: 3 },
  { id: "01JQZ0R0000000000000000002", name: "Cafe Board", devices: 1 },
];
const columns: ColumnDef<Screen>[] = [
  { accessorKey: "name", header: "Screen" },
  { accessorKey: "devices", header: "Devices", meta: { numeric: true } },
];

function setViewport(narrow: boolean) {
  window.matchMedia = ((query: string) =>
    ({
      // "(max-width: 640px)" (and any max-width probe) matches on a narrow phone.
      matches: /max-width/.test(query) ? narrow : !narrow,
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
  setViewport(false);
});

describe("DataTable — narrow (stacked/card) presentation", () => {
  it("renders an ordinary table on a wide viewport", () => {
    setViewport(false);
    render(<DataTable columns={columns} data={rows} label="Screens" />);
    expect(screen.getByRole("table", { name: "Screens" })).toBeInTheDocument();
    expect(
      document.querySelector('[data-slot="data-table"][data-layout="stacked"]'),
    ).toBeNull();
  });

  it("switches to stacked cards on a narrow viewport (no wide table to overflow)", () => {
    setViewport(true);
    render(<DataTable columns={columns} data={rows} label="Screens" />);

    // No <table> element at all → nothing forces the page to scroll sideways.
    expect(screen.queryByRole("table")).not.toBeInTheDocument();

    const region = document.querySelector('[data-slot="data-table"][data-layout="stacked"]');
    expect(region).not.toBeNull();
    // The region keeps the accessible name the `label` prop supplies.
    expect(region).toHaveAttribute("aria-label", "Screens");

    // One card per row, each pairing the column header with the cell value.
    const cards = document.querySelectorAll('[data-slot="data-card"]');
    expect(cards.length).toBe(rows.length);

    const firstCard = cards[0] as HTMLElement;
    expect(within(firstCard).getByText("The Hangar TV")).toBeInTheDocument();
    expect(within(firstCard).getAllByText("Screen").length).toBeGreaterThan(0);
    expect(within(firstCard).getAllByText("Devices").length).toBeGreaterThan(0);
  });

  it("keeps numeric values in the mono tabular face inside a card", () => {
    setViewport(true);
    render(<DataTable columns={columns} data={rows} label="Screens" />);
    const value = screen.getByText("3");
    expect(value.className).toContain("font-mono");
    expect(value.className).toContain("wv-tnum");
  });

  it("renders per-row actions inside each card", () => {
    setViewport(true);
    render(
      <DataTable
        columns={columns}
        data={rows}
        label="Screens"
        rowActions={(row) => <button type="button">{`Edit ${row.name}`}</button>}
      />,
    );
    expect(screen.getByRole("button", { name: "Edit The Hangar TV" })).toBeInTheDocument();
  });

  it("shows the empty state (not a table) when narrow with no rows", () => {
    setViewport(true);
    render(<DataTable columns={columns} data={[]} label="Screens" />);
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
    expect(screen.getByText("Nothing here yet")).toBeInTheDocument();
  });
});
