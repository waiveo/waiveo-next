// @vitest-environment node
import { describe, expect, it } from "vitest";
import {
  CLOCK_PRESETS,
  COUNTDOWN_DEFAULT_LAYOUT,
  COUNTDOWN_PRESETS,
  formatCountdownLayout,
  formatGoTimeLayout,
} from "./go-time-layout";

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

describe("formatCountdownLayout", () => {
  const SEC = 1000;
  const MIN = 60 * SEC;
  const HOUR = 60 * MIN;
  const DAY = 24 * HOUR;

  it("renders every token of the grammar, padded and unpadded", () => {
    const span = 2 * DAY + 3 * HOUR + 4 * MIN + 5 * SEC;
    expect(formatCountdownLayout("DD:HH:MM:SS", span)).toBe("02:03:04:05");
    expect(formatCountdownLayout("D:H:M:S", span)).toBe("2:3:4:5");
    expect(formatCountdownLayout("D days, HH:MM", span)).toBe("2 days, 03:04");
  });

  it("takes a larger unit's remainder out ONLY when that unit appears (the player's rule)", () => {
    // The claim this pins, and the one a fixed hours-mod-24 would break: a
    // two-day span through "HH:MM:SS" reads 48 hours rather than dropping the
    // days silently — the failure would land on exactly the slides (a multi-day
    // countdown) most likely to be authored. Mirrors wvFormatCountdown in
    // player-v3/components/PhotonScene.brs.
    expect(formatCountdownLayout("HH:MM:SS", 2 * DAY)).toBe("48:00:00");
    expect(formatCountdownLayout("DD:HH:MM:SS", 2 * DAY)).toBe("02:00:00:00");
    // …and the same rule one unit down: no H in the layout, so the hours stay in
    // the minutes; no M either, so they stay in the seconds.
    expect(formatCountdownLayout("MM:SS", 1 * HOUR)).toBe("60:00");
    expect(formatCountdownLayout("SS", 1 * HOUR + 1 * SEC)).toBe("3601");
    expect(formatCountdownLayout("HH hours MM minutes", 2 * DAY + 3 * HOUR + 4 * MIN)).toBe("51 hours 04 minutes");
  });

  it("reads a doubled token as one token, never as two singles", () => {
    // "DD" must not be read as two "D"s (which would print the day count twice).
    expect(formatCountdownLayout("DD", 3 * DAY)).toBe("03");
    expect(formatCountdownLayout("D", 3 * DAY)).toBe("3");
    expect(formatCountdownLayout("DDD", 3 * DAY)).toBe("033");
  });

  it("clamps a span that has already passed to zero rather than counting up", () => {
    expect(formatCountdownLayout("HH:MM:SS", 0)).toBe("00:00:00");
    expect(formatCountdownLayout("HH:MM:SS", -5 * HOUR)).toBe("00:00:00");
    expect(formatCountdownLayout("DD:HH:MM:SS", -1)).toBe("00:00:00:00");
  });

  it("truncates a partial second downward, the way an integer countdown must", () => {
    expect(formatCountdownLayout("SS", 1999)).toBe("01");
    expect(formatCountdownLayout("SS", 999)).toBe("00");
  });

  it("passes non-token characters through, and an empty layout means HH:MM:SS", () => {
    expect(formatCountdownLayout("T-minus HH:MM", 90 * MIN)).toBe("T-minus 01:30");
    expect(formatCountdownLayout("", 90 * MIN)).toBe("01:30:00");
    // Nothing to substitute at all is a legal (if pointless) layout: it is drawn
    // literally rather than erroring, exactly as the player draws it.
    expect(formatCountdownLayout("soon", 90 * MIN)).toBe("soon");
  });

  it("every offered preset is LIVE — it renders differently at a different remaining time", () => {
    // The same trap as the clock presets: a preset containing no token is a
    // static string masquerading as a countdown.
    for (const preset of COUNTDOWN_PRESETS) {
      const far = formatCountdownLayout(preset.layout, 2 * DAY + 3 * HOUR + 4 * MIN + 5 * SEC);
      const near = formatCountdownLayout(preset.layout, 61 * SEC);
      expect(far).not.toBe("");
      expect(far, `preset ${preset.layout} did not change with the remaining time`).not.toBe(near);
    }
  });

  it("the default layout constant is the one an empty layout renders through", () => {
    const span = 3 * HOUR + 2 * MIN + 1 * SEC;
    expect(formatCountdownLayout("", span)).toBe(formatCountdownLayout(COUNTDOWN_DEFAULT_LAYOUT, span));
  });
});
