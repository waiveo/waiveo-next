// @vitest-environment node
import { describe, expect, it } from "vitest";
import { CLOCK_PRESETS, formatGoTimeLayout } from "./go-time-layout";

// Monday, 2 February 2026, 15:04:05 local time. February and the 2nd are chosen
// on purpose: the day-of-month is 2, which is ALSO the reference-time token for
// the day — so a formatter that re-scans its own output would visibly corrupt
// the year.
const D = new Date(2026, 1, 2, 15, 4, 5);
/** Morning, single-digit month/day/minute/second — the padding cases. */
const AM = new Date(2026, 0, 5, 9, 7, 3);

describe("formatGoTimeLayout", () => {
  it("renders the 24-hour and 12-hour clock layouts", () => {
    expect(formatGoTimeLayout("15:04:05", D)).toBe("15:04:05");
    expect(formatGoTimeLayout("15:04", D)).toBe("15:04");
    expect(formatGoTimeLayout("3:04 PM", D)).toBe("3:04 PM");
    expect(formatGoTimeLayout("3:04 PM", AM)).toBe("9:07 AM");
    expect(formatGoTimeLayout("03:04:05 pm", AM)).toBe("09:07:03 am");
  });

  it("renders midnight and noon as 12 on the 12-hour clock, never 0", () => {
    expect(formatGoTimeLayout("3:04 PM", new Date(2026, 1, 2, 0, 30))).toBe("12:30 AM");
    expect(formatGoTimeLayout("3:04 PM", new Date(2026, 1, 2, 12, 30))).toBe("12:30 PM");
  });

  it("renders the date tokens without re-scanning its own output", () => {
    // "2006" must yield 2026 intact — a chain of replacements would have the
    // year's own digits eaten by the day/month tokens.
    expect(formatGoTimeLayout("Jan 2, 2006", D)).toBe("Feb 2, 2026");
    expect(formatGoTimeLayout("Monday, January 2", D)).toBe("Monday, February 2");
    expect(formatGoTimeLayout("Mon 15:04", D)).toBe("Mon 15:04");
    expect(formatGoTimeLayout("02/01/2006", D)).toBe("02/02/2026");
    expect(formatGoTimeLayout("06", D)).toBe("26");
    expect(formatGoTimeLayout("_2 Jan", AM)).toBe(" 5 Jan");
  });

  it("passes non-token characters through as literals", () => {
    expect(formatGoTimeLayout("Doors close at 3:04 PM", D)).toBe("Doors close at 3:04 PM");
    expect(formatGoTimeLayout("", D)).toBe("");
  });

  it("every offered preset is LIVE — it renders differently at a different time", () => {
    // The trap this catches: a preset whose characters happen to contain no
    // reference-time token is a static string masquerading as a clock, and it
    // would look perfectly fine in the editor at the one moment it was written.
    // Two distant times must therefore disagree for every preset.
    for (const preset of CLOCK_PRESETS) {
      const now = formatGoTimeLayout(preset.layout, D);
      const later = formatGoTimeLayout(preset.layout, AM);
      expect(now).not.toBe("");
      expect(now, `preset ${preset.layout} did not change with the time`).not.toBe(later);
    }
  });
});
