// ui-schema/1 — the page renderer.
//
// The interpreter's front door: validate the page document (a non-conformant
// document NEVER renders — closed sets are closed, UIS-200), then paint one of
// the four page-type layouts (list-detail, settings-form, dashboard, wizard)
// through the widget catalog over the kit, inside the state store + live SSE
// providers. Every layout is fully responsive: the list-detail two-pane stacks
// on narrow, the dashboard grid collapses to one column, the wizard and form
// stack — no page ever scrolls horizontally at 360px.

import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
import { Button } from "@/components/kit";
import {
  collectVocabLabels,
  evalBindingExpr,
  makeMessageResolver,
  resolvePath,
  resolvePathWithLoc,
  type RenderEnv,
  type RenderScope,
} from "./bindings";
import { validatePage } from "./validate";
import { LiveProvider, type EventSourceFactory } from "./live";
import { RendererProvider, useRenderer } from "./state";
import { runAction } from "./actions";
import type { ActionRef, ActionHandler, WidgetNode, WizardController } from "./types";
import { isTruthy } from "./widgets/common";
import { WidgetNodeView } from "./widgets/widget-node";
import { WizardContext } from "./widgets/wizard-context";
import type { ValidationError } from "./schema";

type PageDoc = Record<string, unknown>;

export interface PageRendererProps {
  /** The ui-schema/1 page document (validated here before any paint). */
  doc: unknown;
  /** The page-level resource namespace root Bindings resolve against (UIS-005). */
  data?: Record<string, unknown>;
  /** Initial `$ui` ephemeral state (UIS-104) — usually empty. */
  initialUi?: Record<string, unknown>;
  /** The api-client seam the action verbs dispatch onto (UIS-161). */
  handler?: ActionHandler;
  /** A `msg:` catalog (MAN-003/111); absent, references humanize. */
  messages?: Record<string, string>;
  /** The viewer's active locale for formatDate/formatNumber (UIS-143). */
  locale?: string;
  /** Host-provided fragment content for `slot` widgets (UIS-185). */
  slots?: Record<string, ReactNode>;
  /** EventSource factory for LiveBindings (UIS-109) — a fake in tests. */
  eventSourceFactory?: EventSourceFactory;
  /** Server field-validation errors to surface inline (bind-path → message): a
   * 422 VALIDATION_FAILED's `errors[]` mapped by the host so the offending
   * FormField shows the message, per the api/1 convention. */
  fieldErrors?: Record<string, string>;
  /** Notified when the ephemeral `$ui` tree changes (e.g. a list-detail row
   * selection). Lets the host scope its own out-of-band state (captured field
   * errors, a conflict-review banner) to the record now in view. Not fired on
   * the initial mount. */
  onUiChange?: (ui: Record<string, unknown>) => void;
  /** Notified with the taxonomy errors when a document is rejected. */
  onValidationError?: (errors: ValidationError[]) => void;
}

function defaultLocale(explicit?: string): string {
  if (explicit) return explicit;
  if (typeof navigator !== "undefined" && navigator.language) return navigator.language;
  return "en-US";
}

export function PageRenderer({
  doc,
  data,
  initialUi,
  handler = {},
  messages,
  locale,
  slots = {},
  eventSourceFactory,
  fieldErrors,
  onUiChange,
  onValidationError,
}: PageRendererProps) {
  const result = useMemo(() => validatePage(doc), [doc]);

  useEffect(() => {
    if (!result.ok) onValidationError?.(result.errors);
  }, [result, onValidationError]);

  // A non-conformant document is refused, never partially rendered (UIS-200).
  if (!result.ok) {
    return (
      <div role="alert" data-slot="renderer-invalid" className="rounded-card border border-[color:var(--wv-err)] bg-[color:var(--wv-err-bg)] p-4 text-sm">
        <p className="font-semibold text-[color:var(--wv-err)]">This page could not be rendered.</p>
        <ul className="mt-2 flex flex-col gap-1 font-mono text-[12px] text-muted-foreground">
          {result.errors.map((e, i) => (
            <li key={i}>
              {e.code} · {e.path}
            </li>
          ))}
        </ul>
      </div>
    );
  }

  const page = doc as PageDoc;
  const env: RenderEnv = {
    msg: makeMessageResolver(messages),
    locale: defaultLocale(locale),
    vocabLabels: collectVocabLabels(page),
    ...(fieldErrors ? { fieldErrors } : {}),
  };
  const fragments = (page.fragments as Record<string, WidgetNode>) ?? {};
  const primarySource =
    page.pageType === "settings-form"
      ? String(page.source)
      : page.pageType === "list-detail" && typeof (page.detail as PageDoc)?.source === "string"
        ? String((page.detail as PageDoc).source)
        : undefined;

  return (
    <LiveProvider factory={eventSourceFactory}>
      <RendererProvider
        initialResource={data ?? {}}
        initialUi={initialUi ?? {}}
        env={env}
        handler={handler}
        fragments={fragments}
        slots={slots}
        primarySource={primarySource}
        onUiChange={onUiChange}
      >
        <PageBody page={page} />
      </RendererProvider>
    </LiveProvider>
  );
}

function PageBody({ page }: { page: PageDoc }) {
  const ctx = useRenderer();
  const contextFeeds = useMemo(() => {
    const feeds: Record<string, unknown> = {};
    const cmap = page.context && typeof page.context === "object" ? (page.context as Record<string, { collection?: string }>) : {};
    for (const [name, ref] of Object.entries(cmap)) {
      const coll = ref?.collection;
      feeds[name] = coll ? (ctx.resource as Record<string, unknown>)?.[coll] : undefined;
    }
    return feeds;
  }, [page, ctx.resource]);

  const base: RenderScope = {
    root: ctx.resource,
    current: ctx.resource,
    currentPath: [],
    currentTree: "resource",
    ui: ctx.ui,
    context: contextFeeds,
  };

  switch (page.pageType) {
    case "list-detail":
      return <ListDetailLayout page={page} base={base} />;
    case "settings-form":
      return <SettingsFormLayout page={page} base={base} />;
    case "dashboard":
      return <DashboardLayout page={page} base={base} />;
    case "wizard":
      return <WizardLayout page={page} base={base} />;
    default:
      return null;
  }
}

// ── list-detail (UIS-020–024) ───────────────────────────────────────────────

/** Place `value` at a (possibly nested) dotted/slashed source path inside a fresh
 * copy of `base`, shallow-cloning each level along the way. A flat source (e.g.
 * `automations`) is the one-segment special case; a multi-segment source (e.g.
 * `collection.rows`, a form the root-Binding grammar accepts) must land exactly
 * where the display's own `source` Binding resolves — not only at the leaf key,
 * which would strand the fetched rows at the wrong location. */
function patchAtPath(base: Record<string, unknown>, path: string, value: unknown): Record<string, unknown> {
  const segs = path
    .split(/[./]/)
    .map((s) => s.split("[")[0])
    .filter((s) => s.length > 0);
  const root: Record<string, unknown> = { ...base };
  if (segs.length === 0) return root;
  let cursor = root;
  for (let i = 0; i < segs.length - 1; i++) {
    const key = segs[i];
    const next = cursor[key];
    const cloned: Record<string, unknown> =
      next !== null && typeof next === "object" && !Array.isArray(next)
        ? { ...(next as Record<string, unknown>) }
        : {};
    cursor[key] = cloned;
    cursor = cloned;
  }
  cursor[segs[segs.length - 1]] = value;
  return root;
}

// The reserved `$ui` slot a list-detail create draft lives in (the New idiom,
// UIS-021). Double-underscore: renderer-reserved, never an author-addressable
// binding target — a page's own `$ui` bookkeeping (UIS-104) never collides with it.
const CREATE_DRAFT_KEY = "__draft";

/**
 * Whether a `$ui` snapshot represents an OPEN list-detail create draft (UIS-021).
 *
 * A host seam (the one wired to `onUiChange`) uses this to reset its own
 * out-of-band state — captured 422 field errors, a conflict-review banner — when a
 * FRESH draft is entered. It has to: a draft keeps `$ui.selected` null across its
 * whole New → Save → Cancel → New lifecycle, so a selection-identity guard alone
 * can't tell that a prior, already-failed draft was dismissed and a new blank one
 * begun. Field errors are keyed only by bind-path (no record identity), so without
 * this reset a prior attempt's error would render on the brand-new, untouched draft.
 */
export function isCreateDraftUi(ui: Record<string, unknown>): boolean {
  return ui[CREATE_DRAFT_KEY] != null;
}

/**
 * Seed a create draft from the page's `newAction` (the declarative create idiom,
 * UIS-021/164). The draft is a fresh in-memory record the detail form binds to:
 *
 *   "newAction": {
 *     "verb": "create",
 *     "target": "<collection>",          // where Save POSTs the new row (UIS-160/161)
 *     "itemDefault": { ...author fields }, // literal-only seed (UIS-108/109)
 *     "scopeFrom": "<Binding>",           // OPTIONAL — sources the universal-envelope
 *                                          //   scope_node; absent → the host supplies
 *                                          //   the current site
 *     "lifecycle": "draft"                // OPTIONAL — the initial lifecycle_state for
 *                                          //   a draft-publish collection
 *   }
 *
 * `itemDefault` supplies the author-owned fields; `scopeFrom`/`lifecycle` supply the
 * universal-envelope fields a new row requires. Both envelope keys are optional — a
 * page that leaves scope to the host omits `scopeFrom`, and a collection that is not
 * draft-publish omits `lifecycle`.
 */
function buildCreateDraft(action: ActionRef, base: RenderScope): Record<string, unknown> {
  const itemDefault = action.itemDefault;
  const seed: Record<string, unknown> =
    itemDefault !== null && typeof itemDefault === "object" && !Array.isArray(itemDefault)
      ? { ...(itemDefault as Record<string, unknown>) }
      : {};
  if (typeof action.scopeFrom === "string") {
    const scopeNode = resolvePath(action.scopeFrom, base);
    if (scopeNode != null) seed.scope_node = scopeNode;
  }
  if (typeof action.lifecycle === "string") seed.lifecycle_state = action.lifecycle;
  return seed;
}

function ListDetailLayout({ page, base }: { page: PageDoc; base: RenderScope }) {
  const ctx = useRenderer();
  const list = page.list as { source: unknown; display: WidgetNode };
  const detail = page.detail as { source: unknown; root: WidgetNode; emptyMsg?: string };
  const newAction = page.newAction as ActionRef | undefined;

  // The create-draft session (UIS-021): New enters a blank detail form bound to a
  // fresh in-memory record held at `$ui.__draft` — NOT the last selection — and
  // Save persists it as a create. `draft` is the live draft (null when not creating).
  const draft = (ctx.ui[CREATE_DRAFT_KEY] ?? null) as Record<string, unknown> | null;
  const creating = draft != null;

  const cancelDraft = useCallback(() => {
    ctx.store.write({ tree: "ui", loc: [CREATE_DRAFT_KEY] }, undefined);
  }, [ctx.store]);

  // Escape exits the draft from anywhere on the page (Cancel is the explicit
  // affordance; Escape is its keyboard equivalent), mounted only while creating.
  useEffect(() => {
    if (!creating) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") cancelDraft();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [creating, cancelDraft]);

  const startDraft = () => {
    if (!newAction) return;
    // Only `create` enters a draft; any other newAction verb dispatches as an
    // ordinary action (this contract reserves newAction for create today, UIS-021).
    if (newAction.verb !== "create") {
      runAction(newAction, base, ctx);
      return;
    }
    // New while an item is selected clears the selection FIRST, then seeds the
    // draft — the detail switches from the old row to the blank create form.
    ctx.store.write({ tree: "ui", loc: ["selected"] }, null);
    ctx.store.write({ tree: "ui", loc: [CREATE_DRAFT_KEY] }, buildCreateDraft(newAction, base));
  };

  const detailSource = String(detail.source);
  const resolved = resolvePathWithLoc(detailSource, base);
  const hasSelection = !creating && resolved.value != null;
  const selectionScope: RenderScope = hasSelection
    ? { ...base, root: resolved.value, current: resolved.value, currentPath: resolved.loc ?? [], currentTree: resolved.tree }
    : base;

  // The draft binds the detail form to the `$ui.__draft` record; `draftCreateTarget`
  // routes its `submit` to the create path (actions.tsx) rather than an update.
  const draftTarget = newAction && typeof newAction.target === "string" ? newAction.target : undefined;
  const draftScope: RenderScope = {
    ...base,
    root: draft ?? {},
    current: draft ?? {},
    currentPath: [CREATE_DRAFT_KEY],
    currentTree: "ui",
    ...(draftTarget !== undefined ? { draftCreateTarget: draftTarget } : {}),
  };

  const emptyMsg = detail.emptyMsg ? ctx.env.msg(String(detail.emptyMsg)) : "Select an item to see its detail.";

  const paginated =
    list.source !== null && typeof list.source === "object" && (list.source as PageDoc).paginated === true;

  return (
    <div className="grid gap-6 lg:grid-cols-2">
      <section aria-label="List" className="flex min-w-0 flex-col gap-3">
        {newAction ? (
          <div className="flex justify-end">
            <Button variant="default" className="wv-touch" onClick={startDraft}>
              New
            </Button>
          </div>
        ) : null}
        {paginated ? (
          <PaginatedList source={list.source as PageDoc} display={list.display} base={base} />
        ) : (
          <WidgetNodeView node={list.display} scope={base} depth={0} />
        )}
      </section>
      <section aria-label="Detail" className="flex min-w-0 flex-col gap-3">
        {creating ? (
          <div className="flex flex-col gap-3">
            <div className="flex justify-end">
              <Button variant="ghost" className="wv-touch" onClick={cancelDraft}>
                Cancel
              </Button>
            </div>
            <WidgetNodeView node={detail.root} scope={draftScope} depth={0} />
          </div>
        ) : hasSelection ? (
          <WidgetNodeView node={detail.root} scope={selectionScope} depth={0} />
        ) : (
          <p className="text-sm text-muted-foreground">{emptyMsg}</p>
        )}
      </section>
    </div>
  );
}

/** A keyset-cursor paginated list.source (UIS-023/024): fetch page-by-page over
 * the api/1 cursor convention, accumulate rows in memory, and render `display`
 * against them; the "load more" affordance is suppressed once a fetch returns
 * `cursor: null` (no further page). */
function PaginatedList({ source, display, base }: { source: PageDoc; display: WidgetNode; base: RenderScope }) {
  const ctx = useRenderer();
  const path = String(source.path);
  const limit = typeof source.limit === "number" ? source.limit : undefined;
  const [items, setItems] = useState<unknown[]>([]);
  const [cursor, setCursor] = useState<string | null | undefined>(undefined);
  const [loading, setLoading] = useState(false);

  const load = useCallback(
    async (c: string | null) => {
      if (!ctx.handler.fetchPage) return;
      setLoading(true);
      try {
        const res = await ctx.handler.fetchPage(path, c, limit);
        setItems((prev) => [...prev, ...res.items]);
        setCursor(res.cursor);
      } finally {
        setLoading(false);
      }
    },
    [ctx.handler, path, limit],
  );

  useEffect(() => {
    void load(null);
  }, [load]);

  const listScope: RenderScope = {
    ...base,
    current: patchAtPath(base.current as Record<string, unknown>, path, items),
  };

  return (
    <div className="flex flex-col gap-3">
      <WidgetNodeView node={display} scope={listScope} depth={0} />
      {typeof cursor === "string" ? (
        <div className="flex justify-center">
          <Button variant="outline" className="wv-touch" disabled={loading} onClick={() => void load(cursor)}>
            Load more
          </Button>
        </div>
      ) : null}
    </div>
  );
}

// ── settings-form (UIS-030/031) ─────────────────────────────────────────────

function SettingsFormLayout({ page, base }: { page: PageDoc; base: RenderScope }) {
  const ctx = useRenderer();
  const resolved = resolvePathWithLoc(String(page.source), base);
  const scope: RenderScope = {
    ...base,
    root: resolved.value,
    current: resolved.value,
    currentPath: resolved.loc ?? [],
    currentTree: "resource",
  };
  const sections = (page.sections as Array<{ titleMsg?: string; fields: WidgetNode[] }>) ?? [];
  const actions = (page.actions as WidgetNode[]) ?? [];

  return (
    <form className="mx-auto flex w-full max-w-2xl flex-col gap-8" onSubmit={(e) => e.preventDefault()}>
      {sections.map((section, si) => (
        <section key={si} className="flex flex-col gap-5 rounded-card border border-border bg-card p-5 wv-elevation sm:p-6">
          {section.titleMsg ? (
            <h2 className="font-display text-[17px] font-semibold">{ctx.env.msg(String(section.titleMsg))}</h2>
          ) : null}
          <div className="flex flex-col gap-5">
            {section.fields.map((field, fi) => (
              <WidgetNodeView key={fi} node={field} scope={scope} depth={0} />
            ))}
          </div>
        </section>
      ))}
      <div className="flex flex-wrap gap-3">
        {actions.map((action, ai) => (
          <WidgetNodeView key={ai} node={action} scope={scope} depth={0} />
        ))}
      </div>
    </form>
  );
}

// ── dashboard (UIS-040/041) ─────────────────────────────────────────────────

const TILE_SPAN: Record<string, string> = {
  small: "",
  medium: "sm:col-span-2",
  large: "sm:col-span-2 lg:col-span-4",
};

function DashboardLayout({ page, base }: { page: PageDoc; base: RenderScope }) {
  const tiles = (page.tiles as Array<{ size: string; widget: WidgetNode }>) ?? [];
  return (
    <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
      {tiles.map((tile, ti) => (
        <div key={ti} className={`min-w-0 ${TILE_SPAN[tile.size] ?? ""}`}>
          {/* Each tile resolves its bindings independently as root bindings
              (UIS-040) — the page has no single page-wide bound resource. */}
          <WidgetNodeView node={tile.widget} scope={base} depth={0} />
        </div>
      ))}
    </div>
  );
}

// ── wizard (UIS-050–052) ────────────────────────────────────────────────────

function WizardLayout({ page, base }: { page: PageDoc; base: RenderScope }) {
  const ctx = useRenderer();
  const steps = (page.steps as Array<{ id: string; titleMsg: string; root: WidgetNode; canAdvanceIf?: unknown }>) ?? [];
  const [stepIndex, setStepIndex] = useState(0);
  const draftSource = page.draftSource;

  // One shared draft Scope across all steps (UIS-052): a real backing resource
  // when draftSource is declared, else the ephemeral $ui.draft (UIS-051).
  let draftScope: RenderScope;
  if (typeof draftSource === "string") {
    const resolved = resolvePathWithLoc(draftSource, base);
    draftScope = {
      ...base,
      root: resolved.value ?? {},
      current: resolved.value ?? {},
      currentPath: resolved.loc ?? [],
      currentTree: "resource",
    };
  } else {
    const draft = (base.ui.draft as Record<string, unknown>) ?? {};
    draftScope = { ...base, root: draft, current: draft, currentPath: ["draft"], currentTree: "ui" };
  }

  const step = steps[stepIndex];
  const canAdvance = step?.canAdvanceIf === undefined ? true : isTruthy(evalBindingExpr(step.canAdvanceIf, draftScope, ctx.env));

  const controller: WizardController = {
    next: () => {
      if (canAdvance) setStepIndex((i) => Math.min(i + 1, steps.length - 1));
    },
    back: () => setStepIndex((i) => Math.max(i - 1, 0)),
    finish: () => {
      const onFinish = page.onFinish as ActionRef | undefined;
      if (onFinish) runAction(onFinish, draftScope, ctx);
    },
  };

  const isFirst = stepIndex === 0;
  const isLast = stepIndex === steps.length - 1;
  if (!step) return null;

  return (
    <WizardContext.Provider value={controller}>
      <div className="mx-auto flex w-full max-w-xl flex-col gap-6">
        <ol className="flex flex-wrap gap-x-2 gap-y-1 text-[13px]">
          {steps.map((s, i) => (
            <li
              key={s.id}
              aria-current={i === stepIndex ? "step" : undefined}
              className={
                i === stepIndex
                  ? "font-semibold text-foreground"
                  : "text-muted-foreground"
              }
            >
              {i > 0 ? <span className="mr-2 text-muted-foreground">/</span> : null}
              {ctx.env.msg(String(s.titleMsg))}
            </li>
          ))}
        </ol>
        <h2 className="font-display text-[19px] font-semibold">{ctx.env.msg(String(step.titleMsg))}</h2>
        <div className="flex flex-col gap-5">
          <WidgetNodeView node={step.root} scope={draftScope} depth={0} />
        </div>
        <div className="flex justify-between gap-3">
          <Button variant="ghost" className="wv-touch" onClick={() => controller.back()} disabled={isFirst}>
            Back
          </Button>
          {isLast ? (
            <Button variant="default" className="wv-touch" onClick={() => controller.finish()}>
              Finish
            </Button>
          ) : (
            <Button variant="default" className="wv-touch" onClick={() => controller.next()} disabled={!canAdvance}>
              Next
            </Button>
          )}
        </div>
      </div>
    </WizardContext.Provider>
  );
}
