import { Badge } from "@/components/kit";
import { useOptionalSession } from "./session-gate";

/**
 * CurrentUser — who you are signed in as, and what authority you hold.
 *
 * The console fetched this on every load and never showed it. The session probe
 * has carried `principal_id`, `kind` and `role` since the gate was written —
 * that file's own doc says the header "can name the operator and offer sign-out
 * without re-fetching" — and only the sign-out half was ever built. So an
 * operator could not answer "which account is this?" from anywhere in the
 * product, which matters most on exactly the boxes where it is hardest to
 * guess: a shared workstation, a second browser profile, a demo unit somebody
 * else claimed.
 *
 * # Why the ROLE is shown, not just the identifier
 *
 * Authority is the half that changes what the console will let you do, and it
 * is invisible otherwise. A viewer who cannot save is owed the reason BEFORE
 * they compose an edit, not as a 403 afterwards.
 *
 * # What is deliberately not here
 *
 * No scope. A principal's role is held at a scope node and may differ across
 * the tree, so a single badge cannot state authority everywhere without
 * sometimes being wrong. What the session reports is the effective role this
 * probe resolved, and naming it plainly is honest; a scope picker belongs with
 * the role-bindings surface, which does not exist yet.
 *
 * Renders NOTHING outside a SessionGate, on the same terms as SignOutButton:
 * the shell is mounted standalone by the design route and by its own tests, and
 * an identity with no session behind it would be a fabrication.
 */
export function CurrentUser() {
  const session = useOptionalSession();
  if (!session) return null;
  const { principal_id: principalID, role } = session.session;

  return (
    <div className="flex min-w-0 items-center gap-2" data-slot="current-user">
      {/* The id is long and machine-shaped; it truncates rather than pushing
          the header's controls off a narrow viewport, and carries its full
          value as a title for the case where someone needs to read it. */}
      <span
        className="hidden max-w-[18ch] truncate font-mono text-xs text-muted-foreground sm:inline"
        title={principalID}
      >
        {principalID}
      </span>
      {/* Announced as authority, not decoration: "viewer" alone beside an id
          reads as a name to a screen reader. */}
      <Badge tone="neutral">
        <span className="sr-only">Signed in with the role </span>
        {role}
      </Badge>
    </div>
  );
}
