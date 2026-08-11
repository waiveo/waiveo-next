// The BUILT-IN cast templates — the starting points an operator picks instead of
// facing an empty 1920×1080 canvas.
//
// ── Why a template is a flavor of cast, not a resource of its own ────────────
//
// A template is exactly a cast that nothing has scheduled: same slides, same
// layers, same validation, same editor. Giving it its own resource kind would
// have duplicated the slide shape, the shared layer gate, the asset-reference
// projection the retention sweep reads, and the Studio itself — four copies that
// would drift, to express one boolean. So the server carries `template: true` on
// the cast row (`data-model/1` DAT-043) and refuses a playlist item that
// references one, which is what makes the flag load-bearing rather than a label:
// a template exists to be EDITED as the source of future casts, and a screen
// playing one would change every time somebody improved the starting point.
//
// ── Why the built-ins live here rather than as seeded rows ──────────────────
//
// A seeded template row would be a row an operator can rename, edit, or delete —
// and then "the built-in menu board" is gone from a box forever, or worse, is
// still listed but no longer what it says. These are program data: always
// present, identical on every box, never a thing to repair. Creating FROM one is
// an ordinary `POST /casts` of the slides below, so nothing about them is a
// special case downstream; from the moment of creation the result is a cast like
// any other. A template an operator saves themselves IS a row (`template: true`),
// because that one is theirs to change.
//
// ── Why `build(nowMs)` rather than a constant ───────────────────────────────
//
// One built-in carries a countdown, whose `target_ms` is an absolute instant and
// must be strictly positive AND in the future to be worth anything — a constant
// baked in here would be in the past by the time anyone used it, and would land
// as a widget reading 00:00:00. Taking `nowMs` as a parameter (rather than
// reading the clock inside) keeps every template a pure function of its input,
// so a test can assert the exact slides a template produces.

import { SLIDE_CANVAS_HEIGHT, SLIDE_CANVAS_WIDTH, type CastSlide } from "@/api";

/** The palette the built-ins are drawn in. Deliberately a small fixed set: a
 * template's job is to look deliberate on a wall the moment it is created, and
 * eight arbitrary hexes scattered through the definitions below would be
 * impossible to keep consistent as they are edited. */
const INK = "#0B1220"; // the near-black every board sits on
const PAPER = "#FFFFFF"; // primary text
const MUTED = "#9AA6BF"; // secondary text
const ACCENT = "#F368C4"; // the Waiveo accent, used as a rule/hairline

/** A full-bleed background rectangle. Every template starts with one, because a
 * slide with no background draws its text over whatever the player's own
 * backdrop is — black today, but not a promise the wire makes. */
function background(color = INK): CastSlide["layers"][number] {
  return { kind: "rect", x: 0, y: 0, w: SLIDE_CANVAS_WIDTH, h: SLIDE_CANVAS_HEIGHT, color };
}

/** A thin horizontal rule in the accent colour — the one piece of ornament the
 * built-ins use, and the reason `rect` is worth having in a template at all. */
function rule(y: number, x = 120, w = 1680): CastSlide["layers"][number] {
  return { kind: "rect", x, y, w, h: 6, color: ACCENT };
}

/**
 * One built-in template: what it is called, what it is FOR, and the slides it
 * produces.
 *
 * `description` is not decoration. The difference between these is not visual —
 * an operator choosing "Menu board" over "Welcome board" is choosing a shape of
 * content, and a gallery of four dark rectangles with names on them would tell
 * them nothing.
 */
export interface CastTemplate {
  id: string;
  name: string;
  description: string;
  /** The cast-wide default dwell time this template ships with, in ms. */
  defaultDurationMs: number;
  /** The slides, as a pure function of the instant the cast is created at. */
  build: (nowMs: number) => CastSlide[];
}

/**
 * The built-ins.
 *
 * Every one of them is VALID as produced — each layer carries the fields
 * `wire.ValidateAuthoredSlideLayers` requires — so "create from this template"
 * lands a cast that can be scheduled onto a screen without a single further
 * edit. That is the whole bar for a template: one that arrives needing an image
 * chosen or an entity picked before it will draw is a form, not a starting
 * point, which is why none of them uses an `image` or `entity` layer (an image
 * would name bytes this box may not have, and an entity a device it may not have
 * adopted).
 */
export const BUILT_IN_TEMPLATES: CastTemplate[] = [
  {
    id: "title-clock",
    name: "Title + clock board",
    description:
      "A headline with the live time and date beneath it. The plainest useful board: a room name, a status line, a standing message.",
    defaultDurationMs: 10_000,
    build: () => [
      {
        id: "title",
        layers: [
          background(),
          { kind: "text", x: 120, y: 220, w: 1680, h: 220, text: "Welcome", font_px: 160, color: PAPER, align: "left" },
          rule(500),
          { kind: "text", x: 120, y: 560, w: 1120, h: 120, text: "Add your message here", font_px: 64, color: MUTED, align: "left" },
          { kind: "clock", x: 1180, y: 540, w: 620, h: 160, text: "3:04 PM", font_px: 128, color: PAPER, align: "right" },
          { kind: "date", x: 1180, y: 720, w: 620, h: 90, text: "Monday, January 2", font_px: 52, color: MUTED, align: "right" },
        ],
      },
    ],
  },
  {
    id: "menu-board",
    name: "Menu board",
    description:
      "A heading and four priced rows, laid out as two slides so a longer list keeps a readable type size. The layout the legacy system's operators used most.",
    defaultDurationMs: 12_000,
    build: () => [
      menuSlide("menu-1", "Today's Menu", [
        ["Soup of the day", "6.50"],
        ["Chicken & rice bowl", "11.00"],
        ["Roast vegetable salad", "9.50"],
        ["Sandwich of the week", "8.00"],
      ]),
      menuSlide("menu-2", "Sides & Drinks", [
        ["Fries", "3.50"],
        ["Seasonal fruit", "3.00"],
        ["Coffee", "2.50"],
        ["Bottled water", "2.00"],
      ]),
    ],
  },
  {
    id: "weather-welcome",
    name: "Welcome + weather",
    description:
      "A greeting with today's date and this site's live conditions. The weather is fetched by the box, so it stays current without anyone touching the cast.",
    defaultDurationMs: 10_000,
    build: () => [
      {
        id: "welcome",
        layers: [
          background(),
          { kind: "text", x: 120, y: 180, w: 1100, h: 200, text: "Good morning", font_px: 140, color: PAPER, align: "left" },
          { kind: "date", x: 120, y: 400, w: 1100, h: 110, text: "Monday, January 2", font_px: 72, color: MUTED, align: "left" },
          rule(580, 120, 1000),
          { kind: "text", x: 120, y: 640, w: 1000, h: 240, text: "Have a good day.", font_px: 72, color: MUTED, align: "left" },
          { kind: "weather", x: 1140, y: 300, w: 660, h: 200, text: "{temp}° {cond}", font_px: 120, color: PAPER, align: "right" },
          { kind: "text", x: 1140, y: 520, w: 660, h: 80, text: "right now", font_px: 44, color: MUTED, align: "right" },
        ],
      },
    ],
  },
  {
    id: "event-countdown",
    name: "Event countdown",
    description:
      "A big countdown to a moment you choose, with the time beside it. Arrives counting down to midnight tonight — open the countdown layer and set the real date.",
    defaultDurationMs: 8_000,
    build: (nowMs) => [
      {
        id: "countdown",
        layers: [
          background(),
          { kind: "text", x: 120, y: 160, w: 1680, h: 140, text: "Doors open in", font_px: 96, color: MUTED, align: "center" },
          {
            kind: "countdown",
            x: 120,
            y: 340,
            w: 1680,
            h: 340,
            text: "DD:HH:MM:SS",
            // Midnight tonight: a real, visibly-provisional instant that counts
            // down the moment the cast is created, rather than a baked constant
            // that would already be in the past and render as 00:00:00.
            target_ms: nextMidnightMs(nowMs),
            font_px: 260,
            color: PAPER,
            align: "center",
          },
          rule(740, 460, 1000),
          { kind: "clock", x: 120, y: 820, w: 1680, h: 120, text: "3:04 PM", font_px: 84, color: MUTED, align: "center" },
        ],
      },
    ],
  },
];

/** One menu slide: a heading, a rule, and up to four `name — price` rows drawn
 * as two columns of text layers. Rows are separate layers rather than one
 * multi-line string so an operator can restyle or reposition a single line — the
 * edit a menu board actually receives. */
function menuSlide(id: string, heading: string, rows: Array<[string, string]>): CastSlide {
  const layers: CastSlide["layers"] = [
    background(),
    { kind: "text", x: 120, y: 90, w: 1680, h: 160, text: heading, font_px: 120, color: PAPER, align: "left" },
    rule(280),
  ];
  rows.forEach(([name, price], i) => {
    const y = 360 + i * 160;
    layers.push({ kind: "text", x: 120, y, w: 1180, h: 130, text: name, font_px: 84, color: PAPER, align: "left" });
    layers.push({ kind: "text", x: 1340, y, w: 460, h: 130, text: price, font_px: 84, color: ACCENT, align: "right" });
  });
  return { id, layers };
}

/** Midnight at the start of the NEXT local day.
 *
 * Duplicated in spirit with the Studio's own insert default and deliberately
 * separate from it: this one is a property of a template's authored content
 * (what the "Event countdown" board counts to when it is created), the other is
 * a property of the editor's insert gesture. Sharing one would couple a template
 * definition to the editor's insert behaviour, and the day one of them wanted a
 * different default the shared helper would have to grow a mode. */
function nextMidnightMs(nowMs: number): number {
  const d = new Date(nowMs);
  d.setHours(24, 0, 0, 0);
  return d.getTime();
}
