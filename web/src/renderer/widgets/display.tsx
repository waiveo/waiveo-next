// ui-schema/1 — display widgets (UIS-070): text, badge, table, stat-tile.
//
// Each maps onto a kit primitive (the only sanctioned component surface): text →
// a flat content span, badge → StatusBadge, table → DataTable, stat-tile →
// StatCard. A `value`/`cell` BindingExpr is read-only (UIS-065) and may be a
// LiveBinding (UIS-109) — the value then re-evaluates over `/events/v1`.

import type { ReactNode } from "react";
import { DataTable, StatCard, StatusBadge, type ColumnDef, type Status } from "@/components/kit";
import { evalBindingExpr, toDisplay, type RenderEnv, type RenderScope } from "../bindings";
import { asLiveBinding, useLive } from "../live";
import { runAction } from "../actions";
import { useRenderer } from "../state";
import type { ActionRef, WidgetNode } from "../types";
import { narrowToItem, resolveArray, type WidgetProps } from "./common";
import { useWizard } from "./wizard-context";
import { WidgetNodeView } from "./widget-node";

/** `announce` (UIS-077) → the ARIA live-region role of that politeness.
 *
 * An UNRECOGNIZED politeness resolves to `polite`, never to a non-live node. The
 * prop exists because an ActionOutcome's sentence lands after the page has been
 * read, and a typo that silently produced a plain span would be invisible to
 * everyone who can see the screen while removing the announcement entirely for
 * everyone who cannot — so the typo costs an over-announcement instead. */
function announceRole(announce: unknown): "status" | "alert" | undefined {
  if (announce === undefined) return undefined;
  return announce === "assertive" ? "alert" : "status";
}

/** `titleMsg` (UIS-079) → the resolved hover text, or nothing.
 *
 * Resolved through `env.msg` like every other operator-facing string, so an
 * explanation stays translatable. Absent stays ABSENT rather than becoming an
 * empty `title`: an empty title attribute is a real thing to a screen reader,
 * and this prop's whole purpose is that the states it explains are
 * distinguishable rather than decorative. */
function titleText(node: { props?: Record<string, unknown> }, msg: (k: string) => string): string | undefined {
  const key = node.props?.titleMsg;
  return key === undefined ? undefined : msg(String(key));
}

export function TextWidget({ node, scope }: WidgetProps) {
  const { env } = useRenderer();
  const value = node.props?.value;
  const live = asLiveBinding(value);
  const staticValue = evalBindingExpr(value, scope, env);
  const resolved = useLive(live ? live.path : null, staticValue);
  const role = announceRole(node.props?.announce);
  const title = titleText(node, env.msg);
  return (
    <span
      data-slot="widget-text"
      className="text-sm text-foreground"
      {...(role ? { role } : {})}
      {...(title === undefined ? {} : { title })}
    >
      {toDisplay(resolved)}
    </span>
  );
}

const BADGE_TONE: Record<string, Status> = {
  neutral: "off",
  positive: "ok",
  warning: "warn",
  critical: "error",
};

/** The rendered tone of a `badge`: `toneFrom` (UIS-078) when declared, else the
 * static `tone` (UIS-070).
 *
 * `toneFrom` WINS when present — a document that declares both has said the tone
 * depends on data, and silently preferring the static one would paint a single
 * tone over every row of exactly the table the prop exists for.
 *
 * A resolved value outside the closed tone vocabulary degrades to `neutral`,
 * matching `announce`'s existing rule for an unrecognized politeness. A dynamic
 * tone CANNOT be refused at page load — the value does not exist until the data
 * does — so the choice is which wrong answer to give, and `neutral` is the only
 * one that does not assert something false: falling back to `positive` would
 * paint a broken device green. */
function badgeTone(node: { props?: Record<string, unknown> }, scope: RenderScope, env: RenderEnv): Status {
  const expr = node.props?.toneFrom;
  const raw = expr === undefined ? node.props?.tone : evalBindingExpr(expr, scope, env);
  return BADGE_TONE[String(raw ?? "neutral")] ?? "off";
}

export function BadgeWidget({ node, scope }: WidgetProps) {
  const { env } = useRenderer();
  const value = node.props?.value;
  const live = asLiveBinding(value);
  const staticValue = evalBindingExpr(value, scope, env);
  const resolved = useLive(live ? live.path : null, staticValue);
  const tone = badgeTone(node, scope, env);
  const title = titleText(node, env.msg);
  return (
    <StatusBadge status={tone} {...(title === undefined ? {} : { title })}>
      {toDisplay(resolved)}
    </StatusBadge>
  );
}

export function StatTileWidget({ node, scope }: WidgetProps) {
  const { env } = useRenderer();
  const value = node.props?.value;
  const live = asLiveBinding(value);
  const staticValue = evalBindingExpr(value, scope, env);
  const resolved = useLive(live ? live.path : null, staticValue);
  const label = env.msg(String(node.props?.labelMsg ?? ""));
  const title = titleText(node, env.msg);
  return (
    <StatCard
      label={label}
      value={toDisplay(resolved)}
      {...(title === undefined ? {} : { title })}
    />
  );
}

interface ColumnDecl {
  headerMsg: string;
  /** A value rendered per row (UIS-070) — mutually exclusive with `cellWidget`. */
  cell?: unknown;
  /** A widget subtree rendered per row with `item` in scope (UIS-071a). */
  cellWidget?: WidgetNode;
  /** The scalar a `cellWidget` column contributes for sorting and search (UIS-071a). */
  cellValue?: unknown;
}

/** Above this many rows a schema-driven table gets the kit's paging and search.
 *
 * A threshold rather than a flag because neither is expressible in the document:
 * ui-schema/1's `table` props are `source` and `columns` (UIS-070) and a host may
 * not invent a prop (Negotiation forbids silent host extensions). Paging IS a
 * host presentation decision, in the same family as the loading skeleton and the
 * stacked-card breakpoint the same component already applies unasked — so it is
 * decided from the data instead. Below the threshold the table is untouched:
 * chrome on a five-row table is noise, and a search box over rows that all fit on
 * screen is a control with nothing to do. */
const RENDERER_PAGE_SIZE = 25;

/** Whether a column's `cell` names a value on the row, and can therefore be
 * SORTED — this is what "the schema says it may" means for a table column.
 *
 * A Binding (a bare path string) or a LiveBinding (`{path, live:true}`) reads a
 * field off the row, so the raw bound value is a well-defined sort key and
 * numbers, timestamps and durations order correctly.
 *
 * A Computed or a JSON literal does not. A literal is the same on every row, so
 * sorting by it is a no-op that still costs a click and a wrong `aria-sort`
 * announcement; and a Computed's value is a rendered STRING — sorting a
 * `formatDate` column lexically puts 1 April after 12 March, which is worse than
 * not offering the sort, because it looks like it worked.
 *
 * Derived per column from the column's own declaration, so the sortable set can
 * never drift from the columns that exist. */
function cellIsRowValue(cell: unknown): boolean {
  if (typeof cell === "string") return true;
  return (
    typeof cell === "object" &&
    cell !== null &&
    "live" in cell &&
    (cell as { live?: unknown }).live === true &&
    typeof (cell as { path?: unknown }).path === "string"
  );
}

/** The leaf field of a Binding path, for a table's accessible name when the
 * document supplies no label (table is not an input, so UIS-075 does not apply). */
function leafName(binding: unknown): string {
  if (typeof binding !== "string") return "Table";
  const last = binding.split(".").pop() ?? binding;
  const name = last.split("[")[0] ?? last;
  return name || "Table";
}

export function TableWidget({ node, scope, depth }: WidgetProps) {
  const ctx = useRenderer();
  const wizard = useWizard();
  const { env } = ctx;
  // `source` is a Binding (UIS-070) and MAY be a LiveBinding (UIS-066/109): unwrap
  // `{path, live:true}` to its `.path` before resolving, and subscribe so pushed
  // rows re-render exactly as text/badge/stat-tile values do (UIS-110). A bare
  // string source is unchanged (asLiveBinding returns null → once-fetched).
  const source = node.props?.source;
  const live = asLiveBinding(source);
  const sourcePath = live ? live.path : String(source);
  const resolved = resolveArray(sourcePath, scope);
  const { loc, tree } = resolved;
  const liveArray = useLive(live ? live.path : null, resolved.array);
  const array = Array.isArray(liveArray) ? liveArray : resolved.array;
  const decls = Array.isArray(node.props?.columns) ? (node.props?.columns as ColumnDecl[]) : [];
  const rowPress = node.on?.rowPress as ActionRef | undefined;

  // Every column gets an ACCESSOR, not just a cell renderer. That is the whole
  // fix behind removing the old `enableSorting: false`: a cell-only column has no
  // value for the table to order (or to match a search against), so sorting could
  // not have worked even if it had been enabled. The accessor returns the RAW
  // bound value and the cell renders it, so a numeric column sorts numerically
  // rather than by its rendered text.
  // A column is one of two shapes (UIS-071a). A `cell` column is unchanged: the
  // accessor evaluates its BindingExpr and the cell renders that value. A
  // `cellWidget` column renders a widget SUBTREE per row under the same `item`
  // scope, and its accessor evaluates `cellValue` — the scalar the column
  // contributes for sorting and search, which a subtree cannot supply itself.
  //
  // The accessor is populated for BOTH shapes rather than omitted for widgets,
  // because it feeds the kit's search as well as its sort: a widget column with
  // no accessor is a column an operator cannot find a row by, and on the device
  // table the port-hint column is exactly the one worth searching.
  const columns: ColumnDef<Record<string, unknown>>[] = decls.map((col, ci) => {
    const widget = col.cellWidget;
    const sortExpr = widget ? col.cellValue : col.cell;
    return {
      id: `col-${ci}`,
      header: env.msg(String(col.headerMsg)),
      // A widget column with no `cellValue` has no sort key at all, so it sorts
      // no more than a Computed one does — same rule, same reason.
      enableSorting: sortExpr !== undefined && cellIsRowValue(sortExpr),
      accessorFn: (row: Record<string, unknown>, index: number) => {
        if (sortExpr === undefined) return undefined;
        const itemScope = narrowToItem(scope, row, index, loc, tree, "item");
        return evalBindingExpr(sortExpr, itemScope, env);
      },
      cell: widget
        ? ({ row }) => {
            const itemScope = narrowToItem(scope, row.original, row.index, loc, tree, "item");
            return <WidgetNodeView node={widget} scope={itemScope} depth={depth + 1} />;
          }
        : ({ getValue }) => toDisplay(getValue()) as ReactNode,
    };
  });

  const data = array as Record<string, unknown>[];
  const label = typeof node.id === "string" ? node.id : leafName(sourcePath);
  // Long-list affordances, decided from the data (see RENDERER_PAGE_SIZE). Search
  // comes WITH paging rather than separately: paging a list you cannot search
  // just hides rows behind a Next button, which is the "find one device in the
  // fleet" problem with extra clicks.
  const long = data.length > RENDERER_PAGE_SIZE;

  const onRowPress = rowPress
    ? (row: Record<string, unknown>, index: number) => {
        const itemScope = narrowToItem(scope, row, index, loc, tree, "item");
        runAction(rowPress, itemScope, ctx, wizard);
      }
    : undefined;

  return (
    <DataTable
      columns={columns}
      data={data}
      label={label}
      {...(onRowPress ? { onRowPress } : {})}
      {...(long
        ? {
            pagination: { pageSize: RENDERER_PAGE_SIZE },
            search: { label: `Search ${label}` },
          }
        : {})}
    />
  );
}
