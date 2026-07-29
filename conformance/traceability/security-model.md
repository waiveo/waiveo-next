# Traceability: security-model/1

One row per requirement ID `contracts/security-model.md` defines. Format: `conformance/traceability/README.md`.

**2026-07-28 drive note:** `conformance/drivers/securitymodel1` executes all
eleven frozen cases against live `internal/app` code, and
`conformance/driven-manifest.json` records all eleven as driven. **TWO rows are
`covered`: SEC-035 and SEC-120.** Every other row stays `TBD-wave1`, and the gap
between "eleven cases pass" and "two rows covered" is the point of this note.

Three rows (SEC-066, SEC-067, SEC-120) were briefly marked `covered` earlier the
same day and were reverted, after an adversarial review demonstrated for each one
a deliberately broken implementation that keeps the whole corpus green. Two of
those three findings have now been closed by adding the case each requirement was
missing; the third is not a case problem at all and is unchanged.

- **SEC-067 — CLOSED as a case defect, row still `TBD-wave1` for a different
  reason.** The finding was that the original case's unauthenticated candidate is
  LARGER than its verifiable one, so the two differ in two variables at once: a
  gate that ignores provenance entirely and admits anything under an absolute
  plausibility cap passes it while letting any client-supplied time walk the
  floor forward. `SEC-067a-invalid-unauthenticated-claim-below-a-verifiable-value`
  holds magnitude fixed the other way — its unauthenticated candidate is SMALLER
  than the verifiable value that follows, both above the floor — so provenance is
  the only rule that can separate them. Reproduced: with a plausibility-cap
  `Advance` that ignores `src`, the original case still PASSES and SEC-067a
  FAILS. The row nonetheless stays `TBD-wave1` on the reachability rule below —
  see the clock-floor paragraph.
- **SEC-120 — CLOSED, row now `covered`.** The finding was that the original
  case replays a code against an ALREADY-CLAIMED box, which proves one-time-grant
  single use (SEC-031's property, not SEC-120's): a handler that skips the code
  check and admits the first caller — pure first-come-first-served, verbatim what
  SEC-120 forbids — passes it.
  `SEC-120a-invalid-unclaimed-box-claimed-without-the-setup-code` runs entirely
  inside the window that handler would give away. Reproduced and closed: against
  a first-come-first-served `Claim` (no code required; the first caller becomes
  owner; everyone after gets `GRANT_ALREADY_REDEEMED`) the original case still
  PASSES and SEC-120a FAILS.
- **SEC-066 — unchanged, still `TBD-wave1`.** Not a case defect: the shipped app
  does not deliver the property. See the clock-floor paragraph below, which has
  been rewritten now that half of its cause is fixed.

A fourth weakness, not in the original three, is closed the same way.
**SEC-035** enumerates three refusals and names a WIRE CODE for each, but its
only case drove the store directly, so the code it compared against came from a
mapping inside the driver itself — a handler that refused correctly and typed the
refusal wrongly kept it green.
`SEC-035a-invalid-grant-refusals-on-the-redemption-endpoint` drives all three
refusals through the mounted route and reads each code out of the Problem body
the route writes. Reproduced and closed: mapping `ErrGrantExpired` to
`UNAUTHENTICATED` on the wire leaves the original case PASSING and fails
SEC-035a; so does typing a replayed code as `GRANT_EXPIRED`; so does deleting the
purpose check outright. The row is now `covered`.

**The mutations behind the two `covered` rows**, so a later reader can re-run
them rather than take this note's word for it. Each was applied to a scratch copy
of the tree (`git archive HEAD`), touching ONLY the implementation — never the
corpus, never the driver:

| row | mutation | result |
|---|---|---|
| SEC-035 | `writeGrantProblem` types an expired grant `UNAUTHENTICATED` | SEC-035a FAILS, SEC-035 passes |
| SEC-035 | `writeGrantProblem` types a replayed one-time code `GRANT_EXPIRED` | SEC-035a FAILS |
| SEC-035 | `RedeemGrant`'s purpose check deleted (a `pairing` code redeems at the setup endpoint) | SEC-035a FAILS |
| SEC-120 | `Claim` requires no code and admits the first caller on an unclaimed box | SEC-120a FAILS, SEC-120 passes |
| SEC-120 | `Claim` refuses every caller (a shut endpoint) | SEC-120a FAILS on its code-holder leg |

Two residuals ride with those rows rather than being hidden by them. SEC-035's
`GRANT_PURPOSE_MISMATCH` clause illustrates itself with "a `pairing`-purpose code
MUST NOT redeem against the credential-reset endpoint"; there is no
credential-reset endpoint in this tree, so the case drives the same general rule
("a code whose grant's `purpose` does not match the redemption endpoint being
called") against the one redemption endpoint that exists. And SEC-120's
"present it printed, as a QR code, or on-screen" clause is satisfied by the
shipped feeder's boot log and by the 0600 code file, but no case asserts it: the
driver asserts that the real bootstrap MINTED a code and handed it back for
presentation, and that the endpoint is claimable only by redeeming it.

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

**SEC-031 is where the bookkeeping used to be inverted, and it is now stated the
right way round.** The case named `SEC-120-invalid-first-boot-claim-outside-window`
proves SEC-031's single-use enforcement, not SEC-120's own clause — replaying a
consumed code is a property of every `one-time` purpose and says nothing about
arrival order. It is therefore cited on BOTH rows, with each row's meaning made
explicit rather than inferred from the filename: on SEC-031 it is the evidence,
and on SEC-120 it is the half of that requirement (`claimable only by redeeming
this grant`, after the grant is spent) that sits beside SEC-120a's half (the same
clause before it is spent, plus the first-come-first-served MUST NOT).
SEC-035a's already-redeemed leg corroborates the same single-use enforcement on
the wire. SEC-031 nonetheless stays `TBD-wave1`, for one reason and not the old
one: its second clause binds a `multi` grant's redemption count, and nothing in
this tree mints a `multi` grant, so no issuing flow declares a bound for a case
to hold anyone to.

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

**The clock floor (SEC-066-068) is a live mechanism that still does not deliver
its requirement — but for one reason now, not two.** `internal/app/auth.ClockFloor`
persists a monotonic floor, guards it against moving backward, clamps it on
restart and reports an assessment, all driven. SEC-066 exists so a time-windowed
check cannot be defeated by turning the host clock back, and the shipped feeder
used to get neither half of that.

The first half is fixed. The auth store — which decides grant `ttl` expiry
(SEC-032/035), every TOTP step (SEC-004), the pending-enrollment window and the
lockout backoff (SEC-090) — was opened through a convenience that hardcoded
`time.Now`, so it ran on the bare host clock while the rest of the process ran on
the floor. Measured with the floor an hour above the wall, the store's clock read
~3.6M ms below it and a floor-expired grant redeemed cleanly. The convenience is
gone (its only caller was the feeder), the clock is a required argument, and the
feeder opens the store on the floor's reading. The floor is now the app's single
notion of current time: the desired-state source, which had a second wall clock of
its own, reads it too.

The second half is not fixed and is deliberately not being faked. No
authenticated time SOURCE is wired, so nothing calls `Advance`, the floor never
leaves 0, `Now()` always equals the wall clock, and the restart clamp never fires
on a real deployment — turning a box's clock back today still moves every
time-windowed check with it. SEC-067 says outright that which authenticated-time
sources a deployment trusts "is implementation-defined per deployment tier", so
inventing one to earn a row would be inventing the trust decision with it.

**SEC-066 and SEC-067 therefore both stay `TBD-wave1`, on the same "no
deployment can reach it" rule applied to the console binding above** — and the
consistency is the point, since SEC-067's gate is exactly as unreachable as
SEC-072's admission rule: real, compiled in, correct, and never called by
anything a deployment runs. Both rows become claimable the moment a source
exists, and nothing else has to change to claim them: SEC-066's case fails
against a floor that is not restored on restart, and SEC-067a fails against a
provenance-blind gate, both verified.

One further clock-floor change rides here because a traceability reader will
otherwise wonder where it went. `clockfloor.go` claimed the floor "rides beside
the auth database because they share a lifecycle: a factory reset that destroys
the credential store has no business leaving a clock floor behind" — a claim no
code made true. The reset now destroys both. No row moves on that: SEC-121 does
not enumerate the floor (SEC-124 does, for a relay-only appliance), and the
frozen SEC-121 case declares nothing about it, so it is covered by
`internal/app/auth`'s own Go tests, which is not the bar this column measures.
Separately, a mutation making `Now()` return the floor and ignore the wall clock
forever — freezing every timestamp in the app at the last verified advance — was
caught by NOTHING in the repository (verified: the whole `go test ./...` suite
passes against it). It is now caught by a Go test, and by no corpus case: every
case and every other test exercises only the side of the clamp where the floor
wins.

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
| SEC-035 | `contracts/security-model.md#grants` | `SEC-035-invalid-grant-expired-rejected`, `SEC-035a-invalid-grant-refusals-on-the-redemption-endpoint` | covered |
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
| SEC-067 | `contracts/security-model.md#app-tier-clock-trust` | `SEC-067-invalid-unauthenticated-time-claim-does-not-advance-floor`, `SEC-067a-invalid-unauthenticated-claim-below-a-verifiable-value` | TBD-wave1 |
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
| SEC-120 | `contracts/security-model.md#first-boot-claim-window-and-factory-reset` | `SEC-120-invalid-first-boot-claim-outside-window`, `SEC-120a-invalid-unclaimed-box-claimed-without-the-setup-code` | covered |
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
