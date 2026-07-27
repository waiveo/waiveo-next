import { ShieldCheck } from "lucide-react";
import { Link } from "react-router";
import { KitIcon } from "@/components/kit";
import { useOptionalSession } from "./session-gate";

/**
 * SecurityLink — the shell's entry point to the caller's own account security
 * (second-factor enrollment, security-model/1 SEC-004).
 *
 * It sits in the HEADER beside sign-out rather than in the primary nav rail, and
 * that placement is the same distinction the rail already draws: the rail lists
 * console RESOURCES (screens, schedules, content), while this acts on the
 * signed-in principal itself. Adding it to the rail would put "who I am" in a
 * list of "what the deployment holds".
 *
 * Like SignOutButton, it renders NOTHING outside a SessionGate: the shell is also
 * mounted standalone (the design route, the shell's own tests), and a link to a
 * page that immediately bounces to sign-in is not a control worth rendering.
 */
export function SecurityLink() {
  const session = useOptionalSession();
  if (!session) return null;
  return (
    <Link
      to="/security"
      aria-label="Account security"
      title="Account security"
      className="inline-flex size-9 items-center justify-center rounded-btn text-muted-foreground transition-colors hover:bg-muted hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
    >
      <KitIcon icon={ShieldCheck} decorative className="size-4" />
    </Link>
  );
}
