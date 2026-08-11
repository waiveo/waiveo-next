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

  it("does NOT answer to the loaded table's accessible name while it is still loading", () => {
    // A skeleton renders the same <Table> with the same header — only the body
    // swaps — so a busy table that kept the loaded name made
    // `findByRole("table", {name})` resolve on the SKELETON. Every synchronous
    // assertion after such an await then races the fetch, which is an await
    // waiting for the wrong thing rather than a slow machine. That shape flaked
    // live-screens-panel.test.tsx about one run in four, and 18 tests in that
    // one file are written that way.
    const { rerender } = render(
      <DataTable columns={columns} data={[]} label="Screens" loading skeletonRows={3} />,
    );
    expect(screen.queryByRole("table", { name: "Screens" })).not.toBeInTheDocument();
    const busy = screen.getByRole("table", { name: "Screens (loading)" });
    expect(busy).toHaveAttribute("aria-busy", "true");

    // ...and it takes the name the moment the data is really there.
    rerender(<DataTable columns={columns} data={rows} label="Screens" />);
    const loaded = screen.getByRole("table", { name: "Screens" });
    expect(loaded).not.toHaveAttribute("aria-busy", "true");
    expect(within(loaded).getByText("The Hangar TV")).toBeInTheDocument();
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

  // A row action is nested inside the pressable row, so the row's own onClick is
  // an ancestor of the button's. Without a guard in the actions cell, pressing
  // "Adopt" also presses the ROW — an invisible state change the operator never
  // asked for (the Devices page silently re-filtered its entities table).
  it("does not press the row when a row ACTION is clicked", async () => {
    const user = userEvent.setup();
    const pressed: string[] = [];
    const acted: string[] = [];
    render(
      <DataTable
        columns={columns}
        data={rows}
        label="Screens"
        onRowPress={(row) => pressed.push(row.name)}
        rowActions={(row) => (
          <button type="button" onClick={() => acted.push(row.name)}>{`Adopt ${row.name}`}</button>
        )}
      />,
    );

    await user.click(screen.getByRole("button", { name: "Adopt Cafe Board" }));
    expect(acted).toEqual(["Cafe Board"]);
    expect(pressed).toEqual([]);

    // …and the row itself is still pressable, so the guard is scoped to the
    // actions cell rather than disabling row press outright.
    await user.click(screen.getByText("Cafe Board"));
    expect(pressed).toEqual(["Cafe Board"]);
  });

  it("does not press the row when a row action is activated from the KEYBOARD", async () => {
    const user = userEvent.setup();
    const pressed: string[] = [];
    const acted: string[] = [];
    render(
      <DataTable
        columns={columns}
        data={rows}
        label="Screens"
        onRowPress={(row) => pressed.push(row.name)}
        rowActions={(row) => (
          <button type="button" onClick={() => acted.push(row.name)}>{`Adopt ${row.name}`}</button>
        )}
      />,
    );

    screen.getByRole("button", { name: "Adopt Cafe Board" }).focus();
    await user.keyboard("{Enter}");
    expect(acted).toEqual(["Cafe Board"]);
    expect(pressed).toEqual([]);
  });

  it("sets numeric cells in the mono tabular face", () => {
    render(<DataTable columns={columns} data={rows} label="Screens" />);
    const cell = screen.getByText("3").closest("td")!;
    expect(cell.className).toContain("font-mono");
    expect(cell.className).toContain("wv-tnum");
  });
});
