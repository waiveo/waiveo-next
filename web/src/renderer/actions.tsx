// ui-schema/1 — the closed action-verb grammar (UIS-160–167).
//
// A widget event (`on.<event>`) carries an ActionRef whose `verb` is one of the
// closed set. Dispatch splits cleanly: the local verbs (`set`, `repeat-add`,
// `repeat-remove`) mutate the renderer's own in-memory trees through the store;
// the persistence/navigation verbs (`navigate`, `submit`, `create`, `delete`,
// `call-action`) resolve onto the injected api-client handler (the seam,
// UIS-161) — the renderer never invents a second write path; and the wizard
// verbs drive the enclosing wizard's step controller (UIS-050). The document is
// already validated, so an out-of-set verb never reaches here.
//
// Two envelope fields wrap that dispatch rather than belonging to any one verb:
//   • `confirm` (UIS-165) — the ActionRef is not dispatched on the event alone;
//     it goes to the renderer's confirm gate and runs, unchanged and in the same
//     Scope, only on the operator's acknowledgement. A dismissal runs nothing.
//   • `outcomeTo` (UIS-166/167) — the four SEAM verbs publish an ActionOutcome
//     into `$ui` across the invocation's lifecycle (pending → ok | error). This
//     is the only channel by which a published refusal code reaches a
//     schema-authored page: before it existed the seam's result was discarded.

import {
  evalBindingExpr,
  parseOutcomeTarget,
  resolvePath,
  resolveWriteLocation,
  type RenderScope,
} from "./bindings";
import { ACTION_DISPATCH_UNWIRED } from "./schema";
import type { RendererContextValue, WriteTarget } from "./state";
import type { ActionOutcome, ActionRef, ConfirmSpec, OutcomeReporter, WizardController } from "./types";

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

/** A validated `confirm` field (UIS-165), or null when the ActionRef declares none.
 *
 * Exported because one ActionRef does NOT reach `runAction`: a `list-detail`
 * page's `newAction` with verb `create` is intercepted by the layout to open a
 * draft form (UIS-021) instead of dispatching. UIS-165 gates an ActionRef of ANY
 * verb, so that path reads the spec through this same function rather than
 * growing a second, drifting notion of what a confirm is. */
export function confirmSpecOf(action: ActionRef): ConfirmSpec | null {
  const c = action.confirm;
  if (c === null || typeof c !== "object" || Array.isArray(c)) return null;
  const spec = c as Record<string, unknown>;
  return typeof spec.titleMsg === "string" ? (spec as unknown as ConfirmSpec) : null;
}

/** Read a rejection for the three fields an ActionOutcome carries (UIS-166).
 *
 * Duck-typed on purpose: the renderer must never import a host's error class —
 * a pack's host is not this console — so it reads the field names an api/1
 * Problem already uses and falls back to `message` for a plain Error. A rejection
 * carrying none of them still settles, with a null code, rather than hanging. */
function problemOf(err: unknown): Pick<ActionOutcome, "code" | "detail" | "traceId"> {
  const rec = err !== null && typeof err === "object" ? (err as Record<string, unknown>) : {};
  const detail =
    typeof rec.detail === "string" ? rec.detail : typeof rec.message === "string" ? rec.message : null;
  return {
    code: typeof rec.code === "string" ? rec.code : null,
    detail,
    traceId: typeof rec.traceId === "string" ? rec.traceId : null,
  };
}

/**
 * Dispatch one SEAM verb (UIS-161), publishing its ActionOutcome when the
 * ActionRef declared an `outcomeTo` (UIS-166/167).
 *
 * `wired` is the injected handler method's own presence, passed separately from
 * `call` because the two answers differ: an unwired seam with no outcome binding
 * stays the silent no-op it has always been, while an unwired seam the page ASKED
 * about settles as ACTION_DISPATCH_UNWIRED — reporting `ok` would claim an act
 * that did not occur, and leaving it `pending` would hang a control on a request
 * nobody sent.
 */
function runSeam(
  action: ActionRef,
  ctx: RendererContextValue,
  wired: boolean,
  call: (report?: OutcomeReporter) => unknown,
): void {
  const loc = parseOutcomeTarget(action.outcomeTo);
  if (!loc) {
    // No outcome binding: fire and forget, exactly as every verb did before v1.1.
    // The catch keeps a rejected seam promise from surfacing as an unhandled
    // rejection — a page that did not ask for the outcome is not told about it.
    if (wired) void Promise.resolve(call()).catch(() => undefined);
    return;
  }
  const target: WriteTarget = { tree: "ui", loc };
  if (!wired) {
    ctx.store.write(target, {
      status: "error",
      code: ACTION_DISPATCH_UNWIRED,
      detail: `no host dispatch is wired for the "${action.verb}" verb, so the invocation did not happen`,
      traceId: null,
    } satisfies ActionOutcome);
    return;
  }

  // The pending record owns the slot for this invocation's whole lifetime, so
  // interim progress merges into a LOCAL copy rather than re-reading the store —
  // no read-modify-write against a tree the operator may be editing meanwhile.
  let settled = false;
  let current: ActionOutcome = { status: "pending" };
  ctx.store.write(target, current);
  const report: OutcomeReporter = (partial) => {
    if (settled) return; // UIS-166: a report after the outcome settled is ignored
    current = { ...current, ...partial, status: "pending" };
    ctx.store.write(target, current);
  };

  let promise: Promise<unknown>;
  try {
    promise = Promise.resolve(call(report));
  } catch (err) {
    // A seam that throws synchronously is a refusal like any other, not a crash
    // that escapes the click handler and leaves the outcome stuck on `pending`.
    promise = Promise.reject(err);
  }
  void promise.then(
    (result) => {
      settled = true;
      ctx.store.write(target, { status: "ok", result } satisfies ActionOutcome);
    },
    (err: unknown) => {
      settled = true;
      ctx.store.write(target, { status: "error", ...problemOf(err) } satisfies ActionOutcome);
    },
  );
}

/** Run one ActionRef against the current scope. `wizard` is present only inside a
 * wizard page; the wizard verbs no-op elsewhere (validation already forbids that
 * combination, UIS-163).
 *
 * A `confirm` (UIS-165) is handled HERE rather than inside the verb switch, so it
 * gates every verb uniformly and gates it before any side effect: what the gate
 * later runs is this same ActionRef, in this same Scope, unchanged. */
export function runAction(
  action: ActionRef,
  scope: RenderScope,
  ctx: RendererContextValue,
  wizard?: WizardController,
): void {
  const confirm = confirmSpecOf(action);
  if (confirm) {
    ctx.requestConfirm(confirm, () => dispatchAction(action, scope, ctx, wizard));
    return;
  }
  dispatchAction(action, scope, ctx, wizard);
}

function dispatchAction(
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
        const target = scope.draftCreateTarget;
        runSeam(action, ctx, handler.create !== undefined, (report) =>
          report ? handler.create?.(target, asObject(scope.root), report) : handler.create?.(target, asObject(scope.root)),
        );
        break;
      }
      // Persist the *bound* resource `target` names — not the whole data root
      // (UIS-160/UIS-005). An explicit `target` is an ordinary Binding, resolved
      // against the enclosing scope before handing to the seam (the same way the
      // `delete` case just below resolves its own target). Absent a target, the
      // page's primary bound resource is meant — and its resolved value is this
      // subtree's outermost Scope (`scope.root`, UIS-005: a root Binding's value
      // BECOMES the Scope it roots), the value `detail.source`/`source` established.
      const explicit = typeof action.target === "string" ? action.target : undefined;
      const submitTarget = explicit ?? primarySource;
      const resource = explicit !== undefined ? resolvePath(explicit, scope) : scope.root;
      runSeam(action, ctx, handler.submit !== undefined, (report) =>
        report ? handler.submit?.(submitTarget, resource, report) : handler.submit?.(submitTarget, resource),
      );
      break;
    }
    case "create": {
      const target = String(action.target);
      const seed = asObject(action.itemDefault);
      runSeam(action, ctx, handler.create !== undefined, (report) =>
        report ? handler.create?.(target, seed, report) : handler.create?.(target, seed),
      );
      break;
    }
    case "delete": {
      const target = String(action.target);
      const resource = resolvePath(target, scope);
      runSeam(action, ctx, handler.remove !== undefined, (report) =>
        report ? handler.remove?.(target, resource, report) : handler.remove?.(target, resource),
      );
      break;
    }
    case "call-action": {
      const name = String(action.action);
      const params = resolveParams(action.params, scope);
      runSeam(action, ctx, handler.callAction !== undefined, (report) =>
        report ? handler.callAction?.(name, params, report) : handler.callAction?.(name, params),
      );
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
      // no array path or index is supplied by the author. An item scope
      // established by a `fragment` `bind` rather than a `repeat`/`table` row
      // (UIS-183) carries no iteration position, so there is no array to splice
      // — no-op rather than removing at a fabricated location.
      const item = scope.item;
      if (item?.arrayPath !== undefined && item.index !== undefined) {
        store.removeItem({ tree: item.tree, loc: item.arrayPath }, item.index);
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
