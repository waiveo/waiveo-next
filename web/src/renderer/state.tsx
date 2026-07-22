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

import { createContext, useContext, useMemo, useRef, useState, type ReactNode } from "react";
import type { PathLoc, RenderEnv } from "./bindings";
import type { ActionHandler, WidgetNode } from "./types";

// ── Immutable tree helpers ──────────────────────────────────────────────────

function getIn(container: unknown, loc: PathLoc): unknown {
  let cursor = container;
  for (const key of loc) {
    if (cursor == null) return undefined;
    cursor = (cursor as Record<string | number, unknown>)[key];
  }
  return cursor;
}

/** Immutably set `value` at `loc`, cloning the containers along the way and
 * synthesizing an array or object for a missing container based on the next
 * key's type (a numeric key implies an array). */
function setIn(container: unknown, loc: PathLoc, value: unknown): unknown {
  if (loc.length === 0) return value;
  const [key, ...rest] = loc;
  const base = container ?? (typeof key === "number" ? [] : {});
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

  const value: RendererContextValue = {
    env,
    store,
    handler,
    fragments,
    slots,
    primarySource,
    resource: state.resource,
    ui: state.ui,
  };

  return <RendererContext.Provider value={value}>{children}</RendererContext.Provider>;
}
