import {
  useEffect,
  useMemo,
  useRef,
  useState,
  type KeyboardEvent as ReactKeyboardEvent,
  type ReactNode,
} from "react";
import {
  flexRender,
  getCoreRowModel,
  getFacetedRowModel,
  getFacetedUniqueValues,
  getFilteredRowModel,
  getPaginationRowModel,
  getSortedRowModel,
  useReactTable,
  type Column,
  type ColumnDef,
  type ColumnFiltersState,
  type PaginationState,
  type RowData,
  type RowSelectionState,
  type SortingState,
} from "@tanstack/react-table";
import { ArrowDown, ArrowUp, ChevronsUpDown } from "lucide-react";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";
import { useMediaQuery } from "@/lib/use-media-query";
import { Button } from "./button";
import { Checkbox } from "./checkbox";
import { EmptyState } from "./empty-state";
import { FormField } from "./form-field";
import { KitIcon } from "./kit-icon";

// Column meta the kit understands: numeric cells render in the mono face with
// tabular numerals and right-align, so figures line up digit-for-digit (HORIZON).
//
// `searchable` and `filter` are how a column OPTS IN to the toolbar. They live on
// the column def rather than in a parallel prop for one reason: the set of
// searchable/filterable columns must be DERIVED from the columns that exist, not
// hand-maintained beside them. A second list is a list that goes stale the first
// time somebody adds a column, and a search box that silently stops covering the
// new column is indistinguishable from one that works.
declare module "@tanstack/react-table" {
  // The augmented interface must mirror the library's generic signature exactly,
  // so the type parameters are structural, not used in the body.
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  interface ColumnMeta<TData extends RowData, TValue> {
    numeric?: boolean;
    align?: "left" | "center" | "right";
    /** Include this column's value in the `search` box's match. When NO column
     * declares it, every column that has a value is searched — an enabled search
     * box that matches nothing is the dead-control failure this kit exists to
     * avoid. */
    searchable?: boolean;
    /** Give this column its own filter control WHEN the call site opts in with
     * `filters`. `"enum"` renders a select whose options are the column's OWN
     * distinct values (faceted from the data), so the option set can never drift
     * from what the rows actually contain.
     *
     * Two halves on purpose, exactly as `searchable` + `search` are: the column
     * says WHICH, the call site says WHETHER. Column defs get shared between a
     * long table and a short one (the cast library and its templates list use
     * the same array), and a meta flag that switched the toolbar on by itself
     * would put filter chrome over four rows with no way to decline it. */
    filter?: "enum";
    /** Visible label for that filter control. Defaults to the column's header
     * when the header is a plain string, else the column id. */
    filterLabel?: string;
  }
}

function alignClass(align: "left" | "center" | "right" | undefined, numeric: boolean | undefined) {
  const a = align ?? (numeric ? "right" : "left");
  return a === "right" ? "text-right" : a === "center" ? "text-center" : "text-left";
}

/** The row-selection column's reserved id. Never a caller's column id — it is
 * prefixed so a data column called "select" cannot collide with it. */
const SELECT_COLUMN_ID = "wv-row-select";

const DEFAULT_PAGE_SIZE = 25;
const DEFAULT_PAGE_SIZE_OPTIONS = [10, 25, 50, 100] as const;
const DEFAULT_SEARCH_DEBOUNCE_MS = 200;

// Row actions and row checkboxes live INSIDE the pressable row, so the row's own
// click handler is an ancestor of theirs. Without this guard every Adopt /
// Delete / Release press ALSO fires `onRowPress` — not a cosmetic problem: on the
// Devices page pressing Adopt silently re-filtered the entities table, and
// cancelling the modal left it filtered with nothing to explain why.
//
// The guard belongs HERE rather than on each button for two reasons: a page
// author cannot see the coupling (it is created by a prop on a different
// component), and it is a whole CLASS of bug — one missed `stopPropagation` on
// one new button in any table reintroduces it. Both event kinds are stopped
// because the row is keyboard-operable too: Enter on a nested button bubbles as
// `keydown` and would press the row as well.
const stopRowPress = (e: { stopPropagation: () => void }) => e.stopPropagation();

/** Search-box configuration. Opting in is passing the object; the columns it
 * covers come from the column defs (`meta.searchable`), never from here. */
export interface DataTableSearch {
  /** Visible, required label — e.g. "Search screens". */
  label: string;
  placeholder?: string;
  /** Debounce before the typed text becomes a filter. Default 200ms. */
  debounceMs?: number;
}

/** Pagination configuration. `true` accepts every default. */
export interface DataTablePagination {
  /** Rows per page on first render. Default 25. */
  pageSize?: number;
  /** Sizes offered in the "Rows per page" control. Default 10/25/50/100. */
  pageSizeOptions?: readonly number[];
}

/** Row-selection configuration.
 *
 * `rowId` is required and is the reason selection is trustworthy: TanStack keys
 * selection by row id, and the DEFAULT id is the row's INDEX — which means a sort,
 * a filter or a page change silently re-points a selection at different rows. A
 * caller-supplied stable id makes "these three" keep meaning these three. */
export interface DataTableSelection<TData> {
  /** Stable identity for a row — a record id, never a position. */
  rowId: (row: TData) => string;
  /** Accessible name for that row's checkbox, e.g. `Select ${row.name}`. */
  rowLabel: (row: TData) => string;
  /** Notified with the currently selected rows whenever the selection changes. */
  onChange?: (rows: TData[]) => void;
  /** The bulk-action slot, rendered only while something is selected. The caller
   * owns what a bulk action DOES; the kit owns only what is selected. */
  bulkActions?: (rows: TData[]) => ReactNode;
}

/**
 * DataTable — the one table idiom, @tanstack/react-table over the themed table.
 * Sorting (headers are real buttons, `aria-sort` on the column), a loading
 * skeleton, an empty state, and optional per-row actions. A gradient never
 * touches it — data lives on the flat content layer.
 *
 * `label` is a REQUIRED accessible name for the table (there is no visible
 * caption by default), so assistive tech always announces what the grid is.
 *
 * Responsive (Matt): a table must never make the PAGE scroll horizontally at
 * 360px. Two defences stack. Below the narrow breakpoint (`max-width: 640px`)
 * the whole grid becomes STACKED CARDS — one card per row pairing each column
 * header with its cell — so there is no wide table to overflow at all. Above it,
 * the ordinary table renders inside a `.wv-table-region` that scrolls sideways
 * INSIDE its own bounds if the columns outrun the container.
 *
 * ── Find, narrow, page, select ──────────────────────────────────────────────
 *
 * Search, per-column filtering, pagination and row selection are all OPT-IN and
 * all off by default, so every existing call site renders exactly what it did
 * before. Each is a prop the caller passes to say "this list is long enough to
 * need it", because chrome on a four-row table is noise.
 *
 * Three decisions are worth stating because they are the ones that go wrong:
 *
 *  1. **The toolbar is OUTSIDE the body.** Legacy rendered the All-Devices
 *     status and last-seen filters INSIDE its zero-results card, so the controls
 *     vanished the moment they matched something and could only be reached by
 *     first making the list empty. Here the search box and the filters are
 *     siblings of the table, never children of its empty state, and there is a
 *     test that filters down to nothing and asserts they are still operable.
 *
 *  2. **Pagination is CLIENT-SIDE, over rows the caller already holds.** api/1
 *     lists are keyset-paginated with an opaque cursor and no total (API-030–036),
 *     and the console's routes walk that cursor to exhaustion with `collectPages`
 *     before they render. So the honest thing for this component to page over is
 *     the array it was handed: every count it shows ("of 132") is a count of rows
 *     that are in memory, and it never invents a server total it cannot know.
 *     `autoResetPageIndex` is deliberately OFF — several routes re-poll on a
 *     timer, and the default would throw an operator back to page 1 every 10
 *     seconds — so the page index is instead clamped when the row count shrinks
 *     and reset explicitly when a filter changes.
 *
 *  3. **"All" means the page.** The header checkbox selects the rows ON SCREEN
 *     and says so in its accessible name. When more rows match than fit on the
 *     page, the bulk bar offers a separate, explicit "Select all N matching rows"
 *     — a select-all that quietly reached rows the operator could not see is how
 *     a bulk delete takes out the wrong records.
 */
export interface DataTableProps<TData, TValue = unknown> {
  columns: ColumnDef<TData, TValue>[];
  data: TData[];
  /** Required accessible name for the table element. */
  label: string;
  loading?: boolean;
  skeletonRows?: number;
  /** Rendered when there are no rows (and not loading). Defaults to an EmptyState. */
  emptyState?: ReactNode;
  /** Trailing per-row actions (e.g. a dropdown of row commands). Clicks and key
   * presses inside them NEVER reach `onRowPress` — the table stops them at the
   * actions cell, so an action button is only ever its own action. */
  rowActions?: (row: TData) => ReactNode;
  /** Whole-row press (the ui-schema `table` `rowPress` event, UIS-070). When set,
   * each row becomes a keyboard-operable button whose accessible name is the row
   * content; the stacked-card presentation presses the card. */
  onRowPress?: (row: TData, index: number) => void;
  /** Opt in to a debounced text search across the searchable columns. */
  search?: DataTableSearch;
  /** Opt in to the per-column filter controls. Which columns get one is declared
   * on the columns themselves (`meta.filter`); this says the list is long enough
   * to be worth narrowing. */
  filters?: boolean;
  /** Opt in to paging. `true` takes the defaults (25 rows/page). */
  pagination?: boolean | DataTablePagination;
  /** Opt in to a row-selection checkbox column and a bulk-action slot. */
  selection?: DataTableSelection<TData>;
  className?: string;
}

export function DataTable<TData, TValue = unknown>({
  columns,
  data,
  label,
  loading = false,
  skeletonRows = 5,
  emptyState,
  rowActions,
  onRowPress,
  search,
  filters = false,
  pagination,
  selection,
  className,
}: DataTableProps<TData, TValue>) {
  const [sorting, setSorting] = useState<SortingState>([]);
  const [columnFilters, setColumnFilters] = useState<ColumnFiltersState>([]);
  const [rowSelection, setRowSelection] = useState<RowSelectionState>({});
  // Two search states on purpose: `searchText` is what the box shows (it must
  // echo a keystroke immediately or the input feels broken), `globalFilter` is
  // what the table filters by (debounced, so a 400-row re-filter does not run on
  // every character).
  const [searchText, setSearchText] = useState("");
  const [globalFilter, setGlobalFilter] = useState("");
  const stacked = useMediaQuery("(max-width: 640px)");

  const paginationEnabled = pagination !== undefined && pagination !== false;
  const pageConfig: DataTablePagination = typeof pagination === "object" ? pagination : {};
  const initialPageSize = pageConfig.pageSize ?? DEFAULT_PAGE_SIZE;
  const pageSizeOptions = pageConfig.pageSizeOptions ?? DEFAULT_PAGE_SIZE_OPTIONS;
  const [pageState, setPageState] = useState<PaginationState>({
    pageIndex: 0,
    pageSize: initialPageSize,
  });

  // `searchEnabled` rather than `search` in the deps: a caller passing an inline
  // object literal changes its identity on every parent render, and a debounce
  // whose timer is cleared and re-armed on every render never fires.
  const searchEnabled = search !== undefined;
  const searchDebounce = search?.debounceMs ?? DEFAULT_SEARCH_DEBOUNCE_MS;
  useEffect(() => {
    if (!searchEnabled) return;
    if (searchDebounce <= 0) {
      setGlobalFilter(searchText);
      return;
    }
    const id = setTimeout(() => setGlobalFilter(searchText), searchDebounce);
    return () => clearTimeout(id);
  }, [searchEnabled, searchDebounce, searchText]);

  // Narrowing the set of rows must always return the operator to the first page:
  // staying on page 4 of a result that now has one page shows an empty table with
  // no explanation. Done in the state transitions rather than through
  // `autoResetPageIndex`, which would also fire on every background re-poll.
  useEffect(() => {
    setPageState((p) => (p.pageIndex === 0 ? p : { ...p, pageIndex: 0 }));
  }, [globalFilter, columnFilters]);

  // The kit's own selection plumbing reads these through a ref so the column defs
  // do not have to be rebuilt (and the table re-keyed) every time a caller passes
  // a fresh inline `selection` object literal.
  const selectionRef = useRef(selection);
  selectionRef.current = selection;
  const hasSelection = selection !== undefined;

  const searchDeclared = useMemo(
    () => columns.some((c) => c.meta?.searchable === true),
    [columns],
  );

  // Interactive props for a pressable row/card (keyboard + pointer), applied only
  // when onRowPress is set so a plain data table stays non-interactive.
  const pressProps = (rowData: TData, index: number) =>
    onRowPress
      ? {
          role: "button" as const,
          tabIndex: 0,
          onClick: () => onRowPress(rowData, index),
          onKeyDown: (e: ReactKeyboardEvent) => {
            if (e.key === "Enter" || e.key === " ") {
              e.preventDefault();
              onRowPress(rowData, index);
            }
          },
          className: "cursor-pointer",
        }
      : {};

  // A row's selection checkbox is the same shape of nesting as a row action and
  // gets the same `stopRowPress` guard (module scope, above): ticking a box must
  // never also open whatever the row opens.
  const actionsFor = (rowData: TData) => (
    <div data-slot="row-actions" onClick={stopRowPress} onKeyDown={stopRowPress}>
      {rowActions?.(rowData)}
    </div>
  );

  const tableColumns = useMemo<ColumnDef<TData, TValue>[]>(() => {
    // An enum-filtered column needs an exact-match filter fn; injected onto a COPY
    // of the caller's def so the caller's array is never mutated.
    const mapped = columns.map((col) =>
      col.meta?.filter === "enum"
        ? {
            ...col,
            filterFn: (row: { getValue: (id: string) => unknown }, columnId: string, value: unknown) =>
              value === "" || value == null ? true : String(row.getValue(columnId)) === String(value),
          }
        : col,
    ) as ColumnDef<TData, TValue>[];
    if (!hasSelection) return mapped;
    const selectColumn: ColumnDef<TData, TValue> = {
      id: SELECT_COLUMN_ID,
      enableSorting: false,
      enableGlobalFilter: false,
      header: ({ table }) => {
        const pageRows = table.getRowModel().rows.length;
        return (
          <div onClick={stopRowPress} onKeyDown={stopRowPress}>
            <Checkbox
              checked={
                table.getIsAllPageRowsSelected()
                  ? true
                  : table.getIsSomePageRowsSelected()
                    ? "indeterminate"
                    : false
              }
              onCheckedChange={(v) => table.toggleAllPageRowsSelected(v === true)}
              // The name says PAGE, because that is what it does. "Select all"
              // over a paginated set is a promise the control cannot keep.
              aria-label={`Select all ${pageRows} row${pageRows === 1 ? "" : "s"} on this page`}
            />
          </div>
        );
      },
      cell: ({ row }) => (
        <div onClick={stopRowPress} onKeyDown={stopRowPress}>
          <Checkbox
            checked={row.getIsSelected()}
            onCheckedChange={(v) => row.toggleSelected(v === true)}
            aria-label={selectionRef.current?.rowLabel(row.original) ?? "Select row"}
          />
        </div>
      ),
    };
    return [selectColumn, ...mapped];
  }, [columns, hasSelection]);

  const table = useReactTable({
    data,
    columns: tableColumns,
    state: {
      sorting,
      columnFilters,
      globalFilter,
      rowSelection,
      ...(paginationEnabled ? { pagination: pageState } : {}),
    },
    onSortingChange: setSorting,
    onColumnFiltersChange: setColumnFilters,
    onGlobalFilterChange: setGlobalFilter,
    onRowSelectionChange: setRowSelection,
    onPaginationChange: setPageState,
    enableRowSelection: hasSelection,
    // Selection is keyed by the caller's stable id, never by row index — see
    // DataTableSelection. Without this a sort re-points every tick.
    ...(hasSelection
      ? { getRowId: (row: TData) => selectionRef.current?.rowId(row) ?? "" }
      : {}),
    // Which columns the search box covers, derived from the defs: those marked
    // `meta.searchable`, or — when none is marked — every column that actually
    // has a value to match. The select column never participates.
    getColumnCanGlobalFilter: (column: Column<TData, unknown>) => {
      if (column.id === SELECT_COLUMN_ID) return false;
      if (!column.accessorFn) return false;
      return searchDeclared ? column.columnDef.meta?.searchable === true : true;
    },
    globalFilterFn: (row, columnId, filterValue) => {
      const value = row.getValue(columnId);
      if (value === null || value === undefined) return false;
      return String(value).toLowerCase().includes(String(filterValue).toLowerCase());
    },
    autoResetPageIndex: false,
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
    getFilteredRowModel: getFilteredRowModel(),
    getFacetedRowModel: getFacetedRowModel(),
    getFacetedUniqueValues: getFacetedUniqueValues(),
    ...(paginationEnabled ? { getPaginationRowModel: getPaginationRowModel() } : {}),
  });

  // A shrinking result set (a filter, a delete, a re-poll that returned fewer
  // rows) can leave the page index past the end. Clamp rather than reset, so
  // paging back from the last page lands where the operator expects.
  //
  // The updater is FUNCTIONAL and the effect does not depend on `pageIndex`, and
  // both of those are load-bearing. A filter change queues the reset-to-page-1
  // above and this clamp in the same commit; a clamp written against the
  // render's own (pre-reset) `pageIndex` would compute "last page of the new
  // result" from the OLD position and overwrite the reset — which is exactly
  // what it did, landing a narrowed search on its last page instead of its
  // first. Reading the queued state instead makes the clamp a no-op once the
  // reset has already brought the index into range.
  const pageCount = paginationEnabled ? table.getPageCount() : 1;
  useEffect(() => {
    if (!paginationEnabled || pageCount <= 0) return;
    setPageState((p) => (p.pageIndex > pageCount - 1 ? { ...p, pageIndex: pageCount - 1 } : p));
  }, [paginationEnabled, pageCount]);

  // Report the selection to the caller from `data` + `rowId` rather than from the
  // table's row model, so a row that has since left the data set stops counting.
  const onSelectionChange = selection?.onChange;
  useEffect(() => {
    if (!onSelectionChange) return;
    const rowId = selectionRef.current?.rowId;
    if (!rowId) return;
    const chosen = new Set(Object.keys(rowSelection).filter((k) => rowSelection[k]));
    onSelectionChange(data.filter((row) => chosen.has(rowId(row))));
  }, [data, onSelectionChange, rowSelection]);

  const filterColumns = filters
    ? table.getAllLeafColumns().filter((c) => c.columnDef.meta?.filter === "enum")
    : [];
  const rows = table.getRowModel().rows;
  const leafCount = table.getVisibleLeafColumns().length;
  const colCount = leafCount + (rowActions ? 1 : 0);
  const totalRows = data.length;
  const matchedRows = table.getFilteredRowModel().rows.length;
  const narrowed = globalFilter !== "" || columnFilters.length > 0;
  const selectedRows = table.getSelectedRowModel().rows;
  const selectedCount = selectedRows.length;

  const clearFilters = () => {
    setSearchText("");
    setGlobalFilter("");
    setColumnFilters([]);
  };

  // "Nothing here yet" is the wrong sentence for a list that HAS rows the filter
  // is hiding — it reads as "this deployment has none", which sends an operator
  // looking for the wrong problem. A narrowed-to-nothing table says which.
  const defaultEmpty =
    narrowed && totalRows > 0 ? (
      <EmptyState
        title="Nothing matched"
        description={`No rows match the current search or filters. ${totalRows} row${totalRows === 1 ? " is" : "s are"} loaded.`}
        action={
          <Button variant="outline" onClick={clearFilters}>
            Clear filters
          </Button>
        }
      />
    ) : (
      (emptyState ?? <EmptyState title="Nothing here yet" />)
    );
  const empty = <div className="p-0">{defaultEmpty}</div>;

  const toolbar =
    search || filterColumns.length > 0 ? (
      <div
        data-slot="table-toolbar"
        className="flex flex-wrap items-end gap-3 rounded-card border border-border bg-card px-4 py-3"
      >
        {search ? (
          <FormField label={search.label} className="min-w-[14rem] flex-1">
            {(control) => (
              <input
                {...control}
                data-slot="table-search"
                type="search"
                autoComplete="off"
                className="h-9 rounded-md border border-border bg-[color:var(--wv-surface-2)] px-2 text-sm"
                value={searchText}
                onChange={(e) => setSearchText(e.target.value)}
                {...(search.placeholder ? { placeholder: search.placeholder } : {})}
              />
            )}
          </FormField>
        ) : null}
        {filterColumns.map((column) => (
          <ColumnEnumFilter key={column.id} column={column} />
        ))}
        {narrowed ? (
          <Button variant="ghost" onClick={clearFilters} aria-label={`Clear the ${label} filters`}>
            Clear filters
          </Button>
        ) : null}
        {/* The count is a live region: an operator who is typing is looking at
            the box, not at the table, and this is what tells them the search is
            doing anything at all. */}
        <p aria-live="polite" aria-atomic="true" data-slot="table-match-count" className="pb-1.5 text-sm text-muted-foreground">
          {narrowed
            ? `${matchedRows} of ${totalRows} row${totalRows === 1 ? "" : "s"} match`
            : `${totalRows} row${totalRows === 1 ? "" : "s"}`}
        </p>
      </div>
    ) : null;

  const bulkBar =
    selection && selectedCount > 0 ? (
      <div
        data-slot="table-bulk-bar"
        className="flex flex-wrap items-center gap-3 rounded-card border border-border bg-[color:var(--wv-surface-2)] px-4 py-3"
      >
        <p aria-live="polite" aria-atomic="true" data-slot="table-selection-count" className="text-sm">
          {selectedCount} of {matchedRows} row{matchedRows === 1 ? "" : "s"} selected
        </p>
        {/* Only offered when it would actually do something the header checkbox
            cannot: reach rows that match but are not on this page. */}
        {paginationEnabled && matchedRows > rows.length && selectedCount < matchedRows ? (
          <Button
            variant="outline"
            onClick={() => table.toggleAllRowsSelected(true)}
          >{`Select all ${matchedRows} matching rows`}</Button>
        ) : null}
        <Button variant="ghost" onClick={() => table.resetRowSelection()}>
          Clear selection
        </Button>
        <div className="ml-auto flex flex-wrap items-center gap-2">
          {selection.bulkActions?.(selectedRows.map((r) => r.original))}
        </div>
      </div>
    ) : null;

  const firstRow = matchedRows === 0 ? 0 : pageState.pageIndex * pageState.pageSize + 1;
  const lastRow = Math.min((pageState.pageIndex + 1) * pageState.pageSize, matchedRows);
  const paginationBar = paginationEnabled ? (
    <div
      data-slot="table-pagination"
      className="flex flex-wrap items-center justify-between gap-3 rounded-card border border-border bg-card px-4 py-3"
    >
      <FormField label="Rows per page">
        {(control) => (
          <select
            {...control}
            data-slot="table-page-size"
            className="h-9 rounded-md border border-border bg-[color:var(--wv-surface-2)] px-2 text-sm"
            value={pageState.pageSize}
            onChange={(e) =>
              setPageState({ pageIndex: 0, pageSize: Number.parseInt(e.target.value, 10) })
            }
          >
            {pageSizeOptions.map((size) => (
              <option key={size} value={size}>
                {size}
              </option>
            ))}
          </select>
        )}
      </FormField>
      {/* The range, announced. A paginated table whose position is only legible
          from which button is disabled is not usable without sight of it. */}
      <p aria-live="polite" aria-atomic="true" data-slot="table-range" className="text-sm text-muted-foreground">
        {matchedRows === 0
          ? "No rows to show"
          : `Showing ${firstRow}–${lastRow} of ${matchedRows} row${matchedRows === 1 ? "" : "s"}`}
      </p>
      <nav aria-label={`${label} pagination`} className="flex items-center gap-2">
        <Button
          variant="outline"
          size="sm"
          disabled={!table.getCanPreviousPage()}
          onClick={() => table.previousPage()}
        >
          Previous
        </Button>
        <span className="text-sm text-muted-foreground">
          Page {Math.min(pageState.pageIndex + 1, Math.max(pageCount, 1))} of {Math.max(pageCount, 1)}
        </span>
        <Button
          variant="outline"
          size="sm"
          disabled={!table.getCanNextPage()}
          onClick={() => table.nextPage()}
        >
          Next
        </Button>
      </nav>
    </div>
  ) : null;

  const chrome = toolbar !== null || bulkBar !== null || paginationBar !== null;

  // ── Narrow: stacked cards (no wide grid to overflow the page) ──────────────
  const body = stacked ? (
    (() => {
      const groups = table.getHeaderGroups();
      const leafHeaders = groups[groups.length - 1]?.headers ?? [];
      const headerLabels = new Map(
        leafHeaders.map((h) => [
          h.column.id,
          h.isPlaceholder ? null : flexRender(h.column.columnDef.header, h.getContext()),
        ]),
      );

      return (
        <div
          data-slot="data-table"
          data-layout="stacked"
          role="group"
          aria-label={label}
          className={cn("flex flex-col gap-3", className)}
        >
          {loading ? (
            Array.from({ length: skeletonRows }).map((_, r) => (
              <div
                key={`skeleton-${r}`}
                data-slot="data-card"
                className="flex flex-col gap-2 rounded-card border border-border bg-card p-4 wv-elevation"
              >
                {Array.from({ length: leafCount }).map((__, c) => (
                  <Skeleton key={`skeleton-${r}-${c}`} className="h-4 w-full max-w-[12rem]" />
                ))}
              </div>
            ))
          ) : rows.length === 0 ? (
            <div className="rounded-card border border-border bg-card">{empty}</div>
          ) : (
            rows.map((row) => {
              const press = pressProps(row.original, row.index);
              const cells = row.getVisibleCells();
              const selectCell = cells.find((c) => c.column.id === SELECT_COLUMN_ID);
              return (
                <div
                  key={row.id}
                  data-slot="data-card"
                  data-state={row.getIsSelected() ? "selected" : undefined}
                  {...press}
                  className={cn(
                    "flex flex-col gap-2 rounded-card border border-border bg-card p-4 wv-elevation",
                    press.className,
                  )}
                >
                  {/* The checkbox is card chrome, not a labelled field: rendering
                      it inside the <dl> would pair it with a checkbox as its own
                      <dt> label. */}
                  {selectCell ? (
                    <div className="flex justify-start">
                      {flexRender(selectCell.column.columnDef.cell, selectCell.getContext())}
                    </div>
                  ) : null}
                  <dl className="flex flex-col gap-2">
                    {cells
                      .filter((cell) => cell.column.id !== SELECT_COLUMN_ID)
                      .map((cell) => {
                        const meta = cell.column.columnDef.meta;
                        return (
                          <div key={cell.id} className="flex items-start justify-between gap-3">
                            <dt className="shrink-0 text-[11px] font-semibold tracking-[0.08em] text-muted-foreground uppercase">
                              {headerLabels.get(cell.column.id)}
                            </dt>
                            <dd
                              className={cn(
                                "min-w-0 text-right text-sm break-words",
                                meta?.numeric && "font-mono wv-tnum",
                              )}
                            >
                              {flexRender(cell.column.columnDef.cell, cell.getContext())}
                            </dd>
                          </div>
                        );
                      })}
                  </dl>
                  {rowActions ? (
                    <div className="flex justify-end border-t border-border pt-2">
                      {actionsFor(row.original)}
                    </div>
                  ) : null}
                </div>
              );
            })
          )}
        </div>
      );
    })()
  ) : (
    // ── Wide: the ordinary table, scrolling INSIDE .wv-table-region if needed ──
    <div
      data-slot="data-table"
      className={cn("overflow-hidden rounded-card border border-border bg-card", className)}
    >
      <div className="wv-table-region">
        <Table aria-label={label}>
          <TableHeader>
            {table.getHeaderGroups().map((group) => (
              <TableRow key={group.id}>
                {group.headers.map((header) => {
                  const canSort = header.column.getCanSort();
                  const sorted = header.column.getIsSorted();
                  const meta = header.column.columnDef.meta;
                  const align = alignClass(meta?.align, meta?.numeric);
                  const content = header.isPlaceholder
                    ? null
                    : flexRender(header.column.columnDef.header, header.getContext());
                  // Spread aria-sort only when it applies, never as an explicit undefined.
                  const sortAttr: { "aria-sort"?: "ascending" | "descending" | "none" } =
                    sorted === "asc"
                      ? { "aria-sort": "ascending" }
                      : sorted === "desc"
                        ? { "aria-sort": "descending" }
                        : canSort
                          ? { "aria-sort": "none" }
                          : {};
                  return (
                    <TableHead key={header.id} className={align} {...sortAttr}>
                      {canSort ? (
                        <button
                          type="button"
                          onClick={header.column.getToggleSortingHandler()}
                          className={cn(
                            "inline-flex items-center gap-1.5 rounded-[6px] outline-none",
                            "hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring",
                            align === "text-right" && "flex-row-reverse",
                          )}
                        >
                          {content}
                          <KitIcon
                            icon={sorted === "asc" ? ArrowUp : sorted === "desc" ? ArrowDown : ChevronsUpDown}
                            decorative
                            className={cn("size-3.5", sorted ? "opacity-100" : "opacity-50")}
                          />
                        </button>
                      ) : (
                        content
                      )}
                    </TableHead>
                  );
                })}
                {rowActions ? (
                  <TableHead className="text-right">
                    <span className="sr-only">Actions</span>
                  </TableHead>
                ) : null}
              </TableRow>
            ))}
          </TableHeader>
          <TableBody>
            {loading ? (
              Array.from({ length: skeletonRows }).map((_, r) => (
                <TableRow key={`skeleton-${r}`}>
                  {Array.from({ length: colCount }).map((__, c) => (
                    <TableCell key={`skeleton-${r}-${c}`}>
                      <Skeleton className="h-4 w-full max-w-[10rem]" />
                    </TableCell>
                  ))}
                </TableRow>
              ))
            ) : rows.length === 0 ? (
              <TableRow className="hover:bg-transparent">
                <TableCell colSpan={colCount} className="p-0">
                  {defaultEmpty}
                </TableCell>
              </TableRow>
            ) : (
              rows.map((row) => {
                const press = pressProps(row.original, row.index);
                return (
                  <TableRow
                    key={row.id}
                    data-state={row.getIsSelected() ? "selected" : undefined}
                    {...press}
                  >
                    {row.getVisibleCells().map((cell) => {
                      const meta = cell.column.columnDef.meta;
                      return (
                        <TableCell
                          key={cell.id}
                          className={cn(
                            alignClass(meta?.align, meta?.numeric),
                            meta?.numeric && "font-mono wv-tnum",
                          )}
                        >
                          {flexRender(cell.column.columnDef.cell, cell.getContext())}
                        </TableCell>
                      );
                    })}
                    {rowActions ? (
                      <TableCell className="text-right">{actionsFor(row.original)}</TableCell>
                    ) : null}
                  </TableRow>
                );
              })
            )}
          </TableBody>
        </Table>
      </div>
    </div>
  );

  // No opt-in capability in play → the exact markup every existing call site
  // already renders, with no wrapper to disturb its container's layout.
  if (!chrome) return body;

  return (
    <div data-slot="data-table-shell" className="flex min-w-0 flex-col gap-3">
      {toolbar}
      {bulkBar}
      {body}
      {paginationBar}
    </div>
  );
}

/** One column's own filter: a select over the DISTINCT values that column
 * actually holds, with a count per option. The options are faceted from the row
 * data, never enumerated by hand — an enum whose members are maintained in a
 * second place is an enum that will be missing a member. */
function ColumnEnumFilter<TData>({ column }: { column: Column<TData, unknown> }) {
  const facets = column.getFacetedUniqueValues();
  const options = useMemo(() => {
    const out: { value: string; count: number }[] = [];
    for (const [value, count] of facets) {
      if (value === null || value === undefined || value === "") continue;
      out.push({ value: String(value), count });
    }
    out.sort((a, b) => a.value.localeCompare(b.value));
    return out;
  }, [facets]);

  const header = column.columnDef.header;
  const name =
    column.columnDef.meta?.filterLabel ?? (typeof header === "string" ? header : column.id);
  const current = (column.getFilterValue() as string | undefined) ?? "";

  return (
    <FormField label={name}>
      {(control) => (
        <select
          {...control}
          data-slot="table-filter"
          className="h-9 rounded-md border border-border bg-[color:var(--wv-surface-2)] px-2 text-sm"
          value={current}
          onChange={(e) => column.setFilterValue(e.target.value === "" ? undefined : e.target.value)}
        >
          <option value="">All</option>
          {options.map((o) => (
            <option key={o.value} value={o.value}>
              {o.value} ({o.count})
            </option>
          ))}
        </select>
      )}
    </FormField>
  );
}
