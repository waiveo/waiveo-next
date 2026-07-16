# Waiveo Next — Capacity Envelope & SLI Catalog

**Version:** 1.0, effective 2026-07-16. Numbers marked ⚑ re-base on bench measurement or soak before they harden.
**Versioning & ownership:** versions like the other companion docs — v1.0 at publication, minor bumps thereafter; the capacity-lane assertions pin to the doc version, and **a target change is a reviewed version bump of this doc, never a silent test-budget edit**. On publication this doc **owns the numbers** — any earlier design figures (relay RSS, GOMEMLIMIT) are superseded; behaviors it mentions in passing (poll backoff, loss markers, leases) are **defined by their contracts** (relay/1, player/1, manifest/1) — this catalog only cites them.
**Units:** memory in **MiB** (matches GOMEMLIMIT / systemd `MemoryHigh=`/`MemoryMax=` semantics); network throughput in **MB/s** (decimal); event size in bytes.

## Reference deployment classes

| Class | Hardware | Runs |
|---|---|---|
| Relay tier-1 floor ⚑ (working assumption — pending hardware-bench measurement) | Pi Zero 2 W-class: 4×A53, 512 MiB, SD | `waiveo-relay` only (`GOMEMLIMIT` ≈48 MiB) |
| Self-hosted box (reference) | 2 GB-class arm64/amd64 (**measured `MemTotal` ≥1,900 MiB** — real 2 GB boards expose ~1.87–1.95 GiB after firmware/kernel carve-outs), GbE NIC, storage ⚑ SSD-class assumed (SD-class degradation stated per row; final storage posture is a separate, pending decision) | relay + app + derive + packs |

## Capacity envelope (v1 design targets — built and tested against)

| Dimension | v1 design target | Why / constraint | Proven by |
|---|---|---|---|
| Screens per relay | **25 tested**; 100 = documentation-only ceiling ⚑ (>25 enrolled raises a typed Repairs warning; no admission rejection in v1; conformance asserts only 25) | Control-plane only (gateway posture: no content bytes); cost = player/1 sessions + program serving inside 48 MiB | Soak lane w/ virtual players |
| Devices polled | **50 @ 5 s** default interval; offline devices back off to 60 s (backoff behavior defined in relay/1) | Matches legacy ECP cadence; 10 polls/s steady bounded by LAN politeness, not CPU | Soak + conformance |
| Edge-rule evaluations | **100/s sustained, 500/s 10-s burst** ⚑ | Compiled rules over in-memory state; target is headroom, not need | Property + soak lanes |
| Telemetry production | **3.5 ev/s steady envelope, 25 ev/s burst** per relay @ 25 screens; decomposition: heartbeats 25×(1/30 s)=0.83 + content.played 25×(0.033–0.1)=0.83–2.5 + state/automation ≈0.3 | Heartbeats are **liveness, latest-only — not durably queued** | Soak |
| Durable telemetry buffer | **≥7 days @ durable design load (≤2.7 ev/s: everything but heartbeats)**; **512 MiB hard cap**, oldest-shed with loss markers. Arithmetic: 2.7 ev/s × 7 d ≈ 1.63 M events × ~300 B ≈ 490 MiB ≤ cap ✓. **Backlog drain:** bulk path ≥128 ev/s ⇒ full buffer drains ≤4 h after WAN restore | Bounds the proof-of-play claim; drain path is distinct from the live 25 ev/s burst ceiling | Soak/capacity lane, fake-clock compressed fill-to-overflow + loss-marker accounting; power-pull mid-write = torture lane |
| Concurrent asset streams (app CAS → screens) | **25 sustained @ ≥4 MB/s each** (≈100 MB/s ≈ 85% of GbE line rate — deliberate); **50 burst = NIC-bound**: aggregate ~110 MB/s, per-stream floor 2 MB/s, range-resume required. SD-class storage box: ~10 streams @ 2 MB/s, players retry/backoff — degraded, not broken | Screen-direct fetch; CPU trivial; NIC then storage IO are the limits | Soak + player conformance |
| api/v1 latency (on-box) | **reads p95 ≤150 ms / p99 ≤400 ms; writes p95 ≤300 ms** under 10 concurrent UI clients + steady device load | UI feel + CLI/MCP usability on the reference box | Nightly capacity lane |
| Workspace / archive | workspace SQLite ≤1 GiB + CAS ≤20 GiB supported; **archive/1 export max ≥22 GiB** (the portable unit carries ALL tenant state — an exportable box is the invariant); restore drill: seeded 2 GiB archive ≤5 min; max-size ≤20 min @ ~20 MB/s; first-screen-playing ≤5 min after restore | Restore-is-an-install-path; drill numbers are the canary's nightly fixture + the release gate | Canary restore drill |
| Per-pack memory enforcement | `MemoryHigh=112 MiB`, `MemoryMax=128 MiB`, derived `--max-old-space-size≈64 MiB` (bare child 46 + full heap 64 ≈ 110 < High ✓); **per-pack idle target ≈70 MiB typical** | manifest `resources.memory` with systemd teeth | Capacity lane asserts limits actually enforce |
| Resident-pack envelope | **≤512 MiB total**: assumes ~5 required packs idling ≈70 MiB (~350) + 1–2 active at full heap; low-traffic packs idle-exit — the envelope is NOT 5×MemoryMax | See memory budget | Nightly capacity lane |

**Reference-box memory budget (against measured MemTotal ≥1,900 MiB, not nominal 2,048):**
OS+journald ≈250 + app ≤300 + relay ≤64 + resident packs ≤512 + derive idle 0 (socket-activated; **peak ≈600 transient; cost-class clamp = 1 concurrent render on the reference box**) → **≤1,726 MiB peak, ≥150 MiB headroom** (174 actual) — headroom target ≥150 MiB at derive peak. SQLite page cache and web-UI serving live **inside** app ≤300. App gates, unambiguously: **idle RSS soft/burn-in gate ≤200 MiB** (legacy all-in-one monolith idles ~189 MiB; a pure-Node app with packs out-of-process idling above it is a defect) and **sustained cap 300 MiB = the enforced P1**. ⚑ Relay ≤64 MiB and the bare arm64 lite-image idle RSS are pending hardware-bench measurements; if the Zero 2 W busts the budget, the tier-1 floor moves before the number does.

## SLI catalog

**Timestamp rule (one meter, not two):** all time-based SLIs are computed on **relay-stamped event time**, reconstructed after any buffer flush; only **cursor_lag** uses app-arrival time. Availability is app-reconstructed from relay-stamped heartbeats. Clocks for staleness/apply run **only while the target screen is scheduled-on and online** (a screen legitimately off per its power schedule pauses the meter).

| SLI | Definition (measurement point) | P1 (burn-in gate = zero P1s over 14 d) | Burn-in target | Steady-state SLA alert |
|---|---|---|---|---|
| content_staleness | **0 if the screen is playing the newest generation applicable to it; else now − publish ts of the newest unapplied applicable generation** (scheduled-on, online screens) | >15 min | p99 ≤60 s | >5 min sustained 10 min, schedule-suppressed |
| apply_latency | desired-state publish → screen reports playing it; clock runs only while the screen is scheduled-on + online | p95 >5 min for 1 h | p95 ≤30 s | p95 >90 s sustained 30 min |
| cursor_lag | newest relay event ts − newest app-acked ts (arrival time) — **declared-offline backlog drain is exempt**; drain instead asserts the ≤4 h bulk-drain target | >30 min while online (non-drain) | ≤60 s steady | >10 min online |
| render_success_rate | derive jobs ok/attempted, rolling 24 h; **stalled = no job reaches a terminal state while the queue is non-empty for 30 min** | <95 % or stalled | ≥99.5 % | <99 % or queue-depth Repairs |
| screen_online_availability | % of *scheduled-on* time connected+playing (3 missed heartbeats = offline) | screen <95 % over 24 h unexplained | ≥99.5 %/screen over the full 14 d; fleet median computed daily ≥99.9 % | per the alerting spec's suppression/flap/dedup rules |
| api_p95 | as envelope row | 2× target sustained 1 h | within target | 2× target 30 min |
| memory guardrails | relay RSS, app RSS via **systemd cgroup accounting** (soak-lane assertions; adding RSS to box-vitals telemetry would be an additive relay/1 schema change — decided there, not here) | relay >64 MiB or app >300 MiB **sustained 30 min** | within budget | relay >56 MiB (GOMEMLIMIT pressure), app >280 MiB, **sustained 10 min** |

**P1** = any cell in the P1 column, fleet data loss, or a quarantined REQUIRED pack. These thresholds are what "zero P1s" in the production gate *means*.

## Non-promises (on the record)

- **No offline playback guarantee:** the relay caches no content; a screen plays only what its player buffered. Yodeck's ~35-day offline caching is a deliberately unmatched capability — the 7-day figure here is *telemetry retention*, a different claim.
- No cloud-tier numbers: that program writes its own catalog on these SLI definitions.
- Targets are per-site (self-hosted = one relay); nothing here implies multi-relay scaling claims.

## Measurement plan

Each envelope row names its lane; the two ⚑ families land first: (1) hardware bench — Zero 2 W relay idle RSS + bare lite-image baseline (moves the tier-1 floor, not the budget); (2) soak — screens-per-relay and edge-eval ceilings with virtual players. The nightly capacity lane replays this table as assertions pinned to this doc's version.
