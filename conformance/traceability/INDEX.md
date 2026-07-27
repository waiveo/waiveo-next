# Conformance Traceability Index

Roll-up of the contract corpus: every contract, its requirement-ID count, its seed-corpus size, its traceability coverage, and its open `draft-note` count. Per-contract detail lives in the sibling `<contract>.md` maps; the requirement text lives in `../../contracts/`.

`covered` = at least one seed corpus case exercises the requirement today AND appears in some conformance driver's driven set (`conformance/driven-manifest.json`, machine-checked by `../../scripts/validate-coverage.mjs` — see `README.md`'s **status** column for the exact bar). `TBD-wave1` = the requirement has a traceability row but its conformance driver/case lands with the wave that implements it (a sanctioned status, not a gap). A high `TBD-wave1` share is expected: the Wave-0 corpus seeds the load-bearing and adversarial cases; exhaustive per-requirement coverage is Wave-1 driver work.

| Contract | Requirements | Seed cases | covered | TBD-wave1 | Open draft-notes |
|---|---|---|---|---|---|
| manifest/1 | 44 | 10 | 20 | 24 | 1 |
| ctx/1 | 43 | 5 | 0 | 43 | 3 |
| rules/1 | 113 | 25 | 58 | 55 | 6 |
| device-class-registry | 29 | 7 | 23 | 6 | 0 |
| data-model/1 | 66 | 20 | 49 | 17 | 0 |
| api/1 | 72 | 14 | 34 | 38 | 1 |
| events/1 | 73 | 15 | 37 | 36 | 10 |
| archive/1 | 66 | 9 | 0 | 66 | 4 |
| relay/1 | 106 | 17 | 68 | 38 | 3 |
| player/1 | 126 | 7 | 46 | 80 | 9 |
| surface/1 | 50 | 8 | 0 | 50 | 1 |
| channel-index | 48 | 11 | 0 | 48 | 0 |
| marketplace/1 | 49 | 14 | 0 | 49 | 4 |
| ui-schema/1 | 73 | 9 | 22 | 51 | 3 |
| security-model | 83 | 8 | 0 | 83 | 7 |
| **Total** | **1041** | **179** | **357** | **684** | **52** |

**Companion artifacts:**
- `../fixtures/automation-builder/` — the ui-schema/1 go/no-go fixture (a complete declarative automation-builder document + render-walkthrough), gated by `../fixtures/fixture-lint.mjs` (wired into the pr/merge CI tiers; asserts every widget/binding/vocabRef the fixture uses is defined in ui-schema/1).
- `../../docs/capacity-sli-catalog.md` — the published capacity-envelope + SLI catalog (a companion reference doc, not a contract; carries no requirement IDs).

**Status:** all 15 contracts are `Status: review` — frozen enough for implementation to build against. The remaining `draft-note` markers are blessed proposed-defaults, measurement-gated cadence values (which resolve on real fleet/bench measurement, not on a decision), or review-only residuals — none blocks `review`; each resolves before its contract advances to `normative`. The previously-tracked cross-corpus scope/mechanism gaps are now closed in-contract: schedule composition and precedence (`data-model/1` DAT-051/DAT-053/DAT-111 — an ancestor cascade resolved by a strict priority→specificity→id order, with layered dayparts and fallback; a daypart is a continuous *state* whose membership is DST-correct by construction, DAT-119, and whose one *event*, the preset batch, fires on every effective-daypart rising edge, DAT-075), the pack process/render isolation owner (`marketplace/1` MKT-026 — a host-runtime, per-deployment-tier responsibility), and the app-side clock-trust floor (`security-model` SEC-066–068 — a persisted monotonic floor mirroring the relay's).
