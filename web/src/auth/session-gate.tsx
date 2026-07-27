import { createContext, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import { Navigate, useLocation } from "react-router";
import { createApi, type SessionSummary, type WaiveoApi } from "@/api";

/**
 * SessionGate — the console's authentication boundary.
 *
 * Every api/1 route is authenticated (security-model/1 SEC-005: a request whose
 * principal cannot be resolved is refused, never default-permitted), so the
 * shell must know whether there IS a session before it renders pages that all
 * assume one. The gate asks once, on mount, and then renders one of three
 * things:
 *
 *   - checking → nothing. Deliberately blank rather than a spinner: the probe is
 *     a single same-origin request that resolves in a few milliseconds, and a
 *     spinner that flashes for one frame is worse than no spinner at all.
 *   - no session → a redirect to /login, carrying where the operator was trying
 *     to go so they land there after signing in.
 *   - session → the children, with the session available via useSession() so a
 *     header can name the operator and offer sign-out without re-fetching.
 *
 * The probe uses the api client's `getOrNull`, which treats a 401 as an ANSWER
 * rather than an error and so does not fire the client's unauthenticated
 * redirect — otherwise this component would bounce the page to /login while
 * already deciding to do exactly that, and /login's own probe would bounce it
 * again.
 */
interface SessionState {
  session: SessionSummary;
  /** Re-reads the session — after a sign-out, or after a role change. */
  refresh(): Promise<void>;
  /** Revokes the session server-side and clears the local state. */
  signOut(): Promise<void>;
}

const SessionContext = createContext<SessionState | null>(null);

/** The current session. Throws outside a SessionGate — a component that needs a
 * session and is mounted where none is guaranteed is a wiring bug, and a silent
 * null would surface it as a confusing render instead. */
export function useSession(): SessionState {
  const ctx = useOptionalSession();
  if (!ctx) throw new Error("useSession must be used inside a SessionGate");
  return ctx;
}

/** The current session, or null outside a SessionGate. For the one class of
 * component that legitimately renders both inside and outside the gate: the
 * shell's own chrome, which the design-system route and the shell's own tests
 * mount standalone. Such a component OMITS its session-dependent controls
 * rather than failing to render. */
export function useOptionalSession(): SessionState | null {
  return useContext(SessionContext);
}

export interface SessionGateProps {
  children: ReactNode;
  /** Injected api client (tests); defaults to the same-origin client. */
  api?: WaiveoApi;
}

type Probe = { status: "checking" } | { status: "anonymous" } | { status: "signed-in"; session: SessionSummary };

export function SessionGate({ children, api }: SessionGateProps) {
  const client = useMemo(() => api ?? createApi(), [api]);
  const [probe, setProbe] = useState<Probe>({ status: "checking" });
  const location = useLocation();

  useEffect(() => {
    let live = true;
    void (async () => {
      try {
        const session = await client.auth.session();
        if (!live) return;
        setProbe(session ? { status: "signed-in", session } : { status: "anonymous" });
      } catch {
        // A transport failure is not an authorization answer, but there is
        // nothing useful to render without a session either. Treating it as
        // anonymous sends the operator to a page that will retry on submit,
        // which is the only recoverable path available from here.
        if (live) setProbe({ status: "anonymous" });
      }
    })();
    return () => {
      live = false;
    };
  }, [client]);

  const value = useMemo<SessionState | null>(() => {
    if (probe.status !== "signed-in") return null;
    return {
      session: probe.session,
      async refresh() {
        const session = await client.auth.session();
        setProbe(session ? { status: "signed-in", session } : { status: "anonymous" });
      },
      async signOut() {
        try {
          await client.auth.logout();
        } catch {
          // A failed revoke is SWALLOWED, not rethrown. The commonest reason
          // logout fails is that the session is already gone — exactly the
          // state the caller was asking for — and leaving the console rendered
          // as signed-in after the operator asked to leave is the worse outcome
          // of the two. The local clear below runs either way.
        }
        setProbe({ status: "anonymous" });
      },
    };
  }, [probe, client]);

  if (probe.status === "checking") return null;
  if (probe.status === "anonymous" || !value) {
    const next = location.pathname + location.search;
    const to = next && next !== "/" ? `/login?next=${encodeURIComponent(next)}` : "/login";
    return <Navigate to={to} replace />;
  }
  return <SessionContext.Provider value={value}>{children}</SessionContext.Provider>;
}
