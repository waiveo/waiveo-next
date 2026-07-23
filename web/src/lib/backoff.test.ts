import { describe, expect, it } from "vitest";
import { reconnectDelay } from "./backoff";

// The shared reconnect-backoff policy both live surfaces use (the LiveHub in the
// renderer and the Activity feed): capped exponential growth with jitter so a
// backend restart can't be met by a tight reconnect loop or a lockstep fleet of
// tabs storming the door.
describe("reconnectDelay — capped exponential backoff with jitter", () => {
  it("backs off exponentially then plateaus at the cap (no unbounded wait)", () => {
    // random pinned to the top of the jitter window → delay == the capped ceiling,
    // so the "grows then caps" envelope is directly assertable.
    const random = () => 1;
    const delays = [0, 1, 2, 3, 4, 5, 6].map((a) =>
      reconnectDelay(a, { baseMs: 100, capMs: 2000, random }),
    );
    expect(delays).toEqual([100, 200, 400, 800, 1600, 2000, 2000]);
    // Strictly increasing until the cap.
    for (let i = 1; i < 5; i++) expect(delays[i]).toBeGreaterThan(delays[i - 1]);
    // Capped — never exceeds capMs, and later attempts plateau (no runaway wait).
    expect(Math.max(...delays)).toBe(2000);
    expect(delays[5]).toBe(delays[6]);
    // No tight loop: every scheduled delay is a real, positive wait.
    expect(Math.min(...delays)).toBeGreaterThan(0);
  });

  it("applies equal jitter within [ceil/2, ceil] so a reconnect fleet decorrelates", () => {
    // attempt 3 → ceil = min(2000, 100*2^3) = 800; the jitter window is [400, 800].
    expect(reconnectDelay(3, { baseMs: 100, capMs: 2000, random: () => 0 })).toBe(400);
    expect(reconnectDelay(3, { baseMs: 100, capMs: 2000, random: () => 0.5 })).toBe(600);
    expect(reconnectDelay(3, { baseMs: 100, capMs: 2000, random: () => 1 })).toBe(800);
    // The default (real) random stays inside the window for many samples.
    for (let i = 0; i < 100; i++) {
      const d = reconnectDelay(3, { baseMs: 100, capMs: 2000 });
      expect(d).toBeGreaterThanOrEqual(400);
      expect(d).toBeLessThanOrEqual(800);
    }
  });

  it("stays at 0 when baseMs is 0 (instant-reconnect test mode)", () => {
    expect(reconnectDelay(5, { baseMs: 0, capMs: 30000, random: () => 0.7 })).toBe(0);
  });

  it("clamps a negative attempt to the first step", () => {
    expect(reconnectDelay(-3, { baseMs: 100, capMs: 2000, random: () => 1 })).toBe(100);
  });
});
