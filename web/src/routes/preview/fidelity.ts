import { isLabelKind, type SlideLayer } from "@/api";
import type { Program } from "./playback";

/**
 * THE FIDELITY LEDGER — everywhere this preview is known to differ from the
 * television, computed against the cast in front of the operator.
 *
 * # Why this file exists
 *
 * `SlideStage` is the CONSOLE's renderer. The wall is painted by BrightScript in
 * `player-v3/`. They are two implementations of one contract and they have
 * already disagreed in production twice this month: `display: blank` was honoured
 * by the conformance double and ignored by the real player (HV-4), and no image
 * or video layer could display at all while the console showed them fine (HV-1).
 * Neither was caught by a test, because nothing in this repository pins what any
 * renderer DRAWS — `conformance/corpora/player-1/` is seven protocol cases and
 * there is no golden-render fixture anywhere.
 *
 * So a preview has exactly two honest options: be right, or say it is not. This
 * module is the second one, and `player-stage.tsx` is as much of the first one as
 * a browser can manage.
 *
 * # What was closed rather than disclosed
 *
 * The preview does NOT reuse `SlideStage`. Several of the Studio canvas's
 * divergences are layout rules a browser can simply obey, and obeying them is
 * better than confessing them — `player-stage.tsx` draws content layers with
 * `scaleToFill` rather than `contain`, labels top-aligned and unwrapped rather
 * than centred and wrapping, nav cells without the editor's dashed outlines, one
 * focus ring rather than an outline per interactive layer, and the player's own
 * grey-and-em-dash placeholder for bytes the origin is not serving. Each of those
 * is named in that file beside the BrightScript it follows.
 *
 * What is left is what a browser CANNOT do, and every item below is one of those.
 * Nothing here is speculative padding: each was verified against the shipped
 * `.brs` source, and each carries the reason it cannot be closed.
 *
 * # How it is used
 *
 * `fidelityNotes(program)` returns only the notes that APPLY to this cast. A
 * static list of caveats gets read once and never again; a list that says "three
 * layers on this cast draw in a typeface the TV does not have" is read every
 * time, because it is about the operator's own work.
 */

/** How far from the truth a note puts the preview. */
export type FidelityLevel =
  /** The preview draws something, and it is not what the wall will draw. */
  | "approximate"
  /** The preview cannot know the real value; what it shows is a stand-in. */
  | "stand-in"
  /** The wall does something the preview does not attempt at all. */
  | "not-shown";

export interface FidelityNote {
  /** Stable key — used for React keys and for the tests to name a note. */
  id: string;
  level: FidelityLevel;
  /** What the preview does. One line, in the operator's terms. */
  title: string;
  /** Why, and where the truth lives. */
  detail: string;
  /** How many layers in this cast it affects, when that is a countable thing. */
  affected?: number;
}

/** Everything one note needs to know about the cast, gathered in one pass. */
interface Census {
  labelNoFontPx: number;
  labelAny: number;
  labelNoColor: number;
  goTimeLayers: number;
  serverValueLayers: number;
  videoLayers: number;
  contentLayers: number;
  droppedDeriveLayers: number;
  underscoreDayLayouts: number;
  slides: number;
}

function census(program: Program): Census {
  const c: Census = {
    labelNoFontPx: 0,
    labelAny: 0,
    labelNoColor: 0,
    goTimeLayers: 0,
    serverValueLayers: 0,
    videoLayers: 0,
    contentLayers: 0,
    droppedDeriveLayers: 0,
    underscoreDayLayouts: 0,
    slides: program.slides.length,
  };
  for (const slide of program.slides) {
    c.droppedDeriveLayers += slide.droppedLayers.length;
    for (const layer of slide.layers) {
      countLayer(c, layer);
    }
  }
  return c;
}

function countLayer(c: Census, layer: SlideLayer): void {
  if (isLabelKind(layer.kind)) {
    c.labelAny++;
    if (!layer.font_px) c.labelNoFontPx++;
    if (!layer.color) c.labelNoColor++;
  }
  if (layer.kind === "clock" || layer.kind === "date" || layer.kind === "countdown") {
    c.goTimeLayers++;
    // `_2` is a real Go reference-time token (space-padded day) that the
    // console's formatter implements and the player's does not — see the note.
    if (layer.kind !== "countdown" && (layer.text ?? "").includes("_2")) c.underscoreDayLayouts++;
  }
  if (layer.kind === "weather" || layer.kind === "entity") c.serverValueLayers++;
  if (layer.kind === "video") c.videoLayers++;
  if (layer.kind === "image" || layer.kind === "video") c.contentLayers++;
}

/**
 * The notes that apply to THIS cast, most consequential first.
 *
 * Ordering is by how likely the difference is to make an operator ship something
 * wrong, not by how interesting it is. A stand-in weather reading changes what a
 * slide SAYS; a typeface changes how it looks. Both are declared; the first is
 * declared louder.
 */
export function fidelityNotes(program: Program): FidelityNote[] {
  const c = census(program);
  const notes: FidelityNote[] = [];

  if (c.serverValueLayers > 0) {
    notes.push({
      id: "server-resolved-values",
      level: "stand-in",
      affected: c.serverValueLayers,
      title: "Weather and entity readings are stand-ins, not today's values",
      detail:
        "The box fills these in when it issues a Lease (internal/slidelive), and the player draws the resolved string verbatim — it performs no lookup of its own. The console is not asking the forecast service or the device plane here, so it substitutes a value shaped like a real answer. The wall may also show an em dash, which is what the box writes when a source cannot answer.",
    });
  }

  if (c.droppedDeriveLayers > 0) {
    notes.push({
      id: "derive-dropped",
      level: "not-shown",
      affected: c.droppedDeriveLayers,
      title: "Rasterized layers that have never been rendered are not drawn here — and are not on the wall either",
      detail:
        "A `derive` layer with no PNG yet is dropped by the projection (internal/feeder/snapshot resolveLayers), so the TV shows nothing where it sits. The Studio canvas draws a CSS approximation of it; this preview deliberately does not, because agreeing with the editor would hide the fact that the layer is currently invisible to every screen.",
    });
  }

  if (c.videoLayers > 0) {
    notes.push({
      id: "video-playback",
      level: "approximate",
      affected: c.videoLayers,
      title: "Video plays here, but through the browser's decoder",
      detail:
        "The player loops the clip for the whole dwell (PhotonScene.brs renderSlideVideo sets loop = true) and this preview does the same, muted. What it cannot promise is the decode: the player hard-codes streamFormat = \"mp4\" and the device's codec support is its own, so a file this browser plays is not proof the panel will.",
    });
  }

  if (c.labelAny > 0) {
    notes.push({
      id: "typeface",
      level: "approximate",
      affected: c.labelAny,
      title: "Text is drawn in a browser font, not the device's system font",
      detail:
        "The player builds every Label with font:SystemFontFile (PhotonScene.brs createSlideLabel). Two typefaces at the same font_px do not occupy the same width, so a line that fits here can clip on the wall. Leave headroom, or check the real screen before shipping a tight fit.",
    });
  }

  if (c.labelNoFontPx > 0) {
    notes.push({
      id: "default-font-size",
      level: "approximate",
      affected: c.labelNoFontPx,
      title: "Layers with no font size chosen will not be this size on the TV",
      detail:
        "createSlideLabel only builds a Font node when font_px is positive; with none it leaves the SceneGraph Label's own default, which is not the size this preview picks. Setting font_px explicitly is the only way to know what the wall draws.",
    });
  }

  if (c.labelNoColor > 0) {
    notes.push({
      id: "default-colour",
      level: "approximate",
      affected: c.labelNoColor,
      title: "Layers with no colour chosen fall back to two different defaults",
      detail:
        "The player assigns a colour only when the layer carries one (wvSlideColor returns empty for an absent value and the caller skips the assignment), so the Label keeps SceneGraph's default. This preview draws white.",
    });
  }

  if (c.underscoreDayLayouts > 0) {
    notes.push({
      id: "go-time-underscore-day",
      level: "approximate",
      affected: c.underscoreDayLayouts,
      title: "A `_2` day token renders differently here than on the TV",
      detail:
        "The console's Go-reference-time table implements `_2` (space-padded day) and the player's wvFormatClockTime table does not, so \"_2 Jan\" previews as \" 5 Jan\" and draws as \"_5 Jan\" on the wall. This is a live divergence between the two formatters, not a limitation of previewing — use `2` instead until it is closed.",
    });
  }

  notes.push({
    id: "no-transitions",
    level: "approximate",
    title: "Every slide change is a hard cut, because that is what the player does",
    detail:
      "There are no transitions anywhere in player-v3: clearSlide tears the old layer tree down and renderSlide builds the new one. Legacy's preview cross-faded, which made casts look smoother in the office than on the wall. This one does not.",
  });

  notes.push({
    id: "no-lease-display",
    level: "not-shown",
    title: "This is a cast, not a screen — nothing here can show a blanked or preempted wall",
    detail:
      "`display: blank`, alert preemption and schedule boundaries are Lease-level instructions a screen receives (player/1 PLY-093), and a cast has no Lease. A preview that is playing does not mean a screen is: it may be showing an override, or nothing at all.",
  });

  notes.push({
    id: "no-overscan",
    level: "not-shown",
    title: "No overscan is simulated",
    detail:
      "A television can crop several percent off every edge. The stage draws the full 1920×1080; the title-safe guide marks the 5% broadcast convention when it is switched on.",
  });

  return notes;
}

/** A one-line summary for the header — how much of this cast is approximated.
 * Present so the honest answer is visible without opening a panel. */
export function fidelitySummary(program: Program): string {
  const c = census(program);
  const parts: string[] = [];
  if (c.serverValueLayers > 0) parts.push(`${c.serverValueLayers} live reading${c.serverValueLayers === 1 ? "" : "s"} stood in for`);
  if (c.droppedDeriveLayers > 0) parts.push(`${c.droppedDeriveLayers} unrendered layer${c.droppedDeriveLayers === 1 ? "" : "s"} omitted`);
  if (parts.length === 0) return "Layout and timing mirror the player; type is drawn in a browser font.";
  return `${parts.join(", ")}. Layout and timing mirror the player; type is drawn in a browser font.`;
}
