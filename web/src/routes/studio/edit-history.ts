// Undo/redo for the Studio — a history layer WRAPPED AROUND the edit model
// rather than mixed into it.
//
// `studioReducer` stays exactly what it was: a pure function from a state and an
// action to the next state. `studioHistoryReducer` below is a reducer over that
// reducer. Every dispatch the editor makes goes through here, so there is no
// such thing as an edit that forgot to record itself — which is the way this
// feature is normally shipped broken (a new action lands, nobody adds it to the
// undo switch, and one operation in the editor is silently unrecoverable).
//
// ── Snapshots, not inverse commands ─────────────────────────────────────────
//
// A step is a SNAPSHOT of `StudioState`, not an inverse of the action.
//
// The usual argument for inverse commands is memory, and it does not apply
// here. A cast is a handful of slides of a handful of layers of a dozen scalar
// fields — kilobytes — and, more importantly, `studioReducer` is already written
// to share structure: `withLayers` slices the slides array and replaces ONE
// slide, `patchLayer` copies ONE layer. So a snapshot is a spine of a few
// pointers over the layer objects the previous snapshot already holds, not a
// deep copy of the document. A hundred of them cost about what one document
// costs.
//
// What inverse commands would cost is correctness. There are 17 action types;
// an inverse table is 17 chances to write an inverse that is subtly not one
// (the inverse of `deleteSlide` has to restore the slide AND its index AND the
// selection; the inverse of `patchLayer` with an explicit `undefined` has to
// re-add a key that was deleted). Every one of those is invisible until an
// operator hits it. A snapshot cannot be subtly wrong: it either is the state or
// it is not.
//
// Coalescing settles it. Merging two snapshots is "do not push the second one",
// which is a no-op. Merging two inverse commands is a real function per pair of
// action types, and it is where editors that chose commands leak steps.
//
// ── What history covers, and where it stops ─────────────────────────────────
//
//   LOADING A CAST CLEARS IT.  `loaded` fires on the initial read and on
//   adopting the server's copy after a 412. Both make the previous document a
//   different document, and an undo across that boundary would write one cast's
//   slides into another cast's draft. The stack, the redo stack and the
//   coalescing run all reset.
//
//   SAVING DOES NOT CLEAR IT, and undoing past the last save IS allowed. The
//   operator's model of ⌘Z is "take back what I just did", and it does not have
//   a notion of a save boundary; an editor that silently emptied the stack on
//   save would punish saving often, which is the habit an editor wants (the
//   `saved` arm of studioReducer already argues exactly this about the
//   selection). Undoing past a save leaves a draft that differs from the box,
//   which is a normal dirty draft — the Save button re-enables and the operator
//   can save the undone version. Nothing is lost either way: the box still holds
//   the saved revision until they do.
//
//   SELECTION IS NOT AN EDIT.  Clicking a different layer or slide does not push
//   a step, so ⌘Z after a click undoes the last EDIT rather than the click. It
//   DOES end the coalescing run (see below), and a step restores the selection
//   that was live when the edit happened, so an undo shows the operator the
//   thing it changed.
//
// ── `dirty` across the boundary ─────────────────────────────────────────────
//
// `dirty` cannot be restored from the snapshot: the snapshot taken before the
// first edit of a session says `false`, and that is still true after an undo —
// unless a SAVE happened in between, in which case the same document is now the
// stale one. So history keeps `baseline`: the state object the last `loaded` or
// `saved` produced. A restored snapshot is clean if and only if it IS that
// object. Reference identity is exact here rather than approximate, because
// every snapshot is an immutable state produced by the same reducer and the
// baseline is one of those very objects — no structural comparison needed, and
// no risk of a deep-equal that says "clean" about a document that is not.
//
// ── Coalescing ──────────────────────────────────────────────────────────────
//
// A drag emits one `patchLayer` per pointermove and a typed word emits one per
// keystroke. Recorded literally, ⌘Z would unwind a 200px drag one pixel at a
// time — an undo stack that is worse than none, and the most common way this
// feature ships broken. Consecutive edits therefore MERGE into one step under
// three rules, in order:
//
//   1. AN EXPLICIT GESTURE WINS.  Between `{type:"gesture",phase:"begin"}` and
//      `phase:"end"` — which the canvas dispatches on pointerdown/pointerup —
//      every edit merges into one step no matter what it touches or how long the
//      operator holds the button. This is the only rule that is exactly right
//      for a drag: a pointer gesture IS one operator action by definition, and
//      an operator who pauses mid-drag to line something up has not performed
//      two of them.
//
//   2. OTHERWISE, SAME KEY WITHIN A WINDOW.  Each action produces a coalescing
//      key or null. Two consecutive edits merge when the keys match and the
//      second lands within COALESCE_WINDOW_MS of the first. That covers typing
//      (every keystroke in one field is one key) and held arrow-key nudges,
//      and it separates a burst from the next burst after a pause.
//
//      The key for a `patchLayer` is the SET OF FIELDS THE PATCH CARRIES, not
//      the set it changed. That distinction is load-bearing: a resize drag
//      always patches {x,y,w,h}, but at a canvas edge x stops changing while w
//      keeps going, so a key derived from what changed would flip mid-gesture
//      and split the drag in two. The panel's X field patches {x} and its Width
//      field patches {w}, so those stay distinct steps.
//
//   3. A NULL KEY NEVER MERGES.  Structural edits — add, delete, duplicate,
//      reorder, insert — are each their own step, always.
//
// A selection change, a save, a load and either end of a gesture all END the
// current run, so the next edit starts a fresh step.
//
// ── The history PANEL ───────────────────────────────────────────────────────
//
// The stack is also a document in its own right, and the Studio shows it: a
// dock panel listing every step with the current position marked, the way every
// editor with a history palette does. Two things here serve it.
//
// `historyRows` projects the two stacks into ONE ordered timeline, because that
// is what an operator sees — `past` and `future` are an implementation of "where
// am I in the list", not two lists. It is a pure function so the panel cannot
// hold its own idea of the order.
//
// `jumpTo` moves several steps at once. It is defined by REPLAYING undo/redo
// rather than by reaching into the stacks, so a jump can never disagree with
// pressing ⌘Z n times — including about `dirty`, the coalescing run, and the
// baseline identity that decides it. A second implementation of the traversal
// is exactly the drift this file's snapshot argument exists to avoid.

import {
  EMPTY_STUDIO_STATE,
  studioReducer,
  type LayerPatch,
  type StudioAction,
  type StudioState,
} from "./cast-model";
import type { DeriveSpec, SlideLayer } from "@/api";
import { KIND_LABEL } from "./layer-catalog";

/** How long a coalescing run stays open between same-key edits, outside an
 * explicit pointer gesture. Long enough that ordinary typing is one step
 * (a slow typist is well inside 600ms per keystroke), short enough that going
 * away, thinking, and coming back to the same field is a second step. */
export const COALESCE_WINDOW_MS = 600;

/** How many steps are kept. Snapshots share structure, so this is cheap — the
 * cap exists so an editor left open all day has a bounded footprint, not
 * because a hundred casts' worth of memory is at stake. The oldest step is
 * dropped, never the newest. */
export const MAX_HISTORY_DEPTH = 100;

export interface StudioHistoryEntry {
  /** The document as it stood BEFORE the edit this entry names. */
  state: StudioState;
  /** What was done to leave that state, as a verb phrase the affordances
   * prefix with "Undo " / "Redo " — "delete slide", "move layer". */
  label: string;
}

export interface StudioHistory {
  present: StudioState;
  /** Oldest step first; the last entry is what ⌘Z takes back. */
  past: StudioHistoryEntry[];
  /** Oldest step first; the last entry is what ⌘⇧Z reapplies. */
  future: StudioHistoryEntry[];
  /** The state the box last confirmed (a load or a save). See the note above:
   * this is how an undo across a save reports `dirty` honestly. */
  baseline: StudioState;
  /** The coalescing run in progress, or null between runs. */
  run: { key: string; at: number } | null;
  /** True between a pointer gesture's begin and end. */
  gesture: boolean;
}

/** Anything the editor dispatches. Every member may carry `at` — the wall clock
 * at the moment of the dispatch — because a reducer may not read one itself and
 * coalescing needs to know how far apart two edits were. An action with no `at`
 * behaves as if it arrived in the same instant as the previous one, so the
 * key-and-barrier rules still hold when a caller does not stamp. */
export type StudioHistoryAction = (
  | { type: "undo" }
  | { type: "redo" }
  /** Go to the point in the timeline where `step` edits have been applied —
   * `step` is `historyRows`' index, and 0 is the document as it was opened. */
  | { type: "jumpTo"; step: number }
  | { type: "gesture"; phase: "begin" | "end" }
  | StudioAction
) & { at?: number };

export const EMPTY_STUDIO_HISTORY: StudioHistory = {
  present: EMPTY_STUDIO_STATE,
  past: [],
  future: [],
  baseline: EMPTY_STUDIO_STATE,
  run: null,
  gesture: false,
};

/** What ⌘Z would take back, or null when there is nothing. */
export function undoLabel(history: StudioHistory): string | null {
  return history.past[history.past.length - 1]?.label ?? null;
}

/** What ⌘⇧Z would reapply, or null when there is nothing. */
export function redoLabel(history: StudioHistory): string | null {
  return history.future[history.future.length - 1]?.label ?? null;
}

/** Where one row of the history panel sits relative to the operator's position:
 * already applied, the state they are looking at, or undone and reachable by
 * redo. */
export type HistoryPosition = "past" | "current" | "future";

export interface HistoryRow {
  /** The point in the timeline this row IS — the number of edits applied at it,
   * and the argument a `jumpTo` takes to land here. Stable while the stack does
   * not change, so it doubles as the React key. */
  step: number;
  label: string;
  position: HistoryPosition;
}

/** The label of the row that is not an edit: the document as the box served it.
 * Photoshop calls this the snapshot; here it is the only row that is always
 * present, including in an editor where nothing has been done yet. */
export const HISTORY_OPEN_LABEL = "open the cast";

/**
 * The two stacks as ONE timeline, oldest first.
 *
 * Row 0 is the opened document, then one row per step in `past` (oldest first),
 * then one per step in `future` — and `future` is traversed BACKWARDS, because
 * its last entry is the next redo and its first is the furthest one away. That
 * reversal is the whole reason this is a function and not a `.map()` at the call
 * site: getting it wrong produces a list that looks plausible and jumps to the
 * wrong state.
 */
export function historyRows(history: StudioHistory): HistoryRow[] {
  const at = history.past.length;
  const rows: HistoryRow[] = [
    { step: 0, label: HISTORY_OPEN_LABEL, position: at === 0 ? "current" : "past" },
  ];
  history.past.forEach((entry, i) => {
    const step = i + 1;
    rows.push({ step, label: entry.label, position: step === at ? "current" : "past" });
  });
  for (let k = 1; k <= history.future.length; k += 1) {
    const entry = history.future[history.future.length - k];
    if (entry) rows.push({ step: at + k, label: entry.label, position: "future" });
  }
  return rows;
}

/** Restore a snapshot, correcting only its `dirty` flag — see the note above on
 * why the flag cannot simply be carried and why identity against the baseline
 * is the right test. The snapshot object itself is returned UNCHANGED whenever
 * the flag already agrees, which is what keeps the baseline's identity intact
 * across a round trip of undo and redo. */
function restore(snapshot: StudioState, baseline: StudioState): StudioState {
  const dirty = snapshot !== baseline;
  if (snapshot.dirty === dirty) return snapshot;
  return { ...snapshot, dirty };
}

export function studioHistoryReducer(
  history: StudioHistory,
  action: StudioHistoryAction,
): StudioHistory {
  switch (action.type) {
    case "undo": {
      const entry = history.past[history.past.length - 1];
      if (!entry) return history;
      return {
        ...history,
        present: restore(entry.state, history.baseline),
        past: history.past.slice(0, -1),
        // The step that is being taken back keeps its label on the way to the
        // redo stack, so "Undo delete slide" is followed by "Redo delete
        // slide" and not by a phrase describing something else.
        future: [...history.future, { state: history.present, label: entry.label }],
        run: null,
        gesture: false,
      };
    }

    case "redo": {
      const entry = history.future[history.future.length - 1];
      if (!entry) return history;
      return {
        ...history,
        present: restore(entry.state, history.baseline),
        past: [...history.past, { state: history.present, label: entry.label }],
        future: history.future.slice(0, -1),
        run: null,
        gesture: false,
      };
    }

    case "jumpTo": {
      // Defined by REPLAY, not by slicing the stacks. Every property an undo
      // establishes — the recomputed `dirty`, the cleared run and gesture, the
      // label that travels with the step — holds for a jump of ten steps
      // because it is literally ten undos. A hand-written slice would be a
      // second traversal to keep in agreement with the first, and the one it
      // would get wrong is `dirty`: the baseline may sit anywhere in the range
      // being crossed.
      //
      // Out-of-range is CLAMPED rather than ignored. The only caller is a list
      // rendered from this very history, so a step outside it means the list
      // and the stack disagreed for a frame; landing at the nearest real point
      // is closer to what was asked than doing nothing.
      //
      // And the replay is a COUNTED loop, not `while (past.length !== target)`.
      // That is not a style preference: `undo` and `redo` both return their
      // input when the stack they draw from is empty, so a condition phrased
      // against the stack's length never becomes false once it runs out — the
      // clamp is what keeps that from happening, and a loop whose termination
      // depends on a bounds check two lines above it is one edit away from
      // hanging the editor. Counting the DISTANCE terminates whatever the clamp
      // does. (Found by deleting the clamp: the tests did not fail, they never
      // returned.)
      const target = Math.max(0, Math.min(action.step, history.past.length + history.future.length));
      const from = history.past.length;
      let next = history;
      for (let i = from; i > target; i -= 1) next = studioHistoryReducer(next, { type: "undo" });
      for (let i = from; i < target; i += 1) next = studioHistoryReducer(next, { type: "redo" });
      return next;
    }

    case "gesture":
      // Both edges end the run: the begin so a gesture never merges into
      // whatever preceded it, the end so nothing merges into the gesture after
      // the pointer is up.
      return { ...history, gesture: action.phase === "begin", run: null };

    default:
      break;
  }

  const next = studioReducer(history.present, action);

  if (action.type === "loaded") {
    // A different document. Anything on the stack belongs to the previous one.
    return { present: next, past: [], future: [], baseline: next, run: null, gesture: false };
  }

  if (action.type === "saved") {
    // The box's copy is the new clean point. The stack survives it — see the
    // note above on why undoing past a save is allowed.
    return { ...history, present: next, baseline: next, run: null };
  }

  // A reducer arm that returned its input did nothing, and doing nothing is not
  // a step. (Half the arms have such a guard: a nudge already at the canvas
  // edge, a rename to the same name, a reorder that goes nowhere.) Without this
  // an arrow key held against the edge would fill the stack with steps that
  // restore the state they were taken from.
  if (next === history.present) return history;

  if (action.type === "selectSlide" || action.type === "selectLayer") {
    // Not an edit, so not a step — but it ends the run, because the next thing
    // typed is being typed at something else.
    return { ...history, present: next, run: null };
  }

  const key = coalesceKey(history.present, action);
  const run = history.run;
  const at = action.at ?? run?.at ?? 0;

  if (
    run !== null &&
    key !== null &&
    // Inside a pointer gesture every edit merges, whatever it touches and
    // however long the operator holds the button; outside one, the key has to
    // match and the burst has to be unbroken.
    (history.gesture || (run.key === key && at - run.at <= COALESCE_WINDOW_MS))
  ) {
    // The step already on the stack holds the state from BEFORE the run began,
    // which is exactly where one undo should land. Nothing is pushed; only the
    // present and the run's clock move.
    return { ...history, present: next, run: { key: run.key, at }, future: [] };
  }

  const pushed = [...history.past, { state: history.present, label: describeEdit(history.present, action) }];
  return {
    ...history,
    present: next,
    past: pushed.length > MAX_HISTORY_DEPTH ? pushed.slice(pushed.length - MAX_HISTORY_DEPTH) : pushed,
    // A new edit makes the redone future unreachable — it branched off a state
    // that is no longer on the path.
    future: [],
    run: key === null ? null : { key, at },
  };
}

// ── Coalescing keys ─────────────────────────────────────────────────────────

/** The geometry members. A patch made only of these came from a drag or from
 * one of the panel's four position/size fields. */
const GEOMETRY_FIELDS: ReadonlySet<string> = new Set(["x", "y", "w", "h"]);

/** The key two consecutive edits must share to merge, or null for "always its
 * own step". Structural edits (add/delete/duplicate/reorder/insert) are null:
 * each is a discrete thing an operator did once and expects back once. */
function coalesceKey(state: StudioState, action: StudioAction): string | null {
  switch (action.type) {
    case "rename":
      return "rename";
    case "setDefaultDuration":
      return "default-duration";
    case "setSlideDuration":
      return `slide-duration:${action.index}`;
    case "moveLayer":
      return `nudge:${state.slideIndex}:${action.index}`;
    case "resizeLayer":
      return `nudge-resize:${state.slideIndex}:${action.index}:${action.handle}`;
    case "patchLayer":
      return `patch:${state.slideIndex}:${action.index}:${patchSignature(state, action.index, action.patch)}`;
    default:
      return null;
  }
}

/** Which fields a patch is ABOUT, as a stable string.
 *
 * The patch's own key set, deliberately, and not the subset whose values
 * differ — see rule 2 above: a clamped resize changes which fields move
 * mid-gesture, and a key derived from that would split one drag into two steps.
 *
 * A `derive` patch is the one case that has to look deeper. The panel rebuilds
 * the WHOLE spec on every keystroke (`applyDerivePatch` returns a complete
 * one), so every rasterized edit would otherwise carry the identical key
 * `derive` and a font-size nudge would merge with the text typed before it.
 * Comparing against the layer's current spec recovers the member actually being
 * edited, which is stable for as long as that member is the one being edited —
 * which is exactly as long as the run should last. */
function patchSignature(state: StudioState, index: number, patch: LayerPatch): string {
  const keys = Object.keys(patch).sort();
  if (keys.length === 1 && keys[0] === "derive") {
    const before = state.slides[state.slideIndex]?.layers[index]?.derive;
    return `derive.${differingSpecMembers(before, patch.derive).join(",")}`;
  }
  return keys.join(",");
}

/** Which members of a derive spec the patch changes. JSON is the comparison
 * because a spec's members are plain wire values (scalars and small objects
 * like `fill` and `border`) with no cycles; a member that compares unequal only
 * because a nested object was rebuilt in a different key order costs an extra
 * undo step at worst, never a merged pair of unrelated edits. */
function differingSpecMembers(before: DeriveSpec | undefined, after: DeriveSpec | undefined): string[] {
  if (!before || !after) return [];
  const was = before as unknown as Record<string, unknown>;
  const now = after as unknown as Record<string, unknown>;
  const names = new Set([...Object.keys(was), ...Object.keys(now)]);
  const changed: string[] = [];
  for (const name of names) {
    if (JSON.stringify(was[name] ?? null) !== JSON.stringify(now[name] ?? null)) changed.push(name);
  }
  return changed.sort();
}

// ── Labels ──────────────────────────────────────────────────────────────────

/** What a patch of one named field is called, in the operator's words. The
 * affordances read "Undo " + this. */
const PATCH_LABELS: Partial<Record<keyof SlideLayer, string>> = {
  text: "edit the text",
  color: "change the colour",
  font_px: "change the font size",
  align: "change the alignment",
  entity_id: "change the entity",
  target_ms: "change the countdown target",
  ping_name: "rename the button event",
  items: "edit the menu",
  derive: "restyle the rasterized layer",
};

/** The verb phrase naming what this action did, recorded when the step is
 * pushed so the button can say what a press will revert. Computed against the
 * state BEFORE the action, which is the only place the previous values still
 * exist (a move and a resize are the same action shape and are told apart by
 * whether the size actually moved). */
function describeEdit(state: StudioState, action: StudioAction): string {
  switch (action.type) {
    case "rename":
      return "rename the cast";
    case "setDefaultDuration":
      return action.durationMs === null ? "clear the default duration" : "change the default duration";
    case "addSlide":
      return "add slide";
    case "duplicateSlide":
      return "duplicate slide";
    case "deleteSlide":
      return "delete slide";
    case "moveSlide":
      return "move slide";
    case "setSlideDuration":
      return action.durationMs === null ? "clear the slide duration" : "change the slide duration";
    case "insertLayer":
      return `add ${KIND_LABEL[action.layer.kind].toLowerCase()} layer`;
    case "deleteLayer":
      return "delete layer";
    case "reorderLayer":
      return "reorder layers";
    case "moveLayer":
      return "move layer";
    case "resizeLayer":
      return "resize layer";
    case "patchLayer":
      return describePatch(state.slides[state.slideIndex]?.layers[action.index], action.patch);
    // Never pushed — the reducer above returns before it labels these — but the
    // switch stays exhaustive so a new action type is a compile error here
    // rather than an unlabelled step in the UI.
    case "selectSlide":
    case "selectLayer":
    case "loaded":
    case "saved":
      return "edit";
  }
}

function describePatch(before: SlideLayer | undefined, patch: LayerPatch): string {
  const keys = Object.keys(patch);

  // A pointer drag patches all four geometry members every frame whether it is
  // moving or resizing the layer, so the shape of the patch cannot tell them
  // apart — only whether the SIZE actually moved can.
  if (keys.length > 0 && keys.every((k) => GEOMETRY_FIELDS.has(k))) {
    const resized =
      before !== undefined &&
      ((patch.w !== undefined && patch.w !== before.w) || (patch.h !== undefined && patch.h !== before.h));
    return resized ? "resize layer" : "move layer";
  }

  if ("asset_ref" in patch) {
    return patch.asset_ref === undefined ? "remove the content" : "change the content";
  }

  for (const key of keys) {
    const label = PATCH_LABELS[key as keyof SlideLayer];
    if (label) return label;
  }
  return "edit the layer";
}

// ── The keyboard shortcut ───────────────────────────────────────────────────

/** The modifier state a keydown carries, narrowed to what the match needs so
 * the matcher is a pure function a test can call with a literal. */
export type ShortcutChord = Pick<KeyboardEvent, "key" | "metaKey" | "ctrlKey" | "shiftKey" | "altKey">;

/**
 * Which history command a keystroke is, or null for "not ours".
 *
 * ⌘Z / Ctrl+Z undo, ⌘⇧Z / Ctrl+⇧Z redo, and Ctrl+Y redoes as well because that
 * is what Windows editors bind — but ONLY with Ctrl, never with ⌘: on macOS
 * Cmd+Y is the browser's own History window, and stealing it would be worse
 * than not offering a third redo binding.
 *
 * Alt disqualifies everything. The Studio already gives Alt a meaning of its own
 * on this surface (Alt+arrow is the one-pixel nudge), and Ctrl+Alt+Z is not an
 * undo anywhere.
 */
export function matchHistoryShortcut(chord: ShortcutChord): "undo" | "redo" | null {
  if (chord.altKey) return null;
  const key = chord.key.toLowerCase();
  if (key === "z") {
    if (!chord.metaKey && !chord.ctrlKey) return null;
    return chord.shiftKey ? "redo" : "undo";
  }
  if (key === "y" && chord.ctrlKey && !chord.metaKey && !chord.shiftKey) return "redo";
  return null;
}

/** The input types that keep their own undo stack — the browser's, over the
 * characters in the field — and must therefore keep the keystroke.
 *
 * An `<input>` with no `type` is a text field, hence the empty string. The other
 * input kinds (colour swatch, range, checkbox, radio, file, button) have no
 * character buffer and no native undo, so a keystroke there is not being taken
 * from anything and the editor's own undo is the useful answer. `<select>` is
 * the same case. */
const TEXT_ENTRY_INPUT_TYPES: ReadonlySet<string> = new Set([
  "",
  "text",
  "search",
  "url",
  "tel",
  "email",
  "password",
  "number",
]);

/** Whether the keystroke landed somewhere the browser's own undo owns it. */
export function isTextEntryTarget(target: EventTarget | null): boolean {
  if (target === null || !(target instanceof HTMLElement)) return false;
  // `isContentEditable` is the real answer and covers a keystroke landing on a
  // DESCENDANT of an editable host. It is also unimplemented in jsdom (and in
  // some embedded engines), where it silently reports false — so the attribute
  // is checked as well rather than trusting one of the two.
  if (target.isContentEditable) return true;
  if (target.closest('[contenteditable]:not([contenteditable="false"])') !== null) return true;
  if (target.tagName === "TEXTAREA") return true;
  if (target.tagName === "INPUT") {
    return TEXT_ENTRY_INPUT_TYPES.has((target as HTMLInputElement).type.toLowerCase());
  }
  return false;
}
