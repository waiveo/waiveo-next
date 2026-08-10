import { useCallback, useEffect, useMemo, useReducer, useRef, useState, type ReactNode } from "react";
import { Link, useNavigate, useSearchParams } from "react-router";
import { ArrowLeft, Save } from "lucide-react";
import { Button, ConfirmModal, FormField, PageHeader, Toaster, toast } from "@/components/kit";
import {
  ApiError,
  RevisionConflictError,
  createApi,
  updateWithReview,
  validateCastSlides,
  type Cast,
  type LayerKind,
  type SlideLayer,
  type WaiveoApi,
} from "@/api";
import { MediaPickerModal } from "@/routes/media/media-library";
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

  const slide = currentSlide(state);
  const layer = selectedLayer(state);
  const problemsBySlide = useMemo(() => validateCastSlides(state.slides), [state.slides]);
  const slideProblems = problemsBySlide.get(state.slideIndex) ?? [];

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
  const onInsert = useCallback((kind: LayerKind) => dispatch({ type: "insertLayer", layer: defaultLayer(kind) }), []);
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
          description="Place native text, shapes, images and a live clock on the 1920×1080 canvas your screens draw directly."
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
                disabled={!state.dirty || saving || etag === null}
                onClick={() => {
                  if (etag !== null) void save(etag);
                }}
              >
                {saving ? "Saving…" : state.dirty ? "Save changes" : "Saved"}
              </Button>
            </>
          }
        />

        <div className="max-w-sm">
          <FormField label="Cast name">
            {(field) => (
              <input
                {...field}
                type="text"
                className="flex min-h-[38px] w-full min-w-0 rounded-input border border-border bg-transparent px-2 py-1 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
                value={state.name}
                onChange={(e) => dispatch({ type: "rename", name: e.target.value })}
              />
            )}
          </FormField>
        </div>

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
              <Button variant="destructive" onClick={() => void save(conflict.etag)}>
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
                layer={layer}
                problems={slideProblems}
                layerIndex={state.layerIndex}
                onPatch={(patch) => {
                  if (state.layerIndex === null) return;
                  dispatch({ type: "patchLayer", index: state.layerIndex, patch });
                }}
                onPickImage={() => setPickerOpen(true)}
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
        selectedRef={layer?.asset_ref}
        onPick={(asset) => {
          if (state.layerIndex === null) return;
          dispatch({
            type: "patchLayer",
            index: state.layerIndex,
            patch: { asset_ref: asset.asset_ref, url: asset.url },
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
          description="Place native text, shapes, images and a live clock on the 1920×1080 canvas your screens draw directly."
        />
        {children}
      </div>
    </div>
  );
}
