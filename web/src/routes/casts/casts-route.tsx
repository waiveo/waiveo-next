import { useCallback, useEffect, useMemo, useState } from "react";
import { useNavigate } from "react-router";
import { Copy, LayoutTemplate, Pencil, Plus, Trash2 } from "lucide-react";
import {
  Button,
  ConfirmModal,
  DataTable,
  EmptyState,
  FormField,
  Modal,
  PageHeader,
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
 */

const SCOPE_SELECTOR = "kind in (org,site,group)";

/** A cast's slide count and its health, for the list. A cast whose slides do not
 * validate is one the projector will DROP — silently, on the TV — so the library
 * is the first place that has to say so. */
function castHealth(slides: CastSlide[]): { slides: number; problems: number } {
  return { slides: slides.length, problems: validateCastSlides(slides).size };
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
  const [busy, setBusy] = useState(false);
  const [pendingDelete, setPendingDelete] = useState<Cast | null>(null);

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
      const created = await client.casts.create({
        scope_node: scope,
        name: newName.trim() || "Untitled cast",
        slides: [newSlide(crypto.randomUUID())],
      });
      setNewOpen(false);
      setNewName("");
      toast.success(`Created ${created.data.name}`);
      navigate(`/studio?id=${encodeURIComponent(created.data.id)}`);
    } catch (err) {
      reportProblem("Couldn't create the cast", err);
    } finally {
      setBusy(false);
    }
  }, [client, navigate, newName, newScope, scopes]);

  const duplicate = useCallback(
    async (cast: Cast) => {
      try {
        const copy = await client.casts.create({
          scope_node: cast.scope_node,
          name: `${cast.name} copy`,
          // Deep-copied: sharing slide objects with the row still in the list
          // would make an edit to one show up in the other.
          slides: cast.slides.map((s) => ({ ...s, id: crypto.randomUUID(), layers: s.layers.map((l) => ({ ...l })) })),
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
      { accessorKey: "name", header: "Name" },
      {
        id: "slides",
        header: "Slides",
        meta: { numeric: true },
        cell: ({ row }) => String(castHealth(row.original.slides).slides),
      },
      {
        id: "health",
        header: "Status",
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
    [],
  );

  return (
    <div className="min-h-screen bg-background text-foreground">
      <div className="mx-auto flex max-w-[1200px] flex-col gap-6 px-4 py-6 lg:px-8">
        <PageHeader
          variant="hero"
          title="Casts"
          description="The slide documents your screens play. Open one in the Studio to lay out text, shapes, images and a live clock on the 1920×1080 canvas."
          actions={
            <Button
              icon={Plus}
              onClick={() => {
                setNewScope(scopes[0]?.id ?? "");
                setNewOpen(true);
              }}
            >
              New cast
            </Button>
          }
        />

        {loadError ? (
          <p role="alert" className="text-sm text-[color:var(--wv-err)]">
            Couldn't load casts — {loadError}
          </p>
        ) : null}

        <DataTable
          label="Casts"
          columns={columns}
          data={casts ?? []}
          loading={casts === null}
          onRowPress={(cast) => openStudio(cast)}
          emptyState={
            <EmptyState
              icon={LayoutTemplate}
              title="No casts yet"
              description="A cast holds the slides a screen plays. Create one to open the Studio."
              action={
                <Button
                  icon={Plus}
                  onClick={() => {
                    setNewScope(scopes[0]?.id ?? "");
                    setNewOpen(true);
                  }}
                >
                  New cast
                </Button>
              }
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
      </div>

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
          <FormField label="Cast name">
            {(field) => (
              <input
                {...field}
                type="text"
                placeholder="Lobby loop"
                className="flex min-h-[38px] w-full min-w-0 rounded-input border border-border bg-transparent px-2 py-1 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
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
                  className="flex min-h-[38px] w-full min-w-0 rounded-input border border-border bg-transparent px-2 py-1 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
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
