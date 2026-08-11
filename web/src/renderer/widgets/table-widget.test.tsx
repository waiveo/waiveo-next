import { describe, expect, it } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { PageRenderer } from "../PageRenderer";

/**
 * The schema-driven `table` widget's SORTING and long-list behaviour.
 *
 * `display.tsx` used to hardcode `enableSorting: false` on every column, so the
 * three biggest pages in the console (screens, schedules, automations — all
 * driven by `.uis.json`) lost even the sorting the kit already provided. Worse,
 * the columns were cell-renderers with no accessor, so there was no value for a
 * table to have ordered by anyway.
 *
 * Sorting is now decided per column from the column's own declaration: a `cell`
 * that names a value on the row (a Binding, or a LiveBinding) can be sorted; a
 * Computed or a literal cannot, because ordering a formatted STRING silently
 * mis-orders the thing it formatted.
 */

const messages: Record<string, string> = {
  "msg:col.name": "Name",
  "msg:col.slides": "Slides",
  "msg:col.summary": "Summary",
};

interface Row {
  id: string;
  name: string;
  slides: number;
}

function docWithColumns(columns: unknown[]) {
  return {
    pageType: "list-detail",
    list: {
      source: "casts",
      display: {
        type: "table",
        id: "Casts",
        props: { source: "casts", columns },
      },
    },
    detail: {
      source: "selected",
      root: { type: "text", props: { value: "id" } },
      emptyMsg: "msg:col.summary",
    },
  };
}

const boundColumns = [
  { headerMsg: "msg:col.name", cell: "item.name" },
  { headerMsg: "msg:col.slides", cell: "item.slides" },
];

const rows: Row[] = [
  { id: "01J8Z3K4N5P6Q7R8S9T0V1W2A1", name: "Zulu board", slides: 9 },
  { id: "01J8Z3K4N5P6Q7R8S9T0V1W2A2", name: "Alpha board", slides: 10 },
  { id: "01J8Z3K4N5P6Q7R8S9T0V1W2A3", name: "Mike board", slides: 2 },
];

/** The first cell of every body row, in render order. */
function firstColumn(): string[] {
  const table = screen.getByRole("table", { name: "Casts" });
  return within(table)
    .getAllByRole("row")
    .slice(1)
    .map((r) => within(r).getAllByRole("cell")[0]?.textContent ?? "");
}

describe("TableWidget — sorting is decided by the column's own declaration", () => {
  it("SORTS a bound column when its header is clicked", async () => {
    const user = userEvent.setup();
    render(
      <PageRenderer doc={docWithColumns(boundColumns)} data={{ casts: rows }} messages={messages} />,
    );
    expect(firstColumn()).toEqual(["Zulu board", "Alpha board", "Mike board"]);

    await user.click(screen.getByRole("button", { name: /name/i }));
    expect(firstColumn()).toEqual(["Alpha board", "Mike board", "Zulu board"]);
    expect(screen.getByRole("columnheader", { name: /name/i })).toHaveAttribute(
      "aria-sort",
      "ascending",
    );

    await user.click(screen.getByRole("button", { name: /name/i }));
    expect(firstColumn()).toEqual(["Zulu board", "Mike board", "Alpha board"]);
  });

  it("sorts a NUMERIC bound column numerically, not by its rendered text", async () => {
    const user = userEvent.setup();
    render(
      <PageRenderer doc={docWithColumns(boundColumns)} data={{ casts: rows }} messages={messages} />,
    );
    // Lexically the strings order "10" < "2" < "9"; numerically the values order
    // 2 < 9 < 10. The accessor returns the RAW bound value, so both directions
    // must come out numeric. (A numeric column sorts descending first — that is
    // the table's own convention for figures, not part of what is under test.)
    const slides = () => {
      const table = screen.getByRole("table", { name: "Casts" });
      return within(table)
        .getAllByRole("row")
        .slice(1)
        .map((r) => within(r).getAllByRole("cell")[1]?.textContent ?? "");
    };
    await user.click(screen.getByRole("button", { name: /slides/i }));
    expect(slides()).toEqual(["10", "9", "2"]); // lexical desc would be 9, 2, 10
    await user.click(screen.getByRole("button", { name: /slides/i }));
    expect(slides()).toEqual(["2", "9", "10"]); // lexical asc would be 10, 2, 9
  });

  it("REFUSES to sort a Computed column — a formatted string is not a sort key", () => {
    const columns = [
      { headerMsg: "msg:col.name", cell: "item.name" },
      {
        headerMsg: "msg:col.summary",
        cell: { compute: "join", args: [{ lit: ["a", "b"] }, { lit: ", " }] },
      },
    ];
    render(<PageRenderer doc={docWithColumns(columns)} data={{ casts: rows }} messages={messages} />);

    // The bound column offers a sort button; the computed one is inert text with
    // no aria-sort at all, so nothing announces an ordering it does not have.
    expect(screen.getByRole("button", { name: /name/i })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /summary/i })).not.toBeInTheDocument();
    expect(screen.getByRole("columnheader", { name: /summary/i })).not.toHaveAttribute("aria-sort");
  });
});

describe("TableWidget — long lists get the kit's paging and search", () => {
  const many: Row[] = Array.from({ length: 40 }, (_, i) => ({
    id: `01J8Z3K4N5P6Q7R8S9T0V1W2${String(i).padStart(2, "0")}`,
    name: `Board ${String(i).padStart(2, "0")}`,
    slides: i,
  }));

  it("leaves a SHORT list exactly as it was — no toolbar, no pagination", () => {
    render(
      <PageRenderer doc={docWithColumns(boundColumns)} data={{ casts: rows }} messages={messages} />,
    );
    expect(document.querySelector('[data-slot="table-pagination"]')).toBeNull();
    expect(document.querySelector('[data-slot="table-toolbar"]')).toBeNull();
  });

  it("pages a long list and searches it", async () => {
    const user = userEvent.setup();
    render(
      <PageRenderer doc={docWithColumns(boundColumns)} data={{ casts: many }} messages={messages} />,
    );

    expect(document.querySelector('[data-slot="table-range"]')).toHaveTextContent(
      "Showing 1–25 of 40 rows",
    );
    expect(screen.queryByText("Board 30")).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Next" }));
    expect(screen.getByText("Board 30")).toBeInTheDocument();

    await user.type(screen.getByLabelText("Search Casts"), "Board 07");
    await waitFor(() => expect(screen.getByText("Board 07")).toBeInTheDocument());
    expect(screen.queryByText("Board 30")).not.toBeInTheDocument();
  });
});
