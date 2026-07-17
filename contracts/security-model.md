# Security Model

**Contract:** security-model/1
**Version:** 1.0
**Status:** review

## Scope

security-model/1 defines the platform's principal/credential/grant model and the mechanisms that keep an app-tier deployment administrable when its ordinary authentication path is degraded or unavailable: the key hierarchy that protects every wrapped secret stub, the credential-reset and break-glass recovery flows, the root-only console binding those flows resolve through, the host-tier privilege boundary and its narrow host-mutation helper, self-hosted TLS (the install-time local CA and its self-signed fallback), the first-boot ownership window, factory-reset key-material destruction, and the tier-granted capability baseline the required-pack set operates under.

- In scope: principal kinds, credential kinds, and the role/scope-node authorization model; session and CSRF handling; the generic enrollment/pairing/recovery grant shape and its anti-abuse floor; the wrapped-secret key hierarchy (data key, box key, recovery passphrase, tenant KMS, workspace signing key) and how it differs from the platform's other two key hierarchies; the routine credential-reset flow; break-glass recovery (`waiveo auth recover`); the app-tier persisted monotonic clock floor, its advance gate, and the trusted/untrusted accuracy assessment recovery surfaces; the console binding's transport, admission rule, and verb set; the host tier's OS-account boundary and the host-mutation helper; lockout policy; self-hosted TLS provisioning and its self-signed fallback; the first-boot claim window and factory-reset key-material destruction, on both the one-box and relay-only appliance topologies; the tier-granted capability baseline and blast-radius accounting for the required-pack set; the systemd hardening baseline; and this contract's own relationship to `events/1`'s audit trail.
- Out of scope: `api/1`'s HTTP conventions — error shape, optimistic concurrency, pagination, `Idempotency-Key` scoping — which this contract's own wire shapes and the console binding both inherit unmodified (referenced, not redefined); `relay/1`'s enrollment protocol (claim-token exchange, in-band renewal, expired-certificate re-enrollment, channel-binding) and its own minimal view of a pairing-grant record (REL-121) — this contract defines the canonical grant shape relay/1's own view specializes, never relay/1's wire protocol itself; the platform's software-artifact trust bundle (root/targets/snapshot/timestamp roles, namespace delegation, the revocation feed) — `channel-index/1`'s trust root, a distinct hierarchy from this contract's own wrapped-secret key hierarchy (Key hierarchy); `archive/1`'s container format, its secret-stub and data-key-wrap carriage, and its emergency-kit artifact (recovery-passphrase content and regeneration) — referenced, not redefined; `ctx/1`'s `secrets.require`/`connections.get` verbs and their opaque-handle return shape; `manifest/1`'s capability/consent structure, egress allowlist, and device-contribution consent rendering — this contract's blast-radius table cites `manifest/1`'s fields, never restates their validation rules; `surface/1`'s mount lifecycle and isolation mechanism (this contract supplies the host-only session-cookie precondition SUR-062 depends on, and stops there); `events/1`'s `audit.event` schema and mandatory-emission list — this contract's flows add entries to that list by reference (Audit), never define a competing audit record; the operational custody of any key this contract's hierarchy involves (an offline ceremony, a hardware-token threshold) — deployment/operational, not a wire-level rule, exactly as `channel-index/1` itself declines to define the equivalent for its own root role.

## Definitions

- **Principal** — the entity a credential, session, or grant is issued to or acts on behalf of. `api/1`'s own Definitions name a principal only as an opaque caller identifier for scoping and audit, and state that the full principal/role model is defined elsewhere; this contract is that elsewhere.
- **Principal kind** — one of `user`, `screen`, `relay`, `pack-service`, `ingest-token`, or the synthetic `system-console` (Principal kinds and credentials).
- **Credential** — a provable means of acting as a principal: one of `password`, `passkey`, `totp`, `oidc-subject`, `api-key`, or `cert` (Principal kinds and credentials). A principal MAY hold more than one credential; a credential belongs to exactly one principal.
- **Role** — an authorization level bound to a `(principal, scope node)` pair: `owner`, `admin`, `operator`, or `viewer` (Roles and scope-node authorization).
- **Scope node** — as defined in `api/1`: a node in the platform's `org → site → group → screen` tree.
- **Session** — a revocable, principal-bound token carrying device metadata and an AAL claim (Sessions).
- **AAL** — Authenticator Assurance Level: the claim on a session or token distinguishing an ordinarily-authenticated session from one minted through recovery-purpose grant redemption (Sessions).
- **Grant** — a short-lived, purpose-scoped, redeemable authorization record: the platform's one generic mechanism behind enrollment, pairing, invites, credential reset, and recovery (Grants).
- **Data key** — the per-workspace symmetric key that wraps every secret stub `archive/1` carries (`archive/1` Definitions: Data key). `archive/1` treats it as an opaque value it carries, re-wrapped, never unwrapped or inspected; this contract is the key hierarchy `archive/1`'s own definition points to.
- **Box key** — the self-hosted key, held in a root-owned keyfile, that wraps a workspace's data key at rest (Key hierarchy).
- **Recovery passphrase** — as defined in `archive/1`: the passphrase printed in a workspace's emergency kit, recovering that workspace's own data key on its own hardware.
- **Workspace signing key** — the per-workspace asymmetric keypair whose private half produces the signature `archive/1` ARC-021 requires over an export's outer header, and whose public half (or a certificate binding to it) `archive/1`'s `signer_key_id` field resolves against; `archive/1` ARC-021's own draft-note defers this signing identity's custody to this contract's key hierarchy (Key hierarchy).
- **Software-artifact trust bundle** — as defined in `channel-index/1`: the root role's metadata plus its delegated key-ids, verifying every signed pack, platform-release, and relay-release artifact. A distinct, unrelated trust root from this contract's own key hierarchy (Key hierarchy).
- **Console binding** — the root-only, host-local channel this contract defines for the `system-console` actor (The console binding).
- **`system-console`** — the synthetic principal a request admitted over the console binding is attributed to; it carries no credential row (Principal kinds and credentials) — admission to the console binding is itself the proof of identity.
- **Host-mutation helper** — the narrow, root-owned, fixed-verb helper the app process (not itself root) invokes to perform an OS-level mutation the web UI or CLI legitimately needs (Host tier and the host-mutation helper).
- **Tier-granted baseline** — the enumerated capability/egress/resources/connections set a pack's trust channel and namespace admit without an install-time consent prompt; the same mechanism `manifest/1` MAN-022 calls a pack's "pre-consented baseline" (Tier-granted capability baseline and blast radius).

## Normative requirements

### Principal kinds and credentials

**[SEC-001]** A principal row MUST carry exactly one `kind`: `user`, `screen`, `relay`, `pack-service`, `ingest-token`, or `system-console`. A platform table MUST NOT store an authentication credential (a password hash or otherwise) as a column of any other resource — every credential is a row in the credential relation (SEC-003), keyed to a principal, never a column on a `users`-shaped table.

**[SEC-002]** `system-console` MUST be the sole principal kind with no corresponding credential row (SEC-003): it is assumable only by a request admitted over the console binding (The console binding), never by presenting a stored secret.

**[SEC-003]** A credential row MUST carry a `kind` — `password`, `passkey`, `totp`, `oidc-subject`, `api-key`, or `cert` — and a reference to the exactly one principal it belongs to. A principal MAY hold more than one credential, including more than one of the same `kind` (for instance, more than one active `api-key`).

**[SEC-004]** `totp` MUST be supported as the guaranteed second-factor floor, available without a secure-context precondition. `passkey` (WebAuthn) credentials MUST be offered only where a secure context exists (Self-hosted TLS) — a deployment on the self-signed fallback (Self-signed fallback) MUST continue to accept `totp` while declining to offer `passkey` enrollment or authentication.

**[SEC-005]** Every `api/1` route MUST authorize its caller against a role bound at a scope node (Roles and scope-node authorization) before executing; a route that cannot resolve an authorization decision for its caller MUST refuse the request rather than default-permit.

### Roles and scope-node authorization

**[SEC-010]** A role-binding record MUST carry `(principal, scope_node, role)`, where `role` is one of `owner`, `admin`, `operator`, or `viewer`. A principal MAY hold different roles at different scope nodes; a role bound at a scope node applies to that node and, absent a more specific binding, to its descendants.

**[SEC-011]** `owner` MUST be the sole role permitted to: acknowledge a capability-widening pack update (`manifest/1` MAN-022), issue a `--new-owner` break-glass grant (Break-glass recovery), and toggle the platform's developer-mode setting (which relaxes pack trust-channel acceptance for local testing) — every toggle, and every dev-channel pack acceptance it subsequently permits, MUST emit its own `audit.event`. A deployment MUST always retain at least one `owner`-role principal in a claimed state: the last remaining `owner` role-binding MUST NOT be deletable through ordinary `api/1` mutation, only through factory reset (First-boot claim window and factory reset), which itself re-opens claim.

**[SEC-012]** `admin` MUST be permitted to issue a routine credential-reset grant (Credential-reset grants) for any `user` principal; clearing or re-enrolling a target's TOTP requires the owner-explicit path SEC-052 states, not plain `admin` authority.

*draft-note: this contract fixes the four role names and SEC-011's owner-exclusivity because those are explicit design decisions; the complete permission matrix for `admin`/`operator`/`viewer` against every individual `api/1` operation is not enumerated here and is left as per-operation `api/1` configuration, not a security-model requirement — flagged rather than silently assumed complete.*

### Sessions

**[SEC-020]** A session row MUST be independently revocable and MUST carry, at minimum, `{session_id, principal_id, device_metadata, aal, created_at, revoked_at}` (`revoked_at` null while active). An API-key credential (SEC-003) MUST be revocable through the same mechanism.

**[SEC-021]** Every session or API-key token MUST carry its `aal` claim in the token's own format (not merely as a database-side attribute invisible to the token itself), so a resource server can make an authorization decision from the token alone.

**[SEC-022]** A session minted through redemption of a `recovery`-purpose grant (Grants) MUST carry `aal: recovery`, distinct from the value an ordinarily-authenticated session carries, and MUST remain restricted — barred from any operation this contract or `api/1` marks as requiring an ordinary AAL — until the target principal completes TOTP re-enrollment, at which point a fresh, unrestricted session replaces it.

**[SEC-023]** A browser session cookie MUST be issued host-only (no `Domain` attribute broadening it beyond the exact host that set it) — the precondition `surface/1` SUR-062 depends on and states but does not itself impose.

**[SEC-024]** Every mutating `api/1` route reachable from a browser session MUST require both `SameSite` cookie scoping and a double-submit CSRF token; `SameSite` alone MUST NOT be treated as sufficient, since it does not distinguish port and `surface/1` may mount an isolation origin on the same registrable domain (`surface/1` Isolation mechanism).

**[SEC-025]** `waiveo login <box|cloud>` MUST mint a session or API-key credential (SEC-003) and persist it mode `0600` in the invoking user's own configuration directory; this credential MUST be revocable identically to any other session (SEC-020). A request admitted over the console binding (The console binding) requires no such stored credential — SO_PEERCRED admission (SEC-072) stands in for one.

### Grants

**[SEC-030]** A grant record MUST carry `{grant_id, purpose, resulting_principal_kind, scope_node, labels, role, ttl, redemption_mode, consent_record, issued_via, issued_at}`, where `purpose` is one of `setup`, `invite`, `pairing`, `relay-claim`, `credential-reset`, `recovery`, or `support`; `redemption_mode` is `one-time` (default) or `multi`; `issued_via` is `api` or `console`. `relay/1`'s own `pairing_grants` record (REL-121) carries the subset of these fields the relay itself must enforce (`grant_id, purpose, resulting_principal_kind, ttl, redemption_mode, issued_at`) — REL-121 is a specialization of this shape, not a competing one.

*draft-note: `role` is meaningful only when `resulting_principal_kind` is `user`; a grant minting a `screen` or `relay` principal MAY leave `role` null. `support` is reserved for a future flow this contract does not yet design — no redemption mechanics are defined for it here.*

**[SEC-031]** A grant MUST be single-use by default (`redemption_mode: one-time`); a `multi` grant MUST state, and a conformant issuer MUST enforce, whatever redemption-count bound the issuing flow declares.

**[SEC-032]** A grant's code or token MUST carry at least 128 bits of entropy. A `credential-reset` or `recovery` purpose grant's `ttl` SHOULD be approximately 15 minutes; other purposes MAY use a longer `ttl` suited to their own flow (an `invite`, for instance).

**[SEC-033]** A grant-issuing endpoint MUST enforce a redemption rate limit and an attempt budget against repeated guesses of a live grant's code — the enforced control; a limit on issuance alone is not a substitute, since guessing a code that already exists is the attack this bounds, not minting new ones.

**[SEC-034]** Every grant creation and every grant redemption MUST emit an `audit.event` (`events/1` EVT-080) carrying that grant's `purpose` and `issued_via` in its payload, so a `recovery`-purpose, console-issued redemption is distinguishable in the audit trail from a routine `invite` redemption months later.

**[SEC-035]** A grant redemption presenting an expired code MUST be refused with `GRANT_EXPIRED` (Error taxonomy); one presenting an already-redeemed `one-time` grant's code MUST be refused with `GRANT_ALREADY_REDEEMED`; one presenting a code whose grant's `purpose` does not match the redemption endpoint being called MUST be refused with `GRANT_PURPOSE_MISMATCH` — a `pairing`-purpose code MUST NOT redeem against the credential-reset endpoint, even if otherwise well-formed.

**[SEC-036]** Redemption of a `one-time` grant (SEC-031) MUST be an atomic check-and-consume operation: the check that a grant is still unredeemed and the marking of it as redeemed MUST occur as a single indivisible step (one transaction, or an equivalent compare-and-swap on the grant's redemption state), so that two concurrent redemption attempts presenting the same code can never both succeed. This closes a double-redemption race for every `one-time` purpose this contract defines, including the first-boot `setup` grant (SEC-120) and the `credential-reset`/`recovery` purposes (Credential-reset grants, Break-glass recovery): of two simultaneous requests presenting the same code, at most one MUST receive success and the other MUST receive `GRANT_ALREADY_REDEEMED` (SEC-035) — a grant's redemption count MUST NOT ever exceed one regardless of concurrency.

### Key hierarchy

**[SEC-040]** A per-workspace **data key** MUST wrap every secret stub the workspace holds (the stubs `archive/1` ARC-070 carries opaquely). This contract, not `archive/1`, is the data key's source of definition; `archive/1` treats it as an opaque value it carries, re-wrapped, per ARC-071.

**[SEC-041]** On self-hosted, the data key MUST itself be wrapped by a **box key** held in a root-owned keyfile, and additionally wrapped for recovery under a **recovery passphrase** generated at the same time and delivered only through the `archive/1` emergency kit (ARC-110–114) — this contract supplies the key material the kit protects; `archive/1` defines the kit's own printable contents and regeneration rule.

**[SEC-042]** On cloud, the data key MUST be wrapped by the tenant's own key in the platform's KMS, in place of a box key and recovery passphrase — cloud has no console binding and no printed kit; this contract's console-binding and physical-kit requirements apply to the self-hosted tier only.

**[SEC-043]** The data key MUST be re-wrappable — for `archive/1` export (under an export passphrase, distinct from the recovery passphrase per ARC-112) and for tier migration — without decrypting, inspecting, or individually re-wrapping any secret stub it wraps. This is the property that makes `archive/1`'s `secret_stubs` array portable as one unit (ARC-071).

**[SEC-044]** This contract's key hierarchy is one of exactly three distinct, unrelated trust roots the platform operates, and a conformant implementation MUST NOT cross-use key material between them: (1) this section's wrapped-secret hierarchy (data key / box key / recovery passphrase / tenant KMS / workspace signing key, SEC-046–048); (2) the software-artifact trust bundle `channel-index/1` defines (root/targets/snapshot/timestamp roles, its delegated revocation role, and its publisher-namespace delegations — `channel-index/1` CHI-011/CHI-012 — verifying signed artifacts); (3) a relay's `desired_state_verification_key`, learned and re-anchored at enrollment (`relay/1` REL-012, REL-074), verifying only that one relay's own desired-state snapshots. A compromise of one MUST NOT be assumed, by any conformant component, to grant standing under either of the other two.

**[SEC-045]** Regenerating a workspace's emergency kit (`archive/1` ARC-114) invalidates the prior recovery passphrase; this contract's own box key and data key MUST NOT change as a side effect of kit regeneration — only the passphrase that re-wraps the data key for recovery purposes changes.

*draft-note: box-key custody is not detailed beyond "root-owned keyfile" here; treated as consistent with the host-tier boundary (Host tier and the host-mutation helper) — root already owns it, and the console binding is gated the same way.*

**[SEC-046]** A per-workspace **workspace signing key** — an asymmetric keypair, distinct from the data key (SEC-040) and from the platform's other two trust roots (SEC-044) — MUST be established at the same time as the workspace's data key. Its private half MUST be the sole key material used to produce the signature `archive/1` ARC-021 requires over an export's outer header; its public half (or a certificate binding to it) MUST be what `archive/1`'s `signer_key_id` field resolves against.

**[SEC-047]** On self-hosted, the workspace signing key's private half MUST be held in the same root-owned keyfile as the box key (SEC-041), whether as an independently persisted secret or deterministically derived from the box key under a fixed, distinct context label — the same per-purpose separation technique `archive/1` ARC-011 uses for its own two export sub-keys.

**[SEC-048]** On cloud, the workspace signing key MUST be protected by the tenant's own key in the platform's KMS (SEC-042), in place of a box key, exactly as the data key is.

### Credential-reset grants

**[SEC-050]** `waiveo user password-reset <user>` MUST create a grant (Grants) with `purpose: credential-reset` and return a one-time code or URL for the issuing admin to hand to the target user; the issuing admin MUST NOT be shown, and MUST have no path to choose, the credential value the target user eventually sets.

**[SEC-051]** No step of the credential-reset flow MUST place secret material — a password, or a grant code standing in for one — in a process's command-line arguments, in a location a shell history file would capture, in `ps` output, or in a journald-logged line. The code or URL SEC-050 returns identifies a grant (Grants); it is not itself a rendered credential.

**[SEC-052]** A `credential-reset`-purpose grant MUST NOT itself authorize a change to the target principal's TOTP enrollment. Clearing or re-enrolling TOTP MUST require either an explicit `owner`-role flag on the issuing command (distinct from a plain reset, and itself a distinct `audit.event` action per SEC-034/EVT-081), or redemption through the console binding (The console binding).

**[SEC-053]** Redemption of a `credential-reset` or `recovery` purpose grant MUST, by default, revoke every existing session and API-key credential (SEC-020) belonging to the target principal; the issuing operator MAY explicitly opt out of this default for a single issuance. This is what makes a post-takeover reset also evict whatever session the attacker was using.

### Break-glass recovery

**[SEC-060]** `waiveo auth recover` MUST resolve exclusively through the console binding (The console binding); a cloud (multi-tenant) deployment MUST NOT expose this operation at all — structurally absent, not merely access-denied, since cloud has no console binding to resolve it through (SEC-042). An attempt against a cloud endpoint MUST be refused with `RECOVERY_NOT_AVAILABLE` (Error taxonomy) if the operation is reachable at all rather than simply unrouted.

**[SEC-061]** The complete break-glass flow — issuing a `recovery`-purpose grant, interactively setting the target credential's new value, and re-enrolling TOTP via a terminal-rendered QR code — MUST complete entirely over the console binding. The console binding carries no TLS session (The console binding), so it is unaffected by the state of the app's HTTPS certificate: an expired or broken web certificate (Self-signed fallback) MUST NOT be able to block any step of this flow.

**[SEC-062]** `recover` MUST report the app's own current clock-accuracy assessment (App-tier clock trust, SEC-066–068) before proceeding with any other recovery step, so an apparent TOTP lockout caused by clock skew — TOTP codes are time-windowed — is diagnosable before the operator assumes a lost secret.

**[SEC-063]** Every `recovery`-purpose grant issued via the console MUST unconditionally (not configurably) trigger a notify event to every `owner`-role principal and set a persistent UI banner recording that a recovery grant was issued via the console, with its timestamp. This requirement MUST NOT be suppressible by any role, since it is the compensating detection control for a threat model that concedes prevention at the root boundary (SEC-041's box key is already root-owned).

**[SEC-064]** `recover` MUST support at least two distinct modes, each emitting its own distinct `audit.event` action (never a shared generic action string): issuing a credential-bearing grant against an existing principal (`--user <principal>`), and creating a brand-new `owner`-role principal (`--new-owner`).

**[SEC-065]** On an unclaimed box (First-boot claim window and factory reset), `recover` MUST regenerate the setup-purpose grant — invalidating whatever setup grant preceded it — rather than mint a `recovery`-purpose grant. A `recovery`-purpose grant request before the box's first-boot claim has completed MUST be refused with `RECOVERY_GRANT_BEFORE_CLAIM` (Error taxonomy).

### App-tier clock trust

**[SEC-066]** The app MUST persist a **monotonic clock floor**: a best-known-time lower bound it updates as it learns later verified time (SEC-067) and never moves backward. On restart, the app MUST NOT adopt a wall-clock reading earlier than this persisted floor — a rolled-back or reset host clock cannot walk the app's own notion of current time behind time it has already verified, so a time-windowed check (a TOTP code, a grant `ttl`, SEC-032/SEC-035) cannot be silently defeated by turning the host clock back. This is the app-tier analog of the relay's persisted clock floor (`relay/1` Clock trust, REL-130); this section is the app-side owner of the floor and the assessment SEC-062 surfaces, distinct from `relay/1`'s relay-side floor and never a restatement of `relay/1`'s own exchange.

**[SEC-067]** The clock floor (SEC-066) MUST advance only from a time value the app can verify independent of an unauthenticated claim — authenticated external time (for instance an authenticated network-time protocol) or a platform-signed timestamp — never a bare host wall-clock reading and never an unauthenticated client-supplied time. An unauthenticated time claim MUST NOT by itself advance the floor. This is the app-tier analog of the relay's advance-gate (`relay/1` Clock trust, REL-132), applied to the app's own floor rather than a relay's; which authenticated-time sources a given deployment trusts is implementation-defined per deployment tier, this contract fixing only that an unverifiable claim can never advance the floor.

**[SEC-068]** The app MUST maintain a `trusted`/`untrusted` clock-accuracy **assessment** derived from the floor's state (SEC-066–067): `untrusted` while it holds no independently verified time above the floor, `trusted` once it does. `recover` (SEC-062) MUST surface this assessment before proceeding; the console binding's clock-floor reset verb (SEC-075) MUST reset it; and every login-failure `audit.event` already carries it (SEC-091). This mirrors `relay/1`'s own `trusted`/`untrusted` clock-state (`relay/1` Clock trust) at app grain — this contract does not restate `relay/1`'s relay-side persistence, hint handling, or re-evaluation mechanics (REL-130–136), defining only the app-side floor (SEC-066), its advance gate (SEC-067), and the assessment these surfaces read.

### The console binding

**[SEC-070]** The app MUST expose a second, host-local binding of `api/1` — the **console binding** — distinct from the ordinary `/api/v1` HTTPS binding (`api/1` API-001): a Unix domain socket, reachable only from the same host, never from the network.

**[SEC-071]** The console binding's socket file MUST be mode `0700`; combined with SO_PEERCRED verification (SEC-072), both a filesystem-level and an application-level check independently gate admission — a defense-in-depth pair, neither alone this contract's sole control.

**[SEC-072]** The app MUST verify, via `SO_PEERCRED` (or the platform's equivalent peer-credential mechanism), that a connecting process's effective uid is `0` before serving any request on the console binding; a connection from any other uid MUST be refused at accept time with no response body (`CONSOLE_PEER_NOT_ROOT`, Error taxonomy — logged, not surfaced to the rejected peer beyond connection closure).

**[SEC-073]** A request admitted over the console binding MUST be attributed to the synthetic `system-console` principal (SEC-002) without any further credential exchange — SO_PEERCRED admission (SEC-072) is this binding's sole authentication mechanism.

**[SEC-074]** The console binding's request and response bodies MUST reuse `api/1`'s own conventions unmodified: the Problem error shape (`api/1` Error shape), `application/json` bodies (API-002), and `Trace-Id` propagation — this contract defines the console binding's transport and admission rule, never a second error shape or body format.

**[SEC-075]** The console binding's verb set MUST be limited to: grant issuance and redemption (Grants), session/API-key revocation (SEC-020), read-only service status, `restore-from-snapshot` (`archive/1`, referenced), and a clock-floor reset (App-tier clock trust, SEC-066–068; relay-analogous, cross-ref `relay/1` Clock trust) — general resource data access MUST NOT be exposed over this binding, even to a caller who has already proven uid-0. A request naming any other verb MUST be refused with `CONSOLE_VERB_NOT_ALLOWED` (Error taxonomy).

**[SEC-076]** A verb MUST NOT be added to the console binding's verb set (SEC-075) unless uid-0 could already perform an equivalent action through direct filesystem or OS-level access on the same host. The binding's purpose is to make a root-equivalent action typed, request/response-shaped, and unconditionally audited (Audit) — never to grant a capability uid-0 does not already possess. `restore-from-snapshot` satisfies this rule because root can already overwrite the workspace file directly; the verb adds audit and safety, never new reach.

**[SEC-077]** Every verb invocation over the console binding MUST emit an `audit.event` (`events/1` EVT-080/EVT-081) attributing the action to `system-console` (SEC-073), with no exception — this is the console binding's own entry on the mandatory-emission list (Audit).

*draft-note: `events/1` EVT-081's own mandatory-emission list, as currently worded, names "every privileged operation performed over a pack-host or relay-host administrative binding" — it does not yet literally name the app-host console binding this section defines. SEC-077 states the intent this contract is explicit about (every console verb is mandatorily audited); closing the literal gap in EVT-081's own enumerated text is a small `events/1` addition this contract flags but does not itself perform (Audit).*

**[SEC-078]** The console binding MUST be reachable, and MUST serve its full verb set (SEC-075), independent of the state of self-hosted TLS (Self-hosted TLS) — including while the app is serving the self-signed fallback (Self-signed fallback) or while no certificate is valid at all. It is the recovery path of last resort precisely because it does not depend on the thing that might be broken.

### Host tier and the host-mutation helper

**[SEC-080]** Host-tier authentication MUST use OS/system accounts only (SSH, `sudo`, PAM) — the platform MUST NOT operate a second, application-level password store for host administration.

**[SEC-081]** A host mutation the web UI or CLI legitimately needs — updating, rolling back, or rebooting the appliance — MUST route through a dedicated, root-owned **host-mutation helper**, never through an ambient root-equivalent capability granted to the app process itself.

**[SEC-082]** The host-mutation helper's verb set MUST be the fixed enum `{apply-update, rollback, reboot}`. It MUST NOT expose network configuration, and MUST NOT expose a shell or an arbitrary-command execution path — a conformant helper accepts exactly these three verbs and nothing shaped like a general remote-execution primitive; a request naming any other verb MUST be refused with `HOST_HELPER_VERB_UNKNOWN` (Error taxonomy).

**[SEC-083]** The host-mutation helper MUST be systemd socket-activated, listening on a Unix domain socket reachable only by the app's own service uid — the inverse admission direction from the console binding (SEC-070–072): here the app is the lesser-privileged caller and the helper is the privileged listener.

**[SEC-084]** `apply-update` MUST accept an artifact reference (a digest, `channel-index/1`-shaped) the calling app process has already resolved and verified against the software-artifact trust bundle (`channel-index/1`) before invoking the helper; the helper itself MUST NOT perform channel-index verification or any network fetch — its own job is mechanical (stage, swap, apply) precisely so its attack surface stays minimal.

**[SEC-085]** Every host-mutation helper verb invocation MUST emit an `audit.event` (`events/1` EVT-080/081), attributed to the principal whose `api/1` request triggered it (not to a synthetic host-helper actor), so a `reboot` traces back to the admin who requested it.

*draft-note: SEC-077's draft-note flags that `events/1` EVT-081's mandatory-emission list does not yet literally name the app-host console binding; the same literal gap applies to the host-mutation helper this section defines — EVT-081's current wording ("every privileged operation performed over a pack-host or relay-host administrative binding") does not name it either. SEC-085 already requires unconditional audit emission on every helper verb regardless; closing EVT-081's literal text is the same small `events/1` addition SEC-077 flags, not a second, separate follow-up (Audit).*

**[SEC-086]** Every host mutation this section does not name (SEC-082) MUST be performed only through the console binding or direct SSH access — never added to the web UI's reachable surface through an unmodeled path. This is what keeps web-admin from quietly becoming root-equivalent under support pressure.

### Lockout policy

**[SEC-090]** Failed-authentication lockout MUST be scoped per `(credential, source-IP class)`, with exponential backoff, never as a single global switch on the target principal — an attacker spraying the login endpoint from one source MUST NOT be able to lock the legitimate owner out of their own account by exhausting a shared, principal-wide attempt budget. A locked-out attempt MUST be refused with `CREDENTIAL_LOCKED` (Error taxonomy).

**[SEC-091]** Every login-failure `audit.event` (`events/1` EVT-080, `action: login.failure`) MUST carry the app's current clock-accuracy assessment (SEC-062) alongside its other fields, so a burst of failures coinciding with a clock-trust transition is diagnosable after the fact.

### Self-hosted TLS

**[SEC-100]** A self-hosted deployment MUST provision TLS from an install-time local certificate authority, with zero dependency on any cloud service — no delegated-DNS certificate scheme (rejected: it requires a cloud DNS dependency even for a fully offline OSS install, and collides with DNS-rebinding protection on some consumer routers).

**[SEC-101]** The platform MUST document, and a client device MUST be able to follow, a trust-install flow for the local CA per client device — this contract does not mandate a specific mechanism (a downloaded root certificate, a QR-code-driven install flow, or another) beyond requiring that one be documented and functional.

**[SEC-102]** `passkey`/WebAuthn credentials (SEC-004) and `surface/1` mounts both require a secure context; a deployment MUST NOT offer either until self-hosted TLS (SEC-100) is live and trusted by the requesting client.

### Self-signed fallback

**[SEC-110]** When the app's TLS certificate is expired, invalid, or otherwise broken, the app MUST continue serving over a **persisted, self-signed fallback** certificate rather than refusing connections outright. The fallback key MUST be generated once and persisted (not regenerated per restart or per connection) so its fingerprint stays stable, making a substituted (MITM) certificate detectable by fingerprint comparison.

**[SEC-111]** Entering the self-signed fallback state (SEC-110) MUST raise a typed Repairs issue and an owner notify event — the fallback is never steady state, and both signals exist to drive the operator toward certificate repair rather than indefinite operation on it. This condition MAY additionally be surfaced to a client as `TLS_FALLBACK_ACTIVE` (Error taxonomy) — informational, not itself request-blocking.

**[SEC-112]** The login page served over the self-signed fallback MUST state plainly that passkey authentication is unavailable (no secure context, SEC-102) and that the remedy is certificate repair — never phrased or styled as a routine click-through security warning.

**[SEC-113]** The self-signed fallback state (SEC-110) MUST NOT be allowed to persist silently. Beyond SEC-111's one-time entry notify, the app MUST re-emit the owner notify event and MUST escalate the Repairs issue's severity/prominence, on a bounded recurring schedule, for as long as the fallback remains active — so an operator who missed or dismissed the first notification is still reached, and the issue's own visibility grows with how long the deployment has run in this degraded state. This requirement bounds silence, not the fallback's own duration, and MUST NOT itself refuse or degrade any request: `TLS_FALLBACK_ACTIVE` remains informational (SEC-111) and basic operation MUST continue uninterrupted — the escalation is entirely in the strength and frequency of the signal, never in blocking operation, since bricking a deployment over its own certificate state is worse than the degraded TLS it would be reacting to.

*draft-note: the exact re-notify cadence and the Repairs-issue severity/prominence escalation steps (both a function of elapsed time in the fallback state) are this contract's own proposal, not fixed by any normative source yet, and are flagged here for confirmation. Separately, an OPTIONAL, owner-configurable enforcement policy MAY be layered on top of SEC-113's mandatory escalation — for instance, blocking passkey enrollment/authentication and other sensitive-credential operations (already unavailable under the fallback, SEC-102/112) after a configurable grace period — but this contract does not make any such blocking mandatory or default-on: SEC-113 itself never blocks basic operation, and a stricter policy is an owner opt-in, never a platform default.*

### First-boot claim window and factory reset

**[SEC-120]** The installer MUST auto-generate a one-time `setup`-purpose grant (Grants) at install time and present it printed, as a QR code, or on-screen; the setup endpoint MUST be claimable only by redeeming this grant. An installed-but-unclaimed box MUST NOT be first-come-first-served to whoever reaches its setup endpoint first on a shared network.

**[SEC-121]** Factory reset on the one-box self-hosted topology MUST destroy all local key material: the workspace and its data key (SEC-040), the box key (SEC-041), the workspace signing key (SEC-046–047), and the relay's device identity plus its enrollment certificate/keypair (`relay/1` Enrollment). This MUST force fresh enrollment on every principal and MUST re-open the first-boot claim window (SEC-120).

**[SEC-122]** For a *stolen* (rather than legitimately resold) box, the complementary control is owner-driven revocation of the relay's enrolled identity (`relay/1` REL-016, REL-066), which a thief's own factory reset cannot prevent or undo — SEC-121's reset is a data-destruction guarantee, not an anti-theft one; SEC-122 is what an owner exercises independent of physical possession of the stolen unit.

**[SEC-123]** On a relay-only appliance (no co-located app), there MUST be no local setup flow; ownership MUST instead be established by redemption of a `relay-claim`-purpose grant (Grants) against the remote app, per `relay/1`'s reserved reverse-claim identity fields (REL-019, REL-125).

**[SEC-124]** A relay-only appliance's factory reset MUST destroy its device identity, its certificate/keypair, its operational state (compiled generation and telemetry queue, `relay/1` REL-142), and its persisted clock floor — performed via a relay-local root socket, this contract's minimal analog of the console binding (The console binding) sized for a device that has no app to bind one to. `relay/1` itself explicitly reserves, and does not define, this socket and its display surface (REL-019); this section is where that reservation is picked up.

**[SEC-125]** A relay-only appliance's printed kit MUST contain a claim code plus revoke/re-claim instructions plus the appliance's direct-outbound-HTTPS site requirement, and MUST NOT contain a passphrase of any kind — a relay-only appliance holds no data key (SEC-040 applies only where a workspace exists) and so has no recovery passphrase to print.

**[SEC-126]** Two recovery artifacts protect two distinct disasters, and neither substitutes for the other: the `archive/1` emergency kit's recovery passphrase (SEC-041) recovers a workspace's **data key** — the dead-box/disk-swap scenario; a break-glass console grant (Break-glass recovery) recovers **login** — the forgotten-password scenario. `waiveo auth recover` MUST always be the documented remedy for a forgotten password; factory reset MUST NOT be documented as a password-recovery path.

### Tier-granted capability baseline and blast radius

**[SEC-130]** A pack's tier-granted baseline is the enumerated `{capability, scope, reason}` set (`manifest/1` MAN-020) its trust channel and namespace admit without an install-time consent prompt — the same mechanism `manifest/1` MAN-022 calls a pack's "pre-consented baseline." This contract does not restate `manifest/1`'s validation rules for that set; it enumerates, per pack, what the baseline actually confers.

**[SEC-131]** An update to a tier-granted pack that widens its `capabilities`, `egress`, `resources`, or `connections` beyond its already-granted baseline MUST still require owner acknowledgment, or park behind a delayed-activation window with a Repairs notice — tier-granted status removes the install-time prompt for the enumerated baseline (SEC-130); it MUST NOT be treated as blanket, permanent, prompt-free consent to anything a later version adds (`manifest/1` MAN-022/023 govern the actual diff mechanics; this requirement states that tier-granted status confers no exemption from them).

**[SEC-132]** The table below is this contract's blast-radius accounting for the named-pack set: for each named pack, its tier-status candidacy, its capability baseline, its egress reach, its device-plane reach, whether it may mount a `surface/1` surface, and the worst case if its own code — not the platform capability it merely configures — is compromised.

| Pack | Tier status | Capability baseline | Egress | Device-plane reach | `surface/1` | Worst case if compromised |
|---|---|---|---|---|---|---|
| `system` | Required (candidate — see draft-note) | Read/write on the platform's typed variable store; secret-stub reference management, write-only (never reads a resolved secret value); Repairs/settings surfaces | None | None | No | Can alter automation-variable values, which constant-fold into the next compiled edge-rule generation (`rules/1` RUL-150) and so can retarget platform-wide rule behavior; can redirect which secret stub a connection resolves to without ever reading a secret's plaintext (`ctx/1` CTX-050 still returns only opaque handles) |
| `slidecast` | Optional (marketplace-installed; commonly recommended at onboarding) | Asset render via `assets.derive` (`html_bundle→png`); CAS put/ref via `ctx.assets`; `playable` content-type contribution; Studio mount | Manifest-declared per pack version | None directly (playback is a `player/1` concern, not pack-mediated) | Yes (Studio, first-party) | Can construct and schedule misleading or malicious on-screen content; through the Studio bridge, can invoke whatever verb the bridge's own allowlist and the operator's session permit (`surface/1`'s audience-scoped token bounds this to that one operator's own authority, never an escalation beyond it) |
| `automation-ui` | Optional | `rules/1` authoring (read/write rule definitions); label/selector read; `automation.run` trace read | None | Indirect only — authored rules reference device entities/selectors; rule *execution* runs through `rules/1`'s closed, compiled vocabulary, never pack code | Conditional (a pre-authorized fallback exists if the declarative UI grammar proves inexpressible for the rule/workflow builder) | Can author a destructive or resource-exhausting automation (e.g. a rule that floods device commands); cannot execute arbitrary code as "a rule" — `rules/1`'s vocabulary is closed and compile-time classified |
| `roku` | Optional | `device.command` scope `media-player` (consent-rendered per `manifest/1` MAN-024); fleet-sideload trigger (relay-executed) | None declared beyond the device plane | Full Roku device class via the relay (ECP commands and polling data for every adopted Roku) | No | Highest device-plane blast radius among named packs: can command every adopted Roku (power state, channel launch) and trigger fleet sideload; the sideload credential itself is a wrapped, point-of-use-resolved stub delivered only in the relay's ephemeral device-command envelope, never persisted desired state or relay disk/logs |
| `device-discovery` | Optional | Candidate-record read/adopt across every device class; `match:` pattern declaration | None | Broad read (whatever the relay's discovery surfaces); adopt is normally an explicit operator action | No | Can enumerate what devices exist on the LAN (reconnaissance value) and, if granted adopt capability, mis-adopt or flood candidate records |
| `device-widgets` | Optional | Entity-state read for dashboard/on-glass tiles; device-command dispatch narrowly scoped per declared action | None | Read broadly (whatever entities its widgets bind to); command only within each action's own declared `capabilityScope` | No | Can expose entity-state values to anyone viewing an affected screen or dashboard (information disclosure) and issue whatever commands its narrowly-scoped actions allow |
| `comms` | Optional | Notification routing-matrix configuration over the platform's own notify verb; in-app inbox read/coalesce | None — SMTP/webhook/push sockets are host-executed platform capabilities, never pack-side | None | No | Can reroute or suppress real alerts, or trigger unwanted notify sends to configured recipients/webhooks; cannot open an arbitrary socket itself |
| `backups` | Optional | Schedule/retention configuration over platform `archive/1` create/restore | None — offsite storage providers are host-executed | None | No | Can trigger an unwanted restore (bounded: `archive/1` ARC-100/101 re-verify trust at restore time regardless, restore is an install path not a bypass) or tamper with retention schedules, opening a silent backup-coverage gap |
| `marketplace` | Optional | Catalog browse and install-trigger UI over the platform install core | None — index resolution is host-executed | None | No | Can misrepresent catalog contents in its own rendered UI to nudge risky installs; cannot forge the actual install-time consent prompt, which the platform renders from the target pack's real, verified manifest (`manifest/1` MAN-022), never from the marketplace pack's own UI |

*draft-note: this table's "Tier status" column is a best-effort scaffold; the definitive required-pack roster is a deployment/packaging concern, not fixed by this contract. The capability/egress/device-plane/surface columns are populated for the full named pack set regardless of eventual tier status, since the widening-consent rule (SEC-131) needs a baseline the moment any deployment grants one.*

### systemd hardening baseline

**[SEC-140]** Every first-party unit on the box (app, relay, `waiveo-derive`, and each per-pack `waiveo-pack@<name>.service`, `ctx/1` CTX-011) MUST run under `ProtectSystem=strict`, `NoNewPrivileges=yes`, and its own dedicated, non-shared system user — never a shared uid across units of different trust levels.

**[SEC-141]** Each unit's `CapabilityBoundingSet` MUST be minimal for its own role; `CAP_NET_RAW` MUST be granted to the relay unit only (its discovery responder needs it) and MUST NOT be granted to the app, `waiveo-derive`, or any pack unit.

**[SEC-142]** The `waiveo-derive` unit MUST run under its own dedicated uid, socket-activated (idle-exit when no job is in flight), with egress denied except loopback — consistent with, and not a restatement of, the job-scoped CAS-input delivery `ctx/1`'s asynchronous `assets.derive` verb carries (`ctx/1` CTX-061).

### Audit

**[SEC-150]** Every flow this contract defines — grant creation and redemption (SEC-034), console-binding verb invocation (SEC-077), host-mutation-helper verb invocation (SEC-085), login success/failure/lockout (SEC-090/091), and any trust-bundle or credential change this contract triggers — MUST emit an `audit.event` per `events/1` EVT-080, satisfying EVT-081's mandatory-emission list. This contract adds no second audit schema; every audit record this contract's flows produce is an ordinary `events/1` `audit.event`.

**[SEC-151]** This contract declines to layer additional tamper-evidence (a hash-chained audit-row log, a self-run signature-transparency log) onto `events/1`'s audit trail for a self-hosted deployment: both are rejected as theater once the adversary already owns the file (root, SEC-041/SEC-121) — `events/1`'s own audit record, correctly and completely emitted (SEC-150), is the mechanism; a stronger tamper-evidence property is not this contract's to invent.

## Wire shapes

```json
// Principal
{
  "principal_id": "01J8Z3K4N5P6Q7R8S9T0V1W2P1",
  "kind": "user",
  "created_at": "2026-07-15T12:00:00Z"
}
```

```json
// Credential (metadata only — never a raw secret value; see ctx/1 CTX-050 for the pack-facing analog)
{
  "credential_id": "01J8Z3K4N5P6Q7R8S9T0V1W2C1",
  "principal_id": "01J8Z3K4N5P6Q7R8S9T0V1W2P1",
  "kind": "totp",
  "created_at": "2026-07-15T12:00:00Z",
  "last_used_at": "2026-07-15T12:05:00Z"
}
```

```json
// RoleBinding
{
  "principal_id": "01J8Z3K4N5P6Q7R8S9T0V1W2P1",
  "scope_node": "01J8Z3K4N5P6Q7R8S9T0V1W2SN",
  "role": "admin"
}
```

```json
// Session
{
  "session_id": "01J8Z3K4N5P6Q7R8S9T0V1W2S1",
  "principal_id": "01J8Z3K4N5P6Q7R8S9T0V1W2P1",
  "device_metadata": { "user_agent": "Mozilla/5.0 ...", "ip_class": "lan" },
  "aal": "standard",
  "created_at": "2026-07-15T12:00:00Z",
  "revoked_at": null
}
```

```json
// Grant — the canonical shape; relay/1's pairing_grants (REL-121) carries the fields it itself needs to enforce
{
  "grant_id": "01J8Z3K4N5P6Q7R8S9T0V1W2G1",
  "purpose": "credential-reset",
  "resulting_principal_kind": "user",
  "scope_node": "01J8Z3K4N5P6Q7R8S9T0V1W2SN",
  "labels": {},
  "role": null,
  "ttl": "PT15M",
  "redemption_mode": "one-time",
  "consent_record": null,
  "issued_via": "api",
  "issued_at": "2026-07-15T12:00:00Z"
}
```

```json
// ConsoleRequest — reuses api/1's Problem error shape and JSON body conventions (API-002, API-010) over the console binding's own transport (SEC-070–074); shown here for the verb enum, not a new envelope
{
  "verb": "session.revoke",
  "params": { "session_id": "01J8Z3K4N5P6Q7R8S9T0V1W2S1" },
  "trace_id": "01J8Z3K4N5P6Q7R8S9T0V1W2T1"
}
```

```json
// HostMutationRequest — over the host-mutation helper's own socket (SEC-081–084); the helper performs no verification of artifact_digest itself
{
  "verb": "apply-update",
  "artifact_digest": "sha256:1f3d...",
  "trace_id": "01J8Z3K4N5P6Q7R8S9T0V1W2T2"
}
```

## Negotiation

This contract's own wire shapes (Principal, Credential, Session, Grant) ride over `api/1`'s `/api/v1` HTTPS binding and inherit its additive-evolution and deprecation policy unmodified (`api/1` Scope) — this contract does not define a second versioning scheme for them.

The console binding (The console binding) negotiates no separate protocol version: it is reachable only once the app itself is running the `api/1` major it implements, and SO_PEERCRED admission (SEC-072) substitutes for the credential exchange an ordinary `/api/v1` request would otherwise need — there is no pre-admission handshake to negotiate.

A security-model minor MAY add a new grant `purpose` value, a new console-binding verb (subject to SEC-076's admission rule), or a new principal/credential `kind`, additively. Narrowing the console binding's verb admission rule (SEC-076), removing a mandatory audit emission (SEC-150), or weakening the key-hierarchy separation (SEC-044) would each be a major.

## Error taxonomy

| code | meaning | retryable |
|---|---|---|
| `GRANT_EXPIRED` | A grant's `ttl` elapsed before redemption (SEC-035). | no — a fresh grant is required |
| `GRANT_ALREADY_REDEEMED` | A `one-time` grant's code was presented a second time (SEC-035). | no |
| `GRANT_PURPOSE_MISMATCH` | A grant's code was presented to a redemption endpoint that does not match its `purpose` (SEC-035). | no |
| `CONSOLE_PEER_NOT_ROOT` | A connection to the console binding was not made by an effective uid-0 process (SEC-072). | no — connect as root |
| `CONSOLE_VERB_NOT_ALLOWED` | A console-binding request named a verb outside SEC-075's enumerated set. | no |
| `RECOVERY_NOT_AVAILABLE` | `waiveo auth recover` (or its API equivalent) was attempted against a cloud endpoint, where it is structurally absent (SEC-060). | no |
| `RECOVERY_GRANT_BEFORE_CLAIM` | A `recovery`-purpose grant was requested on a box that has not yet completed first-boot claim (SEC-065). | no — claim the box first |
| `CREDENTIAL_LOCKED` | The presented credential is locked out under SEC-090's per-`(credential, source-IP class)` policy. | yes — after the stated backoff |
| `HOST_HELPER_VERB_UNKNOWN` | A host-mutation-helper request named a verb outside `{apply-update, rollback, reboot}` (SEC-082). | no |
| `TLS_FALLBACK_ACTIVE` | Informational condition accompanying a response served over the self-signed fallback (SEC-110); not itself request-blocking. | n/a |

## Conformance notes

- Traceability map: `conformance/traceability/security-model.md` — maps every `SEC-NNN` above to the case(s) that exercise it.
- Corpus: `conformance/corpora/security-model/` — one JSON case file per `case-id`.
- SO_PEERCRED admission (SEC-072) and physical console/SSH possession (Break-glass recovery, Host tier and the host-mutation helper) are, by nature, host-process properties a static JSON corpus cannot itself exercise; conformance cases model them as a given input (`peer_uid: 0` or `peer_uid: 1000`) exactly as `api/1`'s own corpus treats an authenticated principal as a given (`api/1` Conformance notes) — the driver harness that actually opens a Unix socket and checks SO_PEERCRED is a systemd-install-smoke-lane concern, not this static corpus's.
- Lockout backoff timing (SEC-090) and grant `ttl` expiry (SEC-032, SEC-035) are timing-dependent and exercised against an injectable clock in a driver harness, not wall-clock sleeps in a static corpus — consistent with the fake-clock policy every other contract's corpus follows.
- The tier-granted blast-radius table (SEC-132) is reference content, not a set of independently case-tested requirements beyond SEC-130/131's own mechanics; its per-pack rows describe a design-time accounting, not wire behavior a conformance case asserts row-by-row.
