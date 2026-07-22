import { useState, type KeyboardEvent as ReactKeyboardEvent, type ReactNode } from "react";
import {
  flexRender,
  getCoreRowModel,
  getSortedRowModel,
  useReactTable,
  type ColumnDef,
  type RowData,
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
import { EmptyState } from "./empty-state";
import { KitIcon } from "./kit-icon";

// Column meta the kit understands: numeric cells render in the mono face with
// tabular numerals and right-align, so figures line up digit-for-digit (HORIZON).
declare module "@tanstack/react-table" {
  // The augmented interface must mirror the library's generic signature exactly,
  // so the type parameters are structural, not used in the body.
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  interface ColumnMeta<TData extends RowData, TValue> {
    numeric?: boolean;
    align?: "left" | "center" | "right";
  }
}

function alignClass(align: "left" | "center" | "right" | undefined, numeric: boolean | undefined) {
  const a = align ?? (numeric ? "right" : "left");
  return a === "right" ? "text-right" : a === "center" ? "text-center" : "text-left";
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
  /** Trailing per-row actions (e.g. a dropdown of row commands). */
  rowActions?: (row: TData) => ReactNode;
  /** Whole-row press (the ui-schema `table` `rowPress` event, UIS-070). When set,
   * each row becomes a keyboard-operable button whose accessible name is the row
   * content; the stacked-card presentation presses the card. */
  onRowPress?: (row: TData, index: number) => void;
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
  className,
}: DataTableProps<TData, TValue>) {
  const [sorting, setSorting] = useState<SortingState>([]);
  const stacked = useMediaQuery("(max-width: 640px)");

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

  const table = useReactTable({
    data,
    columns,
    state: { sorting },
    onSortingChange: setSorting,
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
  });

  const colCount = columns.length + (rowActions ? 1 : 0);
  const rows = table.getRowModel().rows;
  const empty = <div className="p-0">{emptyState ?? <EmptyState title="Nothing here yet" />}</div>;

  // ── Narrow: stacked cards (no wide grid to overflow the page) ──────────────
  if (stacked) {
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
              {Array.from({ length: columns.length }).map((__, c) => (
                <Skeleton key={`skeleton-${r}-${c}`} className="h-4 w-full max-w-[12rem]" />
              ))}
            </div>
          ))
        ) : rows.length === 0 ? (
          <div className="rounded-card border border-border bg-card">{empty}</div>
        ) : (
          rows.map((row) => {
            const press = pressProps(row.original, row.index);
            return (
            <div
              key={row.id}
              data-slot="data-card"
              {...press}
              className={cn(
                "flex flex-col gap-2 rounded-card border border-border bg-card p-4 wv-elevation",
                press.className,
              )}
            >
              <dl className="flex flex-col gap-2">
                {row.getVisibleCells().map((cell) => {
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
                  {rowActions(row.original)}
                </div>
              ) : null}
            </div>
            );
          })
        )}
      </div>
    );
  }

  // ── Wide: the ordinary table, scrolling INSIDE .wv-table-region if needed ──
  return (
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
                  {emptyState ?? <EmptyState title="Nothing here yet" />}
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
                    <TableCell className="text-right">{rowActions(row.original)}</TableCell>
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
}
