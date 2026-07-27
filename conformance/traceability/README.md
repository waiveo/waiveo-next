# Traceability maps

One file per contract, named `<slug>-<major>.md` to match the contract's
`**Contract:** <slug>/<major>` header (e.g. `player-1.md` traces
`contracts/player-1.md`). Each map is a single markdown table linking every
requirement ID the contract defines to the conformance case(s) that exercise
it:

| req-id | contract §anchor | case-id(s) | status |
|---|---|---|---|
| XXX-001 | `contracts/example-1.md#normative-requirements` | XXX-001-basic | covered |
| XXX-002 | `contracts/example-1.md#normative-requirements` | - | TBD-wave1 |

Columns:

- **req-id** — a requirement ID exactly as it appears in the contract's
  `**[XXX-NNN]**` anchor. `scripts/validate-contracts.mjs` fails the build if
  a req-id here isn't defined in some `contracts/*.md` file.
- **contract §anchor** — the contract file and, ideally, a heading-anchor
  fragment pinpointing where the requirement lives: `<file>#<heading-slug>`.
- **case-id(s)** — comma-separated `case_id` value(s) from
  `conformance/corpora/` (envelope format: `conformance/corpora/README.md`)
  that exercise this requirement, or `-` when `status` is `TBD-wave1`.
- **status** — one of exactly two values:
  - `covered` — at least one listed case is a real, frozen corpus case AND
    appears in some conformance driver's DRIVEN set
    (`conformance/driven-manifest.json`) — the bar is EXECUTION, not passing:
    a case a driver drives and FAILs still counts (e.g. api/1's API-111 is
    driven-but-FAILING by design). A case that merely exists in the corpus
    but that no driver actually replays against a live implementation does
    NOT earn `covered`.
  - `TBD-wave1` — coverage is deliberately deferred; the requirement exists
    but no case exercises it yet.

Every requirement a contract defines should get a row here, even if its
status is `TBD-wave1` — an ID with no row at all is undertracked, not merely
unimplemented. Both directions of that are machine-checked by
`scripts/validate-contracts.mjs`: a row's req-id must resolve to a real
requirement defined in the contract this file maps to (forward, check #4),
and every requirement ID a contract defines must have >=1 row in its own
traceability map (reverse, check #5). `scripts/validate-contracts.mjs` does
NOT check the **case-id(s)** or **status** cell contents — that is
`scripts/validate-coverage.mjs`'s job (wired into pr-tier/merge-tier/nightly
right after validate-contracts): it fails the build if a row cites a
case-id with no matching file under `conformance/corpora/`, if a `covered`
row's cases are all absent from every driver's driven set (the row must
downgrade to `TBD-wave1` until a driver actually runs one of them), if a
corpus envelope's own fields (`case_id`, `contract`, `req_ids`) drift from
the contract or filename they claim to match, or if
`conformance/traceability/INDEX.md`'s roll-up numbers disagree with the
real per-contract counts. A row citing a case-id that doesn't exist, or
claiming `covered` on a stale or mismatched case list, is no longer
unenforced — the validator catches both.

This file (`README.md`) documents the format only; it carries no real rows
and is exempt from the "req-id must exist" check the same way
`contracts/README.md` and `contracts/TEMPLATE.md` are exempt from the
contract header and requirement-ID rules.
