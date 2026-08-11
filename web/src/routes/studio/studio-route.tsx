import { useCallback, useEffect, useMemo, useReducer, useRef, useState, type ReactNode } from "react";
import { Link, useNavigate, useSearchParams } from "react-router";
import { ArrowLeft, Save } from "lucide-react";
import { Button, ConfirmModal, FormField, PageHeader, Toaster, toast } from "@/components/kit";
import {
  ApiError,
  RevisionConflictError,
  collectPages,
  createApi,
  updateWithReview,
  validateCastSlides,
  type Cast,
  type Entity,
  type LayerKind,
  type SlideLayer,
  type WaiveoApi,
} from "@/api";
import { MediaPickerModal, useContentLibrary } from "@/routes/media/media-library";
import {
  EMPTY_STUDIO_STATE,
  currentSlide,
  defaultLayer,
  selectedLayer,
  studioReducer,
  studioStateToUpdate,
  type ResizeHandle,
} from "./cast-model";
import { SlideCanvas } from "./slide-canvas";
import { SlideFilmstrip } from "./slide-filmstrip";
import { InsertToolbar, LayerList } from "./layer-list";
import { PropertiesPanel } from "./properties-panel";

/**
 * The Studio — the WYSIWYG slide editor, and the surface the rebuild is judged
 * by. `/studio?id=<castId>`.
 *
 * This file is the HOST SEAM and nothing else: it loads the cast, owns the
 * reducer, and turns a save into an api/1 PATCH under the conventions the client
 * enforces. Every edit is a pure reduction (cast-model.ts) and every pixel is a
 * presentational component, so what "the model that would be saved" is at any
 * moment is a question with one answer, in one place.
 *
 * ── Concurrency ─────────────────────────────────────────────────────────────
 * A save carries the If-Match derived from the read revision (API-022, no
 * unconditional overwrite). The standard console flow on a 412 is "re-read and
 * show the current state for review" — but the reviewed thing here is a whole
 * multi-slide document the operator may have spent an hour on, so adopting the
 * server's copy automatically would DESTROY that work in the name of safety.
 * Instead the draft is kept exactly as it is and the operator is given the two
 * honest choices, both explicit: take the server's version (losing theirs), or
 * overwrite with theirs (against the freshly-read revision). What is forbidden
 * is the silent retry, and neither of these is one.
 *
 * ── Leaving with unsaved work ───────────────────────────────────────────────
 * `beforeunload` covers closing or reloading the tab. In-app navigation is
 * guarded at the one door out (the back link) with a confirm, rather than with
 * react-router's `useBlocker` — that hook requires a data router, and this app
 * mounts a plain `BrowserRouter`, so it would throw rather than guard.
 */

/** The shared class for the two cast-level inputs above the filmstrip. */
const castFieldClass =
  "flex min-h-[38px] w-full min-w-0 rounded-input border border-border bg-transparent px-2 py-1 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring";

/** A fresh slide id. Slides are addressed within their cast, so a UUID is
 * plenty; the server may replace it, which is why the reducer takes the id as
 * data rather than minting one itself. */
function newSlideId(): string {
  return crypto.randomUUID();
}

export default function StudioRoute({ api }: { api?: WaiveoApi }) {
  const client = useMemo(() => api ?? createApi(), [api]);
  const [params] = useSearchParams();
  const castId = params.get("id");
  const navigate = useNavigate();

  const [state, dispatch] = useReducer(studioReducer, EMPTY_STUDIO_STATE);
  const [loaded, setLoaded] = useState<Cast | null>(null);
  const [etag, setEtag] = useState<string | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  /** Set when a save hit a 412; holds the server's current cast for review. */
  const [conflict, setConflict] = useState<{ cast: Cast; etag: string } | null>(null);
  const [pickerOpen, setPickerOpen] = useState(false);
  const [leaveOpen, setLeaveOpen] = useState(false);
  /** Every entity the box knows, for an `entity` widget's subject picker. */
  const [entities, setEntities] = useState<Entity[]>([]);

  // ── The content library, for drawing content-bearing layers ───────────────
  //
  // Loaded unconditionally (not only when the picker opens) because the CANVAS
  // needs it, not just the picker: a layer holds a content-addressed
  // `asset_ref`, and the url for those bytes is minted per response and EXPIRES
  // (internal/feeder/contenturl). So an image is drawn from a url resolved now,
  // never from one saved with the cast.
  //
  // Saving one is what broke: the picker patched the listing's url into the
  // layer, the save persisted it, and reopening the cast after the deadline drew
  // broken images against a url the origin refuses. The server now declines to
  // store it at all (internal/app/store/derivedmembers.go); this is the other
  // half — the live source it is dropped in favour of.
  const { assets: contentAssets } = useContentLibrary(client);
  // An asset picked THIS session is merged over the listing. The picker fetches
  // its own, fresher listing when it opens, so an asset uploaded after the
  // editor mounted is pickable but absent from the map above — and without this
  // the operator would choose an image and watch the canvas keep showing the
  // "nothing chosen" outline. It holds a url, not the document: this is the
  // render-time lookup, and nothing here is ever saved.
  const [pickedUrls, setPickedUrls] = useState<ReadonlyMap<string, string>>(new Map());
  const assetUrls = useMemo(
    () => new Map([...(contentAssets ?? []).map((a) => [a.asset_ref, a.url] as const), ...pickedUrls]),
    [contentAssets, pickedUrls],
  );

  const slide = currentSlide(state);
  const layer = selectedLayer(state);
  const problemsBySlide = useMemo(() => validateCastSlides(state.slides), [state.slides]);
  const slideProblems = problemsBySlide.get(state.slideIndex) ?? [];

  // A PATCH of the slides array is ATOMIC: the server validates every slide and
  // refuses the whole body if ONE of them will not draw (openapi CastSlide, then
  // datamodel checkCastSlides → wire.ValidateAuthoredSlideLayers). So a Save
  // offered while any slide is invalid is a button that discards an hour of work
  // on nine good slides because of a tenth — reported as one opaque toast from
  // the server, at the moment the operator was told the work was safe.
  //
  // New slides now land VALID (cast-model.newSlide), which removes the common
  // way in; this gate is the other half, because validity can still be DESTROYED
  // by ordinary editing — clearing a text layer, deleting the last layer, or
  // inserting an image layer before choosing its bytes. The filmstrip already
  // badges which slides, and the reason is stated next to the button rather than
  // left as an inert control.
  const invalidSlideCount = problemsBySlide.size;
  const blocked = invalidSlideCount > 0;

  // ── Load ──────────────────────────────────────────────────────────────────
  useEffect(() => {
    if (!castId) return;
    let cancelled = false;
    void (async () => {
      try {
        const read = await client.casts.get(castId);
        if (cancelled) return;
        setLoaded(read.data);
        setEtag(read.etag);
        setLoadError(null);
        dispatch({ type: "loaded", cast: read.data });
      } catch (err) {
        if (cancelled) return;
        setLoadError(err instanceof ApiError ? (err.detail ?? err.code) : "The service is unreachable.");
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [client, castId]);

  // ── Entities, for the entity widget's subject picker ──────────────────────
  //
  // Read once when the editor opens rather than when the picker is first shown:
  // the properties panel is a pure presentational component with no client of
  // its own, and a read started by a render would fire again on every re-render
  // of a panel that re-renders on every keystroke.
  //
  // A failure is deliberately NOT surfaced as an error. The entity list is an
  // aid to ONE widget kind out of eight; taking the whole editor down (or
  // shouting at an operator laying out a text slide) because the device plane is
  // unreachable would be a worse answer than the picker degrading to a plain id
  // field, which is exactly what an empty list makes it do.
  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const rows = await collectPages<Entity>((cursor) => client.entities.list({ cursor }));
        if (!cancelled) setEntities(rows);
      } catch {
        if (!cancelled) setEntities([]);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [client]);

  // ── Unsaved-changes guard (tab close / reload) ────────────────────────────
  const dirtyRef = useRef(state.dirty);
  dirtyRef.current = state.dirty;
  useEffect(() => {
    const onBeforeUnload = (e: BeforeUnloadEvent) => {
      if (!dirtyRef.current) return;
      e.preventDefault();
      // Required by the spec for the prompt to appear in some engines; the
      // message itself has been ignored by every browser for years.
      e.returnValue = "";
    };
    window.addEventListener("beforeunload", onBeforeUnload);
    return () => window.removeEventListener("beforeunload", onBeforeUnload);
  }, []);

  // ── Save ──────────────────────────────────────────────────────────────────
  const save = useCallback(
    async (withEtag: string) => {
      if (!castId) return;
      setSaving(true);
      try {
        const outcome = await updateWithReview(client.casts, castId, studioStateToUpdate(state), withEtag);
        if (outcome.status === "conflict") {
          setConflict({ cast: outcome.current.data, etag: outcome.current.etag });
          toast.error("This cast changed elsewhere — choose which version to keep.");
        } else {
          setLoaded(outcome.resource.data);
          setEtag(outcome.resource.etag);
          setConflict(null);
          dispatch({ type: "saved", cast: outcome.resource.data });
          toast.success("Saved");
        }
      } catch (err) {
        if (err instanceof RevisionConflictError) {
          toast.error("This cast changed elsewhere — reload to see the current version.");
        } else if (err instanceof ApiError) {
          toast.error(`Couldn't save — ${Object.values(err.fieldErrors)[0] ?? err.detail ?? err.code}`);
        } else {
          toast.error("Couldn't save — the service is unreachable.");
        }
      } finally {
        setSaving(false);
      }
    },
    [client, castId, state],
  );

  // ── Editing callbacks (thin: every one is a single dispatch) ──────────────
  // `Date.now()` is read HERE, at the moment of the click, rather than inside
  // `defaultLayer` — a countdown's default target is "midnight tonight", and a
  // now captured at render time would be stale by however long the editor has
  // been open.
  const onInsert = useCallback(
    (kind: LayerKind) => dispatch({ type: "insertLayer", layer: defaultLayer(kind, Date.now()) }),
    [],
  );
  const onSelectLayer = useCallback((index: number | null) => dispatch({ type: "selectLayer", index }), []);
  const onGeometry = useCallback(
    (index: number, geometry: Pick<SlideLayer, "x" | "y" | "w" | "h">) =>
      dispatch({ type: "patchLayer", index, patch: geometry }),
    [],
  );
  const onNudge = useCallback(
    (index: number, dx: number, dy: number) => dispatch({ type: "moveLayer", index, dx, dy }),
    [],
  );
  const onResizeBy = useCallback(
    (index: number, handle: ResizeHandle, dx: number, dy: number) =>
      dispatch({ type: "resizeLayer", index, handle, dx, dy }),
    [],
  );
  const onDeleteLayer = useCallback((index: number) => dispatch({ type: "deleteLayer", index }), []);

  if (!castId) {
    return (
      <StudioFrame>
        <p role="alert" className="text-sm text-[color:var(--wv-err)]">
          No cast selected. <Link className="underline" to="/casts">Choose one from the cast library.</Link>
        </p>
      </StudioFrame>
    );
  }

  if (loadError) {
    return (
      <StudioFrame>
        <p role="alert" className="text-sm text-[color:var(--wv-err)]">
          Couldn't open this cast — {loadError}
        </p>
        <Link className="text-sm underline" to="/casts">
          Back to casts
        </Link>
      </StudioFrame>
    );
  }

  if (!loaded) {
    return (
      <StudioFrame>
        <p className="text-sm text-muted-foreground">Opening the cast…</p>
      </StudioFrame>
    );
  }

  return (
    <div className="min-h-screen bg-background text-foreground">
      <div className="mx-auto flex max-w-[1400px] flex-col gap-5 px-4 py-6 lg:px-8">
        <PageHeader
          title="Studio"
          description="Place native text, shapes, images, video and live widgets on the 1920×1080 canvas your screens draw directly."
          actions={
            <>
              <Button
                variant="ghost"
                icon={ArrowLeft}
                onClick={() => (state.dirty ? setLeaveOpen(true) : navigate("/casts"))}
              >
                Back to casts
              </Button>
              {/* No ETag means no read happened, and there is no
                  unconditional-overwrite path to fall back on (API-022) — so
                  the button is inert rather than sending an empty If-Match the
                  server would refuse with a message about preconditions. */}
              <Button
                icon={Save}
                disabled={!state.dirty || saving || etag === null || blocked}
                onClick={() => {
                  if (etag !== null && !blocked) void save(etag);
                }}
              >
                {saving ? "Saving…" : state.dirty ? "Save changes" : "Saved"}
              </Button>
            </>
          }
        />

        {blocked ? (
          <p role="status" className="text-sm text-[color:var(--wv-warn)]">
            {invalidSlideCount === 1
              ? "One slide won't draw yet, so saving is held: the server refuses the whole cast if any slide is invalid."
              : `${invalidSlideCount} slides won't draw yet, so saving is held: the server refuses the whole cast if any slide is invalid.`}{" "}
            The filmstrip marks them — open each and fix what the panel flags.
          </p>
        ) : null}

        {/* Cast-level settings: what the whole document is called, and how long
            its slides hold by default. Both belong beside each other and above
            the filmstrip because both are properties of the CAST — the
            properties panel to the right is per-layer and per-slide, and putting
            a cast-wide control in it would read as "this slide". */}
        <section aria-label="Playback" className="grid max-w-3xl grid-cols-1 gap-4 sm:grid-cols-2">
          <FormField label="Cast name">
            {(field) => (
              <input
                {...field}
                type="text"
                className={castFieldClass}
                value={state.name}
                onChange={(e) => dispatch({ type: "rename", name: e.target.value })}
              />
            )}
          </FormField>
          <FormField
            label="Default slide duration (seconds)"
            help="Applies to every slide that sets no duration of its own. Leave blank and those slides fall back to the playlist's setting, then to the screen's own default. Slides keep looping while this cast is scheduled — a screen always cycles its content."
          >
            {(field) => (
              <input
                {...field}
                type="number"
                min={1}
                step={1}
                className={castFieldClass}
                value={state.defaultDurationMs === null ? "" : String(Math.round(state.defaultDurationMs / 1000))}
                onChange={(e) => {
                  const seconds = Number(e.target.value);
                  // Blank (and anything unparseable) CLEARS, which is a real
                  // statement the save carries as an explicit null — not the
                  // same as zero, which is no dwell time at all and which the
                  // server refuses.
                  dispatch({
                    type: "setDefaultDuration",
                    durationMs:
                      e.target.value.trim() === "" || !Number.isFinite(seconds) || seconds <= 0
                        ? null
                        : seconds * 1000,
                  });
                }}
              />
            )}
          </FormField>
        </section>

        {conflict ? (
          <div
            role="status"
            className="flex flex-col gap-3 rounded-card border border-[color:var(--wv-warn)] bg-[color:var(--wv-warn-bg)] p-4 text-sm text-[color:var(--wv-warn)]"
          >
            <p>
              This cast was changed elsewhere while you were editing (the box is now at revision{" "}
              {conflict.cast.revision}). Your edits are still here — choose which version to keep.
            </p>
            <div className="flex flex-wrap gap-2">
              <Button
                variant="secondary"
                onClick={() => {
                  setLoaded(conflict.cast);
                  setEtag(conflict.etag);
                  dispatch({ type: "loaded", cast: conflict.cast });
                  setConflict(null);
                  toast.success("Loaded the current version");
                }}
              >
                Load the current version (discards yours)
              </Button>
              {/* Same gate as Save: overwriting is still a PATCH of the whole
                  slides array, so an invalid draft would lose the conflict
                  review AND the write. */}
              <Button
                variant="destructive"
                disabled={saving || blocked}
                onClick={() => {
                  if (!blocked) void save(conflict.etag);
                }}
              >
                Overwrite with my version
              </Button>
            </div>
          </div>
        ) : null}

        <SlideFilmstrip
          slides={state.slides}
          activeIndex={state.slideIndex}
          problemsBySlide={problemsBySlide}
          onSelect={(index) => dispatch({ type: "selectSlide", index })}
          onAdd={() => dispatch({ type: "addSlide", id: newSlideId() })}
          onDuplicate={(index) => dispatch({ type: "duplicateSlide", index, id: newSlideId() })}
          onDelete={(index) => dispatch({ type: "deleteSlide", index })}
          onMove={(from, to) => dispatch({ type: "moveSlide", from, to })}
          assetUrls={assetUrls}
        />

        {slide ? (
          <div className="grid min-w-0 grid-cols-1 gap-5 lg:grid-cols-[minmax(0,1fr)_20rem]">
            <div className="flex min-w-0 flex-col gap-3">
              <InsertToolbar onInsert={onInsert} />
              <SlideCanvas
                slide={slide}
                selectedIndex={state.layerIndex}
                onSelect={onSelectLayer}
                onGeometry={onGeometry}
                onNudge={onNudge}
                onResizeBy={onResizeBy}
                onDelete={onDeleteLayer}
                assetUrls={assetUrls}
              />
              <p className="text-[12px] text-muted-foreground">
                Drag a layer to move it, or drag a corner to resize. With a layer focused: arrow keys nudge,
                Alt+arrows nudge by one pixel, Shift+arrows resize, Delete removes it.
              </p>
            </div>

            <aside className="flex min-w-0 flex-col gap-5 rounded-card border border-border bg-card p-4">
              <LayerList
                layers={slide.layers}
                selectedIndex={state.layerIndex}
                onSelect={onSelectLayer}
                onReorder={(from, to) => dispatch({ type: "reorderLayer", from, to })}
                onDelete={onDeleteLayer}
              />
              <PropertiesPanel
                entities={entities}
                layer={layer}
                problems={slideProblems}
                layerIndex={state.layerIndex}
                onPatch={(patch) => {
                  if (state.layerIndex === null) return;
                  dispatch({ type: "patchLayer", index: state.layerIndex, patch });
                }}
                onPickAsset={() => setPickerOpen(true)}
                slides={state.slides}
                slideIndex={state.slideIndex}
                durationMs={slide.duration_ms}
                onDurationChange={(durationMs) =>
                  dispatch({ type: "setSlideDuration", index: state.slideIndex, durationMs })
                }
              />
            </aside>
          </div>
        ) : (
          <p className="text-sm text-muted-foreground">
            This cast has no slides yet — add one to start building it.
          </p>
        )}
      </div>

      <MediaPickerModal
        open={pickerOpen}
        onOpenChange={setPickerOpen}
        api={client}
        // The origin holds one kind of thing (bytes with a digest), so the same
        // picker serves an image layer and a video layer; only the wording
        // follows the selected layer's kind.
        kind={layer?.kind === "video" ? "video" : "image"}
        selectedRef={layer?.asset_ref}
        onPick={(asset) => {
          if (state.layerIndex === null) return;
          // The asset_ref ALONE. `url` is derived — minted per response, with a
          // deadline — so it belongs in the render-time lookup (assetUrls) and
          // never in the document being edited: a draft carrying it would send
          // it on save, and a stored copy is a link that dies. The server
          // strips one anyway (internal/app/store/derivedmembers.go); not
          // putting it in the model is what makes that a backstop rather than
          // the only defence.
          setPickedUrls((prev) => new Map(prev).set(asset.asset_ref, asset.url));
          dispatch({
            type: "patchLayer",
            index: state.layerIndex,
            patch: { asset_ref: asset.asset_ref, url: undefined },
          });
        }}
      />

      <ConfirmModal
        open={leaveOpen}
        onOpenChange={setLeaveOpen}
        title="Leave without saving?"
        description="This cast has unsaved changes. Leaving now discards them."
        confirmLabel="Discard and leave"
        destructive
        onConfirm={() => navigate("/casts")}
      />

      <Toaster />
    </div>
  );
}

/** The empty/error shell — the page identity still renders so the operator is
 * never looking at a blank screen wondering whether it loaded. */
function StudioFrame({ children }: { children: ReactNode }) {
  return (
    <div className="min-h-screen bg-background text-foreground">
      <div className="mx-auto flex max-w-[1400px] flex-col gap-5 px-4 py-6 lg:px-8">
        <PageHeader
          title="Studio"
          description="Place native text, shapes, images, video and live widgets on the 1920×1080 canvas your screens draw directly."
        />
        {children}
      </div>
    </div>
  );
}
