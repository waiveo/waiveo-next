import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { PageRenderer, type ActionHandler } from "@/renderer";
import { FormField, PageHeader, Toaster, toast } from "@/components/kit";
import {
  ApiError,
  collectPages,
  createApi,
  etagForRevision,
  updateWithReview,
  type FieldErrors,
  type ScopeNode,
  type ScopeNodeCreate,
  type ScopeNodeUpdate,
  type WaiveoApi,
} from "@/api";
import screensPageDoc from "./page.uis.json";

/**
 * The Screens route — the fleet's scope-nodes of kind `screen`, as a DOGFOODED
 * ui-schema/1 page. The list-detail document (page.uis.json) is validated and
 * rendered by the SAME PageRenderer an extension page goes through: the list, the
 * detail form, and the create/edit/delete affordances are all expressed in the
 * ui-schema grammar, not hand-built React. This file is only the host seam — it
 * feeds the live scope-nodes in as the page's bound data and wires the closed
 * action verbs (create/submit/delete) onto the typed api/1 client, applying the
 * conventions the client owns: creates carry an Idempotency-Key; edits and
 * deletes carry the If-Match derived from the record's revision (no unconditional
 * overwrite); a 412 REVISION_CONFLICT re-reads and surfaces the current state for
 * review rather than silently retrying; every other non-2xx surfaces its Problem,
 * and a 422 VALIDATION_FAILED's per-field `errors[]` land on the offending
 * FormField (not only a toast).
 */

// The message catalog the document's `msg:` references resolve against.
const messages: Record<string, string> = {
  "msg:screens.col.name": "Name",
  "msg:screens.col.tz": "Time zone",
  "msg:screens.detail.title": "Screen details",
  "msg:screens.detail.empty": "Select a screen to edit it, or add a new one.",
  "msg:screens.detail.name": "Display name",
  "msg:screens.detail.tz": "Time zone",
  "msg:screens.detail.tzPlaceholder": "e.g. America/New_York",
  "msg:screens.detail.save": "Save changes",
  "msg:screens.detail.delete": "Delete screen",
};

/** The scope-node kinds that may parent a screen (DAT-003: a screen is a child of
 * a site or a group). A new screen MUST be attached under one of these — the
 * server rejects a non-org node with a null parent_id (SCOPE_NODE_PARENT_INVALID),
 * so the parent is chosen at create time, never inferred from a sibling screen. */
const PARENT_SELECTOR = "kind in (site,group)";

/** A 422 VALIDATION_FAILED's per-field errors, if this failure is one — the map a
 * form drops straight onto its FormField error slots. `null` for any other error
 * (a conflict, an unreachable service, a non-field Problem). */
function fieldErrorsOf(err: unknown): FieldErrors | null {
  if (err instanceof ApiError && err.status === 422 && Object.keys(err.fieldErrors).length > 0) {
    return err.fieldErrors;
  }
  return null;
}

/** Surface a non-2xx Problem as a toast quoting the machine code + human detail.
 * Used for failures with no on-screen field to pin the message to. */
function reportProblem(context: string, err: unknown): void {
  if (err instanceof ApiError) {
    const fieldMsg = Object.values(err.fieldErrors)[0];
    toast.error(`${context}: ${fieldMsg ?? err.detail ?? err.code}`);
  } else {
    toast.error(`${context}: the service is unreachable.`);
  }
}

/** Pull `{id, revision}` off the resource a ui-schema action hands back. */
function idRev(resource: unknown): { id: string; revision: number } | null {
  if (resource && typeof resource === "object") {
    const r = resource as Partial<ScopeNode>;
    if (typeof r.id === "string" && typeof r.revision === "number") {
      return { id: r.id, revision: r.revision };
    }
  }
  return null;
}

export default function ScreensRoute({ api }: { api?: WaiveoApi }) {
  const client = useMemo(() => api ?? createApi(), [api]);
  const [screens, setScreens] = useState<ScopeNode[] | null>(null);
  // The candidate parents a new screen can be attached under (sites + groups),
  // loaded from the tree — NOT inferred from an existing screen's parent (which
  // is absent when the list is empty and wrong when the org spans several sites).
  const [parents, setParents] = useState<ScopeNode[]>([]);
  // The parent the next "New" attaches under; defaults to the sole/first candidate
  // and is chosen explicitly via the picker when the org has more than one site.
  const [targetParentId, setTargetParentId] = useState<string | null>(null);
  // Field-validation errors from the last save (422), keyed by field → message, so
  // the detail form's FormField shows the message inline; cleared on each attempt.
  const [fieldErrors, setFieldErrors] = useState<FieldErrors>({});
  const [loadError, setLoadError] = useState<string | null>(null);
  // Bumped after every mutation so PageRenderer remounts against the freshly
  // fetched data (the renderer seeds its editable store once, from the initial
  // resource — a reload must re-seed it).
  const [version, setVersion] = useState(0);

  const activeParent = targetParentId ?? parents[0]?.id ?? null;
  // A ref so the (stable) create handler reads the live selection without being
  // rebuilt — and re-dispatched to the renderer — on every picker change.
  const activeParentRef = useRef<string | null>(activeParent);
  activeParentRef.current = activeParent;

  const load = useCallback(async () => {
    try {
      const [rows, parentRows] = await Promise.all([
        collectPages<ScopeNode>((cursor) => client.scopeNodes.list({ selector: "kind=screen", cursor })),
        collectPages<ScopeNode>((cursor) => client.scopeNodes.list({ selector: PARENT_SELECTOR, cursor })),
      ]);
      setScreens(rows);
      setParents(parentRows);
      setLoadError(null);
    } catch (err) {
      setScreens([]);
      setParents([]);
      setLoadError(err instanceof ApiError ? (err.detail ?? err.code) : "The service is unreachable.");
    }
  }, [client]);

  useEffect(() => {
    void load();
  }, [load]);

  const reload = useCallback(async () => {
    setFieldErrors({});
    await load();
    setVersion((v) => v + 1);
  }, [load]);

  const handler: ActionHandler = useMemo(
    () => ({
      // "New": create a screen from the document's itemDefault, attached under the
      // chosen parent site/group, then reload so the fresh row is editable in the
      // detail form (the standard form idiom). A screen MUST have a parent, so with
      // no candidate site there is nothing to attach it to — say so plainly rather
      // than POST a body the server would reject with SCOPE_NODE_PARENT_INVALID.
      create: async (_target, itemDefault) => {
        setFieldErrors({});
        const parent = activeParentRef.current;
        if (!parent) {
          toast.error("Add a site before adding a screen — a screen must live under a site or group.");
          return;
        }
        const body: ScopeNodeCreate = {
          kind: "screen",
          name: typeof itemDefault.name === "string" ? itemDefault.name : "New screen",
          parent_id: parent,
        };
        try {
          const created = await client.scopeNodes.create(body);
          toast.success(`Added ${created.data.name}`);
          await reload();
        } catch (err) {
          reportProblem("Couldn't add the screen", err);
        }
      },
      // "Save changes": persist the edited detail record under its If-Match, using
      // the standard optimistic-concurrency flow — a 412 re-reads and surfaces the
      // current server state for review, never a silent overwrite; a 422's per-field
      // errors land on the offending FormField (not only a toast).
      submit: async (_target, resource) => {
        const meta = idRev(resource);
        if (!meta) return;
        setFieldErrors({});
        const r = resource as ScopeNode;
        const patch: ScopeNodeUpdate = { name: r.name, ...(r.tz !== undefined ? { tz: r.tz } : {}) };
        try {
          const outcome = await updateWithReview(
            client.scopeNodes,
            meta.id,
            patch,
            etagForRevision(meta.revision),
          );
          if (outcome.status === "conflict") {
            toast.error("This screen changed elsewhere. Review the current values and try again.");
          } else {
            toast.success("Saved changes");
          }
          await reload();
        } catch (err) {
          const fields = fieldErrorsOf(err);
          if (fields) {
            // Keep the form mounted (no reload) so the edited values stand and the
            // message can pin to the field the operator must fix.
            setFieldErrors(fields);
            toast.error("Couldn't save the screen — please fix the highlighted fields.");
          } else {
            reportProblem("Couldn't save the screen", err);
          }
        }
      },
      // "Delete screen": remove under its If-Match, then reload.
      remove: async (_target, resource) => {
        const meta = idRev(resource);
        if (!meta) return;
        try {
          await client.scopeNodes.remove(meta.id, etagForRevision(meta.revision));
          toast.success("Deleted screen");
          await reload();
        } catch (err) {
          reportProblem("Couldn't delete the screen", err);
        }
      },
    }),
    [client, reload],
  );

  return (
    <div className="min-h-screen bg-background text-foreground">
      <div className="mx-auto flex max-w-[1200px] flex-col gap-6 px-4 py-6 lg:px-8">
        <PageHeader
          variant="hero"
          title="Screens"
          description="Every display in the fleet. Add a screen, name it, set its time zone — authored through the same declarative page any extension renders through."
        />
        {loadError ? (
          <p role="alert" className="text-sm text-[color:var(--wv-err)]">
            Couldn't load screens — {loadError}
          </p>
        ) : null}
        {/* grammar-gap: the list-detail grammar's `newAction` is a bare verb with a
            static itemDefault — it cannot carry the required parent choice, and a
            screen's parent is immutable after create (ScopeNodeUpdate has no
            parent_id). When the org spans more than one site the operator MUST pick
            where a new screen lands, so the target-site picker is a first-party host
            control until the grammar grows a create-form with a parent selector. */}
        {parents.length > 1 ? (
          <div className="max-w-sm">
            <FormField label="Add new screens under">
              {(field) => (
                <select
                  {...field}
                  className="flex min-h-[44px] w-full min-w-0 rounded-input border border-border bg-transparent px-3 py-1 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
                  value={activeParent ?? ""}
                  onChange={(e) => setTargetParentId(e.target.value)}
                >
                  {parents.map((p) => (
                    <option key={p.id} value={p.id}>
                      {p.name}
                    </option>
                  ))}
                </select>
              )}
            </FormField>
          </div>
        ) : null}
        <main className="min-w-0">
          {screens === null ? (
            <p className="text-sm text-muted-foreground">Loading screens…</p>
          ) : (
            <PageRenderer
              key={version}
              doc={screensPageDoc}
              data={{ screens }}
              messages={messages}
              handler={handler}
              fieldErrors={fieldErrors}
            />
          )}
        </main>
      </div>
      <Toaster />
    </div>
  );
}
