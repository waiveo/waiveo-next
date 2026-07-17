# Conformance Traceability Index

Roll-up of the contract corpus: every contract, its requirement-ID count, its seed-corpus size, its traceability coverage, and its open `draft-note` count. Per-contract detail lives in the sibling `<contract>.md` maps; the requirement text lives in `../../contracts/`.

`covered` = at least one seed corpus case exercises the requirement today. `TBD-wave1` = the requirement has a traceability row but its conformance driver/case lands with the wave that implements it (a sanctioned status, not a gap). A high `TBD-wave1` share is expected: the Wave-0 corpus seeds the load-bearing and adversarial cases; exhaustive per-requirement coverage is Wave-1 driver work.

| Contract | Requirements | Seed cases | covered | TBD-wave1 | Open draft-notes |
|---|---|---|---|---|---|
| manifest/1 | 44 | 5 | 20 | 24 | 1 |
| ctx/1 | 43 | 5 | 8 | 35 | 3 |
| rules/1 | 113 | 25 | 58 | 55 | 6 |
| device-class-registry | 29 | 1 | 18 | 11 | 0 |
| data-model/1 | 63 | 16 | 48 | 15 | 0 |
| api/1 | 70 | 12 | 35 | 35 | 1 |
| events/1 | 73 | 15 | 33 | 40 | 10 |
| archive/1 | 66 | 9 | 23 | 43 | 4 |
| relay/1 | 104 | 14 | 66 | 38 | 2 |
| player/1 | 123 | 7 | 46 | 77 | 9 |
| surface/1 | 50 | 8 | 17 | 33 | 1 |
| channel-index | 48 | 11 | 23 | 25 | 0 |
| marketplace/1 | 49 | 14 | 26 | 23 | 4 |
| ui-schema/1 | 71 | 9 | 22 | 49 | 3 |
| security-model | 83 | 8 | 15 | 68 | 7 |
| **Total** | **1029** | **159** | **458** | **571** | **51** |

**Companion artifacts:**
- `../fixtures/automation-builder/` — the ui-schema/1 go/no-go fixture (a complete declarative automation-builder document + render-walkthrough), gated by `../fixtures/fixture-lint.mjs` (wired into the pr/merge CI tiers; asserts every widget/binding/vocabRef the fixture uses is defined in ui-schema/1).
- `../../docs/capacity-sli-catalog.md` — the published capacity-envelope + SLI catalog (a companion reference doc, not a contract; carries no requirement IDs).

**Status:** all 15 contracts are `Status: review` — frozen enough for implementation to build against. The remaining `draft-note` markers are blessed proposed-defaults, measurement-gated cadence values (which resolve on real fleet/bench measurement, not on a decision), or review-only residuals — none blocks `review`; each resolves before its contract advances to `normative`. The previously-tracked cross-corpus scope/mechanism gaps are now closed in-contract: schedule composition and precedence (`data-model/1` DAT-051/DAT-053/DAT-111 — an ancestor cascade resolved by a strict priority→specificity→id order, with layered dayparts and fallback), the pack process/render isolation owner (`marketplace/1` MKT-026 — a host-runtime, per-deployment-tier responsibility), and the app-side clock-trust floor (`security-model` SEC-066–068 — a persisted monotonic floor mirroring the relay's).
