import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useReducer,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { Link, useNavigate, useSearchParams } from "react-router";
import { ArrowLeft, Grid3x3, Redo2, Save, Undo2 } from "lucide-react";
import { Button, ConfirmModal, KitIcon, Modal, Toaster, toast } from "@/components/kit";
import {
  ApiError,
  RevisionConflictError,
  SLIDE_CANVAS_HEIGHT,
  SLIDE_CANVAS_WIDTH,
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
  alignPatch,
  applyDerivePatch,
  currentSlide,
  defaultLayer,
  deriveLayer,
  duplicateLayerOf,
  selectedLayer,
  staleRasterKeys,
  studioStateToUpdate,
  type AlignTarget,
  type ResizeHandle,
} from "./cast-model";
import {
  EMPTY_STUDIO_HISTORY,
  isTextEntryTarget,
  matchHistoryShortcut,
  redoLabel,
  studioHistoryReducer,
  undoLabel,
  type StudioHistoryAction,
} from "./edit-history";
import { fitToViewport, zoomInFrom, zoomOutFrom } from "./canvas-zoom";
import { matchNudge, matchStudioShortcut, shortcutHints, type StudioCommand } from "./studio-shortcuts";
import { SlideCanvas } from "./slide-canvas";
import { SlideFilmstrip } from "./slide-filmstrip";
import { DerivePickerModal, InsertToolbar, LayerList, WidgetPickerModal } from "./layer-list";
import { PropertiesPanel } from "./properties-panel";
import { HistoryPanel } from "./history-panel";
import { ArrangeControls, type OrderCommand } from "./arrange-controls";
import {
  DockPanel,
  MenuBar,
  SidebarResizer,
  StudioHeaderBar,
  StudioMain,
  StudioMenuRow,
  StudioShell,
  StudioTitleRow,
  StudioToolRail,
  ToolDivider,
  TvFrame,
  ZoomControls,
  type MenuDef,
} from "./studio-chrome";

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
 * ── Why it is full-screen ───────────────────────────────────────────────────
 * It is mounted OUTSIDE the AppShell (see App.tsx) and paints the whole
 * viewport. An editor is not a page: it is a header, a canvas and four docked
 * tool panels that between them want every pixel, and none of them scroll — the
 * REGIONS scroll and the frame holds still, so a drag on the canvas cannot
 * scroll the page out from under the pointer. Rendered in the shell's content
 * column it was giving a sixth of its width to a nav rail whose every
 * destination abandons the cast being edited.
 *
 * The arrangement is the legacy Slidecast Studio's, which the owner hand-built:
 * a two-row header (menu bar over document title), a slide rail down the left,
 * the canvas in a TV frame, a right sidebar of stacked collapsible panels over a
 * resize handle, and a bottom tool rail with a centred zoom cluster. The chrome
 * that draws it is `studio-chrome.tsx`; the reasoning about what was carried
 * over and what was not is there.
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
 * ── Undo ────────────────────────────────────────────────────────────────────
 * Every dispatch goes through `studioHistoryReducer`, which wraps the edit
 * reducer and records a snapshot per step — so an edit cannot be added to the
 * model and forgotten by the history. This file owns only the three things that
 * are not pure: the wall clock each dispatch is stamped with (coalescing needs
 * to know how far apart two edits were, and a reducer may not read a clock),
 * the document-level key handler, and the affordances. Everything else,
 * including what a step IS and when two edits merge into one, is in
 * edit-history.ts with the reasoning. The History panel renders that same stack.
 *
 * ── Leaving with unsaved work ───────────────────────────────────────────────
 * `beforeunload` covers closing or reloading the tab. In-app navigation is
 * guarded at the one door out (the header's Back button, which is present in
 * EVERY state of the editor including the error and empty ones) with the
 * console's confirm modal, rather than with react-router's `useBlocker` — that
 * hook requires a data router, and this app mounts a plain `BrowserRouter`, so
 * it would throw rather than guard. `window.confirm` is not used anywhere in
 * this console and is not used here.
 */

/** The header's cast-name field — legacy's `.cast-name-input`: no chrome until
 * it is touched, so the document title reads as a title and edits as a field. */
const castNameClass =
  "min-w-0 max-w-[420px] flex-1 rounded bg-transparent px-2 py-1 text-[15px] font-semibold text-foreground outline-none transition-colors hover:bg-accent focus:bg-accent focus-visible:ring-2 focus-visible:ring-ring";

const SIDEBAR_MIN = 248;
const SIDEBAR_MAX = 520;
const SIDEBAR_DEFAULT = 304;

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

  // Named `editHistory` rather than `history` so nothing in this file can read
  // as the global `window.history`.
  const [editHistory, rawDispatch] = useReducer(studioHistoryReducer, EMPTY_STUDIO_HISTORY);
  const state = editHistory.present;
  // Every action is stamped with the wall clock ON THE WAY IN. The history
  // reducer is pure and cannot read one, and without a timestamp it can only
  // fall back to "these two arrived together" — which is right inside a pointer
  // gesture and wrong for a burst of typing followed by a pause.
  const dispatch = useCallback((action: StudioHistoryAction) => {
    rawDispatch({ ...action, at: Date.now() });
  }, []);
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
  const [widgetPickerOpen, setWidgetPickerOpen] = useState(false);
  const [derivePickerOpen, setDerivePickerOpen] = useState(false);
  const [shortcutsOpen, setShortcutsOpen] = useState(false);
  /** Every entity the box knows, for an `entity` widget's subject picker. */
  const [entities, setEntities] = useState<Entity[]>([]);

  // ── Workspace layout ──────────────────────────────────────────────────────
  // Panel state is per-session and deliberately NOT persisted. A remembered
  // layout is a real nicety, but it is also a way for an editor to open in a
  // configuration the operator cannot remember choosing (every panel collapsed,
  // the sidebar 500px wide) with no obvious way back; shipping the reset before
  // the memory is the right order.
  // The rail starts folded on a viewport too narrow to hold it, the canvas and
  // the panel column at once — otherwise the three of them overflow the fixed
  // frame and the SIDEBAR is the one pushed off-screen, where nothing can reach
  // it because the frame does not scroll. Read once, at mount: this is a
  // starting position, not a binding, and an editor that folded a panel the
  // operator had just opened because they resized the window would be worse.
  const [railOpen, setRailOpen] = useState(
    () => typeof window === "undefined" || window.innerWidth >= 1024,
  );
  const [layersOpen, setLayersOpen] = useState(true);
  const [historyOpen, setHistoryOpen] = useState(true);
  const [propsOpen, setPropsOpen] = useState(true);
  const [sidebarWidth, setSidebarWidth] = useState(SIDEBAR_DEFAULT);
  const [showGuides, setShowGuides] = useState(false);

  // ── Zoom ──────────────────────────────────────────────────────────────────
  // `null` means "keep fitting" — the viewport recomputes it on every resize, so
  // an editor left open through a window resize stays fitted rather than
  // freezing at the scale it happened to open with. Any explicit zoom pins a
  // number, and Fit puts it back to null.
  //
  // The viewport is held in STATE through a callback ref, not in a `useRef`, and
  // that is the whole correctness of the fit. The element does not exist on the
  // editor's first commit — the cast is still loading, and what renders then is
  // `StudioFrame`, which has no canvas — so an effect keyed on `[]` measured
  // `null`, fell back to 1:1, attached its observer to nothing, and left a 1920
  // canvas at 100% inside an 1100px column forever. A state-holding ref re-runs
  // the effect at the moment the element appears, which is the only moment there
  // is anything to measure.
  const [viewportEl, setViewportEl] = useState<HTMLElement | null>(null);
  const [fitScale, setFitScale] = useState(1);
  const [zoomOverride, setZoomOverride] = useState<number | null>(null);
  const scale = zoomOverride ?? fitScale;

  useLayoutEffect(() => {
    if (!viewportEl) return;
    const measure = () => setFitScale(fitToViewport(viewportEl.clientWidth, viewportEl.clientHeight));
    // Measured synchronously as well as observed: the observer is a no-op shim
    // in the test environment (and fires a frame late in a real one), and a
    // first paint at the wrong scale is a visible jump.
    measure();
    if (typeof ResizeObserver === "undefined") {
      window.addEventListener("resize", measure);
      return () => window.removeEventListener("resize", measure);
    }
    const observer = new ResizeObserver(measure);
    observer.observe(viewportEl);
    return () => observer.disconnect();
  }, [viewportEl]);

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
  const layerCount = slide?.layers.length ?? 0;
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
  // inserting an image layer before choosing its bytes. The slide rail already
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
  }, [client, castId, dispatch]);

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

  // ── Undo / redo ───────────────────────────────────────────────────────────
  const canUndo = editHistory.past.length > 0;
  const canRedo = editHistory.future.length > 0;
  const undoWhat = undoLabel(editHistory);
  const redoWhat = redoLabel(editHistory);
  // The accessible name NAMES the step, so a screen reader and a hover both say
  // what a press will revert rather than leaving the operator to remember.
  const undoName = undoWhat === null ? "Undo" : `Undo ${undoWhat}`;
  const redoName = redoWhat === null ? "Redo" : `Redo ${redoWhat}`;

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
    [client, castId, state, dispatch],
  );

  const savable = state.dirty && !saving && etag !== null && !blocked;

  // ── Editing callbacks (thin: every one is a single dispatch) ──────────────
  // `Date.now()` is read HERE, at the moment of the click, rather than inside
  // `defaultLayer` — a countdown's default target is "midnight tonight", and a
  // now captured at render time would be stale by however long the editor has
  // been open.
  const onInsert = useCallback(
    (kind: LayerKind) => dispatch({ type: "insertLayer", layer: defaultLayer(kind, Date.now()) }),
    [dispatch],
  );
  // A rasterized layer inserts through its own callback because `derive` is not
  // one kind: the SPEC decides what is drawn, so "insert a derive layer" has no
  // meaning without one.
  const onInsertDerive = useCallback(
    (kind: DeriveKind) => dispatch({ type: "insertLayer", layer: deriveLayer(kind) }),
    [dispatch],
  );
  const onSelectLayer = useCallback((index: number | null) => dispatch({ type: "selectLayer", index }), [dispatch]);
  const onGeometry = useCallback(
    (index: number, geometry: Pick<SlideLayer, "x" | "y" | "w" | "h">) =>
      dispatch({ type: "patchLayer", index, patch: geometry }),
    [dispatch],
  );
  const onNudge = useCallback(
    (index: number, dx: number, dy: number) => dispatch({ type: "moveLayer", index, dx, dy }),
    [dispatch],
  );
  const onResizeBy = useCallback(
    (index: number, handle: ResizeHandle, dx: number, dy: number) =>
      dispatch({ type: "resizeLayer", index, handle, dx, dy }),
    [dispatch],
  );
  const onDeleteLayer = useCallback((index: number) => dispatch({ type: "deleteLayer", index }), [dispatch]);
  // The canvas announces the edges of a pointer drag so the hundreds of
  // geometry patches between them collapse to one undo step.
  const onGesture = useCallback(
    (phase: "begin" | "end") => dispatch({ type: "gesture", phase }),
    [dispatch],
  );

  const onAlign = useCallback(
    (target: AlignTarget) => {
      if (state.layerIndex === null || !layer) return;
      dispatch({ type: "patchLayer", index: state.layerIndex, patch: alignPatch(layer, target) });
    },
    [dispatch, state.layerIndex, layer],
  );

  const onOrder = useCallback(
    (command: OrderCommand) => {
      const from = state.layerIndex;
      if (from === null || layerCount === 0) return;
      const to =
        command === "front" ? layerCount - 1
        : command === "forward" ? from + 1
        : command === "backward" ? from - 1
        : 0;
      dispatch({ type: "reorderLayer", from, to });
    },
    [dispatch, state.layerIndex, layerCount],
  );

  const onLeave = useCallback(() => {
    if (state.dirty) setLeaveOpen(true);
    else navigate("/casts");
  }, [state.dirty, navigate]);

  // ── The command table ─────────────────────────────────────────────────────
  //
  // ONE implementation per command, shared by the menu bar, the tool rail and
  // the keyboard. That is not tidiness: the failure it prevents is a menu row
  // and a shortcut that drift into doing subtly different things, which is
  // invisible in review because each of them works.
  const runCommand = useCallback(
    (command: StudioCommand) => {
      switch (command) {
        case "save":
          if (etag !== null && savable) void save(etag);
          return;
        case "zoomIn":
          setZoomOverride(zoomInFrom(scale));
          return;
        case "zoomOut":
          setZoomOverride(zoomOutFrom(scale));
          return;
        case "zoomFit":
          setZoomOverride(null);
          return;
        case "actualSize":
          setZoomOverride(1);
          return;
        case "bringToFront":
          onOrder("front");
          return;
        case "bringForward":
          onOrder("forward");
          return;
        case "sendBackward":
          onOrder("backward");
          return;
        case "sendToBack":
          onOrder("back");
          return;
        case "duplicateLayer":
          if (layer) dispatch({ type: "insertLayer", layer: duplicateLayerOf(layer) });
          return;
        case "deleteLayer":
          if (state.layerIndex !== null) onDeleteLayer(state.layerIndex);
          return;
        case "deselect":
          onSelectLayer(null);
          return;
        case "shortcuts":
          setShortcutsOpen(true);
          return;
      }
    },
    [etag, savable, save, scale, onOrder, layer, dispatch, state.layerIndex, onDeleteLayer, onSelectLayer],
  );

  // Whether a dialog is in front of the editor. Read from a ref inside the key
  // handler so opening the picker does not tear the listener down and rebind
  // it; the value it needs is "is one open right now", not a dependency.
  // A CONFLICT is deliberately not in this list. It renders as an inline status
  // bar, not a dialog: the editor behind it is still the thing the operator is
  // addressing, and taking ⌘Z away at the moment they most want to reconsider an
  // edit before overwriting somebody else's would be the wrong answer.
  const modalOpen = pickerFor !== null || leaveOpen || widgetPickerOpen || derivePickerOpen || shortcutsOpen;
  const modalOpenRef = useRef(modalOpen);
  modalOpenRef.current = modalOpen;
  // Same trick for the command table, which changes every render: the listener
  // must not be rebound on every keystroke.
  const runCommandRef = useRef(runCommand);
  runCommandRef.current = runCommand;
  // …and for the selection, which the nudge acts on.
  const layerIndexRef = useRef(state.layerIndex);
  layerIndexRef.current = state.layerIndex;

  // The shortcuts are bound to the DOCUMENT, because the thing being edited is
  // the whole cast and the operator may have focus anywhere in the editor — the
  // canvas, the slide rail, a layer row, or nothing at all. Bound in an effect
  // whose cleanup removes them, so they exist exactly as long as the Studio is
  // mounted: navigating away takes ⌘Z and ⌘S back to the browser rather than
  // leaving a handler behind that edits a cast nobody is looking at.
  useEffect(() => {
    const onKeyDown = (e: KeyboardEvent) => {
      // A text field keeps its own undo, over the characters in it, and its own
      // Delete, over the character before the caret. Taking either from an
      // operator mid-word would be a worse surprise than not having the
      // shortcut at all — so a keystroke that landed in one is never ours,
      // whichever table would have matched it.
      if (isTextEntryTarget(e.target)) return;
      // A dialog is a modal context: the editor behind it is not what the
      // operator is addressing.
      if (modalOpenRef.current) return;

      // The arrows are matched FIRST because they are the most specific chord:
      // Alt+arrow is the one-pixel nudge, and Alt disqualifies everything in the
      // other two tables precisely so it can be.
      const nudge = matchNudge(e);
      if (nudge !== null) {
        const index = layerIndexRef.current;
        // No selection means the arrows are not ours: leave them to the browser,
        // which will scroll the canvas viewport with them, which is what an
        // operator pressing an arrow at nothing in particular meant.
        if (index === null) return;
        e.preventDefault();
        if (nudge.resize) onResizeBy(index, "se", nudge.dx, nudge.dy);
        else onNudge(index, nudge.dx, nudge.dy);
        return;
      }

      const historyCommand = matchHistoryShortcut(e);
      if (historyCommand !== null) {
        // Claimed whether or not there is anything on the stack. The Studio owns
        // ⌘Z while it is mounted, and a key that sometimes reaches the browser
        // and sometimes does not is the worse of the two behaviours.
        e.preventDefault();
        dispatch({ type: historyCommand });
        return;
      }

      const command = matchStudioShortcut(e);
      if (command === null) return;
      e.preventDefault();
      runCommandRef.current(command);
    };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
    // The two nudge callbacks are listed even though the listener must not be
    // rebound on every render: both are `useCallback`s over `[dispatch]`, so
    // they are as stable as `dispatch` itself and naming them costs nothing.
    // The volatile things this handler reads — the command table and the
    // selection — go through refs above, deliberately.
  }, [dispatch, onNudge, onResizeBy]);

  const keys = shortcutHints();

  // ── The menu bar ──────────────────────────────────────────────────────────
  const menus: MenuDef[] = [
    {
      id: "file",
      label: "File",
      items: [
        { id: "save", label: "Save", shortcut: keys.save, disabled: !savable, run: () => runCommand("save") },
        { id: "sep-1", separator: true },
        { id: "close", label: "Close editor", run: onLeave },
      ],
    },
    {
      id: "edit",
      label: "Edit",
      items: [
        { id: "undo", label: undoName, shortcut: keys.undo, disabled: !canUndo, run: () => dispatch({ type: "undo" }) },
        { id: "redo", label: redoName, shortcut: keys.redo, disabled: !canRedo, run: () => dispatch({ type: "redo" }) },
        { id: "sep-1", separator: true },
        {
          id: "duplicate",
          label: "Duplicate layer",
          shortcut: keys.duplicateLayer,
          disabled: !layer,
          run: () => runCommand("duplicateLayer"),
        },
        {
          id: "delete",
          label: "Delete layer",
          shortcut: keys.deleteLayer,
          disabled: !layer,
          run: () => runCommand("deleteLayer"),
        },
        {
          id: "deselect",
          label: "Deselect",
          shortcut: keys.deselect,
          disabled: !layer,
          run: () => runCommand("deselect"),
        },
      ],
    },
    {
      id: "view",
      label: "View",
      items: [
        { id: "zoom-in", label: "Zoom in", shortcut: keys.zoomIn, run: () => runCommand("zoomIn") },
        { id: "zoom-out", label: "Zoom out", shortcut: keys.zoomOut, run: () => runCommand("zoomOut") },
        { id: "zoom-fit", label: "Fit to window", shortcut: keys.zoomFit, run: () => runCommand("zoomFit") },
        { id: "actual", label: "Actual size", shortcut: keys.actualSize, run: () => runCommand("actualSize") },
        { id: "sep-1", separator: true },
        { id: "guides", label: "Guides", checked: showGuides, run: () => setShowGuides((v) => !v) },
        { id: "sep-2", separator: true },
        { id: "p-slides", label: "Slides panel", checked: railOpen, run: () => setRailOpen((v) => !v) },
        { id: "p-layers", label: "Layers panel", checked: layersOpen, run: () => setLayersOpen((v) => !v) },
        { id: "p-history", label: "History panel", checked: historyOpen, run: () => setHistoryOpen((v) => !v) },
        { id: "p-props", label: "Properties panel", checked: propsOpen, run: () => setPropsOpen((v) => !v) },
      ],
    },
    {
      id: "insert",
      label: "Insert",
      items: [
        { id: "i-text", label: "Text", run: () => onInsert("text") },
        { id: "i-rect", label: "Rectangle", run: () => onInsert("rect") },
        { id: "i-image", label: "Image", run: () => onInsert("image") },
        { id: "i-video", label: "Video", run: () => onInsert("video") },
        { id: "i-clock", label: "Clock", run: () => onInsert("clock") },
        { id: "sep-1", separator: true },
        { id: "i-ping", label: "Button", run: () => onInsert("ping") },
        { id: "i-nav", label: "Menu", run: () => onInsert("nav") },
        { id: "sep-2", separator: true },
        { id: "i-widget", label: "Widget…", run: () => setWidgetPickerOpen(true) },
        { id: "i-derive", label: "Rasterized…", run: () => setDerivePickerOpen(true) },
      ],
    },
    {
      id: "arrange",
      label: "Arrange",
      items: [
        {
          id: "front",
          label: "Bring to front",
          shortcut: keys.bringToFront,
          disabled: !layer || state.layerIndex === layerCount - 1,
          run: () => runCommand("bringToFront"),
        },
        {
          id: "forward",
          label: "Bring forward",
          shortcut: keys.bringForward,
          disabled: !layer || state.layerIndex === layerCount - 1,
          run: () => runCommand("bringForward"),
        },
        {
          id: "backward",
          label: "Send backward",
          shortcut: keys.sendBackward,
          disabled: !layer || state.layerIndex === 0,
          run: () => runCommand("sendBackward"),
        },
        {
          id: "back",
          label: "Send to back",
          shortcut: keys.sendToBack,
          disabled: !layer || state.layerIndex === 0,
          run: () => runCommand("sendToBack"),
        },
        { id: "sep-1", separator: true },
        // Legacy nests these under an Arrange ▸ Align submenu. Flattened here:
        // a hover-submenu is the one menu affordance that is hard to reach with
        // a keyboard, and six rows behind a separator cost nothing.
        { id: "a-left", label: "Align left", disabled: !layer, run: () => onAlign("left") },
        { id: "a-hcenter", label: "Align horizontal centre", disabled: !layer, run: () => onAlign("hcenter") },
        { id: "a-right", label: "Align right", disabled: !layer, run: () => onAlign("right") },
        { id: "a-top", label: "Align top", disabled: !layer, run: () => onAlign("top") },
        { id: "a-vmiddle", label: "Align vertical middle", disabled: !layer, run: () => onAlign("vmiddle") },
        { id: "a-bottom", label: "Align bottom", disabled: !layer, run: () => onAlign("bottom") },
      ],
    },
    {
      id: "slide",
      label: "Slide",
      items: [
        { id: "s-add", label: "Add slide", run: () => dispatch({ type: "addSlide", id: newSlideId() }) },
        {
          id: "s-duplicate",
          label: "Duplicate slide",
          disabled: !slide,
          run: () => dispatch({ type: "duplicateSlide", index: state.slideIndex, id: newSlideId() }),
        },
        {
          id: "s-delete",
          label: "Delete slide",
          disabled: !slide,
          run: () => dispatch({ type: "deleteSlide", index: state.slideIndex }),
        },
        { id: "sep-1", separator: true },
        {
          id: "s-earlier",
          label: "Move slide earlier",
          disabled: state.slideIndex === 0,
          run: () => dispatch({ type: "moveSlide", from: state.slideIndex, to: state.slideIndex - 1 }),
        },
        {
          id: "s-later",
          label: "Move slide later",
          disabled: state.slideIndex >= state.slides.length - 1,
          run: () => dispatch({ type: "moveSlide", from: state.slideIndex, to: state.slideIndex + 1 }),
        },
      ],
    },
    {
      id: "help",
      label: "Help",
      items: [
        {
          id: "shortcuts",
          label: "Keyboard shortcuts",
          shortcut: keys.shortcuts,
          run: () => runCommand("shortcuts"),
        },
      ],
    },
  ];

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
    <StudioShell>
      <StudioHeaderBar>
        <StudioMenuRow
          left={
            <>
              {/* The door OUT, first thing on the bar and present in every state
                  of the editor. It asks before discarding — see the leave modal
                  at the bottom of this file. */}
              {/* One label element, not a visible/screen-reader pair: two of
                  them concatenate into "Back to castsBack to casts" as the
                  accessible name, which is what every by-name query and every
                  screen reader then reads. */}
              <Button variant="ghost" size="sm" icon={ArrowLeft} onClick={onLeave}>
                Back to casts
              </Button>
              <MenuBar menus={menus} />
            </>
          }
          right={
            <>
              {/* Undo and redo live in the HEADER rather than beside the canvas
                  because the header is the one region that renders in every
                  state of the editor. Deleting the last slide replaces the
                  canvas column with a "no slides yet" line, and an undo control
                  that disappeared exactly when the operator had just deleted
                  everything would be missing at the only moment it matters.

                  The accessible name carries the step ("Undo delete slide") in
                  every layout; the visible label follows the width it has. */}
              <Button
                variant="ghost"
                size="sm"
                icon={Undo2}
                aria-label={undoName}
                title={undoName}
                disabled={!canUndo}
                onClick={() => dispatch({ type: "undo" })}
              >
                <span className="hidden lg:inline">Undo</span>
              </Button>
              <Button
                variant="ghost"
                size="sm"
                icon={Redo2}
                aria-label={redoName}
                title={redoName}
                disabled={!canRedo}
                onClick={() => dispatch({ type: "redo" })}
              >
                <span className="hidden lg:inline">Redo</span>
              </Button>
              {/* No ETag means no read happened, and there is no
                  unconditional-overwrite path to fall back on (API-022) — so
                  the button is inert rather than sending an empty If-Match the
                  server would refuse with a message about preconditions. */}
              <Button size="sm" icon={Save} disabled={!savable} onClick={() => runCommand("save")}>
                {saving ? "Saving…" : state.dirty ? "Save changes" : "Saved"}
              </Button>
            </>
          }
        />

        <StudioTitleRow
          left={
            <>
              <input
                type="text"
                aria-label="Cast name"
                className={castNameClass}
                value={state.name}
                placeholder="Untitled cast"
                onChange={(e) => dispatch({ type: "rename", name: e.target.value })}
              />
              {state.dirty ? (
                <span className="shrink-0 rounded px-2 py-0.5 text-[11px] font-medium bg-[color:var(--wv-warn-bg)] text-[color:var(--wv-warn)]">
                  Unsaved changes
                </span>
              ) : null}
            </>
          }
          right={
            <>
              {/* The one CAST-WIDE setting, on the cast-wide row. It was in the
                  properties column, which reads as "this slide" and — with the
                  paragraph of resolution-order help it needs — pushed the
                  selected layer's own fields below the fold. Here it sits beside
                  the document's name, where it belongs, and the explanation is a
                  tooltip rather than five lines of the panel.

                  The visible caption is a prefix of the accessible name so the
                  two agree (WCAG 2.5.3): a voice-control user can say what they
                  can read. */}
              <label
                className="hidden items-center gap-1.5 text-[11px] text-muted-foreground md:flex"
                title="Applies to every slide that sets no duration of its own. Leave blank and those slides fall back to the playlist's setting, then to the screen's own default. Slides keep looping while this cast is scheduled — a screen always cycles its content."
              >
                Default slide duration
                <input
                  type="number"
                  min={1}
                  step={1}
                  aria-label="Default slide duration (seconds)"
                  className="w-[68px] rounded-input border border-border bg-transparent px-1.5 py-0.5 text-right text-[12px] tabular-nums text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
                  placeholder="auto"
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
                <span aria-hidden="true">s</span>
              </label>
              <span className="hidden items-center gap-1.5 rounded bg-[color:var(--wv-surface-2)] px-2 py-1 text-[11px] text-muted-foreground sm:flex">
                <KitIcon icon={Grid3x3} decorative className="size-3" />
                {/* A readout, not a control. Legacy's chip opens a Canvas Size
                    dialog; this wire has no such setting — 1920×1080 is a
                    constant of the player contract (wire.SlideCanvasWidth /
                    Height), not a property of a cast — so a button here would be
                    the console's oldest defect shape: a control that accepts the
                    gesture and cannot perform it. The title says why. */}
                <span title="Every cast is authored on this fixed canvas; a screen scales it to fill whatever panel it has.">
                  {SLIDE_CANVAS_WIDTH} × {SLIDE_CANVAS_HEIGHT}
                </span>
              </span>
            </>
          }
        />
      </StudioHeaderBar>

      {/* Everything the editor has to SAY, in one band under the header rather
          than interleaved with the workspace: a full-screen editor has no
          document flow to push a notice into, and a message that covered part of
          the canvas would be worse than one that costs a strip of height. */}
      {blocked || contentError !== null || conflict ? (
        <div className="flex shrink-0 flex-col gap-2 border-b border-border bg-[color:var(--wv-surface)] px-3 py-2">
          {blocked ? (
            <p role="status" className="text-[13px] text-[color:var(--wv-warn)]">
              {invalidSlideCount === 1
                ? "One slide won't draw yet, so saving is held: the server refuses the whole cast if any slide is invalid."
                : `${invalidSlideCount} slides won't draw yet, so saving is held: the server refuses the whole cast if any slide is invalid.`}{" "}
              The slide rail marks them — open each and fix what the panel flags.
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
            <p role="status" className="text-[13px] text-[color:var(--wv-warn)]">
              Couldn't read the content library ({contentError}). Images, video and rendered layers can't be drawn
              until it answers — nothing has gone missing, and your edits still save.
            </p>
          ) : null}

          {conflict ? (
            <div role="status" className="flex flex-wrap items-center gap-3 text-[13px] text-[color:var(--wv-warn)]">
              <span>
                This cast was changed elsewhere while you were editing (the box is now at revision{" "}
                {conflict.cast.revision}). Your edits are still here — choose which version to keep.
              </span>
              <Button
                size="sm"
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
                size="sm"
                variant="destructive"
                disabled={saving || blocked}
                onClick={() => {
                  if (!blocked) void save(conflict.etag);
                }}
              >
                Overwrite with my version
              </Button>
            </div>
          ) : null}
        </div>
      ) : null}

      <StudioMain>
        {railOpen ? (
          <aside
            data-slot="slide-rail"
            className="flex w-[188px] shrink-0 flex-col border-r border-border bg-[color:var(--wv-surface)]"
          >
            <SlideFilmstrip
              slides={state.slides}
              activeIndex={state.slideIndex}
              problemsBySlide={problemsBySlide}
              defaultDurationMs={state.defaultDurationMs}
              assetUrls={assetUrls}
              staleRasters={staleRasters}
              onSelect={(index) => dispatch({ type: "selectSlide", index })}
              onAdd={() => dispatch({ type: "addSlide", id: newSlideId() })}
              onDuplicate={(index) => dispatch({ type: "duplicateSlide", index, id: newSlideId() })}
              onDelete={(index) => dispatch({ type: "deleteSlide", index })}
              onMove={(from, to) => dispatch({ type: "moveSlide", from, to })}
            />
          </aside>
        ) : null}

        {/* The canvas viewport. It SCROLLS rather than clipping, so a zoom past
            the window is navigable instead of half-visible, and the flex box
            inside centres the frame whenever it does fit — `min-h-full` plus
            `items-center` gives centring below the viewport size and top-left
            origin above it, which is what every editor does and what a plain
            `items-center` alone gets wrong (it centres the overflow and makes
            the top of the artwork unreachable). */}
        <main ref={setViewportEl} data-slot="canvas-viewport" className="relative min-w-0 flex-1 overflow-auto bg-background">
          {slide ? (
            <div className="flex min-h-full min-w-full items-center justify-center p-6">
              <TvFrame>
                <SlideCanvas
                  slide={slide}
                  scale={scale}
                  stageClassName="rounded-none border-0"
                  showGuides={showGuides}
                  selectedIndex={state.layerIndex}
                  assetUrls={assetUrls}
                  staleRasters={staleRasters}
                  onSelect={onSelectLayer}
                  onGeometry={onGeometry}
                  onGesture={onGesture}
                />
              </TvFrame>
            </div>
          ) : (
            <div className="flex h-full items-center justify-center p-6">
              <p className="text-sm text-muted-foreground">
                This cast has no slides yet — add one to start building it.
              </p>
            </div>
          )}
        </main>

        <aside
          data-slot="studio-sidebar"
          className="relative flex shrink-0 flex-col border-l border-border bg-[color:var(--wv-surface)]"
          // The cap is a fraction of the VIEWPORT, so the panel column can never
          // take so much of a small screen that the canvas has nothing left. It
          // is a max-width rather than a clamp on the stored width, because the
          // operator's chosen width should come back when the window does.
          style={{ width: sidebarWidth, maxWidth: "50vw" }}
        >
          <SidebarResizer width={sidebarWidth} min={SIDEBAR_MIN} max={SIDEBAR_MAX} onWidth={setSidebarWidth} />

          <DockPanel
            title="Layers"
            count={layerCount === 0 ? undefined : String(layerCount)}
            collapsed={!layersOpen}
            onToggle={() => setLayersOpen((v) => !v)}
            maxBodyClass="max-h-[34vh]"
          >
            {slide ? (
              <LayerList
                layers={slide.layers}
                selectedIndex={state.layerIndex}
                onSelect={onSelectLayer}
                onReorder={(from, to) => dispatch({ type: "reorderLayer", from, to })}
                onDelete={onDeleteLayer}
              />
            ) : (
              <p className="px-1 py-2 text-[12px] text-muted-foreground">No slide selected.</p>
            )}
          </DockPanel>

          <DockPanel
            title="History"
            count={`${editHistory.past.length + editHistory.future.length + 1}`}
            collapsed={!historyOpen}
            onToggle={() => setHistoryOpen((v) => !v)}
            maxBodyClass="max-h-[26vh]"
          >
            <HistoryPanel history={editHistory} onJump={(step) => dispatch({ type: "jumpTo", step })} />
          </DockPanel>

          <DockPanel
            title="Properties"
            collapsed={!propsOpen}
            onToggle={() => setPropsOpen((v) => !v)}
            grow
          >
            <div className="flex flex-col gap-4 pt-2">
              {slide ? (
                <PropertiesPanel
                  arrange={
                    layer && state.layerIndex !== null ? (
                      <ArrangeControls
                        onAlign={onAlign}
                        onOrder={onOrder}
                        canRaise={state.layerIndex < layerCount - 1}
                        canLower={state.layerIndex > 0}
                      />
                    ) : undefined
                  }
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
              ) : null}
            </div>
          </DockPanel>
        </aside>
      </StudioMain>

      <StudioToolRail>
        <InsertToolbar
          onInsert={onInsert}
          onOpenWidgetPicker={() => setWidgetPickerOpen(true)}
          onOpenDerivePicker={() => setDerivePickerOpen(true)}
        />
        <ToolDivider />
        {/* Centred absolutely, as legacy's zoom cluster is: it belongs to the
            canvas above it, not to the row of tools it shares a bar with.
            ONLY from `xl`, though — below about 1280px the nine insert tools and
            a centred cluster want the same pixels, and the cluster wins because
            it is painted over them. There it joins the flow at the right-hand
            end instead, and the insert group scrolls under it. */}
        <div className="ml-auto shrink-0 xl:pointer-events-none xl:absolute xl:left-1/2 xl:ml-0 xl:-translate-x-1/2">
          <div className="xl:pointer-events-auto">
            <ZoomControls
              zoom={scale}
              fitted={zoomOverride === null}
              onZoomIn={() => runCommand("zoomIn")}
              onZoomOut={() => runCommand("zoomOut")}
              onFit={() => runCommand("zoomFit")}
              onActualSize={() => runCommand("actualSize")}
            />
          </div>
        </div>
        <button
          type="button"
          onClick={() => setShortcutsOpen(true)}
          className="ml-auto hidden shrink-0 rounded px-2 py-1 text-[11px] text-muted-foreground outline-none hover:bg-accent hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring 2xl:block"
        >
          {keys.undo} undo · {keys.save} save · {keys.nudge} nudge · {keys.deleteLayer} delete — all shortcuts
        </button>
      </StudioToolRail>

      <WidgetPickerModal open={widgetPickerOpen} onOpenChange={setWidgetPickerOpen} onInsert={onInsert} />
      <DerivePickerModal
        open={derivePickerOpen}
        onOpenChange={setDerivePickerOpen}
        onInsertDerive={onInsertDerive}
      />

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

      <ShortcutsModal open={shortcutsOpen} onOpenChange={setShortcutsOpen} />

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
    </StudioShell>
  );
}

/** The empty/error shell — the full-screen frame and the door out still render,
 * so an operator whose cast failed to open is never looking at a blank screen
 * with no way back. */
function StudioFrame({ children }: { children: ReactNode }) {
  return (
    <StudioShell>
      <StudioHeaderBar>
        <StudioMenuRow
          left={
            <Button variant="ghost" size="sm" icon={ArrowLeft} asChild>
              <Link to="/casts">Back to casts</Link>
            </Button>
          }
          right={null}
        />
      </StudioHeaderBar>
      <div className="flex flex-1 items-center justify-center p-8">{children}</div>
      <Toaster />
    </StudioShell>
  );
}

/** The keyboard sheet — legacy's KeyboardShortcutsModal, printed from the same
 * table the menus print, so a chord cannot be advertised here and bound nowhere.
 */
function ShortcutsModal({ open, onOpenChange }: { open: boolean; onOpenChange: (open: boolean) => void }) {
  const keys = shortcutHints();
  const groups: Array<{ title: string; rows: Array<[string, string]> }> = [
    {
      title: "Document",
      rows: [
        [keys.save, "Save the cast"],
        [keys.undo, "Undo"],
        [keys.redo, "Redo"],
      ],
    },
    {
      title: "View",
      rows: [
        [keys.zoomIn, "Zoom in"],
        [keys.zoomOut, "Zoom out"],
        [keys.zoomFit, "Fit to window"],
        [keys.actualSize, "Actual size (100%)"],
      ],
    },
    {
      title: "The selected layer",
      rows: [
        [keys.nudge, "Move by 8px"],
        [keys.nudgeFine, "Move by 1px"],
        [keys.resize, "Resize from the bottom-right"],
        [keys.duplicateLayer, "Duplicate"],
        [keys.deleteLayer, "Delete"],
        [keys.deselect, "Deselect"],
      ],
    },
    {
      title: "Stacking order",
      rows: [
        [keys.bringToFront, "Bring to front"],
        [keys.bringForward, "Bring forward"],
        [keys.sendBackward, "Send backward"],
        [keys.sendToBack, "Send to back"],
      ],
    },
  ];
  return (
    <Modal
      title="Keyboard shortcuts"
      description="Every chord here acts on the SELECTED layer and works from anywhere in the editor — except inside a text field, where the browser's own editing keys win."
      open={open}
      onOpenChange={onOpenChange}
      size="lg"
      footer={
        <Button variant="secondary" onClick={() => onOpenChange(false)}>
          Close
        </Button>
      }
    >
      <div className="grid grid-cols-1 gap-5 sm:grid-cols-2">
        {groups.map((group) => (
          <section key={group.title} className="flex flex-col gap-1.5">
            <h3 className="text-[11px] font-semibold uppercase tracking-[0.06em] text-muted-foreground">
              {group.title}
            </h3>
            <dl className="flex flex-col gap-1">
              {group.rows.map(([chord, what]) => (
                <div key={what} className="flex items-baseline justify-between gap-3 text-[13px]">
                  <dt className="text-muted-foreground">{what}</dt>
                  <dd>
                    <kbd className="rounded border border-border bg-[color:var(--wv-surface-2)] px-1.5 py-0.5 font-mono text-[11px]">
                      {chord}
                    </kbd>
                  </dd>
                </div>
              ))}
            </dl>
          </section>
        ))}
      </div>
    </Modal>
  );
}
