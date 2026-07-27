// The api/1 `auth` family — sign in, sign out, read the current session, and
// claim an unclaimed box.
//
// There is no token handling anywhere in this file, and that is the design. The
// session rides an HttpOnly, host-only cookie (`security-model.md` SEC-023) the
// browser attaches to every same-origin request by itself; a token this script
// could read would be a token a script injection could exfiltrate. What the
// client does carry is the double-submit CSRF token, read from its own
// deliberately script-readable cookie and echoed by ApiClient on every mutating
// request (SEC-024) — see client.ts.

import { ApiClient } from "./client";

/** The principal kinds `security-model.md` SEC-001 defines. */
export type PrincipalKind = "user" | "screen" | "relay" | "pack-service" | "ingest-token" | "system-console";

/** The four roles SEC-010 defines, strongest last. */
export type Role = "viewer" | "operator" | "admin" | "owner";

/** Authenticator Assurance Level (SEC-021/022). A `recovery` session is minted
 * by redeeming a recovery-purpose grant and stays restricted until the target
 * principal completes TOTP re-enrolment. */
export type AAL = "standard" | "recovery";

/** The caller's own session, as `/auth/session`, `/auth/login` and
 * `/auth/setup` all return it. It carries no session token — see the file doc.
 * `csrf_token` is present only on the two responses that MINT a session. */
export interface SessionSummary {
  principal_id: string;
  kind: PrincipalKind;
  role: Role;
  aal: AAL;
  session_id: string;
  csrf_token?: string;
}

export interface LoginRequest {
  identifier: string;
  password: string;
}

/** A first-boot claim (SEC-120): the one-time setup code the installer
 * presented, plus the credential the first owner is choosing. */
export interface ClaimRequest {
  code: string;
  identifier: string;
  password: string;
}

export interface AuthModule {
  /** Exchange credentials for a session. Throws ApiError on failure — 401 for a
   * bad identifier or password (deliberately indistinguishable, so the response
   * cannot be used to enumerate accounts), 429 / CREDENTIAL_LOCKED once the
   * per-credential lockout has engaged (SEC-090), 422 for a malformed body. */
  login(body: LoginRequest): Promise<SessionSummary>;
  /** Revoke the calling session and clear its cookies. */
  logout(): Promise<void>;
  /** Read the calling principal's own session, or null when unauthenticated.
   * This is the ONE call that treats a 401 as an answer rather than an error:
   * "am I signed in?" is exactly the question whose negative response is a 401,
   * so a null return is the honest result and not a failure to report. */
  session(): Promise<SessionSummary | null>;
  /** Claim an unclaimed workspace by redeeming the one-time setup code. */
  claim(body: ClaimRequest): Promise<SessionSummary>;
}

export function createAuthModule(client: ApiClient): AuthModule {
  return {
    async login(body) {
      return client.post<SessionSummary>("/auth/login", body);
    },
    async logout() {
      await client.postNoContent("/auth/logout");
    },
    async session() {
      return client.getOrNull<SessionSummary>("/auth/session");
    },
    async claim(body) {
      return client.post<SessionSummary>("/auth/setup", body);
    },
  };
}
