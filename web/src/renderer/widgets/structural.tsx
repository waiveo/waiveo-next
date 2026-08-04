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
import { asLiveBinding, useLive } from "../live";
import { evalBindingExpr, resolvePath } from "../bindings";
import { useRenderer } from "../state";
import type { WidgetNode } from "../types";
import { FRAGMENT_DEPTH_CEILING, narrowToFragmentBind, narrowToItem, resolveArray, type WidgetProps } from "./common";
import { FRAGMENT_RECURSION_DEPTH_EXCEEDED } from "../schema";
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
  // `bind` is a Binding (UIS-070) and MAY be a LiveBinding (UIS-109): unwrap to
  // its `.path` for the initial resolve AND subscribe (UIS-110) so a live-bound
  // list grows over events/1 rather than freezing at its once-fetched snapshot.
  const live = asLiveBinding(node.bind);
  const bindPath = live ? live.path : String(node.bind);
  const resolved = resolveArray(bindPath, scope);
  const { loc, tree } = resolved;
  const liveArray = useLive(live ? live.path : null, resolved.array);
  const array = Array.isArray(liveArray) ? liveArray : resolved.array;
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
  // A `bind` narrows the scope (UIS-183) and MAY be a LiveBinding (UIS-109): its
  // rescope value re-evaluates over events/1 (UIS-110). Resolve + subscribe
  // UNCONDITIONALLY (rules of hooks) — ahead of the fail-closed early returns —
  // so `useLive` is called in the same order every render regardless of them.
  const live = asLiveBinding(node.bind);
  const narrowed =
    node.bind !== undefined ? narrowToFragmentBind(scope, live ? live.path : String(node.bind)) : null;
  const liveCurrent = useLive(live ? live.path : null, narrowed ? narrowed.current : undefined);

  if (!fragment) return null; // an unresolved ref was rejected at validation (UIS-180)
  // Fail closed at a finite ceiling rather than exhaust the stack (UIS-182).
  //
  // The refusal carries its published CODE, not only prose. UIS-182 says this
  // failure "MUST fail closed as FRAGMENT_RECURSION_DEPTH_EXCEEDED", and the code
  // is the machine-readable half of that: the validation surface already renders
  // `code · path` for every static refusal (PageRenderer), and a render-time
  // refusal that showed only a sentence would be the one failure in this renderer
  // nothing could identify programmatically.
  //
  // It was declared in ERROR_CODES and passed to nothing, which is how the
  // published-error-code gate found it. The ceiling itself was already enforced
  // and tested — what was missing was the name.
  if (depth + 1 > FRAGMENT_DEPTH_CEILING) {
    return (
      <div
        role="alert"
        data-slot="fragment-recursion-exceeded"
        data-wv-code={FRAGMENT_RECURSION_DEPTH_EXCEEDED}
        className="text-sm text-[color:var(--wv-err)]"
      >
        {FRAGMENT_RECURSION_DEPTH_EXCEEDED} · fragment recursion depth exceeded
      </div>
    );
  }
  // Absent a bind the fragment is scope-transparent; present, the (possibly live)
  // narrowed value becomes the new enclosing Scope's `current` (UIS-183).
  let childScope = scope;
  if (narrowed) {
    childScope = { ...narrowed, current: liveCurrent };
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
