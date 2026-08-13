// The Studio's keyboard, as pure matchers.
//
// The failures a rendered test does not catch are all here: a chord that is
// claimed when it should reach the browser, a chord that is advertised in the
// menus and bound to nothing, and the shifted-punctuation layouts where ⌘⇧]
// arrives as "}".

import { describe, expect, it } from "vitest";
import type { ShortcutChord } from "./edit-history";
import {
  NUDGE,
  NUDGE_FINE,
  matchNudge,
  matchStudioShortcut,
  shortcutHints,
  type StudioCommand,
} from "./studio-shortcuts";

const chord = (over: Partial<ShortcutChord> & { key: string }): ShortcutChord => ({
  metaKey: false,
  ctrlKey: false,
  shiftKey: false,
  altKey: false,
  ...over,
});

describe("the command chords", () => {
  it("binds the document and view commands on either primary modifier", () => {
    expect(matchStudioShortcut(chord({ key: "s", metaKey: true }))).toBe("save");
    expect(matchStudioShortcut(chord({ key: "s", ctrlKey: true }))).toBe("save");
    expect(matchStudioShortcut(chord({ key: "0", metaKey: true }))).toBe("zoomFit");
    expect(matchStudioShortcut(chord({ key: "1", metaKey: true }))).toBe("actualSize");
    expect(matchStudioShortcut(chord({ key: "d", metaKey: true }))).toBe("duplicateLayer");
    expect(matchStudioShortcut(chord({ key: "c", metaKey: true }))).toBe("copyLayer");
    expect(matchStudioShortcut(chord({ key: "x", metaKey: true }))).toBe("cutLayer");
    expect(matchStudioShortcut(chord({ key: "v", metaKey: true }))).toBe("pasteLayer");
    // Unmodified c/x/v are typing, not commands — the letters an operator uses
    // most. A matcher that claimed them would make the canvas swallow text.
    expect(matchStudioShortcut(chord({ key: "c" }))).toBeNull();
    expect(matchStudioShortcut(chord({ key: "v" }))).toBeNull();
    // …and Alt disqualifies them, as it does every other chord in this table.
    expect(matchStudioShortcut(chord({ key: "c", metaKey: true, altKey: true }))).toBeNull();
  });

  it("reads the shifted punctuation a US layout actually sends", () => {
    // ⌘+ arrives as "=" with shift on some layouts and "+" on others; ⌘⇧] as
    // "}". A table written only for the unshifted glyph binds neither.
    expect(matchStudioShortcut(chord({ key: "+", metaKey: true }))).toBe("zoomIn");
    expect(matchStudioShortcut(chord({ key: "=", metaKey: true }))).toBe("zoomIn");
    expect(matchStudioShortcut(chord({ key: "_", metaKey: true }))).toBe("zoomOut");
    expect(matchStudioShortcut(chord({ key: "-", metaKey: true }))).toBe("zoomOut");
    expect(matchStudioShortcut(chord({ key: "}", metaKey: true, shiftKey: true }))).toBe("bringToFront");
    expect(matchStudioShortcut(chord({ key: "]", metaKey: true, shiftKey: true }))).toBe("bringToFront");
    expect(matchStudioShortcut(chord({ key: "{", metaKey: true, shiftKey: true }))).toBe("sendToBack");
  });

  it("tells the shifted stacking commands from the unshifted ones", () => {
    expect(matchStudioShortcut(chord({ key: "]", metaKey: true }))).toBe("bringForward");
    expect(matchStudioShortcut(chord({ key: "[", metaKey: true }))).toBe("sendBackward");
  });

  it("takes the two unmodified keys only when no modifier is down", () => {
    expect(matchStudioShortcut(chord({ key: "Delete" }))).toBe("deleteLayer");
    expect(matchStudioShortcut(chord({ key: "Backspace" }))).toBe("deleteLayer");
    expect(matchStudioShortcut(chord({ key: "Escape" }))).toBe("deselect");
    // ⌘⌫ is the browser's/OS's, not ours.
    expect(matchStudioShortcut(chord({ key: "Backspace", metaKey: true }))).toBeNull();
  });

  it("refuses Alt throughout, because Alt+arrow is the one-pixel nudge", () => {
    expect(matchStudioShortcut(chord({ key: "s", metaKey: true, altKey: true }))).toBeNull();
    expect(matchStudioShortcut(chord({ key: "Delete", altKey: true }))).toBeNull();
  });

  it("leaves an unbound key alone", () => {
    expect(matchStudioShortcut(chord({ key: "k", metaKey: true }))).toBeNull();
    expect(matchStudioShortcut(chord({ key: "a" }))).toBeNull();
    expect(matchStudioShortcut(chord({ key: "F5" }))).toBeNull();
  });
});

describe("the arrow keys", () => {
  it("moves by 8, and by 1 with Alt", () => {
    expect(matchNudge(chord({ key: "ArrowRight" }))).toEqual({ dx: NUDGE, dy: 0, resize: false });
    expect(matchNudge(chord({ key: "ArrowUp" }))).toEqual({ dx: 0, dy: -NUDGE, resize: false });
    expect(matchNudge(chord({ key: "ArrowLeft", altKey: true }))).toEqual({
      dx: -NUDGE_FINE,
      dy: 0,
      resize: false,
    });
  });

  it("resizes with Shift", () => {
    expect(matchNudge(chord({ key: "ArrowDown", shiftKey: true }))).toEqual({ dx: 0, dy: NUDGE, resize: true });
  });

  it("leaves a modified arrow to the browser", () => {
    // ⌘← is back/word-motion, Ctrl+→ is a desktop switch. Claiming either would
    // be worse than not offering the nudge.
    expect(matchNudge(chord({ key: "ArrowLeft", metaKey: true }))).toBeNull();
    expect(matchNudge(chord({ key: "ArrowRight", ctrlKey: true }))).toBeNull();
  });

  it("is not an arrow at all for any other key", () => {
    expect(matchNudge(chord({ key: "a" }))).toBeNull();
    expect(matchNudge(chord({ key: "Enter" }))).toBeNull();
  });
});

describe("what the menus print", () => {
  it("names a chord for every command the matcher can return", () => {
    // The failure this catches is a menu row promising a key nothing binds, or a
    // command bound and never advertised. The list is written out literally so
    // ADDING a command without a hint fails here.
    const commands: StudioCommand[] = [
      "save",
      "zoomIn",
      "zoomOut",
      "zoomFit",
      "actualSize",
      "bringToFront",
      "bringForward",
      "sendBackward",
      "sendToBack",
      "duplicateLayer",
      "copyLayer",
      "cutLayer",
      "pasteLayer",
      "deleteLayer",
      "deselect",
      "shortcuts",
    ];
    const hints = shortcutHints();
    for (const command of commands) {
      expect(hints[command], command).toBeTruthy();
    }
  });
});
