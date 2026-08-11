// ui-schema/1 — page-scoped state + the renderer context.
//
// state.tsx owns the two renderer-held trees a page interacts with: the editable
// `resource` (the primary bound record/namespace an input writes back into,
// UIS-065, and `submit` persists, UIS-160) and `ui`, the ephemeral `$ui` state
// (page-scoped, never persisted, UIS-104). Writes are immutable so React
// re-renders and every widget re-reads its binding against the new tree. Context
// feeds are read-only (UIS-152) and ride in through the render environment, not
// this store. The RendererContext carries the store plus the injected api-client
// handler (the action seam), the document's fragments, the host's slot content,
// and the render environment.

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { ConfirmModal } from "@/components/kit";
import type { PathLoc, RenderEnv } from "./bindings";
import type { ActionHandler, ConfirmSpec, WidgetNode } from "./types";

// ── Immutable tree helpers ──────────────────────────────────────────────────

function getIn(container: unknown, loc: PathLoc): unknown {
  let cursor = container;
  for (const key of loc) {
    if (cursor == null) return undefined;
    cursor = (cursor as Record<string | number, unknown>)[key];
  }
  return cursor;
}

/** Whether `v` can hold nested members — the only thing a write is allowed to
 * descend THROUGH. A scalar cannot: `{...("18:00:00")}` is `{0:"1",1:"8",…}`,
 * so spreading a string on the way to `after.event` would character-spread it
 * and write garbage the page never showed anyone. A record that holds a scalar
 * where a nested write needs a container is a real shape the builders produce
 * (a `time` condition's `after` is the string `"18:00:00"`; re-typing that
 * condition to `sun` makes the very next write target `after.event`), so the
 * write REPLACES the scalar with a fresh container rather than exploding it. */
function isWritableContainer(v: unknown): boolean {
  return typeof v === "object" && v !== null;
}

/** Immutably set `value` at `loc`, cloning the containers along the way and
 * synthesizing an array or object for a missing (or scalar) container based on
 * the next key's type (a numeric key implies an array). */
function setIn(container: unknown, loc: PathLoc, value: unknown): unknown {
  if (loc.length === 0) return value;
  const [key, ...rest] = loc;
  const base = isWritableContainer(container) ? container : typeof key === "number" ? [] : {};
  if (Array.isArray(base)) {
    const copy = base.slice();
    copy[key as number] = setIn(copy[key as number], rest, value);
    return copy;
  }
  const copy: Record<string | number, unknown> = { ...(base as Record<string | number, unknown>) };
  copy[key] = setIn(copy[key], rest, value);
  return copy;
}

/** Immutably append `item` to the array at `loc` (UIS-162 repeat-add). */
function appendIn(container: unknown, loc: PathLoc, item: unknown): unknown {
  const arr = getIn(container, loc);
  const next = Array.isArray(arr) ? [...arr, item] : [item];
  return setIn(container, loc, next);
}

/** Immutably remove index `i` from the array at `loc` (UIS-162 repeat-remove). */
function removeIn(container: unknown, loc: PathLoc, i: number): unknown {
  const arr = getIn(container, loc);
  const next = Array.isArray(arr) ? arr.filter((_, idx) => idx !== i) : [];
  return setIn(container, loc, next);
}

// ── The store ───────────────────────────────────────────────────────────────

export interface WriteTarget {
  tree: "resource" | "ui";
  loc: PathLoc;
}

export interface RendererStore {
  readonly resource: unknown;
  readonly ui: Record<string, unknown>;
  write(target: WriteTarget, value: unknown): void;
  appendItem(target: WriteTarget, item: unknown): void;
  removeItem(target: WriteTarget, index: number): void;
}

export interface RendererContextValue {
  env: RenderEnv;
  store: RendererStore;
  handler: ActionHandler;
  fragments: Record<string, WidgetNode>;
  slots: Record<string, ReactNode>;
  /** The confirm gate (UIS-165): an ActionRef declaring `confirm` hands its own
   * dispatch here instead of running it, and the renderer runs `run` only on the
   * operator's acknowledgement. It lives on the context rather than as a
   * parameter threaded through every `runAction` call site so that EVERY event —
   * a button press, a table `rowPress`, an input `change`, a wizard `onFinish` —
   * is gated by the same one gate, with no site left un-plumbed. */
  requestConfirm: (spec: ConfirmSpec, run: () => void) => void;
  /** The page's own primary bound resource path — `submit`'s default target
   * (UIS-160): `source` for settings-form, `detail.source` for list-detail. */
  primarySource: string | undefined;
  /** Current editable tree + ephemeral state (re-read each render). */
  resource: unknown;
  ui: Record<string, unknown>;
}

const RendererContext = createContext<RendererContextValue | null>(null);

export function useRenderer(): RendererContextValue {
  const ctx = useContext(RendererContext);
  if (!ctx) throw new Error("useRenderer must be used within a RendererProvider");
  return ctx;
}

export interface RendererProviderProps {
  initialResource: unknown;
  initialUi?: Record<string, unknown>;
  env: RenderEnv;
  handler: ActionHandler;
  fragments: Record<string, WidgetNode>;
  slots: Record<string, ReactNode>;
  primarySource?: string | undefined;
  /** Notified when the ephemeral `$ui` tree changes (UIS-104) — e.g. a
   * list-detail `rowPress` writing `$ui.selected`. The host seam owns state the
   * renderer cannot see (captured field-validation errors, a conflict-review
   * banner); this is how it learns a selection moved so it can scope/clear that
   * state to the record now in view. Not fired for the initial mount. */
  onUiChange?: ((ui: Record<string, unknown>) => void) | undefined;
  /** Notified when the editable `resource` tree changes (UIS-065) — every
   * bound input's write. The host seam owns surfaces that must agree with the
   * record the operator is LOOKING at rather than the one the server last sent
   * (the Automations page's raw-JSON hatch is the case this exists for: seeded
   * from server state alone, it shows a stale rule and its Apply silently
   * discards whatever the builder holds). Not fired for the initial mount. */
  onResourceChange?: ((resource: unknown) => void) | undefined;
  children: ReactNode;
}

export function RendererProvider({
  initialResource,
  initialUi,
  env,
  handler,
  fragments,
  slots,
  primarySource,
  onUiChange,
  onResourceChange,
  children,
}: RendererProviderProps) {
  const [state, setState] = useState<{ resource: unknown; ui: Record<string, unknown> }>(() => ({
    resource: initialResource,
    ui: initialUi ?? {},
  }));
  // A ref of the latest state so the (stable) store methods can read the current
  // trees inside an event handler without re-creating themselves each render.
  const stateRef = useRef(state);
  stateRef.current = state;

  // Report `$ui` transitions to the host seam, skipping the initial mount (the
  // host already knows the seed it passed as `initialUi`). Fires post-commit, so
  // a handler that setState()s the host is safe.
  const onUiChangeRef = useRef(onUiChange);
  onUiChangeRef.current = onUiChange;
  const mountedRef = useRef(false);
  useEffect(() => {
    if (!mountedRef.current) {
      mountedRef.current = true;
      return;
    }
    onUiChangeRef.current?.(state.ui);
  }, [state.ui]);

  // The same seam for the editable record (UIS-065). Separate mount guard: the
  // two trees change independently, and a host that only watches `$ui` cannot
  // see an edit the operator made inside the form.
  const onResourceChangeRef = useRef(onResourceChange);
  onResourceChangeRef.current = onResourceChange;
  const resourceMountedRef = useRef(false);
  useEffect(() => {
    if (!resourceMountedRef.current) {
      resourceMountedRef.current = true;
      return;
    }
    onResourceChangeRef.current?.(state.resource);
  }, [state.resource]);

  const store = useMemo<RendererStore>(
    () => ({
      get resource() {
        return stateRef.current.resource;
      },
      get ui() {
        return stateRef.current.ui;
      },
      write({ tree, loc }, value) {
        setState((s) =>
          tree === "ui"
            ? { ...s, ui: setIn(s.ui, loc, value) as Record<string, unknown> }
            : { ...s, resource: setIn(s.resource, loc, value) },
        );
      },
      appendItem({ tree, loc }, item) {
        setState((s) =>
          tree === "ui"
            ? { ...s, ui: appendIn(s.ui, loc, item) as Record<string, unknown> }
            : { ...s, resource: appendIn(s.resource, loc, item) },
        );
      },
      removeItem({ tree, loc }, index) {
        setState((s) =>
          tree === "ui"
            ? { ...s, ui: removeIn(s.ui, loc, index) as Record<string, unknown> }
            : { ...s, resource: removeIn(s.resource, loc, index) },
        );
      },
    }),
    [],
  );

  // The confirm gate (UIS-165). One pending request at a time: a confirmation is
  // modal by construction, and a second event cannot reach a control the open
  // dialog has already taken focus away from.
  const [pendingConfirm, setPendingConfirm] = useState<{ spec: ConfirmSpec; run: () => void } | null>(null);
  const requestConfirm = useCallback((spec: ConfirmSpec, run: () => void) => {
    setPendingConfirm({ spec, run });
  }, []);

  const value: RendererContextValue = {
    env,
    store,
    handler,
    fragments,
    slots,
    requestConfirm,
    primarySource,
    resource: state.resource,
    ui: state.ui,
  };

  return (
    <RendererContext.Provider value={value}>
      {children}
      {pendingConfirm ? (
        // The console's single dialog idiom — deliberately NOT window.confirm,
        // which blocks the browser-automation harness this project verifies its UI
        // with, so a control gated behind one is a control nothing can prove works.
        <ConfirmModal
          open
          onOpenChange={(open) => {
            // Any dismissal — Cancel, Escape, the overlay, the close button —
            // drops the request. UIS-165: a dismissal dispatches nothing at all.
            if (!open) setPendingConfirm(null);
          }}
          title={env.msg(String(pendingConfirm.spec.titleMsg))}
          {...(pendingConfirm.spec.bodyMsg
            ? { description: env.msg(String(pendingConfirm.spec.bodyMsg)) }
            : {})}
          {...(pendingConfirm.spec.confirmLabelMsg
            ? { confirmLabel: env.msg(String(pendingConfirm.spec.confirmLabelMsg)) }
            : {})}
          {...(pendingConfirm.spec.cancelLabelMsg
            ? { cancelLabel: env.msg(String(pendingConfirm.spec.cancelLabelMsg)) }
            : {})}
          destructive={pendingConfirm.spec.destructive === true}
          onConfirm={() => {
            const { run } = pendingConfirm;
            setPendingConfirm(null);
            run();
          }}
        />
      ) : null}
    </RendererContext.Provider>
  );
}
