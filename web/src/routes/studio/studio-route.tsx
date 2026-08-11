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
  type DeriveKind,
  type LayerKind,
  type SlideLayer,
  type WaiveoApi,
} from "@/api";
import { MediaPickerModal, useContentLibrary } from "@/routes/media/media-library";
import {
  EMPTY_STUDIO_STATE,
  applyDerivePatch,
  currentSlide,
  defaultLayer,
  deriveLayer,
  selectedLayer,
  staleRasterKeys,
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
  /**
   * What the content picker is choosing FOR, or null when it is closed.
   *
   * `layer` places bytes on an `image`/`video` layer; `font` attaches a face to
   * a rasterized text layer's `derive.font_asset_ref`. One picker with a target
   * rather than two dialogs: the origin holds one kind of thing (bytes with a
   * digest), and a second copy of the same modal is a second place to forget a
   * rule about it.
   */
  const [pickerFor, setPickerFor] = useState<"layer" | "font" | null>(null);
  const [leaveOpen, setLeaveOpen] = useState(false);
  /** Every entity the box knows, for an `entity` widget's subject picker. */
  const [entities, setEntities] = useState<Entity[]>([]);

  // The content origin's listing, read ONCE for the whole editor and used for
  // two things that must not disagree: resolving every layer's `asset_ref` into
  // a url the canvas can draw, and stocking the picker. `url` is a DERIVED
  // member — producers mint it at projection time and nothing should write one
  // onto an authored layer — so a canvas that waited for an authored url drew
  // nothing for every cast written by anything else, `waiveo-derive`'s rasters
  // first among them.
  //
  // A failure is NOT surfaced as an editor error, for the same reason the entity
  // read is not: taking a text-layout session down because the origin is briefly
  // unreachable is a worse answer than degrading. But it IS surfaced, twice, and
  // that is the correction: the origin has THREE states (in flight, answered,
  // failed) and the canvas models two, so a failed read used to be
  // indistinguishable from "answered, and the origin holds nothing" — which the
  // canvas renders as BYTES MISSING on every finished layer in the cast. An
  // operator who reads that goes and re-uploads or re-renders assets that were
  // never gone.
  //
  // So a failure leaves `assetUrls` NULL — the origin's answer is unknown, and
  // nothing may be reported missing on the strength of a listing we do not have
  // — and says so once, in a status line, rather than as a per-layer badge that
  // states something false about each one.
  const { assets: contentAssets, error: contentError, reload: reloadContent } = useContentLibrary(client);
  const assetUrls = useMemo(() => {
    if (contentError !== null || contentAssets === null) return null;
    return new Map(contentAssets.map((a) => [a.asset_ref, a.url]));
  }, [contentAssets, contentError]);

  // Which drawn rasters this editing session has already invalidated. Computed
  // against the cast AS READ, so it survives every edit without a round trip —
  // see cast-model.staleRasterKeys for why it is not a recomputed digest.
  const staleRasters = useMemo(
    () => (loaded ? staleRasterKeys(loaded.slides, state.slides) : null),
    [loaded, state.slides],
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
  // A rasterized layer inserts through its own callback because `derive` is not
  // one kind: the SPEC decides what is drawn, so "insert a derive layer" has no
  // meaning without one.
  const onInsertDerive = useCallback(
    (kind: DeriveKind) => dispatch({ type: "insertLayer", layer: deriveLayer(kind) }),
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

        {/* The origin's listing failed. Said ONCE, here, rather than as a
            per-layer badge: the canvas cannot tell "the origin has no such
            digest" from "we could not ask", and badging every finished layer
            BYTES MISSING because the box blinked is what sends an operator to
            re-upload assets that were never gone. Layout, text and widget
            editing all still work, so this is a status line and not an error
            page. */}
        {contentError !== null ? (
          <p role="status" className="text-sm text-[color:var(--wv-warn)]">
            Couldn't read the content library ({contentError}). Images, video and rendered layers can't be drawn
            until it answers — nothing has gone missing, and your edits still save.
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
          assetUrls={assetUrls}
          staleRasters={staleRasters}
          onSelect={(index) => dispatch({ type: "selectSlide", index })}
          onAdd={() => dispatch({ type: "addSlide", id: newSlideId() })}
          onDuplicate={(index) => dispatch({ type: "duplicateSlide", index, id: newSlideId() })}
          onDelete={(index) => dispatch({ type: "deleteSlide", index })}
          onMove={(from, to) => dispatch({ type: "moveSlide", from, to })}
        />

        {slide ? (
          <div className="grid min-w-0 grid-cols-1 gap-5 lg:grid-cols-[minmax(0,1fr)_20rem]">
            <div className="flex min-w-0 flex-col gap-3">
              <InsertToolbar onInsert={onInsert} onInsertDerive={onInsertDerive} />
              <SlideCanvas
                slide={slide}
                selectedIndex={state.layerIndex}
                assetUrls={assetUrls}
                staleRasters={staleRasters}
                onSelect={onSelectLayer}
                onGeometry={onGeometry}
                onNudge={onNudge}
                onResizeBy={onResizeBy}
                onDelete={onDeleteLayer}
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
                onPickAsset={() => setPickerFor("layer")}
                onPickFont={() => setPickerFor("font")}
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
        open={pickerFor !== null}
        onOpenChange={(open) => setPickerFor(open ? (pickerFor ?? "layer") : null)}
        assets={contentAssets}
        error={contentError}
        // Passed straight through, NOT wrapped: the picker re-reads on open,
        // and a closure minted per render would make that read re-fire on every
        // render the read itself triggers.
        onReload={reloadContent}
        // The origin holds one kind of thing (bytes with a digest), so the same
        // picker serves an image layer, a video layer and a rasterized text
        // layer's embedded face; only the wording follows what it is choosing
        // for.
        kind={pickerFor === "font" ? "font" : layer?.kind === "video" ? "video" : "image"}
        selectedRef={pickerFor === "font" ? layer?.derive?.font_asset_ref : layer?.asset_ref}
        onPick={(asset) => {
          if (state.layerIndex === null) return;
          if (pickerFor === "font") {
            // The FACE goes on the spec, not the layer: `font_asset_ref` is part
            // of what the rasterizer draws, so it belongs to the digest — which
            // is what makes attaching a font mark the raster stale and get it
            // re-rendered rather than silently keeping the old picture.
            const spec = layer?.derive;
            if (!spec) return;
            dispatch({
              type: "patchLayer",
              index: state.layerIndex,
              patch: { derive: applyDerivePatch(spec, { font_asset_ref: asset.asset_ref }) },
            });
            return;
          }
          // The REF is authored; the url is NOT, and this is the one writer that
          // ever put one on a layer. `url` is DERIVED — producers mint it at
          // projection time — so an authored one is a value nothing re-checks:
          // it survives an export/restore, it outlives a signed url's expiry,
          // and on any canvas that trusts it over the origin's listing it draws
          // dead bytes while reporting nothing missing. The explicit `undefined`
          // REMOVES the key (applyPatch deletes rather than serialising a null),
          // so re-picking on a layer that carries a legacy url clears it. The
          // server strips one anyway (internal/app/store/derivedmembers.go); not
          // putting it in the model is what makes that a backstop rather than
          // the only defence.
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
