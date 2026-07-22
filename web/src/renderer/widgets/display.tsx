// ui-schema/1 — display widgets (UIS-070): text, badge, table, stat-tile.
//
// Each maps onto a kit primitive (the only sanctioned component surface): text →
// a flat content span, badge → StatusBadge, table → DataTable, stat-tile →
// StatCard. A `value`/`cell` BindingExpr is read-only (UIS-065) and may be a
// LiveBinding (UIS-109) — the value then re-evaluates over `/events/v1`.

import type { ReactNode } from "react";
import { DataTable, StatCard, StatusBadge, type ColumnDef, type Status } from "@/components/kit";
import { evalBindingExpr, toDisplay } from "../bindings";
import { asLiveBinding, useLive } from "../live";
import { runAction } from "../actions";
import { useRenderer } from "../state";
import type { ActionRef } from "../types";
import { narrowToItem, resolveArray, type WidgetProps } from "./common";
import { useWizard } from "./wizard-context";

export function TextWidget({ node, scope }: WidgetProps) {
  const { env } = useRenderer();
  const value = node.props?.value;
  const live = asLiveBinding(value);
  const staticValue = evalBindingExpr(value, scope, env);
  const resolved = useLive(live ? live.path : null, staticValue);
  return <span data-slot="widget-text" className="text-sm text-foreground">{toDisplay(resolved)}</span>;
}

const BADGE_TONE: Record<string, Status> = {
  neutral: "off",
  positive: "ok",
  warning: "warn",
  critical: "error",
};

export function BadgeWidget({ node, scope }: WidgetProps) {
  const { env } = useRenderer();
  const value = node.props?.value;
  const live = asLiveBinding(value);
  const staticValue = evalBindingExpr(value, scope, env);
  const resolved = useLive(live ? live.path : null, staticValue);
  const tone = BADGE_TONE[String(node.props?.tone ?? "neutral")] ?? "off";
  return <StatusBadge status={tone}>{toDisplay(resolved)}</StatusBadge>;
}

export function StatTileWidget({ node, scope }: WidgetProps) {
  const { env } = useRenderer();
  const value = node.props?.value;
  const live = asLiveBinding(value);
  const staticValue = evalBindingExpr(value, scope, env);
  const resolved = useLive(live ? live.path : null, staticValue);
  const label = env.msg(String(node.props?.labelMsg ?? ""));
  return <StatCard label={label} value={toDisplay(resolved)} />;
}

interface ColumnDecl {
  headerMsg: string;
  cell: unknown;
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
  const source = node.props?.source;
  const { array, loc, tree } = resolveArray(String(source), scope);
  const decls = Array.isArray(node.props?.columns) ? (node.props?.columns as ColumnDecl[]) : [];
  const rowPress = node.on?.rowPress as ActionRef | undefined;

  const columns: ColumnDef<Record<string, unknown>>[] = decls.map((col, ci) => ({
    id: `col-${ci}`,
    header: env.msg(String(col.headerMsg)),
    enableSorting: false,
    cell: ({ row }) => {
      const itemScope = narrowToItem(scope, row.original, row.index, loc, tree, "item");
      return toDisplay(evalBindingExpr(col.cell, itemScope, env)) as ReactNode;
    },
  }));

  const data = array as Record<string, unknown>[];
  const label = typeof node.id === "string" ? node.id : leafName(source);

  const onRowPress = rowPress
    ? (row: Record<string, unknown>, index: number) => {
        const itemScope = narrowToItem(scope, row, index, loc, tree, "item");
        runAction(rowPress, itemScope, ctx, wizard);
      }
    : undefined;

  void depth;
  return (
    <DataTable columns={columns} data={data} label={label} {...(onRowPress ? { onRowPress } : {})} />
  );
}
