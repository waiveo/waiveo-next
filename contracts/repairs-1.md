# Repairs

**Contract:** repairs/1
**Version:** 1.0
**Status:** draft

## Scope

repairs/1 defines the platform's registry of **typed, user-fixable issues**: what a Repairs issue is, how a detector produces one, how an issue's identity keeps a persistent condition from accumulating duplicates, and how an issue offers a remediation an operator can invoke.

It exists because four other contracts already depend on it. `security-model.md` SEC-111 requires entering the self-signed TLS fallback to "raise a typed Repairs issue"; SEC-113 requires that issue's severity and prominence to ESCALATE on a bounded recurring schedule for as long as the fallback persists; SEC-131 parks a capability-widening pack update behind "a Repairs notice"; and the `system` capability grants access to "Repairs/settings surfaces". Each of those was written against a surface no contract defined, and this contract defines it.

- In scope: the issue record's shape and identity; what a detector is and what it returns; the severity vocabulary and how escalation is expressed; the remediation model; the five core detectors the platform itself ships.
- Out of scope: the `api/1` operations that read issues and invoke remediations (`api/1`'s own conventions govern their shape); the pack-facing `ctx.health.report` verb by which a pack raises an issue of its own (`ctx/1`, a separate concern and a separate wave); the per-component health metrics an operator reads alongside issues (`events/1` EVT-070/071's `box.vitals` and the platform's own health route); the notification transport that carries an owner notify event (a platform notify provider).

## Definitions

- **Issue** — one typed, operator-visible statement that something is wrong and, where possible, what to do about it. An issue is a persisted row, not a log line or an event.
- **Detector** — a named function the platform runs on a schedule, which examines some part of the deployment and returns zero or more issues.
- **Subject** — the specific thing an issue is about: a disk, a pack, a device, a certificate. Two issues from one detector about two subjects are two issues.
- **Remediation** — a named action an issue's TYPE declares, which an operator may invoke to resolve that issue, identified by id and carrying no caller-supplied payload.
- **Severity** — how loudly an issue presents, drawn from a closed vocabulary, and the field SEC-113's escalation moves.

## Normative requirements

### The issue record

**[REP-001]** An issue MUST be a persisted row, not an in-memory value recomputed at boot. An issue's `first_seen_at` is what `security-model.md` SEC-113's escalation is measured from — "for as long as the fallback remains active" is a statement about elapsed time — and a registry rebuilt from scratch on every restart cannot answer how long a condition has persisted. It would reset the escalation of exactly the long-running degraded state SEC-113 exists to make impossible to ignore.

**[REP-002]** An issue MUST carry at minimum `{issue_id, type, subject, severity, first_seen_at, last_seen_at, title, detail}`. `type` names the class of problem and is what a remediation is declared against (REP-020); `subject` names the specific thing it is about (REP-011); `title` and `detail` are operator-facing prose this contract does not constrain beyond requiring them present.

**[REP-003]** An issue MUST NOT carry secret material, and a detector MUST NOT place a credential, token, key, or passphrase into `title`, `detail`, or `subject`. A Repairs issue is read by every principal a Repairs surface admits and is retained for as long as the condition persists, so a secret placed in one is a secret with a wide audience and a long life. Where an issue is ABOUT a credential, it names it — an id, a label, an expiry — never its value.

### Identity and lifecycle

**[REP-010]** A detector MUST be a named function the platform runs on a schedule, returning zero or more issues. It MUST NOT be a callback invoked from the code path that discovers a problem: a detector that only fires when something is executing cannot report a condition that persists while nothing runs, which is the shape of most of the conditions here (a full disk, an expiring certificate, a quarantined pack nobody is invoking).

**[REP-011]** An issue's identity MUST be stable per `(detector, subject)`. A detector re-running against an unchanged condition MUST update the existing issue's `last_seen_at` rather than create a second one.

This is the requirement everything else rests on. Without a stable identity, a detector on a five-minute schedule produces two hundred and eighty-eight issues a day about one full disk; an operator cannot tell a persistent condition from a recurring one; and SEC-113's escalation has nothing to escalate, because every issue is new.

**[REP-012]** A detector that no longer returns an issue for a `(detector, subject)` it previously returned MUST cause that issue to be RESOLVED rather than deleted, and a resolved issue MUST retain its `first_seen_at`. An operator asking "was this happening before?" is asking a question only the retained record answers, and a condition that clears and returns is a different fact from one that never cleared.

**[REP-013]** Severity MUST be one of `info`, `warning`, or `critical`. A detector supplies an issue's initial severity; the platform MAY raise it over time under a policy the issue's own type declares (SEC-113 is one such policy, for the TLS fallback). Severity MUST NOT be lowered automatically — a condition becoming less loud without a human deciding so is how an escalating problem quietly stops being reported.

### Remediation

**[REP-020]** A remediation MUST be declared by an issue's TYPE and invoked by ID. An issue MUST NOT carry an executable payload — no shell command, no URL to fetch, no script — and a Repairs surface MUST NOT invoke anything an issue's own row names.

The distinction is the whole of this requirement's value. A remediation declared by type is a fixed, reviewable set of actions the platform implements; a remediation carried by an issue makes the Repairs surface an EXECUTION primitive whose inputs are whatever wrote the row. Those are different products, and only one of them is a diagnosis surface.

**[REP-021]** An issue type MAY declare no remediation. Not every condition is fixable from the platform — a failing disk, a network the operator must reconfigure — and a surface that required one would push implementers toward inventing an action rather than reporting a fact.

**[REP-022]** Invoking a remediation MUST be idempotent with respect to the issue: invoking it against an already-resolved issue MUST NOT re-apply the action, and MUST answer that the issue is resolved. An operator clicking twice, or clicking a stale page, is ordinary.

### The five core detectors

**[REP-030]** The platform MUST ship at least these five detectors. Each is named here with the condition it reports; the thresholds and schedules are implementation choices, and an implementation MUST state the values it chose.

| detector | condition | remediation |
|---|---|---|
| `disk_pressure` | Free space on a path the platform writes to has fallen below its floor. | Declared: the retention sweep. |
| `clock_state` | The deployment's clock is untrusted, or has moved backward past its floor. | None — the fix is external time configuration. |
| `box_vitals` | A relay reports a physical or operational fault in its `box.vitals` (`events/1` EVT-070/071): overheating, throttled, undervolted. | None — the fix is physical. |
| `pack_quarantine` | A pack is quarantined after a crash loop and is not running. | Declared: retry the pack, or revert it to its previous version. |
| `device_control_blocked` | A device refused a control command in a way that names an operator-fixable device setting. | None — the fix is on the device, and the issue carries the exact setting path. |

**[REP-031]** `device_control_blocked` MUST carry, in its `detail`, the exact setting path an operator must change on the device. The condition this detector exists for is a device-side privacy or network setting that silently blocks control — a state indistinguishable, from the platform's side, from a device that is off or unreachable. An issue that says only "the device did not respond" sends an operator to check cables for a problem that is two menus deep in the device's own settings.

**[REP-032]** A detector's own failure MUST NOT be silent. A detector that cannot run — a path it cannot stat, a peer it cannot reach — MUST itself raise an issue saying so rather than returning zero issues, because "no issues" and "the check did not run" are the same answer to an operator and profoundly different facts. This is the same discipline the platform's own gates apply to a check that silently matches nothing.

## Wire shapes

```json
// Issue — one typed, operator-visible problem
{
  "issue_id": "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5",
  "type": "disk_pressure",
  "subject": "/var/lib/waiveo/content",
  "severity": "warning",
  "first_seen_at": 1752537600000,
  "last_seen_at": 1752541200000,
  "resolved_at": null,
  "title": "Content storage is nearly full",
  "detail": "3.1 GiB free of 64 GiB (4.8%). Below the 10% floor.",
  "remediation": { "id": "run_retention_sweep", "label": "Reclaim unreferenced content" }
}
```

`remediation` is present only where the issue's TYPE declares one (REP-020/021), and carries an id and a label — never a command, a path to execute, or a URL to fetch.

## Error taxonomy

| code | meaning | retryable |
|---|---|---|
| `ISSUE_RESOLVED` | A remediation was invoked against an issue that is already resolved (REP-022). Not a failure — the requested end state holds. | no |
| `REMEDIATION_UNKNOWN` | The named remediation is not one this issue's type declares (REP-020). | no — invoke a declared remediation |

## Conformance notes

- Traceability map: `conformance/traceability/repairs-1.md`.
- Corpus: `conformance/corpora/repairs-1/`.
- Every requirement here is `TBD-wave1`: this contract is published as TEXT ahead of its implementation, deliberately, so the surface is reviewed before anything is built against it.
- REP-011's stable identity and REP-012's resolve-don't-delete are the two requirements a corpus case can pin most directly — both are statements about what a second detector run does to an existing row, which a case can present. REP-003 (no secret material) is the opposite: it forbids content, and no case can enumerate what a future detector might place in a string. It is enforceable by review of what the platform's own detectors emit, and is stated here so that review has something to check against.
