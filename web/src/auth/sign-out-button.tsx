import { useState } from "react";
import { LogOut } from "lucide-react";
import { Button } from "@/components/kit";
import { useOptionalSession } from "./session-gate";

/**
 * SignOutButton — the shell's sign-out control.
 *
 * It revokes the session SERVER-SIDE (`POST /auth/logout`) rather than merely
 * clearing local state: the session is a real row the platform can revoke
 * (security-model/1 SEC-020), and dropping a cookie locally would leave that row
 * live and usable by anyone holding the token. Revoking it also tears down any
 * open events/1 stream the session authenticated (EVT-114), which "just forget
 * the cookie" would not.
 *
 * The button is disabled while the request is in flight so a double-click cannot
 * fire two revocations — harmless, since revocation is idempotent, but the
 * disabled state is also what tells the operator the click registered.
 *
 * It renders NOTHING outside a SessionGate: the shell it lives in is also
 * mounted standalone (the design route, the shell's own tests), and a sign-out
 * control with no session to sign out of is not a control worth rendering.
 */
export function SignOutButton() {
  const session = useOptionalSession();
  const [busy, setBusy] = useState(false);

  if (!session) return null;
  const { signOut } = session;

  return (
    <Button
      variant="ghost"
      size="icon"
      icon={LogOut}
      aria-label="Sign out"
      disabled={busy}
      onClick={() => {
        if (busy) return;
        setBusy(true);
        void signOut().finally(() => setBusy(false));
      }}
    />
  );
}
