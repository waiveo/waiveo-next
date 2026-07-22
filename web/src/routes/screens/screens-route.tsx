import { useCallback, useEffect, useMemo, useState } from "react";
import { PageRenderer, type ActionHandler } from "@/renderer";
import { PageHeader, Toaster, toast } from "@/components/kit";
import {
  ApiError,
  collectPages,
  createApi,
  etagForRevision,
  updateWithReview,
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
 * review rather than silently retrying; every other non-2xx surfaces its Problem.
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

/** Surface a non-2xx Problem as a toast quoting the machine code + human detail. */
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
  const [loadError, setLoadError] = useState<string | null>(null);
  // Bumped after every mutation so PageRenderer remounts against the freshly
  // fetched data (the renderer seeds its editable store once, from the initial
  // resource — a reload must re-seed it).
  const [version, setVersion] = useState(0);

  const load = useCallback(async () => {
    try {
      const rows = await collectPages<ScopeNode>((cursor) =>
        client.scopeNodes.list({ selector: "kind=screen", cursor }),
      );
      setScreens(rows);
      setLoadError(null);
    } catch (err) {
      setScreens([]);
      setLoadError(err instanceof ApiError ? (err.detail ?? err.code) : "The service is unreachable.");
    }
  }, [client]);

  useEffect(() => {
    void load();
  }, [load]);

  const reload = useCallback(async () => {
    await load();
    setVersion((v) => v + 1);
  }, [load]);

  const handler: ActionHandler = useMemo(
    () => ({
      // "New": create a screen from the document's itemDefault, enriched with the
      // parent site of an existing screen when one is known, then reload so the
      // fresh row is editable in the detail form (the standard form idiom).
      create: async (_target, itemDefault) => {
        const parent = screens?.[0]?.parent_id ?? undefined;
        const body: ScopeNodeCreate = {
          kind: "screen",
          name: typeof itemDefault.name === "string" ? itemDefault.name : "New screen",
          ...(parent ? { parent_id: parent } : {}),
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
      // current server state for review, never a silent overwrite.
      submit: async (_target, resource) => {
        const meta = idRev(resource);
        if (!meta) return;
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
          reportProblem("Couldn't save the screen", err);
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
    [client, screens, reload],
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
            />
          )}
        </main>
      </div>
      <Toaster />
    </div>
  );
}
