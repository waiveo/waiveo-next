# Conformance Traceability Index

Roll-up of the contract corpus: every contract, its requirement-ID count, its seed-corpus size, its traceability coverage, and its open `draft-note` count. Per-contract detail lives in the sibling `<contract>.md` maps; the requirement text lives in `../../contracts/`.

`covered` = at least one seed corpus case exercises the requirement today. `TBD-wave1` = the requirement has a traceability row but its conformance driver/case lands with the wave that implements it (a sanctioned status, not a gap). A high `TBD-wave1` share is expected: the Wave-0 corpus seeds the load-bearing and adversarial cases; exhaustive per-requirement coverage is Wave-1 driver work.

| Contract | Requirements | Seed cases | covered | TBD-wave1 | Open draft-notes |
|---|---|---|---|---|---|
| manifest/1 | 44 | 5 | 20 | 24 | 1 |
| ctx/1 | 43 | 5 | 8 | 35 | 3 |
| rules/1 | 113 | 25 | 58 | 55 | 6 |
| device-class-registry | 29 | 1 | 18 | 11 | 0 |
| data-model/1 | 59 | 7 | 39 | 20 | 2 |
| api/1 | 70 | 12 | 35 | 35 | 1 |
| events/1 | 73 | 15 | 33 | 40 | 10 |
| archive/1 | 66 | 9 | 23 | 43 | 4 |
| relay/1 | 104 | 14 | 66 | 38 | 2 |
| player/1 | 123 | 7 | 46 | 77 | 9 |
| surface/1 | 50 | 8 | 17 | 33 | 1 |
| channel-index | 48 | 11 | 23 | 25 | 0 |
| marketplace/1 | 48 | 14 | 26 | 22 | 5 |
| ui-schema/1 | 71 | 9 | 22 | 49 | 3 |
| security-model | 80 | 6 | 12 | 68 | 8 |
| **Total** | **1021** | **148** | **446** | **575** | **55** |

**Companion artifacts:**
- `../fixtures/automation-builder/` — the ui-schema/1 go/no-go fixture (a complete declarative automation-builder document + render-walkthrough), gated by `../fixtures/fixture-lint.mjs` (wired into the pr/merge CI tiers; asserts every widget/binding/vocabRef the fixture uses is defined in ui-schema/1).
- `../../docs/capacity-sli-catalog.md` — the published capacity-envelope + SLI catalog (a companion reference doc, not a contract; carries no requirement IDs).

**Status:** 14 of the 15 contracts are `Status: review` — frozen enough for implementation to build against. `data-model/1` remains `Status: draft`: its cross-schedule composition/precedence behavior is still undefined within its own domain (DAT-051, DAT-111), the one genuinely-open scope question left in the corpus. The remaining `draft-note` markers on the `review` contracts are blessed proposed-defaults, measurement-gated cadence values (which resolve on real fleet/bench measurement, not on a decision), or review-only residuals — none blocks `review`; each resolves before its contract advances to `normative`. Two cross-corpus mechanism gaps are tracked as coverage residuals, each contract's own requirement being complete: the pack process/render isolation mechanism's normative owner (`marketplace/1` MKT-024) and the app-side clock-trust floor's owner (`security-model` SEC-062).
