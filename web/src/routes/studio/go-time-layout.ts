// Formatting a clock layer's time the way the PLAYER will.
//
// A `clock` layer's `text` is a Go reference-time LAYOUT (wire.Layer's doc fixes
// that convention explicitly: `"15:04:05"`, `"3:04 PM"`, `"Mon 15:04"` — not
// strftime), and the Roku renders the current local time through it, refreshed
// every second. The Studio is a WYSIWYG editor, so the preview has to speak the
// same grammar: an editor that showed the raw layout string, or showed a
// browser-locale time, would let an operator ship a slide whose clock reads as
// literal garbage on the TV and only discover it there.
//
// This is a SUBSET of Go's reference-time vocabulary — the date/time tokens a
// signage clock uses. Anything unrecognised is passed through as a literal,
// which is also Go's behaviour for non-token characters, so the preview degrades
// to showing exactly the characters that will appear rather than guessing.
// Timezone tokens (-0700, MST) are deliberately absent: the player formats in
// the screen's own local time, which the browser cannot know.

/** One reference-time token and how to render it from a Date. */
interface Token {
  layout: string;
  render: (d: Date) => string;
}

const MONTHS = [
  "January", "February", "March", "April", "May", "June",
  "July", "August", "September", "October", "November", "December",
];
const DAYS = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];

const pad2 = (n: number): string => String(n).padStart(2, "0");
/** Go's 12-hour clock renders midnight and noon as 12, never 0. */
const hour12 = (d: Date): number => d.getHours() % 12 || 12;

// Ordered LONGEST FIRST within the scan below; a shared prefix must never win
// over the longer token that contains it ("Mon" inside "Monday", "2" inside
// "2006"). The scan sorts by length so this array's order is documentation, not
// load-bearing.
const TOKENS: Token[] = [
  { layout: "2006", render: (d) => String(d.getFullYear()) },
  { layout: "January", render: (d) => MONTHS[d.getMonth()] ?? "" },
  { layout: "Monday", render: (d) => DAYS[d.getDay()] ?? "" },
  { layout: "Jan", render: (d) => (MONTHS[d.getMonth()] ?? "").slice(0, 3) },
  { layout: "Mon", render: (d) => (DAYS[d.getDay()] ?? "").slice(0, 3) },
  { layout: "15", render: (d) => pad2(d.getHours()) },
  { layout: "PM", render: (d) => (d.getHours() < 12 ? "AM" : "PM") },
  { layout: "pm", render: (d) => (d.getHours() < 12 ? "am" : "pm") },
  { layout: "01", render: (d) => pad2(d.getMonth() + 1) },
  { layout: "02", render: (d) => pad2(d.getDate()) },
  { layout: "03", render: (d) => pad2(hour12(d)) },
  { layout: "04", render: (d) => pad2(d.getMinutes()) },
  { layout: "05", render: (d) => pad2(d.getSeconds()) },
  { layout: "06", render: (d) => pad2(d.getFullYear() % 100) },
  { layout: "_2", render: (d) => String(d.getDate()).padStart(2, " ") },
  { layout: "1", render: (d) => String(d.getMonth() + 1) },
  { layout: "2", render: (d) => String(d.getDate()) },
  { layout: "3", render: (d) => String(hour12(d)) },
  { layout: "4", render: (d) => String(d.getMinutes()) },
  { layout: "5", render: (d) => String(d.getSeconds()) },
];

const BY_LENGTH = [...TOKENS].sort((a, b) => b.layout.length - a.layout.length);

/**
 * Render `date` through a Go reference-time `layout`.
 *
 * Single left-to-right scan, longest token first. Deliberately NOT a chain of
 * string replacements: substituting "1" for the month would then re-substitute
 * inside the "2026" a previous step had already written, which is exactly the
 * class of bug that makes a clock read "20February6".
 */
export function formatGoTimeLayout(layout: string, date: Date): string {
  let out = "";
  let i = 0;
  while (i < layout.length) {
    const token = BY_LENGTH.find((t) => layout.startsWith(t.layout, i));
    if (token) {
      out += token.render(date);
      i += token.layout.length;
    } else {
      out += layout[i];
      i += 1;
    }
  }
  return out;
}

/** The clock layouts the Studio offers as one-click presets. Every one is a
 * layout the player accepts; the field stays free-text for anything else. */
export const CLOCK_PRESETS: Array<{ layout: string; label: string }> = [
  { layout: "3:04 PM", label: "3:04 PM" },
  { layout: "3:04:05 PM", label: "3:04:05 PM" },
  { layout: "15:04", label: "15:04" },
  { layout: "15:04:05", label: "15:04:05" },
  { layout: "Mon 15:04", label: "Mon 15:04" },
  { layout: "Monday, January 2", label: "Monday, January 2" },
  { layout: "Jan 2, 2006", label: "Jan 2, 2006" },
  { layout: "02/01/2006", label: "02/01/2006" },
];

/** The date layouts the Studio offers as one-click presets for a `date` layer.
 * A date IS a time format on this wire — the player runs `clock` and `date`
 * through the SAME Go-reference-time formatter and differs only in how often it
 * re-renders — so these are drawn from the same vocabulary as CLOCK_PRESETS and
 * simply omit the time-of-day tokens. */
export const DATE_PRESETS: Array<{ layout: string; label: string }> = [
  { layout: "Monday, January 2", label: "Monday, January 2" },
  { layout: "Mon, Jan 2", label: "Mon, Jan 2" },
  { layout: "January 2, 2006", label: "January 2, 2006" },
  { layout: "Jan 2, 2006", label: "Jan 2, 2006" },
  { layout: "2006-01-02", label: "2006-01-02" },
  { layout: "02/01/2006", label: "02/01/2006" },
  { layout: "01/02/2006", label: "01/02/2006" },
];

/** The remaining-time layouts the Studio offers for a `countdown` layer. */
export const COUNTDOWN_PRESETS: Array<{ layout: string; label: string }> = [
  { layout: "HH:MM:SS", label: "HH:MM:SS" },
  { layout: "DD:HH:MM:SS", label: "DD:HH:MM:SS" },
  { layout: "D days, HH:MM", label: "D days, HH:MM" },
  { layout: "D:HH:MM:SS", label: "D:HH:MM:SS" },
  { layout: "HH hours MM minutes", label: "HH hours MM minutes" },
];

/** The layout a countdown with no authored `text` renders through — the
 * player's own default, stated here so the preview shows what the wall will. */
export const COUNTDOWN_DEFAULT_LAYOUT = "HH:MM:SS";

/** One countdown token and the unit it draws from. Ordered longest-first for the
 * same reason the reference-time table is: `DD` must never be read as two `D`s. */
const COUNTDOWN_TOKENS: Array<{ layout: string; unit: "d" | "h" | "m" | "s"; pad: boolean }> = [
  { layout: "DD", unit: "d", pad: true },
  { layout: "HH", unit: "h", pad: true },
  { layout: "MM", unit: "m", pad: true },
  { layout: "SS", unit: "s", pad: true },
  { layout: "D", unit: "d", pad: false },
  { layout: "H", unit: "h", pad: false },
  { layout: "M", unit: "m", pad: false },
  { layout: "S", unit: "s", pad: false },
];

/**
 * Render a remaining span (in milliseconds) through a countdown layout, the way
 * the PLAYER will (`wvFormatCountdown` in player-v3/components/PhotonScene.brs).
 *
 * Two rules make this more than a division, and both are the player's:
 *
 *  1. A larger unit's remainder is taken out ONLY when that unit APPEARS in the
 *     layout. `"HH:MM:SS"` on a two-day span therefore reads 48 hours rather
 *     than silently dropping the days — a fixed hours-mod-24 would be wrong on
 *     exactly the slides (a multi-day countdown) most likely to be authored.
 *  2. A span that has passed clamps to zero. A finished countdown reads
 *     `00:00:00`; negative numbers on a wall read as a rendering fault.
 *
 * The countdown grammar is deliberately NOT the reference-time one — Go has no
 * reference DURATION, and `15` would mean "hour of day" on a value that has
 * none — so this is a second small formatter rather than a reuse of
 * `formatGoTimeLayout`.
 */
export function formatCountdownLayout(layout: string, remainingMs: number): string {
  const fmt = layout === "" ? COUNTDOWN_DEFAULT_LAYOUT : layout;
  let secs = Math.max(0, Math.floor(remainingMs / 1000));

  let days = 0;
  let hours = 0;
  let mins = 0;
  if (fmt.includes("D")) {
    days = Math.floor(secs / 86400);
    secs -= days * 86400;
  }
  if (fmt.includes("H")) {
    hours = Math.floor(secs / 3600);
    secs -= hours * 3600;
  }
  if (fmt.includes("M")) {
    mins = Math.floor(secs / 60);
    secs -= mins * 60;
  }
  const value: Record<"d" | "h" | "m" | "s", number> = { d: days, h: hours, m: mins, s: secs };

  let out = "";
  let i = 0;
  while (i < fmt.length) {
    const token = COUNTDOWN_TOKENS.find((t) => fmt.startsWith(t.layout, i));
    if (token) {
      const n = value[token.unit];
      out += token.pad ? String(n).padStart(2, "0") : String(n);
      i += token.layout.length;
    } else {
      out += fmt[i];
      i += 1;
    }
  }
  return out;
}
