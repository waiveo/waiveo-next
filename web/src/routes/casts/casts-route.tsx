import { useCallback, useEffect, useMemo, useState } from "react";
import { useNavigate } from "react-router";
import { BookmarkPlus, Copy, Download, LayoutTemplate, Pencil, Plus, Trash2, Upload } from "lucide-react";
import {
  Button,
  ConfirmModal,
  DataTable,
  EmptyState,
  FormField,
  Modal,
  PageHeader,
  StatCard,
  Toaster,
  toast,
  type ColumnDef,
} from "@/components/kit";
import {
  ApiError,
  collectPages,
  createApi,
  etagForRevision,
  validateCastSlides,
  type Cast,
  type CastSlide,
  type ScopeNode,
  type WaiveoApi,
} from "@/api";
import { newSlide } from "@/routes/studio/cast-model";
import { SlideStage } from "@/routes/studio/slide-canvas";
import { useContentLibrary } from "@/routes/media/media-library";
import { BUILT_IN_TEMPLATES } from "./cast-templates";

/**
 * The cast library — every authored slide document on this box, and the door
 * into the Studio.
 *
 * A cast is scoped like every other authored row (DAT-003): it lives under a
 * scope node, and a box with no site cannot hold one. So "New cast" asks WHERE
 * before it asks what, in the same shape the Screens page asks it — inferring
 * the scope from a sibling row is wrong the moment an org has two sites, and
 * impossible when it has no casts yet.
 *
 * Duplicate is a client-side read-then-create rather than a server verb: the
 * cast IS its slides, the list already carries them, and a POST of the same
 * document under a new name is exactly what duplication means. Doing it here
 * keeps the API surface to plain CRUD.
 *
 * ── Templates ───────────────────────────────────────────────────────────────
 * A template is a cast carrying `template: true` (`data-model/1` DAT-043), not a
 * resource of its own — so this page reads ONE list and splits it, and both
 * halves get the same editor, the same validation and the same health column for
 * free. The split matters in both directions: a template must not clutter the
 * list of things you can schedule, and a cast must not appear as a starting
 * point. The server enforces the half that counts (a playlist item naming a
 * template is refused), so this is presentation over a real rule rather than a
 * convention only the console honours.
 *
 * Creating FROM a template is a plain `POST /casts` of its slides. That is why
 * the built-ins (cast-templates.ts) can be program data rather than seeded rows:
 * from the moment of creation the result is an ordinary cast, and nothing
 * downstream has a template case at all.
 */

const SCOPE_SELECTOR = "kind in (org,site,group)";

/** The "Start from" value meaning an empty one-slide cast. A sentinel string
 * rather than null so the select has one value type; the two prefixes below keep
 * a built-in and a saved template from ever colliding on an id. */
const SOURCE_BLANK = "blank";
const SOURCE_BUILT_IN = "builtin:";
const SOURCE_SAVED = "cast:";

/** What a chosen starting point contributes to the create body. `slides` is
 * undefined for a blank cast, which is the caller's signal to seed one. */
interface StartingPoint {
  defaultName: string;
  slides?: CastSlide[];
  defaultDurationMs?: number;
}

/**
 * Resolve a "Start from" selection into the parts of a create body.
 *
 * A pure function of the selection, the saved templates already in hand and the
 * clock, so what "create from the Event countdown template" produces is a
 * question with an exact answer a test can assert — including its countdown's
 * target, which is the one part that depends on when the button was pressed.
 *
 * An unrecognised selection falls back to blank rather than throwing: the only
 * way to reach one is a saved template deleted in another tab between the list
 * read and the click, and losing a name is a better answer than losing the
 * click.
 */
function startingPoint(source: string, saved: Cast[], nowMs: number): StartingPoint {
  if (source.startsWith(SOURCE_BUILT_IN)) {
    const tpl = BUILT_IN_TEMPLATES.find((t) => t.id === source.slice(SOURCE_BUILT_IN.length));
    if (tpl) {
      return { defaultName: tpl.name, slides: tpl.build(nowMs), defaultDurationMs: tpl.defaultDurationMs };
    }
  }
  if (source.startsWith(SOURCE_SAVED)) {
    const tpl = saved.find((c) => c.id === source.slice(SOURCE_SAVED.length));
    if (tpl) {
      return {
        defaultName: tpl.name,
        // Deep-copied with fresh slide ids: the new cast is its own document and
        // must not share layer objects with the template row still in the list.
        slides: tpl.slides.map((sl) => ({ ...sl, id: crypto.randomUUID(), layers: sl.layers.map((l) => ({ ...l })) })),
        // Spread rather than `?? undefined`: under exactOptionalPropertyTypes an
        // optional member holding undefined is not the same as an absent one,
        // and "the template had no default" must be the absent one.
        ...(tpl.default_duration_ms ? { defaultDurationMs: tpl.default_duration_ms } : {}),
      };
    }
  }
  return { defaultName: "Untitled cast" };
}

/** A cast's slide count and its health, for the list. A cast whose slides do not
 * validate is one the projector will DROP — silently, on the TV — so the library
 * is the first place that has to say so. */
/** The library thumbnail's scale off the 1920x1080 canvas — 80x45, the same
 * 16:9 the slide really is, so nothing is cropped or letterboxed. The Studio
 * filmstrip picks its own scale for a taller strip; this is a table row. */
const THUMB_SCALE = 80 / 1920;

function castHealth(slides: CastSlide[]): { slides: number; problems: number } {
  return { slides: slides.length, problems: validateCastSlides(slides).size };
}

/** The shared class for this page's text/select inputs. */
const fieldClass =
  "flex min-h-[38px] w-full min-w-0 rounded-input border border-border bg-transparent px-2 py-1 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring";

/** The one-line explanation of a built-in starting point, or "" when the
 * selection is blank or a saved template (whose explanation is its own name and
 * the slides the operator built). */
function describeSource(source: string): string {
  if (!source.startsWith(SOURCE_BUILT_IN)) return "";
  return BUILT_IN_TEMPLATES.find((t) => t.id === source.slice(SOURCE_BUILT_IN.length))?.description ?? "";
}

function reportProblem(context: string, err: unknown): void {
  if (err instanceof ApiError) {
    toast.error(`${context}: ${Object.values(err.fieldErrors)[0] ?? err.detail ?? err.code}`);
  } else {
    toast.error(`${context}: the service is unreachable.`);
  }
}

export default function CastsRoute({ api }: { api?: WaiveoApi }) {
  const client = useMemo(() => api ?? createApi(), [api]);
  const navigate = useNavigate();

  const [casts, setCasts] = useState<Cast[] | null>(null);
  const [scopes, setScopes] = useState<ScopeNode[]>([]);
  const [loadError, setLoadError] = useState<string | null>(null);

  const [newOpen, setNewOpen] = useState(false);
  const [newName, setNewName] = useState("");
  const [newScope, setNewScope] = useState<string>("");
  /** Which starting point "New cast" builds from. See `SOURCE_BLANK`. */
  const [newSource, setNewSource] = useState<string>(SOURCE_BLANK);
  const [busy, setBusy] = useState(false);
  const [pendingDelete, setPendingDelete] = useState<Cast | null>(null);
  /** The cast a "Save as template" is being named for, and the name so far. */
  const [templateFrom, setTemplateFrom] = useState<Cast | null>(null);
  const [templateName, setTemplateName] = useState("");
  /** The `.cast` import: the chosen file, where it lands, and an optional
   * rename. Held here rather than inside the modal so a failed import keeps the
   * operator's choices — retyping them after a bad file is pure tax. */
  const [importOpen, setImportOpen] = useState(false);
  const [importFile, setImportFile] = useState<File | null>(null);
  const [importScope, setImportScope] = useState("");
  const [importName, setImportName] = useState("");

  // ONE list, split. A template is a cast with the flag set, so reading them
  // separately would be two round trips for one answer — and would let the two
  // halves disagree about a row that changed between them.
  // The content origin's ref->url listing, for the thumbnails' image layers.
  // NULL on a failed or in-flight read, exactly as the Studio treats it: a
  // listing we do not have must never be used to report bytes missing. A
  // thumbnail then draws its text and shapes and omits the picture, which is a
  // smaller lie than a BYTES MISSING badge on assets that are present.
  const { assets: contentAssets, error: contentError } = useContentLibrary(client);
  const assetUrls = useMemo(() => {
    if (contentError !== null || contentAssets === null) return null;
    return new Map(contentAssets.map((a) => [a.asset_ref, a.url]));
  }, [contentAssets, contentError]);

  const templates = useMemo(() => (casts ?? []).filter((c) => c.template === true), [casts]);
  const schedulable = useMemo(() => (casts ?? []).filter((c) => c.template !== true), [casts]);

  const load = useCallback(async () => {
    try {
      const [rows, scopeRows] = await Promise.all([
        collectPages<Cast>((cursor) => client.casts.list({ cursor })),
        collectPages<ScopeNode>((cursor) => client.scopeNodes.list({ selector: SCOPE_SELECTOR, cursor })),
      ]);
      setCasts(rows);
      setScopes(scopeRows);
      setLoadError(null);
    } catch (err) {
      setCasts([]);
      setLoadError(err instanceof ApiError ? (err.detail ?? err.code) : "The service is unreachable.");
    }
  }, [client]);

  useEffect(() => {
    void load();
  }, [load]);

  const openStudio = useCallback((cast: Cast) => navigate(`/studio?id=${encodeURIComponent(cast.id)}`), [navigate]);

  const create = useCallback(async () => {
    const scope = newScope || scopes[0]?.id;
    if (!scope) {
      toast.error("Add a site before creating a cast — a cast lives under a site or group.");
      return;
    }
    setBusy(true);
    try {
      // A cast starts with ONE slide rather than none: the Studio's canvas needs
      // a slide to draw, and "create, then find the add-slide button" is a step
      // with no decision in it. That slide comes from the SAME `newSlide` the
      // Studio's own add-slide uses, so the two can never disagree about what a
      // new slide is — and it carries a layer, because `CastSlide.layers` is
      // `minItems: 1` and a zero-layer slide is refused at the door (the primary
      // action of this whole page would fail with a red toast and no cast).
      //
      // A template simply supplies different slides. The POST is the same one,
      // which is the point: nothing downstream of creation knows a template was
      // involved, and the result is an ordinary schedulable cast.
      const start = startingPoint(newSource, templates, Date.now());
      const created = await client.casts.create({
        scope_node: scope,
        name: newName.trim() || start.defaultName,
        slides: start.slides ?? [newSlide(crypto.randomUUID())],
        // Sent only when the starting point states one — a blank cast has no
        // opinion about dwell time and must not have one invented for it.
        ...(start.defaultDurationMs ? { default_duration_ms: start.defaultDurationMs } : {}),
      });
      setNewOpen(false);
      setNewName("");
      setNewSource(SOURCE_BLANK);
      toast.success(`Created ${created.data.name}`);
      navigate(`/studio?id=${encodeURIComponent(created.data.id)}`);
    } catch (err) {
      reportProblem("Couldn't create the cast", err);
    } finally {
      setBusy(false);
    }
  }, [client, navigate, newName, newScope, newSource, scopes, templates]);

  /** Open the create modal already pointed at a starting point. */
  const openNew = useCallback(
    (source: string) => {
      setNewScope(scopes[0]?.id ?? "");
      setNewSource(source);
      setNewOpen(true);
    },
    [scopes],
  );

  /**
   * Save an existing cast AS a template: a new row carrying the same slides with
   * `template: true`.
   *
   * A COPY rather than a flag flipped on the original, and that is the whole
   * design. Flipping would take a cast that screens are playing out of service
   * the instant it succeeded — the server refuses a playlist item referencing a
   * template, so it would in fact fail with a 422 the operator could do nothing
   * about. "Save as template" means "keep this shape for next time", not "stop
   * showing this", so it leaves the original exactly where it was.
   */
  const saveAsTemplate = useCallback(
    async (cast: Cast, name: string) => {
      setBusy(true);
      try {
        const created = await client.casts.create({
          scope_node: cast.scope_node,
          name: name.trim() || `${cast.name} template`,
          // Fresh slide ids: the copy is its own document, and ids are only
          // required to be unique WITHIN a cast, so reusing them would be legal
          // but would make the two look like the same slides to anything
          // comparing them.
          slides: cast.slides.map((sl) => ({ ...sl, id: crypto.randomUUID(), layers: sl.layers.map((l) => ({ ...l })) })),
          ...(cast.default_duration_ms ? { default_duration_ms: cast.default_duration_ms } : {}),
          template: true,
        });
        toast.success(`Saved ${created.data.name} as a template`);
        setTemplateFrom(null);
        await load();
      } catch (err) {
        reportProblem("Couldn't save the template", err);
      } finally {
        setBusy(false);
      }
    },
    [client, load],
  );

  /**
   * Import a `.cast` bundle (parity row 1.9) — the design AND the images it
   * draws, from another box.
   *
   * The placement is asked for, never inferred. A bundle deliberately carries no
   * scope node (it names a tree the source box had and this one does not), and
   * guessing would put an operator's design at a node they did not choose and,
   * half the time, cannot see.
   *
   * The server holds the bundle to the SAME rules the editor is held to and
   * answers a refusal with a `detail` that says which kind of wrong it is
   * ("that is not a cast bundle" / "this file has been altered" / "this bundle
   * is incomplete"). Those are three different next actions, so the toast shows
   * the server's sentence rather than a generic one of its own.
   */
  const runImport = useCallback(async () => {
    const scope = importScope || scopes[0]?.id;
    if (!importFile) {
      toast.error("Choose a .cast bundle to import.");
      return;
    }
    if (!scope) {
      toast.error("Add a site before importing a cast — a cast lives under a site or group.");
      return;
    }
    setBusy(true);
    try {
      // Read to bytes here rather than handing the File to fetch: a File's
      // identity as a request body varies with the runtime (a browser streams
      // it; a test environment's fetch does not necessarily recognise the same
      // File class), and a bundle is a few megabytes — small enough that
      // materializing it is free and certain.
      const bytes = await importFile.arrayBuffer();
      const imported = await client.casts.importBundle(bytes, scope, importName.trim() || undefined);
      setImportOpen(false);
      setImportFile(null);
      setImportName("");
      toast.success(`Imported ${imported.name}`);
      await load();
    } catch (err) {
      reportProblem("Couldn't import that bundle", err);
    } finally {
      setBusy(false);
    }
  }, [client, importFile, importName, importScope, load, scopes]);

  const duplicate = useCallback(
    async (cast: Cast) => {
      try {
        const copy = await client.casts.create({
          scope_node: cast.scope_node,
          name: `${cast.name} copy`,
          // Deep-copied: sharing slide objects with the row still in the list
          // would make an edit to one show up in the other.
          slides: cast.slides.map((s) => ({ ...s, id: crypto.randomUUID(), layers: s.layers.map((l) => ({ ...l })) })),
          // The cast-wide fallback duration is part of what the cast IS: every
          // slide that states no `duration_ms` of its own is timed by it, so a
          // copy without it plays at the player's default instead and looks
          // identical in the library while running at a different pace.
          ...(cast.default_duration_ms ? { default_duration_ms: cast.default_duration_ms } : {}),
        });
        toast.success(`Duplicated as ${copy.data.name}`);
        await load();
      } catch (err) {
        reportProblem("Couldn't duplicate the cast", err);
      }
    },
    [client, load],
  );

  const remove = useCallback(
    async (cast: Cast) => {
      try {
        await client.casts.remove(cast.id, etagForRevision(cast.revision));
        toast.success(`Deleted ${cast.name}`);
        await load();
      } catch (err) {
        reportProblem("Couldn't delete the cast", err);
      } finally {
        setPendingDelete(null);
      }
    },
    [client, load],
  );

  const columns: ColumnDef<Cast>[] = useMemo(
    () => [
      {
        id: "thumb",
        header: "",
        // The FIRST SLIDE, drawn. Restoring this is the single most-missed thing
        // about the legacy library: operators name casts badly and recognise them
        // by picture, so a table of names is a table they have to open one by one.
        //
        // It is the SAME renderer the Studio canvas and its filmstrip use, at a
        // smaller scale — not a screenshot, not a server-rendered preview, and
        // nothing new to keep in step with how a slide actually draws. A cast
        // whose first slide changes shows the change here immediately.
        //
        // Deliberately not sortable and not searchable: there is no ordering of
        // pictures an operator means, and `enableSorting: false` on a column with
        // no accessor is what the header would do anyway.
        enableSorting: false,
        cell: ({ row }) => {
          const first = row.original.slides[0];
          if (!first) {
            // An empty cast is a real state (New cast makes one), and a blank box
            // of the same size keeps the rows from jumping height.
            return (
              <div
                aria-hidden
                className="h-[45px] w-[80px] rounded-[4px] border border-dashed border-border"
              />
            );
          }
          return (
            // w-fit, because SlideStage sizes itself to exactly 1920x1080 *
            // scale (80x45 here) and a plain block wrapper would stretch to the
            // column, framing the picture with dead space to its right.
            <div className="w-fit overflow-hidden rounded-[4px] border border-border">
              <SlideStage slide={first} scale={THUMB_SCALE} assetUrls={assetUrls} />
            </div>
          );
        },
      },
      { accessorKey: "name", header: "Name", meta: { searchable: true } },
      {
        id: "slides",
        header: "Slides",
        // An accessor returning the NUMBER, so the column sorts 2 < 9 < 10 rather
        // than by the rendered string.
        accessorFn: (cast) => castHealth(cast.slides).slides,
        meta: { numeric: true },
        cell: ({ getValue }) => String(getValue()),
      },
      {
        id: "health",
        header: "Status",
        // The cell says HOW MANY slides need attention; the filter needs the two
        // states an operator actually browses by, so the accessor is the coarse
        // one. Ready/Needs attention are the only two values, faceted from data.
        accessorFn: (cast) =>
          castHealth(cast.slides).problems === 0 ? "Ready" : "Needs attention",
        meta: { filter: "enum", filterLabel: "Status" },
        cell: ({ row }) => {
          const { problems } = castHealth(row.original.slides);
          return problems === 0 ? (
            <span className="text-[color:var(--wv-ok)]">Ready</span>
          ) : (
            <span className="text-[color:var(--wv-warn)]">
              {problems === 1 ? "1 slide needs attention" : `${problems} slides need attention`}
            </span>
          );
        },
      },
    ],
    [assetUrls],
  );

  return (
    <div className="min-h-screen bg-background text-foreground">
      <div className="mx-auto flex max-w-[1200px] flex-col gap-6 px-4 py-6 lg:px-8">
        <PageHeader
          variant="hero"
          title="Casts"
          description="The slide documents your screens play. Open one in the Studio to lay out text, shapes, images and a live clock on the 1920×1080 canvas."
          actions={
            <div className="flex items-center gap-2">
              {/* Import sits beside New because it IS a way of making a cast —
                  the one an operator uses when the design already exists on
                  another box. */}
              <Button
                variant="secondary"
                icon={Upload}
                onClick={() => {
                  setImportScope(scopes[0]?.id ?? "");
                  setImportOpen(true);
                }}
              >
                Import .cast
              </Button>
              <Button icon={Plus} onClick={() => openNew(SOURCE_BLANK)}>
                New cast
              </Button>
            </div>
          }
        />

        {loadError ? (
          <p role="alert" className="text-sm text-[color:var(--wv-err)]">
            Couldn't load casts — {loadError}
          </p>
        ) : null}

        {/* The library's stats. Legacy carried five tiles here and this console
            carried none, which is a real part of the owner's "visual differences"
            note: the page answered "which casts exist" and never "what shape is
            my library in".

            Every figure is DERIVED from the rows already loaded — no extra
            request, and nothing here can disagree with the table below it. They
            are rendered only once the list has actually arrived, because a row of
            confident zeros during a fetch says the library is empty, which is a
            statement, not a placeholder. */}
        {casts !== null ? (
          <div className="flex flex-wrap gap-4">
            <StatCard label="Casts" value={String(schedulable.length)} hint="schedulable documents" />
            <StatCard
              label="Slides"
              value={String(schedulable.reduce((n, c) => n + c.slides.length, 0))}
              hint="across every cast"
            />
            <StatCard
              label="Ready"
              value={String(schedulable.filter((c) => castHealth(c.slides).problems === 0).length)}
              hint="every slide validates"
            />
            <StatCard
              label="Needs attention"
              value={String(schedulable.filter((c) => castHealth(c.slides).problems > 0).length)}
              // Named for the consequence, not the rule: a slide that fails
              // validation is DROPPED by the projector, silently, on the TV.
              hint="a projector would drop slides"
            />
            <StatCard label="Templates" value={String(templates.length)} hint="starting points" />
          </div>
        ) : null}

        <DataTable
          label="Casts"
          columns={columns}
          data={schedulable}
          loading={casts === null}
          search={{ label: "Search casts", placeholder: "Cast name" }}
          filters
          pagination
          onRowPress={(cast) => openStudio(cast)}
          emptyState={
            <EmptyState
              icon={LayoutTemplate}
              title="No casts yet"
              description="A cast holds the slides a screen plays. Start from a template, or create a blank one to open the Studio."
              action={<Button icon={Plus} onClick={() => openNew(SOURCE_BLANK)}>New cast</Button>}
            />
          }
          // The whole ROW is pressable (it opens the Studio), so every action
          // inside it must stop the click bubbling — otherwise "Delete" would
          // open the confirm AND navigate away from the page showing it, and
          // "Duplicate" would leave for the original rather than staying to see
          // the copy appear.
          rowActions={(cast) => (
            <div className="flex items-center justify-end gap-1">
              <Button
                size="icon"
                variant="ghost"
                icon={Pencil}
                aria-label={`Open ${cast.name} in the Studio`}
                onClick={(e) => {
                  e.stopPropagation();
                  openStudio(cast);
                }}
              />
              <Button
                size="icon"
                variant="ghost"
                icon={Copy}
                aria-label={`Duplicate ${cast.name}`}
                onClick={(e) => {
                  e.stopPropagation();
                  void duplicate(cast);
                }}
              />
              {/* Export is an ANCHOR, not a button: a bundle carries every
                  image the design draws, and the browser streams it straight to
                  disk from a link. A fetch-into-a-Blob would hold the whole
                  thing in the tab for no benefit. The row itself is pressable,
                  so the click must stop here or the download would also
                  navigate to the Studio. */}
              <a
                className="inline-flex size-8 items-center justify-center rounded-btn hover:bg-accent"
                href={client.casts.exportUrl(cast.id)}
                download
                aria-label={`Export ${cast.name} as a .cast bundle`}
                onClick={(e) => e.stopPropagation()}
              >
                <Download aria-hidden className="size-4" />
              </a>
              <Button
                size="icon"
                variant="ghost"
                icon={BookmarkPlus}
                aria-label={`Save ${cast.name} as a template`}
                onClick={(e) => {
                  e.stopPropagation();
                  setTemplateFrom(cast);
                  setTemplateName(`${cast.name} template`);
                }}
              />
              <Button
                size="icon"
                variant="ghost"
                icon={Trash2}
                aria-label={`Delete ${cast.name}`}
                onClick={(e) => {
                  e.stopPropagation();
                  setPendingDelete(cast);
                }}
              />
            </div>
          )}
        />
        {/* Templates. A second table rather than a filter toggle on the first:
            these are not casts you are choosing between, they are the shapes you
            START from, and the actions on them are different ones (create-from,
            not schedule). The built-ins are listed inside the create modal
            rather than here, because they are program data with no row to act
            on — nothing to rename, nothing to delete, always present. */}
        <section aria-label="Templates" className="flex flex-col gap-3">
          <div>
            <h2 className="text-base font-semibold">Your templates</h2>
            <p className="text-sm text-muted-foreground">
              Starting points saved from a cast. A template is never scheduled itself — create a cast from it and
              schedule that, so improving the template never changes what a screen is already showing.
            </p>
          </div>
          <DataTable
            label="Templates"
            columns={columns}
            data={templates}
            loading={casts === null}
            onRowPress={(cast) => openStudio(cast)}
            emptyState={
              <EmptyState
                icon={BookmarkPlus}
                title="No saved templates"
                description="Save any cast as a template from its row, and it becomes a starting point for new casts. Four built-in templates are always available in the New cast dialog."
              />
            }
            rowActions={(cast) => (
              <div className="flex items-center justify-end gap-1">
                <Button
                  size="sm"
                  variant="secondary"
                  icon={Plus}
                  aria-label={`New cast from ${cast.name}`}
                  onClick={(e) => {
                    e.stopPropagation();
                    openNew(`${SOURCE_SAVED}${cast.id}`);
                  }}
                >
                  New cast from this
                </Button>
                <Button
                  size="icon"
                  variant="ghost"
                  icon={Pencil}
                  aria-label={`Open ${cast.name} in the Studio`}
                  onClick={(e) => {
                    e.stopPropagation();
                    openStudio(cast);
                  }}
                />
                <Button
                  size="icon"
                  variant="ghost"
                  icon={Trash2}
                  aria-label={`Delete ${cast.name}`}
                  onClick={(e) => {
                    e.stopPropagation();
                    setPendingDelete(cast);
                  }}
                />
              </div>
            )}
          />
        </section>
      </div>

      <Modal
        title="Import a cast"
        description="A .cast bundle carries a design and every image it draws — export one from another box, then bring it in here."
        open={importOpen}
        onOpenChange={setImportOpen}
        footer={
          <>
            <Button variant="secondary" onClick={() => setImportOpen(false)}>
              Cancel
            </Button>
            <Button disabled={busy || !importFile} onClick={() => void runImport()}>
              {busy ? "Importing…" : "Import"}
            </Button>
          </>
        }
      >
        <div className="flex flex-col gap-4">
          <FormField label="Bundle" help="The .cast file another box's Export produced.">
            {(field) => (
              <input
                {...field}
                type="file"
                accept=".cast,application/zip"
                className={fieldClass}
                onChange={(e) => setImportFile(e.target.files?.[0] ?? null)}
              />
            )}
          </FormField>
          <FormField
            label="Where it lives"
            help="A bundle carries no placement — it names a tree the other box had, not this one — so the cast lands where you put it."
          >
            {(field) => (
              <select
                {...field}
                className={fieldClass}
                value={importScope}
                onChange={(e) => setImportScope(e.target.value)}
              >
                {scopes.map((n) => (
                  <option key={n.id} value={n.id}>
                    {n.name}
                  </option>
                ))}
              </select>
            )}
          </FormField>
          <FormField
            label="Name (optional)"
            help="Leave blank to keep the bundle's own name. Rename when you are importing a second copy onto the same box."
          >
            {(field) => (
              <input
                {...field}
                className={fieldClass}
                value={importName}
                onChange={(e) => setImportName(e.target.value)}
                placeholder="Keep the bundle's name"
              />
            )}
          </FormField>
        </div>
      </Modal>

      <Modal
        title="New cast"
        description="Name it and choose where it lives. You can rename it in the Studio."
        open={newOpen}
        onOpenChange={setNewOpen}
        footer={
          <>
            <Button variant="secondary" onClick={() => setNewOpen(false)}>
              Cancel
            </Button>
            <Button disabled={busy} onClick={() => void create()}>
              {busy ? "Creating…" : "Create and open"}
            </Button>
          </>
        }
      >
        <div className="flex flex-col gap-4">
          <FormField
            label="Start from"
            help="A template supplies the slides. You can change everything about them afterwards — the new cast is an ordinary cast from the moment it is created."
          >
            {(field) => (
              <select
                {...field}
                className={fieldClass}
                value={newSource}
                onChange={(e) => setNewSource(e.target.value)}
              >
                <option value={SOURCE_BLANK}>Blank cast (one empty slide)</option>
                <optgroup label="Built-in templates">
                  {BUILT_IN_TEMPLATES.map((t) => (
                    <option key={t.id} value={`${SOURCE_BUILT_IN}${t.id}`}>
                      {t.name}
                    </option>
                  ))}
                </optgroup>
                {templates.length > 0 ? (
                  <optgroup label="Your templates">
                    {templates.map((t) => (
                      <option key={t.id} value={`${SOURCE_SAVED}${t.id}`}>
                        {t.name}
                      </option>
                    ))}
                  </optgroup>
                ) : null}
              </select>
            )}
          </FormField>
          {/* What the chosen template IS. The names alone do not distinguish a
              menu board from a welcome board for anyone who has not seen them,
              and this is the last moment before a row exists. */}
          {describeSource(newSource) ? (
            <p className="text-[13px] text-muted-foreground">{describeSource(newSource)}</p>
          ) : null}
          <FormField label="Cast name">
            {(field) => (
              <input
                {...field}
                type="text"
                placeholder="Lobby loop"
                className={fieldClass}
                value={newName}
                onChange={(e) => setNewName(e.target.value)}
              />
            )}
          </FormField>
          {scopes.length === 0 ? (
            <p role="alert" className="text-sm text-[color:var(--wv-err)]">
              There is no site to put a cast under yet. Add one on the Screens page first.
            </p>
          ) : (
            <FormField label="Lives under">
              {(field) => (
                <select
                  {...field}
                  className={fieldClass}
                  value={newScope}
                  onChange={(e) => setNewScope(e.target.value)}
                >
                  {scopes.map((s) => (
                    <option key={s.id} value={s.id}>
                      {s.name}
                    </option>
                  ))}
                </select>
              )}
            </FormField>
          )}
        </div>
      </Modal>

      <Modal
        title={templateFrom ? `Save "${templateFrom.name}" as a template` : "Save as a template"}
        description="This copies the cast's slides into a new template. The cast itself keeps playing exactly as it is."
        open={templateFrom !== null}
        onOpenChange={(open) => {
          if (!open) setTemplateFrom(null);
        }}
        footer={
          <>
            <Button variant="secondary" onClick={() => setTemplateFrom(null)}>
              Cancel
            </Button>
            <Button
              disabled={busy}
              onClick={() => {
                if (templateFrom) void saveAsTemplate(templateFrom, templateName);
              }}
            >
              {busy ? "Saving…" : "Save template"}
            </Button>
          </>
        }
      >
        <FormField label="Template name">
          {(field) => (
            <input
              {...field}
              type="text"
              className={fieldClass}
              value={templateName}
              onChange={(e) => setTemplateName(e.target.value)}
            />
          )}
        </FormField>
      </Modal>

      <ConfirmModal
        open={pendingDelete !== null}
        onOpenChange={(open) => {
          if (!open) setPendingDelete(null);
        }}
        title={pendingDelete ? `Delete ${pendingDelete.name}?` : "Delete cast?"}
        description="Its slides go with it. A schedule still pointing at this cast will have nothing to play."
        confirmLabel="Delete"
        destructive
        onConfirm={() => {
          if (pendingDelete) void remove(pendingDelete);
        }}
      />

      <Toaster />
    </div>
  );
}
