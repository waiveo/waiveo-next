# Conformance Traceability Index

Roll-up of the contract corpus: every contract, its requirement-ID count, its seed-corpus size, its traceability coverage, and its open `draft-note` count. Per-contract detail lives in the sibling `<contract>.md` maps; the requirement text lives in `../../contracts/`.

`covered` = at least one seed corpus case exercises the requirement today AND appears in some conformance driver's driven set (`conformance/driven-manifest.json`, machine-checked by `../../scripts/validate-coverage.mjs` — see `README.md`'s **status** column for the exact bar). `TBD-wave1` = the requirement has a traceability row but its conformance driver/case lands with the wave that implements it (a sanctioned status, not a gap). A high `TBD-wave1` share is expected: the Wave-0 corpus seeds the load-bearing and adversarial cases; exhaustive per-requirement coverage is Wave-1 driver work.

| Contract | Requirements | Seed cases | covered | TBD-wave1 | Open draft-notes |
|---|---|---|---|---|---|
| manifest/1 | 47 | 12 | 20 | 27 | 1 |
| ctx/1 | 43 | 5 | 0 | 43 | 3 |
| rules/1 | 118 | 25 | 58 | 60 | 6 |
| device-class-registry | 29 | 7 | 23 | 6 | 0 |
| data-model/1 | 82 | 32 | 54 | 28 | 0 |
| api/1 | 96 | 17 | 38 | 58 | 1 |
| events/1 | 80 | 23 | 50 | 30 | 12 |
| archive/1 | 67 | 9 | 9 | 58 | 4 |
| relay/1 | 130 | 21 | 75 | 55 | 3 |
| player/1 | 128 | 7 | 46 | 82 | 9 |
| surface/1 | 50 | 8 | 0 | 50 | 1 |
| channel-index | 48 | 11 | 0 | 48 | 0 |
| marketplace/1 | 61 | 27 | 0 | 61 | 4 |
| ui-schema/1 | 82 | 20 | 31 | 51 | 3 |
| security-model | 92 | 13 | 11 | 81 | 7 |
| repairs/1 | 13 | 0 | 0 | 13 | 0 |
| **Total** | **1166** | **237** | **415** | **751** | **54** |

**Companion artifacts:**
- `../fixtures/automation-builder/` — the ui-schema/1 go/no-go fixture (a complete declarative automation-builder document + render-walkthrough), gated two ways, both wired into the pr/merge CI tiers: `../fixtures/fixture-lint.mjs` asserts every widget/binding/vocabRef the fixture uses is defined in ui-schema/1, and `../../web/src/renderer/fixture-automation-builder.test.tsx` renders it against its own `sample-data.json` through the real renderer and asserts the structure it declares actually paints.
- `../../docs/capacity-sli-catalog.md` — the published capacity-envelope + SLI catalog (a companion reference doc, not a contract; carries no requirement IDs).

**Status:** all 15 contracts are `Status: review` — frozen enough for implementation to build against. The remaining `draft-note` markers are blessed proposed-defaults, measurement-gated cadence values (which resolve on real fleet/bench measurement, not on a decision), or review-only residuals — none blocks `review`; each resolves before its contract advances to `normative`. The previously-tracked cross-corpus scope/mechanism gaps are now closed in-contract: schedule composition and precedence (`data-model/1` DAT-051/DAT-053/DAT-111 — an ancestor cascade resolved by a strict priority→specificity→id order, with layered dayparts and fallback; a daypart is a continuous *state* whose membership is DST-correct by construction, DAT-119, and whose one *event*, the preset batch, fires on every effective-daypart rising edge, DAT-075), the pack process/render isolation owner (`marketplace/1` MKT-026 — a host-runtime, per-deployment-tier responsibility), and the app-side clock-trust floor (`security-model` SEC-066–068 — a persisted monotonic floor mirroring the relay's).
