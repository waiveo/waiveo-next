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
import {
  dashboardCollections,
  isSettingsForm,
  isWizard,
  loadPackCatalog,
  primaryCollection,
  resolveTitle,
  shapeCollection,
} from "./catalog";

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
  /** A dashboard's per-tile collections (UIS-040) — empty on every other page
   * type, which binds one page-wide collection through `collection` instead. */
  tileCollections: string[];
  /** The names the manifest marks `singleton: true` (MAN-056), so a tile bound
   * to one reads the record rather than a one-element array. */
  singletonCollections: string[];
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
  // A dashboard's rows, keyed by collection. Separate from `rows` rather than
  // folded into it because the two answer different questions: `rows` is THE
  // bound resource a list-detail pages and a settings-form edits, and a
  // dashboard has no such thing (UIS-040).
  const [tileRows, setTileRows] = useState<Record<string, PackRow[]>>({});
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
      const declaredCollections = manifest.dataModel?.collections ?? [];
      const collectionNames = new Set(declaredCollections.map((c) => c.name));
      const singletonCollections = declaredCollections.filter((c) => c.singleton).map((c) => c.name);
      const collection = validation.ok ? primaryCollection(rawDoc, collectionNames) : null;
      // A dashboard binds no page-wide collection and several per-tile ones
      // (UIS-040). Without this the route handed the renderer an empty data
      // namespace and every tile rendered blank.
      const tileCollections = validation.ok ? dashboardCollections(rawDoc, collectionNames) : [];

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
      // Fetched together rather than in sequence: a dashboard's tiles are
      // independent by construction, so serializing them would make an eight-tile
      // page eight round trips deep for no ordering benefit.
      setTileRows(
        Object.fromEntries(
          await Promise.all(tileCollections.map(async (c) => [c, await loadRows(c)] as const)),
        ),
      );
      setLoaded({
        doc: rawDoc,
        validation,
        messages,
        collection,
        tileCollections,
        singletonCollections,
        defaultScopeNode,
        pageTitle,
        packTitle,
      });
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
  const tileCollections = useMemo(() => loaded?.tileCollections ?? [], [loaded]);
  const singletons = useMemo(() => new Set(loaded?.singletonCollections ?? []), [loaded]);
  // Derived from the loaded document rather than stored beside it: the page type
  // IS the document's, and a second copy could disagree with it after a reload.
  const settingsForm = isSettingsForm(loaded?.doc);
  const wizard = isWizard(loaded?.doc);

  const reload = useCallback(async () => {
    setFieldErrors({});
    if (collection) setRows(await loadRows(collection));
    if (tileCollections.length > 0) {
      setTileRows(
        Object.fromEntries(
          await Promise.all(tileCollections.map(async (c) => [c, await loadRows(c)] as const)),
        ),
      );
    }
    setVersion((v) => v + 1);
  }, [collection, tileCollections, loadRows]);

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

  // Whether this page binds ONE RECORD rather than a list — which decides both
  // the shape the renderer is handed and whether a save may create the record it
  // is editing.
  //
  // Two page types do, for reasons the contract states separately rather than
  // one rule covering both: a settings-form always (MAN-064 makes its source a
  // declared singleton), and a wizard when its `draftSource` names a singleton
  // collection (UIS-051 leaves the target open, so this is read off MAN-056
  // rather than assumed). Deliberately NOT "any page whose collection is a
  // singleton": a list-detail pointed at one is still a list, and collapsing it
  // to a record would empty the table it exists to show.
  const bindsSingleRecord = settingsForm || (wizard && collection !== null && singletons.has(collection));

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
      // "Run": invoke an action the pack DECLARED, through the same management
      // route (MAN-101) an operator or an automation rule reaches it by.
      //
      // This seam was entirely unwired, and an unwired seam with no `outcomeTo`
      // is a complete silent no-op (runSeam): a pack shipping a page with a
      // `call-action` button had a button that did nothing — no request, no
      // error, no toast. Every other half of the actions plane existed; a pack's
      // own UI was the one caller that could not reach it.
      //
      // The name is a pack-LOCAL `actions[].name` (ui-schema/1 Actions,
      // MAN-100), not a publisher-qualified one: the page belongs to the pack, so
      // the pack is implied. (An automation rule, which belongs to no pack, must
      // qualify it — rules/1 RUL-231.)
      callAction: async (name, params) => {
        try {
          const inv = await client.packs.invokeAction(packId, name, params);
          // "Started", never "Done". The call is QUEUED — the pack leases the
          // invocation and works on it after this response — and api/1 exposes no
          // way to read an invocation back, so acceptance is genuinely the last
          // thing observable here. A "Done" toast would be a claim the console
          // cannot support and, for a long action, one the operator would read
          // before anything had happened.
          toast.success(`Started ${name}`);
          return inv;
        } catch (err) {
          reportProblem(`Couldn't start ${name}`, err);
          return undefined;
        }
      },
      // "Save": persist the edited row under its If-Match via the standard
      // optimistic-concurrency flow — a 412 re-reads the current state for review,
      // a 422's per-field errors land on the offending FormField. The whole row is
      // sent; the server ignores host-owned envelope fields (MAN-051).
      submit: async (_target, resource) => {
        if (!collection) {
          // No manifest-declared collection backs this page: a dashboard (which
          // binds none by design, UIS-040), or a wizard with no draftSource
          // (whose draft is ephemeral and persists through onFinish's
          // call-action instead, UIS-051). There is nowhere to write, so report
          // honestly rather than fabricating a green "Saved" for a save that
          // never happened.
          toast.error("Saving isn't available for this page yet.");
          return;
        }
        const meta = rowRef(resource);
        // A page binding ONE record — a settings-form (MAN-064), or a wizard
        // whose draftSource names a singleton (UIS-051/MAN-056) — has no record
        // on the pack's first visit: nothing seeds one at install, because a pack
        // ships no rows. So the first save CREATES it and every save after
        // updates it. Without this the page would render, accept input, and have
        // nowhere to put it — which is the defect this whole path closes.
        //
        // A wizard is if anything the more obvious case: it exists to produce a
        // record that did not exist before.
        if (!meta && bindsSingleRecord) {
          if (!defaultScopeNode) {
            toast.error("This workspace has no scope to attach records to yet.");
            return;
          }
          setFieldErrors({});
          try {
            await client
              .packData(packId, collection)
              .create({ ...(resource as PackRowWrite), scope_node: defaultScopeNode });
            toast.success("Saved changes");
            await reload();
          } catch (err) {
            const fields = fieldErrorsOf(err);
            if (fields) {
              setFieldErrors(fields);
              toast.error("Couldn't save — please fix the highlighted fields.");
            } else {
              reportProblem("Couldn't save", err);
            }
          }
          return;
        }
        if (!meta) {
          // No record identity on the resource and no create path for this page
          // type: a list-detail save with nothing selected, or a wizard whose
          // draftSource names a collection that holds many rows (so "the record
          // it edits" does not identify one). Nothing can be written, and this
          // used to `return` in silence — the same shape of no-op as the branches
          // above it, and the reason the whole page family is being audited.
          toast.error("There's no record to save here yet.");
          return;
        }
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
        if (!meta) {
          // Nothing identifies a row to delete — the resource carries no
          // entity_id/revision. Reported rather than returned in silence, for the
          // reason the submit path just above states.
          toast.error("There's no record to delete here.");
          return;
        }
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
        // Only a `list-detail`'s `list.source` can be paginated (UIS-023/024) —
        // a table widget's own `source` prop is an ordinary Binding and the
        // validator refuses the `{path, paginated}` form there. So this seam only
        // ever fires on a page that HAS a page-wide collection, and there is
        // nothing for a dashboard to resolve here.
        if (!collection) return { items: [], cursor: null };
        const page = await client
          .packData(packId, collection)
          .list({ cursor, ...(limit !== undefined ? { limit } : {}) });
        return { items: page.items, cursor: page.cursor };
      },
    }),
    [client, packId, collection, bindsSingleRecord, defaultScopeNode, reload],
  );

  // A settings-form's root Binding resolves to the single RECORD it edits
  // (UIS-005), not to a list — so it is handed the one row as an object, and an
  // empty object before that row exists so the form renders blank and editable
  // rather than not at all. Every other page type binds the rows array.
  // A dashboard's namespace carries EVERY collection its tiles read, since each
  // tile resolves its own root Binding (UIS-040); every other page type carries
  // the one it binds. The two are exclusive by page type, so the spread cannot
  // collide.
  const data = collection
    ? { [collection]: shapeCollection(rows, bindsSingleRecord) }
    : Object.fromEntries(
        Object.entries(tileRows).map(([c, rs]) => [c, shapeCollection(rs, singletons.has(c))]),
      );

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
