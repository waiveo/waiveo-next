import { describe, expect, it } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ColumnDef } from "@tanstack/react-table";
import { DataTable } from "./data-table";

// Fixture rows — fixture ULIDs, no real data.
type Screen = { id: string; name: string; devices: number };
const rows: Screen[] = [
  { id: "01JQZ0R0000000000000000001", name: "The Hangar TV", devices: 3 },
  { id: "01JQZ0R0000000000000000002", name: "Cafe Board", devices: 1 },
];
const columns: ColumnDef<Screen>[] = [
  { accessorKey: "name", header: "Screen" },
  { accessorKey: "devices", header: "Devices", meta: { numeric: true } },
];

describe("DataTable", () => {
  it("renders rows under a table with the required accessible name", () => {
    render(<DataTable columns={columns} data={rows} label="Screens" />);
    expect(screen.getByRole("table", { name: "Screens" })).toBeInTheDocument();
    expect(screen.getByText("The Hangar TV")).toBeInTheDocument();
    expect(screen.getByText("Cafe Board")).toBeInTheDocument();
  });

  it("sorts via a header button and reflects direction in aria-sort", async () => {
    const user = userEvent.setup();
    render(<DataTable columns={columns} data={rows} label="Screens" />);

    const screenHeader = screen.getByRole("columnheader", { name: /screen/i });
    expect(screenHeader).toHaveAttribute("aria-sort", "none");

    await user.click(screen.getByRole("button", { name: /screen/i }));
    expect(screenHeader).toHaveAttribute("aria-sort", "ascending");

    // Ascending by name → "Cafe Board" is now the first data row.
    const bodyRows = screen.getAllByRole("row").slice(1);
    expect(within(bodyRows[0]).getByText("Cafe Board")).toBeInTheDocument();
  });

  it("shows a loading skeleton and no data while loading", () => {
    const { container } = render(
      <DataTable columns={columns} data={[]} label="Screens" loading skeletonRows={3} />,
    );
    expect(container.querySelectorAll('[data-slot="skeleton"]').length).toBeGreaterThan(0);
    expect(screen.queryByText("The Hangar TV")).not.toBeInTheDocument();
  });

  it("renders the empty state when there are no rows", () => {
    render(<DataTable columns={columns} data={[]} label="Screens" />);
    expect(screen.getByText("Nothing here yet")).toBeInTheDocument();
  });

  it("renders per-row actions with an Actions column", () => {
    render(
      <DataTable
        columns={columns}
        data={rows}
        label="Screens"
        rowActions={(row) => <button type="button">{`Edit ${row.name}`}</button>}
      />,
    );
    expect(screen.getByText("Actions")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Edit The Hangar TV" })).toBeInTheDocument();
  });

  it("sets numeric cells in the mono tabular face", () => {
    render(<DataTable columns={columns} data={rows} label="Screens" />);
    const cell = screen.getByText("3").closest("td")!;
    expect(cell.className).toContain("font-mono");
    expect(cell.className).toContain("wv-tnum");
  });
});
