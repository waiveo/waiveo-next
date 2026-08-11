import type { ShortcutChord } from "./edit-history";

/**
 * The Studio's keyboard, minus undo/redo — those live in `edit-history.ts`
 * because they are the history's own contract and are bound whether or not this
 * file exists.
 *
 * Everything here is a PURE matcher over a chord, for the same reason
 * `matchHistoryShortcut` is one: a keyboard map buried in a `useEffect` can only
 * be tested by rendering the whole editor and firing synthetic events, so in
 * practice it is tested for the two chords somebody remembered. This one is a
 * function a test calls with a literal, and the table below is the same data the
 * menus and the shortcut sheet print — so a binding that exists and is never
 * advertised, or advertised and never bound, is a difference you can see in one
 * file.
 *
 * ── The two that look reckless ──────────────────────────────────────────────
 * `⌘0` / `⌘+` / `⌘-` are the browser's page zoom, and `⌘D` is its bookmark. Both
 * are taken, deliberately, and both are what every web-based design tool takes:
 * inside a canvas editor the operator means the CANVAS, and a zoom that
 * sometimes scaled the artwork and sometimes scaled the chrome around it would
 * be the worse surprise. They are claimed only while the Studio is mounted, and
 * only outside a text field.
 */

export type StudioCommand =
  | "save"
  | "zoomIn"
  | "zoomOut"
  | "zoomFit"
  | "actualSize"
  | "bringToFront"
  | "bringForward"
  | "sendBackward"
  | "sendToBack"
  | "duplicateLayer"
  | "deleteLayer"
  | "deselect"
  | "shortcuts";

/** True on a platform whose primary modifier is ⌘. Read once at module load —
 * a keyboard does not change platform mid-session — and defensively, because
 * `navigator.platform` is deprecated and absent in some runtimes. */
export function isApplePlatform(): boolean {
  if (typeof navigator === "undefined") return false;
  const ua = `${navigator.platform ?? ""} ${navigator.userAgent ?? ""}`;
  return /Mac|iPhone|iPad|iPod/i.test(ua);
}

/** The glyph for the primary modifier, for every hint the editor prints. */
export function modKey(): string {
  return isApplePlatform() ? "⌘" : "Ctrl+";
}

/** Whether the chord holds the platform's primary modifier and ONLY that.
 *
 * Both ⌘ and Ctrl are accepted on every platform: a Mac keyboard on a Linux box
 * and a PC keyboard on a Mac are both ordinary, and refusing one of them makes
 * the editor feel broken to whoever is holding it. Alt is refused throughout —
 * the Studio already gives Alt a meaning of its own (the one-pixel nudge). */
function primary(chord: ShortcutChord): boolean {
  return (chord.metaKey || chord.ctrlKey) && !chord.altKey;
}

/** `e.key` with the shifted punctuation folded back to the unshifted glyph, so a
 * binding can be written once. ⌘⇧] reports "}" on a US layout and "]" on
 * several others; both mean the same chord to the operator. */
function unshift(key: string): string {
  switch (key) {
    case "}":
      return "]";
    case "{":
      return "[";
    case "+":
      return "=";
    case "_":
      return "-";
    default:
      return key;
  }
}

/** How far an arrow key moves the selected layer, in canvas pixels. */
export const NUDGE = 8;
/** …and with Alt held, for placing something precisely. */
export const NUDGE_FINE = 1;

/** An arrow-key move or resize of the selected layer. */
export interface NudgeCommand {
  dx: number;
  dy: number;
  /** Shift turns the arrows into a resize from the bottom-right grip, so the
   * whole geometry is reachable without a pointer. */
  resize: boolean;
}

/**
 * The arrow keys, as a delta — or null when the keystroke is not one.
 *
 * This is a SEPARATE matcher from `matchStudioShortcut` because it returns data
 * rather than a name, and it is matched FIRST because it is the most specific:
 * an arrow with Alt held is a one-pixel nudge, and Alt disqualifies every other
 * chord in this file precisely so that it can be.
 *
 * The nudge is deliberately bound at the DOCUMENT and acts on the SELECTED
 * layer, not on a focused one. It used to be an `onKeyDown` on the canvas's hit
 * target, which meant it only worked when that element had focus — and the hit
 * target `preventDefault`s its own pointerdown (to stop the browser dragging the
 * artwork), so clicking a layer never focused it and the arrows did nothing at
 * all. The one gesture every editor has, unreachable by the one gesture every
 * operator makes. Selection is the thing the operator can see; focus is not.
 */
export function matchNudge(chord: ShortcutChord): NudgeCommand | null {
  // A modified arrow belongs to the browser or the OS (word-wise caret motion,
  // Space navigation, desktop switching) — never to the canvas.
  if (chord.metaKey || chord.ctrlKey) return null;
  const step = chord.altKey ? NUDGE_FINE : NUDGE;
  const resize = chord.shiftKey;
  switch (chord.key) {
    case "ArrowLeft":
      return { dx: -step, dy: 0, resize };
    case "ArrowRight":
      return { dx: step, dy: 0, resize };
    case "ArrowUp":
      return { dx: 0, dy: -step, resize };
    case "ArrowDown":
      return { dx: 0, dy: step, resize };
    default:
      return null;
  }
}

/** Which Studio command a keystroke is, or null for "not ours". */
export function matchStudioShortcut(chord: ShortcutChord): StudioCommand | null {
  const key = unshift(chord.key);

  // The two unmodified keys. Checked FIRST and only when no modifier is down,
  // so ⌘⌫ (whatever the browser does with it) is never read as a delete.
  if (!chord.metaKey && !chord.ctrlKey && !chord.altKey) {
    if (key === "Delete" || key === "Backspace") return "deleteLayer";
    if (key === "Escape") return "deselect";
    // The universal "what are the keys" chord, on the one key that carries it
    // without a modifier. Shift is implicit in `?` on most layouts, so it is
    // neither required nor refused.
    if (key === "?") return "shortcuts";
    return null;
  }

  if (!primary(chord)) return null;

  if (chord.shiftKey) {
    switch (key.toLowerCase()) {
      case "]":
        return "bringToFront";
      case "[":
        return "sendToBack";
      default:
        return null;
    }
  }

  switch (key.toLowerCase()) {
    case "s":
      return "save";
    case "d":
      return "duplicateLayer";
    case "0":
      return "zoomFit";
    case "1":
      return "actualSize";
    case "=":
      return "zoomIn";
    case "-":
      return "zoomOut";
    case "]":
      return "bringForward";
    case "[":
      return "sendBackward";
    case "/":
      return "shortcuts";
    default:
      return null;
  }
}

/**
 * What each binding is PRINTED as — in the menus, in the tool rail's hint, and
 * in the shortcut sheet.
 *
 * One table, read by all three, because the failure this prevents is the one
 * that is invisible in review: a menu row promising ⌘E for something nothing
 * binds. Undo and redo are included even though their matcher lives elsewhere,
 * because their hints are printed by the same menus.
 */
export function shortcutHints(): Record<StudioCommand | "undo" | "redo" | "nudge" | "nudgeFine" | "resize", string> {
  const mod = modKey();
  return {
    undo: `${mod}Z`,
    redo: `${mod}⇧Z`,
    save: `${mod}S`,
    zoomIn: `${mod}+`,
    zoomOut: `${mod}−`,
    zoomFit: `${mod}0`,
    actualSize: `${mod}1`,
    bringToFront: `${mod}⇧]`,
    bringForward: `${mod}]`,
    sendBackward: `${mod}[`,
    sendToBack: `${mod}⇧[`,
    duplicateLayer: `${mod}D`,
    deleteLayer: "Del",
    deselect: "Esc",
    shortcuts: "?",
    nudge: "← ↑ → ↓",
    nudgeFine: "Alt + arrows",
    resize: "⇧ + arrows",
  };
}
