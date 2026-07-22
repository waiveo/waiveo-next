// ui-schema/1 — structural widgets (UIS-070): section, repeat, switch, fragment,
// slot.
//
// These compose the content tree rather than paint a value: `section` groups and
// optionally collapses; `repeat` narrows the scope per array element (UIS-107);
// `switch` renders the matching case (UIS-202 — no match and no default renders
// nothing); `fragment` is pure substitution with optional scope-narrowing bind
// (UIS-181/183) and a finite recursion ceiling (UIS-182); `slot` renders the
// host-provided fragment content for a named insertion point (UIS-185).

import { useState } from "react";
import { KitIcon } from "@/components/kit";
import { ChevronDown, ChevronRight } from "lucide-react";
import { asLiveBinding } from "../live";
import { evalBindingExpr, resolvePath } from "../bindings";
import { useRenderer } from "../state";
import type { WidgetNode } from "../types";
import { FRAGMENT_DEPTH_CEILING, narrowToFragmentBind, narrowToItem, resolveArray, type WidgetProps } from "./common";
import { WidgetNodeView } from "./widget-node";

export function SectionWidget({ node, scope, depth }: WidgetProps) {
  const { env } = useRenderer();
  const collapsible = node.props?.collapsible === true;
  const [collapsed, setCollapsed] = useState(node.props?.defaultCollapsed === true);
  const title = node.props?.titleMsg ? env.msg(String(node.props.titleMsg)) : undefined;
  const children = Array.isArray(node.children) ? node.children : [];
  const body = (
    <div className="flex flex-col gap-4">
      {children.map((child, i) => (
        <WidgetNodeView key={i} node={child} scope={scope} depth={depth} />
      ))}
    </div>
  );
  return (
    <section data-slot="widget-section" className="flex flex-col gap-3">
      {title ? (
        collapsible ? (
          <button
            type="button"
            aria-expanded={!collapsed}
            onClick={() => setCollapsed((v) => !v)}
            className="flex items-center gap-1.5 text-left font-display text-[15px] font-semibold outline-none focus-visible:ring-2 focus-visible:ring-ring"
          >
            <KitIcon icon={collapsed ? ChevronRight : ChevronDown} decorative className="size-4" />
            {title}
          </button>
        ) : (
          <h3 className="font-display text-[15px] font-semibold">{title}</h3>
        )
      ) : null}
      {collapsible && collapsed ? null : body}
    </section>
  );
}

export function RepeatWidget({ node, scope, depth }: WidgetProps) {
  const { env } = useRenderer();
  const live = asLiveBinding(node.bind);
  const bindPath = live ? live.path : String(node.bind);
  const { array, loc, tree } = resolveArray(bindPath, scope);
  const itemScopeName = typeof node.props?.itemScope === "string" ? node.props.itemScope : "item";
  const template = node.props?.itemTemplate as WidgetNode | undefined;
  if (!template) return null;
  if (array.length === 0) {
    const emptyMsg = node.props?.emptyMsg ? env.msg(String(node.props.emptyMsg)) : null;
    return emptyMsg ? <p className="text-sm text-muted-foreground">{emptyMsg}</p> : null;
  }
  return (
    <div className="flex flex-col gap-3">
      {array.map((element, i) => (
        <WidgetNodeView
          key={i}
          node={template}
          scope={narrowToItem(scope, element, i, loc, tree, itemScopeName)}
          depth={depth}
        />
      ))}
    </div>
  );
}

interface SwitchCase {
  when: unknown;
  render: WidgetNode;
}

export function SwitchWidget({ node, scope, depth }: WidgetProps) {
  const { env } = useRenderer();
  const discriminant = evalBindingExpr(node.props?.discriminant, scope, env);
  const cases = Array.isArray(node.props?.cases) ? (node.props?.cases as SwitchCase[]) : [];
  const match = cases.find((c) => c.when === discriminant);
  const chosen = match?.render ?? (node.props?.default as WidgetNode | undefined);
  return chosen ? <WidgetNodeView node={chosen} scope={scope} depth={depth} /> : null;
}

export function FragmentWidget({ node, scope, depth }: WidgetProps) {
  const ctx = useRenderer();
  const ref = String(node.props?.ref);
  const fragment = ctx.fragments[ref];
  if (!fragment) return null; // an unresolved ref was rejected at validation (UIS-180)
  // Fail closed at a finite ceiling rather than exhaust the stack (UIS-182).
  if (depth + 1 > FRAGMENT_DEPTH_CEILING) {
    return (
      <div role="alert" className="text-sm text-[color:var(--wv-err)]">
        Fragment recursion depth exceeded
      </div>
    );
  }
  // A bind narrows the scope (UIS-183); absent, the fragment is scope-transparent.
  let childScope = scope;
  if (node.bind !== undefined) {
    const live = asLiveBinding(node.bind);
    childScope = narrowToFragmentBind(scope, live ? live.path : String(node.bind));
  }
  // params become $params.<name> inside the fragment (UIS-184), resolved against
  // this node's own enclosing scope at reference time.
  const paramsDecl = node.props?.params;
  if (paramsDecl !== null && typeof paramsDecl === "object" && !Array.isArray(paramsDecl)) {
    const params: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(paramsDecl)) {
      params[k] = typeof v === "string" ? resolvePath(v, scope) : v;
    }
    childScope = { ...childScope, params };
  }
  return <WidgetNodeView node={fragment} scope={childScope} depth={depth + 1} />;
}

export function SlotWidget({ node }: WidgetProps) {
  const ctx = useRenderer();
  const name = String(node.props?.name);
  return <>{ctx.slots[name] ?? null}</>;
}
