// ui-schema/1 — the closed action-verb grammar (UIS-160–163).
//
// A widget event (`on.<event>`) carries an ActionRef whose `verb` is one of the
// closed set. Dispatch splits cleanly: the local verbs (`set`, `repeat-add`,
// `repeat-remove`) mutate the renderer's own in-memory trees through the store;
// the persistence/navigation verbs (`navigate`, `submit`, `create`, `delete`,
// `call-action`) resolve onto the injected api-client handler (the seam,
// UIS-161) — the renderer never invents a second write path; and the wizard
// verbs drive the enclosing wizard's step controller (UIS-050). The document is
// already validated, so an out-of-set verb never reaches here.

import { evalBindingExpr, resolvePath, resolveWriteLocation, type RenderScope } from "./bindings";
import type { RendererContextValue } from "./state";
import type { ActionRef, WizardController } from "./types";

function deepClone<T>(v: T): T {
  if (typeof structuredClone === "function") return structuredClone(v);
  return JSON.parse(JSON.stringify(v)) as T;
}

/** Resolve an ActionRef `params`-style object of literal/Binding values
 * (UIS-108/160/184): a string is a Binding, everything else a literal. */
function resolveParams(value: unknown, scope: RenderScope): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  if (value !== null && typeof value === "object" && !Array.isArray(value)) {
    for (const [k, v] of Object.entries(value)) {
      out[k] = typeof v === "string" ? resolvePath(v, scope) : v;
    }
  }
  return out;
}

/** Substitute `{name}` / `:name` placeholders in a navigate `to` template. */
function substitute(to: string, params: Record<string, unknown>): string {
  return to
    .replace(/\{(\w+)\}/g, (_, n: string) => (params[n] !== undefined ? String(params[n]) : `{${n}}`))
    .replace(/:(\w+)/g, (_, n: string) => (params[n] !== undefined ? String(params[n]) : `:${n}`));
}

function asObject(v: unknown): Record<string, unknown> {
  return v !== null && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>) : {};
}

/** Run one ActionRef against the current scope. `wizard` is present only inside a
 * wizard page; the wizard verbs no-op elsewhere (validation already forbids that
 * combination, UIS-163). */
export function runAction(
  action: ActionRef,
  scope: RenderScope,
  ctx: RendererContextValue,
  wizard?: WizardController,
): void {
  const { store, handler, env, primarySource } = ctx;
  switch (action.verb) {
    case "navigate": {
      const params = resolveParams(action.params, scope);
      handler.navigate?.(substitute(String(action.to ?? ""), params), params);
      break;
    }
    case "submit": {
      // Inside a list-detail create draft (the New idiom, UIS-021), the bound
      // record is a fresh in-memory draft, not a persisted row — so Save persists
      // it as a CREATE through the same host create path (UIS-160/161), naming the
      // draft's target collection, rather than an If-Match update of a record that
      // does not exist yet. The draft (scope.root) carries the seeded fields plus
      // the universal-envelope defaults the page declared.
      if (scope.draftCreateTarget !== undefined) {
        void handler.create?.(scope.draftCreateTarget, asObject(scope.root));
        break;
      }
      // Persist the *bound* resource `target` names — not the whole data root
      // (UIS-160/UIS-005). An explicit `target` is an ordinary Binding, resolved
      // against the enclosing scope before handing to the seam (the same way the
      // `delete` case just below resolves its own target). Absent a target, the
      // page's primary bound resource is meant — and its resolved value is this
      // subtree's outermost Scope (`scope.root`, UIS-005: a root Binding's value
      // BECOMES the Scope it roots), the value `detail.source`/`source` established.
      if (typeof action.target === "string") {
        void handler.submit?.(action.target, resolvePath(action.target, scope));
      } else {
        void handler.submit?.(primarySource, scope.root);
      }
      break;
    }
    case "create": {
      void handler.create?.(String(action.target), asObject(action.itemDefault));
      break;
    }
    case "delete": {
      const target = String(action.target);
      void handler.remove?.(target, resolvePath(target, scope));
      break;
    }
    case "call-action": {
      void handler.callAction?.(String(action.action), resolveParams(action.params, scope));
      break;
    }
    case "set": {
      const loc = resolveWriteLocation(String(action.target), scope);
      if (loc) store.write(loc, evalBindingExpr(action.value, scope, env));
      break;
    }
    case "repeat-add": {
      const loc = resolveWriteLocation(String(action.target), scope);
      if (loc) store.appendItem(loc, deepClone(asObject(action.itemDefault)));
      break;
    }
    case "repeat-remove": {
      // The array + index come from the item's own iteration context (UIS-162);
      // no array path or index is supplied by the author.
      if (scope.item) {
        store.removeItem({ tree: scope.item.arrayTree, loc: scope.item.arrayPath }, scope.item.index);
      }
      break;
    }
    case "wizard-next":
      wizard?.next();
      break;
    case "wizard-back":
      wizard?.back();
      break;
    case "wizard-finish":
      wizard?.finish();
      break;
  }
}
