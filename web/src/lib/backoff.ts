// Shared reconnect-backoff policy for the console's two live surfaces — the
// renderer's LiveHub (`live` bindings over events/1) and the Activity feed. A
// backend restart (the feeder bounces, every open SSE connection 502s at once)
// must NOT be met by a tight reconnect loop that storms the door and floods the
// console; and a fleet of long-open tabs must not all retry in lockstep. This is
// the one place that policy lives so both surfaces behave identically.

export interface BackoffOptions {
  /** Delay for the first retry (ms); attempt 0 targets ~baseMs. `0` disables the
   * wait entirely (instant reconnect — the mode tests drive). */
  baseMs: number;
  /** Hard ceiling the exponential growth is clamped to (ms), so the wait never
   * runs away — the tab keeps trying to recover on a bounded cadence. */
  capMs: number;
  /** [0,1) randomness source for jitter; injectable so tests are deterministic.
   * Defaults to Math.random. */
  random?: () => number;
}

/**
 * Capped exponential backoff with "equal jitter" (AWS terminology). For a 0-based
 * reconnect `attempt`:
 *
 *   ceil  = min(capMs, baseMs · 2^attempt)
 *   delay = ceil/2 + random()·ceil/2        ∈ [ceil/2, ceil]
 *
 * Equal jitter (not full jitter) is deliberate: the `ceil/2` floor guarantees the
 * delay always grows with the attempt and never collapses toward zero, so a run
 * of failures can't hot-loop — while the random half still decorrelates a fleet
 * of tabs reconnecting after the same restart. For any fixed `random` the
 * sequence is non-decreasing in `attempt` and plateaus once the cap is reached,
 * so "backs off then caps" is a directly assertable envelope. With `baseMs: 0`
 * the delay is always 0.
 */
export function reconnectDelay(
  attempt: number,
  { baseMs, capMs, random = Math.random }: BackoffOptions,
): number {
  const exp = baseMs * 2 ** Math.max(0, attempt);
  const ceil = Math.min(capMs, exp);
  const half = ceil / 2;
  return half + random() * half;
}
