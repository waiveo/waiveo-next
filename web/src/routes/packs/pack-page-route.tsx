import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useParams } from "react-router";
import { AlertTriangle } from "lucide-react";
import { PageRenderer, isCreateDraftUi, validatePage, type ActionHandler, type ValidationResult } from "@/renderer";
import { EmptyState, PageHeader, Toaster, toast } from "@/components/kit";
import {
  ApiError,
  collectPages,
  createApi,
  etagForRevision,
  updateWithReview,
  type FieldErrors,
  type PackRow,
  type PackRowWrite,
  type ScopeNode,
  type WaiveoApi,
} from "@/api";
import { loadPackCatalog, primaryCollection, resolveTitle } from "./catalog";

/**
 * The scope node a create attaches a new pack row under (MAN-051: a pack row
 * attaches to ANY scope node). A self-hosted deployment is one-org/one-site and
 * ALWAYS carries a resolvable scope after `make dev-up`, so this resolves the
 * deployment's ROOT deterministically: prefer the org root, else a site, else the
 * topmost node of any kind (a node whose parent is not itself a queryable row).
 * Returns `null` ONLY for a genuinely scope-less deployment — which cannot happen
 * after `make dev-up`.
 *
 * The org root is now always the answer against a seeded store, and the two
 * fallbacks are defence rather than the live path. They used to be the live path:
 * the demo seed materialized only a Demo Site whose org ancestor was a reference to
 * a node that was never inserted, so `kind=org` alone found nothing and a cold-open
 * create would have wrongly refused. The server no longer accepts that store — a
 * scope node's parent has to resolve — so the seed inserts the org root it names.
 * The fallbacks stay because this function must not depend on that: it also runs
 * against a per-scope subtree, where the root legitimately is not carried.
 */
export function resolveDefaultScopeNode(nodes: ScopeNode[]): string | null {
  if (nodes.length === 0) return null;
  const byId = (a: ScopeNode, b: ScopeNode) => (a.id < b.id ? -1 : a.id > b.id ? 1 : 0);
  const firstOfKind = (kind: ScopeNode["kind"]): string | null =>
    nodes.filter((n) => n.kind === kind).sort(byId)[0]?.id ?? null;
  // The org root first, then a site (the self-hosted invariant guarantees one),
  // then the topmost node of ANY kind — a node whose parent is absent from the
  // queryable set — with a sortable-ULID tiebreak so the choice is deterministic
  // across page loads.
  const org = firstOfKind("org");
  if (org) return org;
  const site = firstOfKind("site");
  if (site) return site;
  const ids = new Set(nodes.map((n) => n.id));
  const roots = nodes.filter((n) => n.parent_id === null || !ids.has(n.parent_id));
  return [...(roots.length > 0 ? roots : nodes)].sort(byId)[0].id;
}

/**
 * The bounded selector for the create-default's COMMON case: the deployment's org
 * and/or site roots. Self-hosted is one-org/one-site (data-model/1), so this is a
 * 1–2 row query — NOT a walk of every group and screen in the fleet. Mirrors the
 * screens route's `PARENT_SELECTOR` idiom (a filtered scope-node query, never the
 * whole tree, to pick a create target).
 */
const DEFAULT_SCOPE_SELECTOR = "kind in (org,site)";

/**
 * Resolve the create-default scope node against a live client. The common path is a
 * bounded `kind in (org,site)` query — `resolveDefaultScopeNode` prefers the org,
 * else the site (the self-hosted invariant guarantees one after `make dev-up`). ONLY
 * a deployment with neither an org nor a site (effectively unreachable post-`make
 * dev-up`) pays the full unfiltered walk, reserved as the last-resort fallback so a
 * pack page whose selector matched a root never fetches every scope node just to pick
 * a default. Returns `null` only for a genuinely scope-less deployment.
 */
async function resolveDefaultScopeNodeVia(
  scopeNodes: WaiveoApi["scopeNodes"],
): Promise<string | null> {
  const preferred = await collectPages<ScopeNode>((cursor) =>
    scopeNodes.list({ selector: DEFAULT_SCOPE_SELECTOR, cursor }),
  );
  const fromPreferred = resolveDefaultScopeNode(preferred);
  if (fromPreferred) return fromPreferred;
  // Neither an org nor a site exists — walk the full set to pick any root node
  // (MAN-051: a pack row attaches to ANY scope node). This branch is unreachable
  // after a stock `make dev-up`; it exists so a non-standard tree still resolves.
  const all = await collectPages<ScopeNode>((cursor) => scopeNodes.list({ cursor }));
  return resolveDefaultScopeNode(all);
}

/**
 * The pack page route (`/p/{publisher}/{name}/{path}`) — an INSTALLED extension's
 * page, rendered through the EXACT same renderer + kit a core page uses. This is
 * the uniformity promise reaching third-party content: the route fetches the page
 * document + the pack's locale catalog, validates the document, and paints it via
 * PageRenderer with the catalog as its `msg:` source (en-fallback per MAN-111).
 *
 * Two safety invariants ride the renderer, unchanged: a document that fails
 * `validatePage` NEVER renders — it shows the standard error EmptyState carrying
 * the taxonomy code, not a crash or a partial paint; and NO pack code executes —
 * the page doc, the catalog, and every row are DATA the console fetched. A
 * list-detail page's rows come from the pack-data api/1 surface (the SAME
 * ResourceModule shape a scope-node uses), and its create/edit/delete verbs
 * dispatch onto that surface through the renderer's existing action seam, honoring
 * If-Match/412 and 422 field mapping exactly as every core editor does.
 */

/** A 422 VALIDATION_FAILED's per-field errors, or null for any other failure. */
function fieldErrorsOf(err: unknown): FieldErrors | null {
  if (err instanceof ApiError && err.status === 422 && Object.keys(err.fieldErrors).length > 0) {
    return err.fieldErrors;
  }
  return null;
}

/** Surface a non-2xx Problem as a toast quoting the machine code + human detail. */
function reportProblem(context: string, err: unknown): void {
  if (err instanceof ApiError) {
    const fieldMsg = Object.values(err.fieldErrors)[0];
    toast.error(`${context}: ${fieldMsg ?? err.detail ?? err.code}`);
  } else {
    toast.error(`${context}: the service is unreachable.`);
  }
}

/** Pull the `{entity_id, revision}` a pack-data mutation needs off the row a
 * ui-schema action hands back. */
function rowRef(resource: unknown): { id: string; revision: number } | null {
  if (resource && typeof resource === "object") {
    const r = resource as { entity_id?: unknown; revision?: unknown };
    if (typeof r.entity_id === "string" && typeof r.revision === "number") {
      return { id: r.entity_id, revision: r.revision };
    }
  }
  return null;
}

interface Loaded {
  doc: unknown;
  validation: ValidationResult;
  messages: Record<string, string>;
  collection: string | null;
  defaultScopeNode: string | null;
  pageTitle: string;
  packTitle: string;
}

export default function PackPageRoute({ api }: { api?: WaiveoApi }) {
  const client = useMemo(() => api ?? createApi(), [api]);
  const params = useParams();
  const publisher = params.publisher ?? "";
  const name = params.name ?? "";
  const path = params["*"] ?? "";
  const packId = `${publisher}/${name}`;

  const [loaded, setLoaded] = useState<Loaded | null>(null);
  const [rows, setRows] = useState<PackRow[]>([]);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [fieldErrors, setFieldErrors] = useState<FieldErrors>({});
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const selectedIdRef = useRef<string | null>(null);
  // Whether a create draft was open on the last `$ui` we saw, so onUiChange can spot
  // a FRESH draft opening (a New→Cancel→New cycle the selection guard can't, since
  // the draft leaves `$ui.selected` null throughout).
  const draftOpenRef = useRef(false);
  const [conflictReview, setConflictReview] = useState(false);
  // Bumped after every mutation so PageRenderer remounts against the freshly
  // fetched rows (the renderer seeds its editable store once, from the initial
  // resource — a reload must re-seed it).
  const [version, setVersion] = useState(0);

  const loadRows = useCallback(
    (coll: string) =>
      collectPages<PackRow>((cursor) => client.packData(packId, coll).list({ cursor })),
    [client, packId],
  );

  const load = useCallback(async () => {
    setLoaded(null);
    setLoadError(null);
    setSelectedId(null);
    selectedIdRef.current = null;
    setConflictReview(false);
    setFieldErrors({});
    try {
      const [packRead, rawDoc, messages] = await Promise.all([
        client.packs.get(packId),
        client.packs.pageDoc(packId, path),
        loadPackCatalog(client.packs, packId),
      ]);
      const manifest = packRead.data.manifest;
      const validation = validatePage(rawDoc);
      const collectionNames = new Set((manifest.dataModel?.collections ?? []).map((c) => c.name));
      const collection = validation.ok ? primaryCollection(rawDoc, collectionNames) : null;

      // A row must carry a scope_node (MAN-051). Resolve the deployment's root scope
      // deterministically so a cold-open create always has a target: the org root if
      // present, else the site (the self-hosted invariant guarantees one after
      // `make dev-up`), else any scope node — a pack row attaches to ANY (MAN-051).
      // A bounded `kind in (org,site)` query serves the common case; only a tree with
      // neither pays the full walk (see resolveDefaultScopeNodeVia).
      // auth-seam: consent (MAN-022) is auto-granted in the dev-POC and a created row
      // is attached to this resolved root; a real deployment gates the write on
      // granted consent and lets the operator choose the target scope.
      const defaultScopeNode = await resolveDefaultScopeNodeVia(client.scopeNodes).catch(() => null);

      const pageEntry = (manifest.ui?.pages ?? []).find((p) => p.path === path);
      const pageTitle = pageEntry
        ? resolveTitle(messages, pageEntry.titleMsg)
        : resolveTitle(messages, manifest.displayName);
      const packTitle = resolveTitle(messages, manifest.displayName);

      setRows(collection ? await loadRows(collection) : []);
      setLoaded({ doc: rawDoc, validation, messages, collection, defaultScopeNode, pageTitle, packTitle });
    } catch (err) {
      setLoadError(
        err instanceof ApiError ? (err.detail ?? err.code) : "The extension page is unreachable.",
      );
    }
  }, [client, packId, path, loadRows]);

  useEffect(() => {
    void load();
  }, [load]);

  const collection = loaded?.collection ?? null;

  const reload = useCallback(async () => {
    setFieldErrors({});
    if (collection) setRows(await loadRows(collection));
    setVersion((v) => v + 1);
  }, [collection, loadRows]);

  // The renderer owns the selection (`$ui.selected`); moving to a different row
  // retires any captured 422 field errors (keyed by bind-path, no row identity)
  // and exits the conflict-review state that belonged to the row just left.
  // Entering a FRESH create draft (UIS-021) does the same reset — otherwise a prior
  // failed create's field error would leak onto the brand-new blank draft, since the
  // draft keeps `$ui.selected` null across its whole New→Cancel→New lifecycle.
  const onUiChange = useCallback((ui: Record<string, unknown>) => {
    const next = typeof ui.selected === "string" ? ui.selected : null;
    const draftOpen = isCreateDraftUi(ui);
    const enteringDraft = draftOpen && !draftOpenRef.current;
    draftOpenRef.current = draftOpen;
    if (next === selectedIdRef.current && !enteringDraft) return;
    selectedIdRef.current = next;
    setSelectedId(next);
    setFieldErrors({});
    setConflictReview(false);
  }, []);

  const defaultScopeNode = loaded?.defaultScopeNode ?? null;

  const handler: ActionHandler = useMemo(
    () => ({
      // "New": create a row from the document's itemDefault, attached under the
      // deployment's resolved root scope, then reload so the fresh row is editable in
      // the detail form.
      create: async (_target, itemDefault) => {
        if (!collection) return;
        setFieldErrors({});
        setConflictReview(false);
        // The draft MAY carry a page-declared scope_node (newAction.scopeFrom,
        // UIS-021); absent, the host attaches the row under the resolved root scope
        // (org → site → any; MAN-051).
        const scopeNode = typeof itemDefault.scope_node === "string" ? itemDefault.scope_node : defaultScopeNode;
        if (!scopeNode) {
          toast.error("This workspace has no scope to attach records to yet.");
          return;
        }
        const body: PackRowWrite = { ...itemDefault, scope_node: scopeNode };
        try {
          const created = await client.packData(packId, collection).create(body);
          toast.success("Added a record");
          // The fresh row becomes the selection (UIS-021): its detail form reopens
          // on the created record rather than collapsing to the empty prompt.
          const newId = created.data.entity_id;
          selectedIdRef.current = newId;
          setSelectedId(newId);
          await reload();
        } catch (err) {
          const fields = fieldErrorsOf(err);
          if (fields) {
            setFieldErrors(fields);
            toast.error("Couldn't add the record — please fix the highlighted fields.");
          } else {
            reportProblem("Couldn't add the record", err);
          }
        }
      },
      // "Save": persist the edited row under its If-Match via the standard
      // optimistic-concurrency flow — a 412 re-reads the current state for review,
      // a 422's per-field errors land on the offending FormField. The whole row is
      // sent; the server ignores host-owned envelope fields (MAN-051).
      submit: async (_target, resource) => {
        if (!collection) {
          // No manifest-declared collection backs this page — every page type but
          // list-detail this wave (a settings-form persists to a target that lands
          // with a later wave). There is nowhere to write, so report honestly
          // rather than fabricating a green "Saved" for a save that never happened.
          toast.error("Saving isn't available for this page yet.");
          return;
        }
        const meta = rowRef(resource);
        if (!meta) return;
        setFieldErrors({});
        setConflictReview(false);
        try {
          const outcome = await updateWithReview(
            client.packData(packId, collection),
            meta.id,
            resource as PackRowWrite,
            etagForRevision(meta.revision),
          );
          if (outcome.status === "conflict") {
            toast.error("This record changed elsewhere. Review the current values and try again.");
            setConflictReview(true);
          } else {
            toast.success("Saved changes");
          }
          await reload();
        } catch (err) {
          const fields = fieldErrorsOf(err);
          if (fields) {
            setFieldErrors(fields);
            toast.error("Couldn't save the record — please fix the highlighted fields.");
          } else {
            reportProblem("Couldn't save the record", err);
          }
        }
      },
      // "Delete": remove under its If-Match, then reload.
      remove: async (_target, resource) => {
        if (!collection) return;
        const meta = rowRef(resource);
        if (!meta) return;
        setConflictReview(false);
        try {
          await client.packData(packId, collection).remove(meta.id, etagForRevision(meta.revision));
          toast.success("Deleted record");
          await reload();
        } catch (err) {
          reportProblem("Couldn't delete the record", err);
        }
      },
      // A paginated `list.source` (UIS-023/024) is fetched page-by-page by the
      // renderer's PaginatedList through this seam — it bypasses the eagerly
      // preloaded resource tree, so an unwired fetchPage would leave the list
      // permanently empty. Serve one api/1 keyset page of the SAME bound
      // collection's rows (its root key IS this page's primary collection); a
      // source that names no declared collection has nothing to page.
      fetchPage: async (_path, cursor, limit) => {
        if (!collection) return { items: [], cursor: null };
        const page = await client
          .packData(packId, collection)
          .list({ cursor, ...(limit !== undefined ? { limit } : {}) });
        return { items: page.items, cursor: page.cursor };
      },
    }),
    [client, packId, collection, defaultScopeNode, reload],
  );

  const data = collection ? { [collection]: rows } : {};

  return (
    <div className="min-h-screen bg-background text-foreground">
      <div className="mx-auto flex max-w-[1200px] flex-col gap-6 px-4 py-6 lg:px-8">
        <PageHeader
          variant="hero"
          title={loaded?.pageTitle ?? "Extension page"}
          description={
            loaded?.packTitle
              ? `Provided by the ${loaded.packTitle} extension — rendered through the same declarative surface every page uses.`
              : "An installed extension's page."
          }
        />
        {loadError ? (
          <p role="alert" className="text-sm text-[color:var(--wv-err)]">
            Couldn't load this page — {loadError}
          </p>
        ) : loaded === null ? (
          <p className="text-sm text-muted-foreground">Loading…</p>
        ) : !loaded.validation.ok ? (
          // The standard error EmptyState: an invalid document never renders (its
          // taxonomy code is surfaced), never a crash or a partial paint.
          <div data-slot="pack-page-invalid" className="rounded-card border border-border bg-card">
            <EmptyState
              icon={AlertTriangle}
              title="This page could not be displayed"
              description={`The extension page is not valid ui-schema/1: ${loaded.validation.errors
                .map((e) => e.code)
                .join(", ")}.`}
            />
            <ul className="mx-auto mb-8 flex max-w-md flex-col gap-1 px-6 font-mono text-[12px] text-muted-foreground">
              {loaded.validation.errors.map((e, i) => (
                <li key={i}>
                  {e.code} · {e.path}
                </li>
              ))}
            </ul>
          </div>
        ) : (
          <>
            {conflictReview ? (
              <p
                role="status"
                className="rounded-card border border-[color:var(--wv-warn)] bg-[color:var(--wv-warn-bg)] p-4 text-sm text-[color:var(--wv-warn)]"
              >
                This record was changed elsewhere while you were editing. The current values are
                shown below — review them, then save again to apply your change.
              </p>
            ) : null}
            <main className="min-w-0">
              <PageRenderer
                key={version}
                doc={loaded.doc}
                data={data}
                initialUi={{ selected: selectedId }}
                messages={loaded.messages}
                handler={handler}
                fieldErrors={fieldErrors}
                onUiChange={onUiChange}
              />
            </main>
          </>
        )}
      </div>
      <Toaster />
    </div>
  );
}
