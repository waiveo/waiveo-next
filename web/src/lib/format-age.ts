/** Render a millisecond age as a short human duration, or an em dash for the
 * never sentinel (a negative age). Never "0s ago" for never — that is the single
 * most misleading thing a fleet view can say.
 *
 * It lives here rather than beside its first caller because a second surface now
 * needs the identical reading: the screens panel renders how long ago a player
 * checked in, and the discovery inventory renders how long ago a device was seen
 * and how long it has been on the network. Two spellings of "3h ago" on two
 * pages describing the same kind of fact is the sort of drift an operator reads
 * as a difference in meaning. */
export function formatAge(ms: number): string {
  if (ms < 0) return "—";
  if (ms < 1_000) return "just now";
  const s = Math.floor(ms / 1_000);
  if (s < 60) return `${s}s ago`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ago`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ago`;
  return `${Math.floor(h / 24)}d ago`;
}
