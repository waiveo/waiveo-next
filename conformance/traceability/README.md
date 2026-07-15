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
  - `covered` — at least one listed case exercises this requirement today.
  - `TBD-wave1` — coverage is deliberately deferred; the requirement exists
    but no case exercises it yet.

Every requirement a contract defines should get a row here, even if its
status is `TBD-wave1` — an ID with no row at all is undertracked, not merely
unimplemented. That completeness isn't machine-checked yet; only the reverse
direction is (a row's req-id must resolve to a real requirement somewhere in
the corpus) — closing that gap is future work, not assumed here.

This file (`README.md`) documents the format only; it carries no real rows
and is exempt from the "req-id must exist" check the same way
`contracts/README.md` and `contracts/TEMPLATE.md` are exempt from the
contract header and requirement-ID rules.
