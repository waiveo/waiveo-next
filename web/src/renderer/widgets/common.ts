// ui-schema/1 — shared widget-rendering helpers.
//
// The scope-narrowing rules (UIS-107 repeat items, UIS-183 fragment bind) and the
// small evaluation helpers every widget module needs, kept out of the recursive
// widget-dispatch module so the category files and the dispatcher can both import
// them without a cycle.

import {
  evalBindingExpr,
  resolvePathWithLoc,
  type PathLoc,
  type RenderEnv,
  type RenderScope,
} from "../bindings";
import type { WidgetNode } from "../types";

/** Props every widget component receives. `depth` counts fragment nesting so
 * self-referential fragments fail closed at a finite ceiling (UIS-182). */
export interface WidgetProps {
  node: WidgetNode;
  scope: RenderScope;
  depth: number;
}

/** The enforced self-referential recursion ceiling (UIS-182 requires >= 16). */
export const FRAGMENT_DEPTH_CEILING = 32;

/** Truthiness for `visibleIf` / `canAdvanceIf` — an empty array is falsy, matching
 * `isEmpty`'s intent, otherwise ordinary JS truthiness. */
export function isTruthy(v: unknown): boolean {
  return Array.isArray(v) ? v.length > 0 : Boolean(v);
}

/** Evaluate a node's `visibleIf` (UIS-063). Absent is equivalent to true. */
export function isVisible(node: WidgetNode, scope: RenderScope, env: RenderEnv): boolean {
  if (node.visibleIf === undefined) return true;
  return isTruthy(evalBindingExpr(node.visibleIf, scope, env));
}

/** Narrow a scope to a `repeat`/`table` item (UIS-107): `current` becomes the
 * element, the itemScope name addresses it, and the array's location is recorded
 * so `repeat-remove` can splice it (UIS-162). */
export function narrowToItem(
  scope: RenderScope,
  element: unknown,
  index: number,
  arrayLoc: PathLoc | null,
  arrayTree: "resource" | "ui",
  itemScopeName: string,
): RenderScope {
  const currentPath: PathLoc = arrayLoc ? [...arrayLoc, index] : [...scope.currentPath];
  return {
    ...scope,
    current: element,
    currentPath,
    currentTree: arrayTree,
    item: { name: itemScopeName, value: element, index, arrayPath: arrayLoc ?? [], arrayTree },
  };
}

/** Narrow a scope for a `fragment` node's `bind` (UIS-183) — a fresh enclosing
 * Scope for the referenced subtree; `root` is unchanged (bind narrows `current`). */
export function narrowToFragmentBind(scope: RenderScope, bind: string): RenderScope {
  const { value, loc, tree } = resolvePathWithLoc(bind, scope);
  return {
    ...scope,
    current: value,
    currentPath: loc ?? scope.currentPath,
    currentTree: tree,
  };
}

/** Resolve a `repeat`/`table` `source`/`bind` to its array value + location. */
export function resolveArray(
  binding: string,
  scope: RenderScope,
): { array: unknown[]; loc: PathLoc | null; tree: "resource" | "ui" } {
  const { value, loc, tree } = resolvePathWithLoc(binding, scope);
  return { array: Array.isArray(value) ? value : [], loc, tree };
}
