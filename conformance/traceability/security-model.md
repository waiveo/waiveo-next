# Traceability: security-model/1

One row per requirement ID `contracts/security-model.md` defines. Format: `conformance/traceability/README.md`.

**2026-07-28 first-drive note:** `conformance/drivers/securitymodel1` now
executes all eight frozen cases against live `internal/app` code — the first
time any of them ran at all. Every case PASSes, and `conformance/driven-manifest.json`
records all eight as driven. **ZERO rows flip to `covered`**, and the gap
between "eight cases pass" and "no rows covered" is the point of this note.

Three rows (SEC-066, SEC-067, SEC-120) were briefly marked `covered` and were
reverted the same day after an adversarial review demonstrated, for each one, a
deliberately broken implementation that keeps the whole corpus green:

- **SEC-067** — the case's unauthenticated candidate is LARGER than its
  verifiable one, so the two differ in two variables at once. An
  implementation that ignores provenance entirely and accepts any
  plausible-magnitude claim passes the case, while doing exactly what SEC-067
  forbids. The case cannot isolate the variable the requirement is about.
- **SEC-120** — the case replays a code against an ALREADY-CLAIMED box, which
  proves one-time-grant single use (SEC-031's property) and not SEC-120's. A
  handler that skips the code check entirely and admits the first caller —
  pure first-come-first-served, verbatim what SEC-120 forbids — passes.
- **SEC-066** — see the clock-floor note below; the shipped app does not yet
  buy the property, and one clause of the requirement is caught by no case.

In each instance the mutation was caught only by `internal/app/auth`'s own Go
tests — which is precisely the bar this column disclaims elsewhere in this
file. Strengthening those cases is tracked work; until then the honest status
is `TBD-wave1`.

`covered` here keeps `README.md`'s meaning — a listed case exercises this
requirement today — with two additions this contract forces: the requirement
must be something the SHIPPED APP actually does, and the case must FAIL
against an implementation that violates the requirement. Most of
security-model/1's mechanisms exist in this tree only as components with no
caller:

- **The console binding has no transport.** There is no Unix domain socket
  (SEC-070), no `0700` socket file (SEC-071), and nothing reads `SO_PEERCRED`
  (SEC-072). `internal/app/auth.Console` implements the admission rule, the
  closed verb set and the `system-console` attribution, and the driver
  exercises all three with the injected `peer_uid` this contract's own
  Conformance notes bless — but no deployment can reach it, so SEC-072/073/
  075/076/077 stay `TBD-wave1`. SEC-072 additionally has no frozen case for
  its refusal half (the corpus carries no `peer_uid: 1000` case); that half
  is covered by `internal/app/auth`'s own Go tests, which is not the bar this
  column measures.
- **The credential-reset flow has no caller.** SEC-050 names
  `waiveo user password-reset <user>`; no such command exists, and no
  `api/1` route serves the flow either. `Store.IssueCredentialResetGrant` /
  `RedeemCredentialResetGrant` are real and fully driven — the grant shape is
  read back off the persisted row, the admin handoff is searched for the
  target's actual credential values, and the eviction is confirmed on both the
  session token and the API key's own credential row — but an operator cannot
  invoke any of it, so SEC-050/051/053 stay `TBD-wave1`.
- **Break-glass recovery does not exist.** SEC-060–065 are unimplemented,
  which is also why SEC-068 stays `TBD-wave1` despite its assessment being
  live and driven: SEC-068 requires `recover` to surface the assessment before
  proceeding, and there is no `recover`.

Two requirements are partially proven and stay `TBD-wave1` for that reason
alone:

- **SEC-035** enumerates three refusals; the corpus reaches `GRANT_EXPIRED`
  (driven, on this row's own case) and `GRANT_ALREADY_REDEEMED` (driven, but
  on SEC-120's case, whose envelope does not claim SEC-035).
  `GRANT_PURPOSE_MISMATCH` is implemented and reached by no frozen case.
- **SEC-031** is proven for the single-use enforcement half only; nothing in
  this tree mints a `multi` grant, so its redemption-count bound is
  unexercised.

**SEC-121 is the one to read carefully.** Its case is driven through the real
mounted delete operation and PASSes, and the destruction is proved by
inspection rather than by a delete call returning nil: the data key is asked
four independent ways (presence flag, sealer derivation, private half, and the
key files on disk), and the claim window's re-opening is proved by re-running
the real first-boot bootstrap afterwards. The row still does NOT flip, because
SEC-121's text also enumerates the box key and the relay's device identity plus
enrollment certificate — neither of which exists in this tree at all. Their
expected `false` values would be satisfied vacuously, since both were already
absent BEFORE the reset, so the driver refuses to assert them and records
explicit notes on the case instead.

**SEC-032** stays `TBD-wave1` because its own MUST is about code entropy
(>=128 bits, satisfied at 256 by `MintGrantCode`) and no frozen case asserts
it; the `~15 minute ttl` clause the SEC-035 case does exercise is a SHOULD.

**The clock floor (SEC-066-068) is a live mechanism that does not yet deliver
its requirement.** `internal/app/auth.ClockFloor` persists a monotonic floor,
guards it against moving backward, clamps it on restart and reports an
assessment — all driven. But SEC-066 exists so a time-windowed check cannot be
defeated by turning the host clock back, and the shipped feeder gets neither
half of that: the auth store (which decides grant `ttl` and every TOTP step)
is opened with `auth.OpenDefault` and runs on the bare wall clock, not on the
floor; and no authenticated time source is wired, so nothing calls `Advance`
and the floor never leaves 0. SEC-066 therefore stays `TBD-wave1` on the same
"no deployment can reach it" rule applied to the console binding above.

| req-id | contract §anchor | case-id(s) | status |
|---|---|---|---|
| SEC-001 | `contracts/security-model.md#principal-kinds-and-credentials` | - | TBD-wave1 |
| SEC-002 | `contracts/security-model.md#principal-kinds-and-credentials` | - | TBD-wave1 |
| SEC-003 | `contracts/security-model.md#principal-kinds-and-credentials` | - | TBD-wave1 |
| SEC-004 | `contracts/security-model.md#principal-kinds-and-credentials` | - | TBD-wave1 |
| SEC-005 | `contracts/security-model.md#principal-kinds-and-credentials` | - | TBD-wave1 |
| SEC-010 | `contracts/security-model.md#roles-and-scope-node-authorization` | - | TBD-wave1 |
| SEC-011 | `contracts/security-model.md#roles-and-scope-node-authorization` | - | TBD-wave1 |
| SEC-012 | `contracts/security-model.md#roles-and-scope-node-authorization` | - | TBD-wave1 |
| SEC-020 | `contracts/security-model.md#sessions` | - | TBD-wave1 |
| SEC-021 | `contracts/security-model.md#sessions` | - | TBD-wave1 |
| SEC-022 | `contracts/security-model.md#sessions` | - | TBD-wave1 |
| SEC-023 | `contracts/security-model.md#sessions` | - | TBD-wave1 |
| SEC-024 | `contracts/security-model.md#sessions` | - | TBD-wave1 |
| SEC-025 | `contracts/security-model.md#sessions` | - | TBD-wave1 |
| SEC-030 | `contracts/security-model.md#grants` | - | TBD-wave1 |
| SEC-031 | `contracts/security-model.md#grants` | `SEC-120-invalid-first-boot-claim-outside-window` | TBD-wave1 |
| SEC-032 | `contracts/security-model.md#grants` | `SEC-035-invalid-grant-expired-rejected` | TBD-wave1 |
| SEC-033 | `contracts/security-model.md#grants` | - | TBD-wave1 |
| SEC-034 | `contracts/security-model.md#grants` | - | TBD-wave1 |
| SEC-035 | `contracts/security-model.md#grants` | `SEC-035-invalid-grant-expired-rejected` | TBD-wave1 |
| SEC-036 | `contracts/security-model.md#grants` | - | TBD-wave1 |
| SEC-040 | `contracts/security-model.md#key-hierarchy` | - | TBD-wave1 |
| SEC-041 | `contracts/security-model.md#key-hierarchy` | - | TBD-wave1 |
| SEC-042 | `contracts/security-model.md#key-hierarchy` | - | TBD-wave1 |
| SEC-043 | `contracts/security-model.md#key-hierarchy` | - | TBD-wave1 |
| SEC-044 | `contracts/security-model.md#key-hierarchy` | - | TBD-wave1 |
| SEC-045 | `contracts/security-model.md#key-hierarchy` | - | TBD-wave1 |
| SEC-046 | `contracts/security-model.md#key-hierarchy` | - | TBD-wave1 |
| SEC-047 | `contracts/security-model.md#key-hierarchy` | - | TBD-wave1 |
| SEC-048 | `contracts/security-model.md#key-hierarchy` | - | TBD-wave1 |
| SEC-050 | `contracts/security-model.md#credential-reset-grants` | `SEC-050-valid-credential-reset-grant-flow` | TBD-wave1 |
| SEC-051 | `contracts/security-model.md#credential-reset-grants` | `SEC-050-valid-credential-reset-grant-flow` | TBD-wave1 |
| SEC-052 | `contracts/security-model.md#credential-reset-grants` | - | TBD-wave1 |
| SEC-053 | `contracts/security-model.md#credential-reset-grants` | `SEC-050-valid-credential-reset-grant-flow` | TBD-wave1 |
| SEC-060 | `contracts/security-model.md#break-glass-recovery` | - | TBD-wave1 |
| SEC-061 | `contracts/security-model.md#break-glass-recovery` | - | TBD-wave1 |
| SEC-062 | `contracts/security-model.md#break-glass-recovery` | - | TBD-wave1 |
| SEC-063 | `contracts/security-model.md#break-glass-recovery` | - | TBD-wave1 |
| SEC-064 | `contracts/security-model.md#break-glass-recovery` | - | TBD-wave1 |
| SEC-065 | `contracts/security-model.md#break-glass-recovery` | - | TBD-wave1 |
| SEC-066 | `contracts/security-model.md#app-tier-clock-trust` | `SEC-066-valid-monotonic-floor-survives-restart` | TBD-wave1 |
| SEC-067 | `contracts/security-model.md#app-tier-clock-trust` | `SEC-067-invalid-unauthenticated-time-claim-does-not-advance-floor` | TBD-wave1 |
| SEC-068 | `contracts/security-model.md#app-tier-clock-trust` | `SEC-067-invalid-unauthenticated-time-claim-does-not-advance-floor` | TBD-wave1 |
| SEC-070 | `contracts/security-model.md#the-console-binding` | - | TBD-wave1 |
| SEC-071 | `contracts/security-model.md#the-console-binding` | - | TBD-wave1 |
| SEC-072 | `contracts/security-model.md#the-console-binding` | `SEC-072-valid-console-admission-uid0` | TBD-wave1 |
| SEC-073 | `contracts/security-model.md#the-console-binding` | `SEC-072-valid-console-admission-uid0` | TBD-wave1 |
| SEC-074 | `contracts/security-model.md#the-console-binding` | - | TBD-wave1 |
| SEC-075 | `contracts/security-model.md#the-console-binding` | `SEC-075-invalid-console-verb-not-allowed` | TBD-wave1 |
| SEC-076 | `contracts/security-model.md#the-console-binding` | `SEC-075-invalid-console-verb-not-allowed` | TBD-wave1 |
| SEC-077 | `contracts/security-model.md#the-console-binding` | - | TBD-wave1 |
| SEC-078 | `contracts/security-model.md#the-console-binding` | - | TBD-wave1 |
| SEC-080 | `contracts/security-model.md#host-tier-and-the-host-mutation-helper` | - | TBD-wave1 |
| SEC-081 | `contracts/security-model.md#host-tier-and-the-host-mutation-helper` | - | TBD-wave1 |
| SEC-082 | `contracts/security-model.md#host-tier-and-the-host-mutation-helper` | - | TBD-wave1 |
| SEC-083 | `contracts/security-model.md#host-tier-and-the-host-mutation-helper` | - | TBD-wave1 |
| SEC-084 | `contracts/security-model.md#host-tier-and-the-host-mutation-helper` | - | TBD-wave1 |
| SEC-085 | `contracts/security-model.md#host-tier-and-the-host-mutation-helper` | - | TBD-wave1 |
| SEC-086 | `contracts/security-model.md#host-tier-and-the-host-mutation-helper` | - | TBD-wave1 |
| SEC-090 | `contracts/security-model.md#lockout-policy` | - | TBD-wave1 |
| SEC-091 | `contracts/security-model.md#lockout-policy` | - | TBD-wave1 |
| SEC-100 | `contracts/security-model.md#self-hosted-tls` | - | TBD-wave1 |
| SEC-101 | `contracts/security-model.md#self-hosted-tls` | - | TBD-wave1 |
| SEC-102 | `contracts/security-model.md#self-hosted-tls` | - | TBD-wave1 |
| SEC-110 | `contracts/security-model.md#self-signed-fallback` | - | TBD-wave1 |
| SEC-111 | `contracts/security-model.md#self-signed-fallback` | - | TBD-wave1 |
| SEC-112 | `contracts/security-model.md#self-signed-fallback` | - | TBD-wave1 |
| SEC-113 | `contracts/security-model.md#self-signed-fallback` | - | TBD-wave1 |
| SEC-120 | `contracts/security-model.md#first-boot-claim-window-and-factory-reset` | `SEC-120-invalid-first-boot-claim-outside-window` | TBD-wave1 |
| SEC-121 | `contracts/security-model.md#first-boot-claim-window-and-factory-reset` | `SEC-121-valid-factory-reset-destroys-key-material` | TBD-wave1 |
| SEC-122 | `contracts/security-model.md#first-boot-claim-window-and-factory-reset` | - | TBD-wave1 |
| SEC-123 | `contracts/security-model.md#first-boot-claim-window-and-factory-reset` | - | TBD-wave1 |
| SEC-124 | `contracts/security-model.md#first-boot-claim-window-and-factory-reset` | - | TBD-wave1 |
| SEC-125 | `contracts/security-model.md#first-boot-claim-window-and-factory-reset` | - | TBD-wave1 |
| SEC-126 | `contracts/security-model.md#first-boot-claim-window-and-factory-reset` | - | TBD-wave1 |
| SEC-130 | `contracts/security-model.md#tier-granted-capability-baseline-and-blast-radius` | - | TBD-wave1 |
| SEC-131 | `contracts/security-model.md#tier-granted-capability-baseline-and-blast-radius` | - | TBD-wave1 |
| SEC-132 | `contracts/security-model.md#tier-granted-capability-baseline-and-blast-radius` | - | TBD-wave1 |
| SEC-140 | `contracts/security-model.md#systemd-hardening-baseline` | - | TBD-wave1 |
| SEC-141 | `contracts/security-model.md#systemd-hardening-baseline` | - | TBD-wave1 |
| SEC-142 | `contracts/security-model.md#systemd-hardening-baseline` | - | TBD-wave1 |
| SEC-150 | `contracts/security-model.md#audit` | - | TBD-wave1 |
| SEC-151 | `contracts/security-model.md#audit` | - | TBD-wave1 |
