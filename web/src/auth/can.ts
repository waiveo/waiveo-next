import type { Role } from "@/api";

/**
 * Whether a role reaches an authority level (`security-model` SEC-010).
 *
 * # This hides controls; it does not enforce anything
 *
 * Authority is decided by the server, on every request, and that has not
 * changed: a caller who forges a role in the browser gets a 403 exactly as
 * before. What this fixes is the CONSOLE, which until now showed every operator
 * every control and let them discover their authority as a refusal after
 * pressing it — the register's own words, "a viewer is shown every control and
 * discovers the refusal as a 403".
 *
 * So a hidden control is a courtesy, and a shown one is never a promise. The
 * rule that follows: a page MUST NOT skip its refusal handling because it
 * gated the affordance. Both halves stay.
 *
 * # The ranking is the server's, mirrored
 *
 * viewer < operator < admin < owner, exactly `internal/app/auth`'s own
 * `roleRank`. It is duplicated rather than derived because there is no wire
 * field that publishes the ORDER — only a role name — and a client that
 * invented its own order could hide a control an operator is entitled to, which
 * is a worse failure than showing one they are not. If the server's ranking
 * ever changes, this is the second place to change; the test names that.
 */
const RANK: Record<Role, number> = {
  viewer: 1,
  operator: 2,
  admin: 3,
  owner: 4,
};

/** Whether `have` reaches `need`.
 *
 * An unknown role — a wire value this build does not know — reaches NOTHING.
 * A future role name is far likelier to be a narrower one than a broader one,
 * and the failure of hiding a control from someone entitled to it is a support
 * question, while the failure of showing one is a 403 an operator reads as the
 * product being broken. */
export function can(have: Role | undefined, need: Role): boolean {
  if (!have) return false;
  const from = RANK[have];
  const to = RANK[need];
  if (from === undefined || to === undefined) return false;
  return from >= to;
}
