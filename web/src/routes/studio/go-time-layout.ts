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
