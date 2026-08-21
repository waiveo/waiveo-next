# Traceability: relay/1

One row per requirement ID `contracts/relay-1.md` defines. Format: `conformance/traceability/README.md`.

**REL-123 scope note (read with the row below):** REL-123 names THREE
enforcement points — player-certificate issuance, channel-token issuance, and
per-connection credential verification. `REL-123-valid-revocation-enforced-while-disconnected`
drives the two this relay performs: channel-token issuance
(`internal/relay/playerserver`'s redemption path) and credential verification
(the presented-token check on `GET /player/v1/program`), both from the relay's
own last-synced copy — with the app peer torn down, and again across a process
restart that carries nothing but the durable store. It asserts NOTHING about
player-certificate issuance, because this codebase issues no player certificate:
a redemption answers with trust anchors and a channel token, `player/1`'s own
token-renewal route (PLY-074) is unimplemented, and there is no surface for a
differential oracle to observe. `covered` here therefore means "the enforcement
this relay performs is driven end to end", not "all three decisions are
exercised". The third becomes exercisable only when player-certificate issuance
exists; until then this row would read as a claim about behaviour with no
implementation behind it, which is what this note exists to prevent.

| req-id | contract §anchor | case-id(s) | status |
|---|---|---|---|
| REL-001 | `contracts/relay-1.md#versioning--transport-surface` | - | TBD-wave1 |
| REL-002 | `contracts/relay-1.md#versioning--transport-surface` | - | TBD-wave1 |
| REL-003 | `contracts/relay-1.md#versioning--transport-surface` | - | TBD-wave1 |
| REL-004 | `contracts/relay-1.md#versioning--transport-surface` | - | TBD-wave1 |
| REL-005 | `contracts/relay-1.md#versioning--transport-surface` | `REL-030-valid-hello-negotiate-channel-binding` | covered |
| REL-006 | `contracts/relay-1.md#versioning--transport-surface` | `REL-110-valid-device-candidate-and-command` | covered |
| REL-007 | `contracts/relay-1.md#versioning--transport-surface` | - | TBD-wave1 |
| REL-010 | `contracts/relay-1.md#enrollment` | `REL-010-valid-fresh-enroll` | covered |
| REL-011 | `contracts/relay-1.md#enrollment` | `REL-010-valid-fresh-enroll` | covered |
| REL-012 | `contracts/relay-1.md#enrollment` | `REL-010-valid-fresh-enroll` | covered |
| REL-013 | `contracts/relay-1.md#enrollment` | `REL-010-valid-fresh-enroll` | covered |
| REL-014 | `contracts/relay-1.md#enrollment` | `REL-010-valid-fresh-enroll`, `REL-015-valid-renew-ahead-of-expiry`, `REL-020-valid-re-enroll-after-cert-expiry` | covered |
| REL-015 | `contracts/relay-1.md#enrollment` | `REL-015-valid-renew-ahead-of-expiry` | covered |
| REL-016 | `contracts/relay-1.md#enrollment` | - | TBD-wave1 |
| REL-017 | `contracts/relay-1.md#enrollment` | `REL-020-valid-re-enroll-after-cert-expiry` | covered |
| REL-018 | `contracts/relay-1.md#enrollment` | - | TBD-wave1 |
| REL-019 | `contracts/relay-1.md#enrollment` | - | TBD-wave1 |
| REL-020 | `contracts/relay-1.md#expired-certificate-re-enrollment` | `REL-020-valid-re-enroll-after-cert-expiry` | covered |
| REL-021 | `contracts/relay-1.md#expired-certificate-re-enrollment` | `REL-020-valid-re-enroll-after-cert-expiry` | covered |
| REL-022 | `contracts/relay-1.md#expired-certificate-re-enrollment` | `REL-022-invalid-re-enroll-superseded-cert` | covered |
| REL-023 | `contracts/relay-1.md#expired-certificate-re-enrollment` | `REL-020-valid-re-enroll-after-cert-expiry` | covered |
| REL-024 | `contracts/relay-1.md#expired-certificate-re-enrollment` | `REL-020-valid-re-enroll-after-cert-expiry` | covered |
| REL-025 | `contracts/relay-1.md#expired-certificate-re-enrollment` | `REL-020-valid-re-enroll-after-cert-expiry` | covered |
| REL-026 | `contracts/relay-1.md#expired-certificate-re-enrollment` | `REL-027-invalid-re-enroll-pop-signature-invalid` | covered |
| REL-027 | `contracts/relay-1.md#expired-certificate-re-enrollment` | `REL-027-invalid-re-enroll-pop-signature-invalid` | covered |
| REL-028 | `contracts/relay-1.md#expired-certificate-re-enrollment` | `REL-027-invalid-re-enroll-pop-signature-invalid` | covered |
| REL-029 | `contracts/relay-1.md#expired-certificate-re-enrollment` | `REL-027-invalid-re-enroll-pop-signature-invalid` | covered |
| REL-030 | `contracts/relay-1.md#hello--negotiate` | `REL-030-valid-hello-negotiate-channel-binding` | covered |
| REL-031 | `contracts/relay-1.md#hello--negotiate` | `REL-030-valid-hello-negotiate-channel-binding` | covered |
| REL-032 | `contracts/relay-1.md#hello--negotiate` | `REL-030-valid-hello-negotiate-channel-binding` | covered |
| REL-033 | `contracts/relay-1.md#hello--negotiate` | `REL-030-valid-hello-negotiate-channel-binding` | covered |
| REL-034 | `contracts/relay-1.md#hello--negotiate` | `REL-030-valid-hello-negotiate-channel-binding` | covered |
| REL-035 | `contracts/relay-1.md#hello--negotiate` | `REL-030-valid-hello-negotiate-channel-binding` | covered |
| REL-036 | `contracts/relay-1.md#hello--negotiate` | `REL-030-valid-hello-negotiate-channel-binding` | covered |
| REL-037 | `contracts/relay-1.md#hello--negotiate` | - | TBD-wave1 |
| REL-038 | `contracts/relay-1.md#hello--negotiate` | `REL-030-valid-hello-negotiate-channel-binding` | covered |
| REL-039 | `contracts/relay-1.md#hello--negotiate` | `REL-030-valid-hello-negotiate-channel-binding` | covered |
| REL-040 | `contracts/relay-1.md#hello--negotiate` | - | TBD-wave1 |
| REL-041 | `contracts/relay-1.md#hello--negotiate` | - | TBD-wave1 |
| REL-050 | `contracts/relay-1.md#desired-state-pull` | `REL-056-valid-generation-apply-atomic-swap`, `REL-051-valid-ahead-generation-full-snapshot` | covered |
| REL-051 | `contracts/relay-1.md#desired-state-pull` | `REL-056-valid-generation-apply-atomic-swap`, `REL-051-valid-ahead-generation-full-snapshot` | covered |
| REL-052 | `contracts/relay-1.md#desired-state-pull` | `REL-056-valid-generation-apply-atomic-swap`, `REL-070-valid-generation-reapply-idempotent-noop`, `REL-051-valid-ahead-generation-full-snapshot` | covered |
| REL-053 | `contracts/relay-1.md#desired-state-pull` | `REL-056-valid-generation-apply-atomic-swap` | covered |
| REL-054 | `contracts/relay-1.md#desired-state-pull` | `REL-056-valid-generation-apply-atomic-swap`, `REL-071-invalid-wrong-peer-key-snapshot-rejected`, `REL-051-valid-ahead-generation-full-snapshot` | covered |
| REL-055 | `contracts/relay-1.md#desired-state-pull` | `REL-056-valid-generation-apply-atomic-swap`, `REL-061-valid-preempt-priority-screen-program-offline`, `REL-123-valid-revocation-enforced-while-disconnected` | covered |
| REL-056 | `contracts/relay-1.md#desired-state-pull` | `REL-056-valid-generation-apply-atomic-swap` | covered |
| REL-057 | `contracts/relay-1.md#desired-state-pull` | `REL-057-valid-state-changed-nudge-triggers-pull` | covered |
| REL-060 | `contracts/relay-1.md#desired-state-snapshot-sections` | `REL-056-valid-generation-apply-atomic-swap` | covered |
| REL-061 | `contracts/relay-1.md#desired-state-snapshot-sections` | `REL-056-valid-generation-apply-atomic-swap`, `REL-061-valid-preempt-priority-screen-program-offline` | covered |
| REL-061a | `contracts/relay-1.md#desired-state-snapshot-sections` | - | TBD-wave1 |
| REL-062 | `contracts/relay-1.md#desired-state-snapshot-sections` | `REL-056-valid-generation-apply-atomic-swap` | covered |
| REL-063 | `contracts/relay-1.md#desired-state-snapshot-sections` | `REL-056-valid-generation-apply-atomic-swap` | covered |
| REL-064 | `contracts/relay-1.md#desired-state-snapshot-sections` | `REL-056-valid-generation-apply-atomic-swap` | covered |
| REL-065 | `contracts/relay-1.md#desired-state-snapshot-sections` | `REL-056-valid-generation-apply-atomic-swap` | covered |
| REL-066 | `contracts/relay-1.md#desired-state-snapshot-sections` | `REL-056-valid-generation-apply-atomic-swap`, `REL-123-valid-revocation-enforced-while-disconnected` | covered |
| REL-066a | `contracts/relay-1.md#desired-state-snapshot-sections` | - | TBD-wave1 |
| REL-066b | `contracts/relay-1.md#desired-state-snapshot-sections` | - | TBD-wave1 |
| REL-066c | `contracts/relay-1.md#desired-state-snapshot-sections` | - | TBD-wave1 |
| REL-066d | `contracts/relay-1.md#desired-state-snapshot-sections` | - | TBD-wave1 |
| REL-067 | `contracts/relay-1.md#desired-state-snapshot-sections` | `REL-056-valid-generation-apply-atomic-swap` | covered |
| REL-068 | `contracts/relay-1.md#desired-state-snapshot-sections` | `REL-056-valid-generation-apply-atomic-swap` | covered |
| REL-070 | `contracts/relay-1.md#idempotent-apply--enrollment-anchored-trust` | `REL-070-valid-generation-reapply-idempotent-noop` | covered |
| REL-071 | `contracts/relay-1.md#idempotent-apply--enrollment-anchored-trust` | `REL-071-invalid-wrong-peer-key-snapshot-rejected` | covered |
| REL-072 | `contracts/relay-1.md#idempotent-apply--enrollment-anchored-trust` | `REL-071-invalid-wrong-peer-key-snapshot-rejected` | covered |
| REL-073 | `contracts/relay-1.md#idempotent-apply--enrollment-anchored-trust` | - | TBD-wave1 |
| REL-074 | `contracts/relay-1.md#idempotent-apply--enrollment-anchored-trust` | - | TBD-wave1 |
| REL-075 | `contracts/relay-1.md#idempotent-apply--enrollment-anchored-trust` | - | TBD-wave1 |
| REL-090 | `contracts/relay-1.md#telemetry-upstream` | `REL-090-valid-telemetry-overflow-loss-marker` | covered |
| REL-090a | `contracts/relay-1.md#telemetry-upstream` | `REL-090-valid-telemetry-overflow-loss-marker`, `REL-090a-valid-entry-without-usable-trace-id-still-delivered` | covered |
| REL-091 | `contracts/relay-1.md#telemetry-upstream` | `REL-090-valid-telemetry-overflow-loss-marker` | covered |
| REL-092 | `contracts/relay-1.md#telemetry-upstream` | `REL-090-valid-telemetry-overflow-loss-marker` | covered |
| REL-093 | `contracts/relay-1.md#telemetry-upstream` | `REL-090-valid-telemetry-overflow-loss-marker` | covered |
| REL-094 | `contracts/relay-1.md#telemetry-upstream` | `REL-094-valid-telemetry-latest-only-heartbeat-superseded` | covered |
| REL-095 | `contracts/relay-1.md#telemetry-upstream` | - | TBD-wave1 |
| REL-096 | `contracts/relay-1.md#telemetry-upstream` | `REL-090-valid-telemetry-overflow-loss-marker` | covered |
| REL-097 | `contracts/relay-1.md#telemetry-upstream` | - | TBD-wave1 |
| REL-100 | `contracts/relay-1.md#loss-marker` | `REL-090-valid-telemetry-overflow-loss-marker` | covered |
| REL-101 | `contracts/relay-1.md#loss-marker` | `REL-090-valid-telemetry-overflow-loss-marker` | covered |
| REL-102 | `contracts/relay-1.md#loss-marker` | - | TBD-wave1 |
| REL-103 | `contracts/relay-1.md#loss-marker` | `REL-090-valid-telemetry-overflow-loss-marker` | covered |
| REL-104 | `contracts/relay-1.md#loss-marker` | `REL-094-valid-telemetry-latest-only-heartbeat-superseded` | covered |
| REL-105 | `contracts/relay-1.md#loss-marker` | `REL-090-valid-telemetry-overflow-loss-marker` | covered |
| REL-110 | `contracts/relay-1.md#device-plane` | `REL-110-valid-device-candidate-and-command` | covered |
| REL-110a | `contracts/relay-1.md#device-plane` | `REL-110-valid-device-candidate-and-command`, `REL-110b-valid-derived-identity-across-relays` | covered |
| REL-110b | `contracts/relay-1.md#device-plane` | `REL-110b-valid-derived-identity-across-relays` | covered |
| REL-110c | `contracts/relay-1.md#device-plane` | - | TBD-wave1 |
| REL-110d | `contracts/relay-1.md#device-plane` | - | TBD-wave1 |
| REL-111 | `contracts/relay-1.md#device-plane` | `REL-110-valid-device-candidate-and-command` | covered |
| REL-111a | `contracts/relay-1.md#device-plane` | `REL-110-valid-device-candidate-and-command`, `REL-110b-valid-derived-identity-across-relays` | covered |
| REL-112 | `contracts/relay-1.md#device-plane` | `REL-110-valid-device-candidate-and-command` | covered |
| REL-113 | `contracts/relay-1.md#device-plane` | `REL-110-valid-device-candidate-and-command` | covered |
| REL-114 | `contracts/relay-1.md#device-plane` | - | TBD-wave1 |
| REL-115 | `contracts/relay-1.md#device-plane` | - | TBD-wave1 |
| REL-120 | `contracts/relay-1.md#player-credential-authority` | - | TBD-wave1 |
| REL-121 | `contracts/relay-1.md#player-credential-authority` | `REL-056-valid-generation-apply-atomic-swap` | covered |
| REL-121a | `contracts/relay-1.md#player-credential-authority` | - | TBD-wave1 |
| REL-121b | `contracts/relay-1.md#player-credential-authority` | `REL-121b-invalid-second-relay-redeems-bound-grant` | covered |
| REL-121c | `contracts/relay-1.md#player-credential-authority` | - | TBD-wave1 |
| REL-122 | `contracts/relay-1.md#player-credential-authority` | - | TBD-wave1 |
| REL-123 | `contracts/relay-1.md#player-credential-authority` (scope note above) | `REL-123-valid-revocation-enforced-while-disconnected` | covered |
| REL-124 | `contracts/relay-1.md#player-credential-authority` | - | TBD-wave1 |
| REL-124a | `contracts/relay-1.md#player-credential-authority` | - | TBD-wave1 |
| REL-124b | `contracts/relay-1.md#player-credential-authority` | - | TBD-wave1 |
| REL-124c | `contracts/relay-1.md#player-credential-authority` | - | TBD-wave1 |
| REL-124d | `contracts/relay-1.md#player-credential-authority` | - | TBD-wave1 |
| REL-125 | `contracts/relay-1.md#player-credential-authority` | - | TBD-wave1 |
| REL-126 | `contracts/relay-1.md#player-credential-authority` | - | TBD-wave1 |
| REL-130 | `contracts/relay-1.md#clock-trust` | `REL-133-valid-clock-hint-bounded` | covered |
| REL-131 | `contracts/relay-1.md#clock-trust` | - | TBD-wave1 |
| REL-132 | `contracts/relay-1.md#clock-trust` | `REL-133-valid-clock-hint-bounded` | covered |
| REL-133 | `contracts/relay-1.md#clock-trust` | `REL-133-valid-clock-hint-bounded` | covered |
| REL-134 | `contracts/relay-1.md#clock-trust` | - | TBD-wave1 |
| REL-135 | `contracts/relay-1.md#clock-trust` | - | TBD-wave1 |
| REL-136 | `contracts/relay-1.md#clock-trust` | `REL-136-valid-coldboot-skew-tolerant-connect` | covered |
| REL-137 | `contracts/relay-1.md#clock-trust` | - | TBD-wave1 |
| REL-140 | `contracts/relay-1.md#gateway-posture` | - | TBD-wave1 |
| REL-141 | `contracts/relay-1.md#gateway-posture` | - | TBD-wave1 |
| REL-142 | `contracts/relay-1.md#gateway-posture` | - | TBD-wave1 |
| REL-142a | `contracts/relay-1.md#gateway-posture` | - | TBD-wave1 |
| REL-143 | `contracts/relay-1.md#gateway-posture` | - | TBD-wave1 |
| REL-150 | `contracts/relay-1.md#multi-relay-identity` | - | TBD-wave1 |
| REL-151 | `contracts/relay-1.md#multi-relay-identity` | - | TBD-wave1 |
| REL-152 | `contracts/relay-1.md#multi-relay-identity` | - | TBD-wave1 |
| REL-153 | `contracts/relay-1.md#multi-relay-identity` | `REL-110b-valid-derived-identity-across-relays` | covered |
| REL-153a | `contracts/relay-1.md#multi-relay-identity` | - | TBD-wave1 |
| REL-153b | `contracts/relay-1.md#multi-relay-identity` | - | TBD-wave1 |
| REL-153c | `contracts/relay-1.md#multi-relay-identity` | - | TBD-wave1 |
