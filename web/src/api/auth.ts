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

import { ApiClient, ApiError } from "./client";

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
  /** The second-factor code (`security-model.md` SEC-004). Optional in SHAPE and
   * mandatory in EFFECT for any principal holding a `totp` credential: such a
   * login is REFUSED without it, and the refusal names the factor to collect
   * (see `secondFactorRequired`). There is no intermediate credential between
   * the two calls — the client simply calls this same operation again carrying
   * everything, and nothing is minted until both factors are satisfied. */
  totp_code?: string;
}

/** A pending TOTP enrollment, returned exactly once by `enrollTotp`. Nothing
 * here authenticates until `confirmTotp` arms it. */
export interface TotpEnrollment {
  /** The shared secret as base32, for an operator typing it by hand. */
  secret: string;
  /** The same secret as an `otpauth://` URI, for a QR code. */
  otpauth_uri: string;
}

/** The armed `totp` credential — metadata only, never a raw secret value. */
export interface TotpCredential {
  credential_id: string;
  principal_id: string;
  kind: "totp";
  created_at: number;
  /** How many OTHER sessions and API keys arming the credential revoked. The
   * calling session is never among them — it just proved the new factor. */
  revoked_sessions: number;
}

/** Reports whether a failed login was refused for a MISSING second factor, as
 * opposed to a wrong credential.
 *
 * The distinction is carried by a Problem extension member rather than by a
 * distinct error code, because it is not a distinct KIND of refusal — the caller
 * is still simply not authenticated. It is present only where the presented
 * password was accepted, and is deliberately absent from a wrong password, a
 * wrong code and a replayed code alike, so a client cannot use its absence to
 * learn which of those happened. */
export function secondFactorRequired(err: unknown): boolean {
  return err instanceof ApiError && err.status === 401 && err.problem?.second_factor === "totp";
}

/** A first-boot claim (SEC-120): the one-time setup code the installer
 * presented, plus the credential the first owner is choosing. */
export interface ClaimRequest {
  code: string;
  identifier: string;
  password: string;
}

/** One api-key as a LISTING describes it (SEC-003e).
 *
 * There is no secret member and there never will be: the plaintext is returned
 * exactly once, by the mint, and SEC-003e forbids returning it — or any PREFIX
 * of it — afterwards. A prefix is a shortcut for an attacker holding a partial
 * capture and buys a legitimate operator nothing a label does not. */
export interface ApiKey {
  id: string;
  label: string;
  created_at: number;
  /** 0 / absent when the key has never been presented. */
  last_used_at?: number;
  /** 0 / absent when the key does not expire (SEC-003d). */
  expires_at?: number;
}

/** A freshly minted key — the ONLY shape that ever carries the plaintext. */
export interface MintedApiKey {
  id: string;
  label: string;
  /** The plaintext, returned exactly once (SEC-003e). Nothing can retrieve it
   * again; a caller that loses it must revoke the key and mint another. */
  key: string;
  created_at: number;
  expires_at?: number;
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
  /** The caller's own live api-keys (SEC-003e). Never carries a secret. */
  apiKeys(): Promise<ApiKey[]>;
  /** Mint an api-key for the CALLER, returning its plaintext exactly once
   * (SEC-003a–e). Requires admin at the workspace root. */
  mintApiKey(body: { label: string; expires_at?: number }): Promise<MintedApiKey>;
  /** Revoke one of the caller's own keys (SEC-020). */
  revokeApiKey(id: string): Promise<void>;
  /** Claim an unclaimed workspace by redeeming the one-time setup code, minting
   * the first `owner` principal and its session.
   *
   * The refusals, and what a caller may infer from each:
   *   - 401 UNAUTHENTICATED — no live `setup` grant matches this code. That is
   *     the answer for a WRONG code and, identically, for a box that is already
   *     claimed: claiming shuts the window and the box drops every `setup` grant
   *     it holds, so afterwards no code resolves at all. The two are deliberately
   *     indistinguishable, and a caller must not present them as distinct.
   *   - 403 GRANT_ALREADY_REDEEMED — the code is real and has been spent: either
   *     it was used before, or this is the losing half of two concurrent claims
   *     presenting one code (SEC-036 admits exactly one). Only a holder of the
   *     genuine code can ever see this, which is why it may say more.
   *   - 403 GRANT_EXPIRED — the code aged out of its ttl (SEC-035).
   *   - 403 GRANT_PURPOSE_MISMATCH — a real code of some OTHER purpose (a pairing
   *     code, say) was presented here.
   *   - 429 RATE_LIMITED — the attempt budget guarding against code-guessing is
   *     spent for this source (SEC-033).
   *   - 422 VALIDATION_FAILED — a field is missing; `fieldErrors` names which. */
  claim(body: ClaimRequest): Promise<SessionSummary>;
  /** Begin a second-factor enrollment for the CALLING principal, returning the
   * shared secret once. Throws ApiError 403 when this principal already holds
   * one — replacing an existing second factor is a credential-reset or recovery
   * operation (SEC-052), not a self-service one. */
  enrollTotp(): Promise<TotpEnrollment>;
  /** Arm the pending enrollment by returning a code computed from it. On
   * success every OTHER session of this principal is revoked; the calling
   * session survives, having just proven the new factor. */
  confirmTotp(code: string): Promise<TotpCredential>;
}

export function createAuthModule(client: ApiClient): AuthModule {
  return {
    async login(body) {
      // `exchange`, not `post`: a 401 here is this request's credentials being
      // refused, never a session going away, so it must not bounce the sign-in
      // page to the sign-in page and discard the reason.
      return client.exchange<SessionSummary>("/auth/login", body);
    },
    async logout() {
      await client.postNoContent("/auth/logout");
    },
    async session() {
      return client.getOrNull<SessionSummary>("/auth/session");
    },
    async apiKeys() {
      const page = await client.list<ApiKey>("/auth/api-keys");
      return page.items;
    },
    async mintApiKey(body) {
      // `action`, so the POST carries an Idempotency-Key: a retry-on-timeout
      // that re-minted would leave a SECOND live credential whose plaintext
      // was never shown, and SEC-003e means it can never be shown again.
      return client.action<MintedApiKey>("/auth/api-keys", body);
    },
    async revokeApiKey(id) {
      await client.discard(`/auth/api-keys/${encodeURIComponent(id)}`);
    },
    async claim(body) {
      // Same exemption as `login`: an unredeemable setup code answers 401, and
      // the setup page has to be able to say so.
      return client.exchange<SessionSummary>("/auth/setup", body);
    },
    async enrollTotp() {
      return client.post<TotpEnrollment>("/auth/totp/enroll", {});
    },
    async confirmTotp(code) {
      return client.post<TotpCredential>("/auth/totp/confirm", { code });
    },
  };
}
