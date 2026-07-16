# Conformance Traceability Index

Roll-up of the contract corpus: every contract, its requirement-ID count, its seed-corpus size, its traceability coverage, and its open `draft-note` count. Per-contract detail lives in the sibling `<contract>.md` maps; the requirement text lives in `../../contracts/`.

`covered` = at least one seed corpus case exercises the requirement today. `TBD-wave1` = the requirement has a traceability row but its conformance driver/case lands with the wave that implements it (a sanctioned status, not a gap). A high `TBD-wave1` share is expected: the Wave-0 corpus seeds the load-bearing and adversarial cases; exhaustive per-requirement coverage is Wave-1 driver work.

| Contract | Requirements | Seed cases | covered | TBD-wave1 | Open draft-notes |
|---|---|---|---|---|---|
| manifest/1 | 43 | 5 | 20 | 23 | 1 |
| ctx/1 | 42 | 5 | 8 | 34 | 2 |
| rules/1 | 113 | 25 | 58 | 55 | 6 |
| device-class-registry | 29 | 1 | 18 | 11 | 1 |
| api/1 | 52 | 9 | 25 | 27 | 1 |
| events/1 | 71 | 15 | 33 | 38 | 10 |
| archive/1 | 66 | 9 | 23 | 43 | 4 |
| relay/1 | 103 | 14 | 66 | 37 | 4 |
| player/1 | 120 | 7 | 46 | 74 | 12 |
| surface/1 | 50 | 8 | 17 | 33 | 3 |
| channel-index | 45 | 10 | 22 | 23 | 1 |
| ui-schema/1 | 65 | 9 | 22 | 43 | 3 |
| security-model | 80 | 6 | 12 | 68 | 8 |
| **Total** | **879** | **123** | **370** | **509** | **56** |

**Companion artifacts:**
- `../fixtures/automation-builder/` — the ui-schema/1 go/no-go fixture (a complete declarative automation-builder document + render-walkthrough), gated by `../fixtures/fixture-lint.mjs` (wired into the pr/merge CI tiers; asserts every widget/binding/vocabRef the fixture uses is defined in ui-schema/1).
- `../../docs/capacity-sli-catalog.md` — the published capacity-envelope + SLI catalog (a companion reference doc, not a contract; carries no requirement IDs).

**Status:** every contract is `Status: draft`. The 56 open `draft-note` markers are the pending decisions/confirmations to resolve before any contract advances to `normative`.
