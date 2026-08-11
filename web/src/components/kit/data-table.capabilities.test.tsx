import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import type { ColumnDef } from "@tanstack/react-table";
import { DataTable } from "./data-table";
// The component's own source, for the theme-token guard at the bottom of this
// file (Vite's `?raw` — jsdom cannot evaluate Tailwind, so the only place the
// mistake is visible is the text).
import dataTableSource from "./data-table.tsx?raw";

/**
 * The find/narrow/page/select contract, DRIVEN rather than rendered.
 *
 * Every case here types into a box, clicks a control, or ticks a checkbox and
 * then asserts what CHANGED. This console has twice shipped a control that
 * passed an "it renders" test and did nothing when pressed, so a capability with
 * only a presence assertion is treated here as untested.
 */

type Screen = { id: string; name: string; site: string; status: string; devices: number };

function fixture(n: number): Screen[] {
  return Array.from({ length: n }, (_, i) => ({
    id: `01JQZ0R00000000000000000${String(i).padStart(2, "0")}`,
    name: `Screen ${String(i).padStart(2, "0")}`,
    site: i % 2 === 0 ? "Hangar" : "Cafe",
    status: i % 3 === 0 ? "live" : "stale",
    devices: i,
  }));
}

const rows: Screen[] = [
  { id: "01JQZ0R0000000000000000001", name: "The Hangar TV", site: "Hangar", status: "live", devices: 3 },
  { id: "01JQZ0R0000000000000000002", name: "Cafe Board", site: "Cafe", status: "stale", devices: 1 },
  { id: "01JQZ0R0000000000000000003", name: "Lobby Panel", site: "Cafe", status: "live", devices: 2 },
];

const columns: ColumnDef<Screen>[] = [
  { accessorKey: "name", header: "Screen", meta: { searchable: true } },
  { accessorKey: "site", header: "Site" },
  { accessorKey: "status", header: "Status", meta: { filter: "enum" } },
  { accessorKey: "devices", header: "Devices", meta: { numeric: true } },
];

/** Names of the rows currently rendered in the table body. */
function visibleNames(): string[] {
  const body = screen.getAllByRole("row").slice(1);
  return body
    .map((r) => within(r).queryAllByRole("cell")[0]?.textContent ?? "")
    .filter((t) => t.startsWith("Screen") || t.startsWith("The ") || t.startsWith("Cafe") || t.startsWith("Lobby"));
}

afterEach(() => {
  vi.useRealTimers();
});

// ── Nothing opted in ────────────────────────────────────────────────────────

describe("DataTable — the capabilities are opt-in", () => {
  it("renders no toolbar, no pagination and no checkboxes by default", () => {
    // `columns` DOES declare a filterable column, and that must still not put a
    // toolbar over a table whose call site never asked for one: the same column
    // array is shared between a long list and a short one (the cast library and
    // its templates list), and the short one must be able to decline.
    render(<DataTable columns={columns} data={rows} label="Screens" />);
    expect(document.querySelector('[data-slot="table-toolbar"]')).toBeNull();
    expect(document.querySelector('[data-slot="table-pagination"]')).toBeNull();
    expect(document.querySelector('[data-slot="data-table-shell"]')).toBeNull();
    expect(screen.queryAllByRole("checkbox")).toHaveLength(0);
    expect(screen.queryByRole("searchbox")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("Status")).not.toBeInTheDocument();
  });
});

// ── Search ──────────────────────────────────────────────────────────────────

describe("DataTable — search", () => {
  it("filters the rows to what was TYPED", async () => {
    const user = userEvent.setup();
    render(
      <DataTable columns={columns} data={rows} label="Screens" search={{ label: "Search screens" }} />,
    );

    await user.type(screen.getByLabelText("Search screens"), "cafe");

    await waitFor(() => expect(screen.queryByText("The Hangar TV")).not.toBeInTheDocument());
    expect(screen.getByText("Cafe Board")).toBeInTheDocument();
    expect(screen.queryByText("Lobby Panel")).not.toBeInTheDocument();
  });

  it("announces the match count in a live region and restores it when cleared", async () => {
    const user = userEvent.setup();
    render(
      <DataTable columns={columns} data={rows} label="Screens" search={{ label: "Search screens" }} />,
    );
    const count = document.querySelector('[data-slot="table-match-count"]')!;
    expect(count).toHaveAttribute("aria-live", "polite");
    // Anchored: "3 rows" is a substring of "1 of 3 rows match", so a loose match
    // would pass in both the filtered and unfiltered states and prove nothing.
    expect(count).toHaveTextContent(/^3 rows$/);

    const box = screen.getByLabelText("Search screens");
    await user.type(box, "cafe");
    await waitFor(() => expect(count).toHaveTextContent(/^1 of 3 rows match$/));

    await user.clear(box);
    await waitFor(() => expect(count).toHaveTextContent(/^3 rows$/));
    expect(screen.getByText("The Hangar TV")).toBeInTheDocument();
  });

  it("DEBOUNCES: the filter does not apply until the delay has elapsed", () => {
    // Fake timers plus a raw change event rather than userEvent: this case is
    // about WHEN the filter lands, so the clock has to be the test's, not the
    // machine's, and userEvent's own keystroke waits would blur the boundary.
    vi.useFakeTimers();
    render(
      <DataTable
        columns={columns}
        data={rows}
        label="Screens"
        search={{ label: "Search screens", debounceMs: 300 }}
      />,
    );

    const box = screen.getByLabelText("Search screens");
    fireEvent.change(box, { target: { value: "cafe" } });
    expect(box).toHaveValue("cafe"); // the box echoes immediately…

    act(() => {
      vi.advanceTimersByTime(299);
    });
    expect(screen.getByText("The Hangar TV")).toBeInTheDocument(); // …the table has not

    act(() => {
      vi.advanceTimersByTime(1);
    });
    expect(screen.queryByText("The Hangar TV")).not.toBeInTheDocument();
  });

  it("searches only the columns the DEFS declare searchable", async () => {
    const user = userEvent.setup();
    // `name` is the only column carrying meta.searchable. "Cafe" is the
    // discriminator: it is in ONE name (Cafe Board) and TWO sites (Cafe Board's
    // and Lobby Panel's). If the declaration were ignored and every column were
    // searched, Lobby Panel would match on its SITE and stay on screen.
    render(
      <DataTable columns={columns} data={rows} label="Screens" search={{ label: "Search screens" }} />,
    );

    await user.type(screen.getByLabelText("Search screens"), "Cafe");
    await waitFor(() => expect(screen.queryByText("The Hangar TV")).not.toBeInTheDocument());
    expect(screen.getByText("Cafe Board")).toBeInTheDocument();
    expect(screen.queryByText("Lobby Panel")).not.toBeInTheDocument();
  });

  it("searches EVERY valued column when no column declares itself searchable", async () => {
    const user = userEvent.setup();
    const undeclared: ColumnDef<Screen>[] = [
      { accessorKey: "name", header: "Screen" },
      { accessorKey: "site", header: "Site" },
    ];
    render(
      <DataTable columns={undeclared} data={rows} label="Screens" search={{ label: "Search screens" }} />,
    );

    // "Hangar" is a SITE here, and the site column is not marked — with nothing
    // marked, every valued column is searched, so the row matches.
    await user.type(screen.getByLabelText("Search screens"), "Hangar");
    await waitFor(() => expect(screen.queryByText("Cafe Board")).not.toBeInTheDocument());
    expect(screen.getByText("The Hangar TV")).toBeInTheDocument();
  });
});

// ── Per-column filtering ────────────────────────────────────────────────────

describe("DataTable — per-column filtering", () => {
  it("offers the column's OWN distinct values, with counts, and narrows on choose", async () => {
    const user = userEvent.setup();
    render(<DataTable columns={columns} data={rows} label="Screens" filters />);

    const select = screen.getByLabelText("Status");
    expect(within(select).getByRole("option", { name: "live (2)" })).toBeInTheDocument();
    expect(within(select).getByRole("option", { name: "stale (1)" })).toBeInTheDocument();

    await user.selectOptions(select, "stale");
    expect(screen.getByText("Cafe Board")).toBeInTheDocument();
    expect(screen.queryByText("The Hangar TV")).not.toBeInTheDocument();

    await user.selectOptions(select, "");
    expect(screen.getByText("The Hangar TV")).toBeInTheDocument();
  });

  it("DERIVES the option set from the data — a new value needs no list edited", () => {
    const extra: Screen[] = [
      ...rows,
      { id: "01JQZ0R0000000000000000004", name: "Yard Sign", site: "Yard", status: "rejected", devices: 0 },
    ];
    render(<DataTable columns={columns} data={extra} label="Screens" filters />);
    const select = screen.getByLabelText("Status");
    expect(within(select).getByRole("option", { name: "rejected (1)" })).toBeInTheDocument();
  });

  it("keeps the filter controls REACHABLE when the filter matches nothing", async () => {
    // Legacy rendered All Devices' status filter inside its zero-results card, so
    // the control disappeared the moment it matched something. The toolbar here
    // is a sibling of the table, never a child of its empty state.
    const user = userEvent.setup();
    render(
      <DataTable columns={columns} data={rows} label="Screens" search={{ label: "Search screens" }} filters />,
    );

    await user.type(screen.getByLabelText("Search screens"), "zzzz");
    await waitFor(() => expect(screen.getByText("Nothing matched")).toBeInTheDocument());
    expect(screen.getByLabelText("Search screens")).toBeInTheDocument();
    expect(screen.getByLabelText("Status")).toBeInTheDocument();

    // …and the way back out is operable from there.
    await user.click(screen.getByRole("button", { name: /clear the screens filters/i }));
    await waitFor(() => expect(screen.getByText("The Hangar TV")).toBeInTheDocument());
  });
});

// ── Pagination ──────────────────────────────────────────────────────────────

describe("DataTable — pagination", () => {
  it("shows one page at a time and pages FORWARD on click", async () => {
    const user = userEvent.setup();
    render(
      <DataTable
        columns={columns}
        data={fixture(30)}
        label="Screens"
        pagination={{ pageSize: 10 }}
      />,
    );

    expect(visibleNames()).toHaveLength(10);
    expect(screen.getByText("Screen 00")).toBeInTheDocument();
    expect(screen.queryByText("Screen 10")).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Next" }));
    expect(screen.getByText("Screen 10")).toBeInTheDocument();
    expect(screen.queryByText("Screen 00")).not.toBeInTheDocument();
  });

  it("announces the visible range in a live region", async () => {
    const user = userEvent.setup();
    render(
      <DataTable columns={columns} data={fixture(30)} label="Screens" pagination={{ pageSize: 10 }} />,
    );
    const range = document.querySelector('[data-slot="table-range"]')!;
    expect(range).toHaveAttribute("aria-live", "polite");
    expect(range).toHaveTextContent("Showing 1–10 of 30 rows");

    await user.click(screen.getByRole("button", { name: "Next" }));
    expect(range).toHaveTextContent("Showing 11–20 of 30 rows");
  });

  it("disables Previous on the first page and Next on the last", async () => {
    const user = userEvent.setup();
    render(
      <DataTable columns={columns} data={fixture(12)} label="Screens" pagination={{ pageSize: 10 }} />,
    );
    expect(screen.getByRole("button", { name: "Previous" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Next" })).toBeEnabled();

    await user.click(screen.getByRole("button", { name: "Next" }));
    expect(screen.getByRole("button", { name: "Next" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Previous" })).toBeEnabled();
  });

  it("re-pages when the rows-per-page control is CHANGED", async () => {
    const user = userEvent.setup();
    render(
      <DataTable
        columns={columns}
        data={fixture(30)}
        label="Screens"
        pagination={{ pageSize: 10, pageSizeOptions: [10, 25] }}
      />,
    );
    expect(visibleNames()).toHaveLength(10);

    await user.selectOptions(screen.getByLabelText("Rows per page"), "25");
    expect(visibleNames()).toHaveLength(25);
    expect(document.querySelector('[data-slot="table-range"]')).toHaveTextContent(
      "Showing 1–25 of 30 rows",
    );
  });

  it("CLAMPS the page index when the data shrinks under it", async () => {
    const user = userEvent.setup();
    const { rerender } = render(
      <DataTable columns={columns} data={fixture(30)} label="Screens" pagination={{ pageSize: 10 }} />,
    );
    await user.click(screen.getByRole("button", { name: "Next" }));
    await user.click(screen.getByRole("button", { name: "Next" }));
    expect(document.querySelector('[data-slot="table-range"]')).toHaveTextContent("Showing 21–30");

    // A re-poll returns far fewer rows: page 3 no longer exists.
    rerender(
      <DataTable columns={columns} data={fixture(12)} label="Screens" pagination={{ pageSize: 10 }} />,
    );
    await waitFor(() =>
      expect(document.querySelector('[data-slot="table-range"]')).toHaveTextContent(
        "Showing 11–12 of 12 rows",
      ),
    );
    expect(screen.getByText("Screen 10")).toBeInTheDocument();
  });

  it("does NOT reset the page when the data is merely re-polled (same size)", async () => {
    const user = userEvent.setup();
    const { rerender } = render(
      <DataTable columns={columns} data={fixture(30)} label="Screens" pagination={{ pageSize: 10 }} />,
    );
    await user.click(screen.getByRole("button", { name: "Next" }));
    expect(document.querySelector('[data-slot="table-range"]')).toHaveTextContent("Showing 11–20");

    // A fresh array with the same content — what a 10s poll produces. An operator
    // reading page 2 must not be thrown back to page 1 every refresh.
    rerender(
      <DataTable columns={columns} data={fixture(30)} label="Screens" pagination={{ pageSize: 10 }} />,
    );
    // The table's auto-reset (if it were on) fires from a QUEUED callback, not
    // synchronously with the render — so the assertion has to let the microtask
    // queue drain first or it would pass before the reset had a chance to happen.
    await act(async () => {});
    expect(document.querySelector('[data-slot="table-range"]')).toHaveTextContent("Showing 11–20");
  });

  it("returns to the first page when a SEARCH narrows the set", async () => {
    const user = userEvent.setup();
    // Sized so the narrowed result is still MULTI-page (10 matches at 5/page).
    // With a single-page result the clamp alone would move the operator to page
    // 1 and this case would pass without any reset existing.
    render(
      <DataTable
        columns={columns}
        data={fixture(30)}
        label="Screens"
        pagination={{ pageSize: 5 }}
        search={{ label: "Search screens" }}
      />,
    );
    await user.click(screen.getByRole("button", { name: "Next" }));
    await user.click(screen.getByRole("button", { name: "Next" }));
    expect(document.querySelector('[data-slot="table-range"]')).toHaveTextContent("Showing 11–15");

    await user.type(screen.getByLabelText("Search screens"), "Screen 2");
    await waitFor(() =>
      expect(document.querySelector('[data-slot="table-range"]')).toHaveTextContent(
        "Showing 1–5 of 10 rows",
      ),
    );
  });
});

// ── Row selection ───────────────────────────────────────────────────────────

const selectionProps = {
  rowId: (r: Screen) => r.id,
  rowLabel: (r: Screen) => `Select ${r.name}`,
};

describe("DataTable — row selection", () => {
  it("selects a row when its checkbox is CLICKED and reports it to the caller", async () => {
    const user = userEvent.setup();
    const seen: string[][] = [];
    render(
      <DataTable
        columns={columns}
        data={rows}
        label="Screens"
        selection={{ ...selectionProps, onChange: (r) => seen.push(r.map((x) => x.name)) }}
      />,
    );

    await user.click(screen.getByRole("checkbox", { name: "Select Cafe Board" }));
    expect(screen.getByRole("checkbox", { name: "Select Cafe Board" })).toBeChecked();
    expect(seen.at(-1)).toEqual(["Cafe Board"]);
    expect(document.querySelector('[data-slot="table-selection-count"]')).toHaveTextContent(
      "1 of 3 rows selected",
    );
  });

  it("names the header checkbox for the PAGE, and selects only the page", async () => {
    const user = userEvent.setup();
    render(
      <DataTable
        columns={columns}
        data={fixture(30)}
        label="Screens"
        pagination={{ pageSize: 10 }}
        selection={selectionProps}
      />,
    );

    const all = screen.getByRole("checkbox", { name: "Select all 10 rows on this page" });
    await user.click(all);
    expect(document.querySelector('[data-slot="table-selection-count"]')).toHaveTextContent(
      "10 of 30 rows selected",
    );

    // Page 2's rows were NOT swept in by a header labelled "all".
    await user.click(screen.getByRole("button", { name: "Next" }));
    expect(screen.getByRole("checkbox", { name: "Select Screen 10" })).not.toBeChecked();
  });

  it("offers an explicit select-all-matching that DOES reach off-page rows", async () => {
    const user = userEvent.setup();
    render(
      <DataTable
        columns={columns}
        data={fixture(30)}
        label="Screens"
        pagination={{ pageSize: 10 }}
        selection={selectionProps}
      />,
    );
    await user.click(screen.getByRole("checkbox", { name: "Select all 10 rows on this page" }));
    await user.click(screen.getByRole("button", { name: "Select all 30 matching rows" }));
    expect(document.querySelector('[data-slot="table-selection-count"]')).toHaveTextContent(
      "30 of 30 rows selected",
    );

    await user.click(screen.getByRole("button", { name: "Next" }));
    expect(screen.getByRole("checkbox", { name: "Select Screen 10" })).toBeChecked();
  });

  it("hands the selected rows to the bulk-action slot and drives it", async () => {
    const user = userEvent.setup();
    const acted: string[][] = [];
    render(
      <DataTable
        columns={columns}
        data={rows}
        label="Screens"
        selection={{
          ...selectionProps,
          bulkActions: (chosen) => (
            <button type="button" onClick={() => acted.push(chosen.map((c) => c.name))}>
              Retire selected
            </button>
          ),
        }}
      />,
    );

    // The slot is not rendered while nothing is selected — a bulk bar over an
    // empty selection is chrome that can only do nothing.
    expect(screen.queryByRole("button", { name: "Retire selected" })).not.toBeInTheDocument();

    await user.click(screen.getByRole("checkbox", { name: "Select Cafe Board" }));
    await user.click(screen.getByRole("checkbox", { name: "Select Lobby Panel" }));
    await user.click(screen.getByRole("button", { name: "Retire selected" }));
    expect(acted).toEqual([["Cafe Board", "Lobby Panel"]]);
  });

  it("keeps a selection pointed at the same ROWS across a sort and a re-poll", async () => {
    const user = userEvent.setup();
    const { rerender } = render(
      <DataTable columns={columns} data={rows} label="Screens" selection={selectionProps} />,
    );

    await user.click(screen.getByRole("checkbox", { name: "Select The Hangar TV" }));
    await user.click(screen.getByRole("button", { name: /screen/i }));

    // Ascending by name puts Cafe Board first; the tick must have travelled with
    // The Hangar TV rather than staying on the first row.
    expect(screen.getByRole("checkbox", { name: "Select The Hangar TV" })).toBeChecked();
    expect(screen.getByRole("checkbox", { name: "Select Cafe Board" })).not.toBeChecked();

    // …and across a re-poll that returns the SAME rows in a different order,
    // which is where a position-keyed selection actually goes wrong: the tick
    // would stay on the slot and quietly re-point at whatever now occupies it.
    rerender(
      <DataTable
        columns={columns}
        data={[rows[2]!, rows[1]!, rows[0]!]}
        label="Screens"
        selection={selectionProps}
      />,
    );
    expect(screen.getByRole("checkbox", { name: "Select The Hangar TV" })).toBeChecked();
    expect(screen.getByRole("checkbox", { name: "Select Lobby Panel" })).not.toBeChecked();
  });

  it("clears the whole selection on demand", async () => {
    const user = userEvent.setup();
    render(<DataTable columns={columns} data={rows} label="Screens" selection={selectionProps} />);
    await user.click(screen.getByRole("checkbox", { name: "Select Cafe Board" }));
    await user.click(screen.getByRole("button", { name: "Clear selection" }));
    expect(document.querySelector('[data-slot="table-bulk-bar"]')).toBeNull();
    expect(screen.getByRole("checkbox", { name: "Select Cafe Board" })).not.toBeChecked();
  });

  it("does not PRESS the row when its checkbox is ticked", async () => {
    const user = userEvent.setup();
    const pressed: string[] = [];
    render(
      <DataTable
        columns={columns}
        data={rows}
        label="Screens"
        selection={selectionProps}
        onRowPress={(r) => pressed.push(r.name)}
      />,
    );

    await user.click(screen.getByRole("checkbox", { name: "Select Cafe Board" }));
    expect(screen.getByRole("checkbox", { name: "Select Cafe Board" })).toBeChecked();
    expect(pressed).toEqual([]);

    // …and the row is still pressable, so the guard is scoped to the checkbox.
    await user.click(screen.getByText("Cafe Board"));
    expect(pressed).toEqual(["Cafe Board"]);
  });

  it("stops counting a selected row that has left the data set", async () => {
    const user = userEvent.setup();
    const { rerender } = render(
      <DataTable columns={columns} data={rows} label="Screens" selection={selectionProps} />,
    );
    await user.click(screen.getByRole("checkbox", { name: "Select Cafe Board" }));
    expect(document.querySelector('[data-slot="table-selection-count"]')).toHaveTextContent(
      "1 of 3 rows selected",
    );

    rerender(
      <DataTable
        columns={columns}
        data={rows.filter((r) => r.name !== "Cafe Board")}
        label="Screens"
        selection={selectionProps}
      />,
    );
    expect(document.querySelector('[data-slot="table-bulk-bar"]')).toBeNull();
  });
});

// ── Both skies ──────────────────────────────────────────────────────────────

describe("DataTable — the new chrome under both themes", () => {
  it.each(["dark", "light"] as const)("is operable under the %s theme", async (theme) => {
    document.documentElement.setAttribute("data-theme", theme);
    const user = userEvent.setup();
    render(
      <DataTable
        columns={columns}
        data={rows}
        label="Screens"
        search={{ label: "Search screens" }}
        pagination={{ pageSize: 2 }}
        selection={selectionProps}
      />,
    );
    await user.click(screen.getByRole("checkbox", { name: "Select Cafe Board" }));
    expect(screen.getByRole("checkbox", { name: "Select Cafe Board" })).toBeChecked();
    await user.click(screen.getByRole("button", { name: "Next" }));
    expect(screen.getByText("Lobby Panel")).toBeInTheDocument();
    document.documentElement.removeAttribute("data-theme");
  });

  it("paints the new chrome from THEME TOKENS, never a literal colour", async () => {
    // The two themes are swapped by re-pointing tokens, so a literal colour
    // anywhere in the kit is a pixel that does not turn with the sky. Asserted
    // against the source because that is where the mistake is made — no rendered
    // check can see it in jsdom, which does not evaluate Tailwind at all.
    const source = dataTableSource;
    expect(source).not.toMatch(/#[0-9a-fA-F]{3,8}\b/);
    expect(source).not.toMatch(/\b(rgba?|hsla?|oklch)\(/);
    // Every arbitrary colour value resolves through a --wv-* custom property.
    for (const [, value] of source.matchAll(/\[color:([^\]]+)\]/g)) {
      expect(value).toMatch(/^var\(--wv-[a-z0-9-]+\)$/);
    }
  });
});

// ── Narrow (stacked) presentation ───────────────────────────────────────────

describe("DataTable — capabilities on a narrow viewport", () => {
  function setNarrow() {
    window.matchMedia = ((query: string) =>
      ({
        matches: /max-width/.test(query),
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
    window.matchMedia = ((query: string) =>
      ({
        matches: false,
        media: query,
        onchange: null,
        addEventListener: () => {},
        removeEventListener: () => {},
        addListener: () => {},
        removeListener: () => {},
        dispatchEvent: () => false,
      }) as unknown as MediaQueryList) as unknown as typeof window.matchMedia;
  });

  it("keeps search, filter, selection and paging operable as stacked cards", async () => {
    setNarrow();
    const user = userEvent.setup();
    render(
      <DataTable
        columns={columns}
        data={rows}
        label="Screens"
        search={{ label: "Search screens" }}
        pagination={{ pageSize: 2 }}
        selection={selectionProps}
      />,
    );

    expect(screen.queryByRole("table")).not.toBeInTheDocument();
    expect(document.querySelectorAll('[data-slot="data-card"]')).toHaveLength(2);

    // The checkbox is card chrome, not a labelled field paired with a <dt>.
    const firstCard = document.querySelectorAll('[data-slot="data-card"]')[0] as HTMLElement;
    const box = within(firstCard).getByRole("checkbox", { name: "Select The Hangar TV" });
    await user.click(box);
    expect(box).toBeChecked();
    expect(within(firstCard).queryByText("Select The Hangar TV")).not.toBeInTheDocument();

    await user.type(screen.getByLabelText("Search screens"), "Lobby");
    await waitFor(() => expect(document.querySelectorAll('[data-slot="data-card"]')).toHaveLength(1));
  });
});
