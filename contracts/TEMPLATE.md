# <Contract Title>

**Contract:** <slug>/1
**Version:** 1.0
**Status:** draft

<!--
  Door-doc template. To start a new contract:
    1. Copy this file to contracts/<slug>-<major>.md (e.g. contracts/player-1.md).
    2. Replace every <angle-bracket> placeholder, including the header block above.
    3. Delete this comment block and the example rows under "Normative requirements".
    4. Run `node scripts/validate-contracts.mjs` before committing.

  This file is exempt from both the header-format check and the requirement-ID
  rules (see the EXEMPT_NAMES set in scripts/validate-contracts.mjs, alongside
  README.md) — it exists to show the required shape, not to satisfy it. Its
  own **[XXX-NNN]** example anchors below are inert placeholders, not real IDs.
-->

## Scope

One paragraph: what domain this contract governs, and what it explicitly does
not. Push anything out of scope to the contract that does cover it, by name.

- In scope: <bullet list>
- Out of scope: <bullet list — name the contract that owns each item instead>

## Definitions

Terms this contract uses normatively below, defined once so requirement text
can stay terse and unambiguous.

- **<Term>** — <definition>.
- **<Term>** — <definition>.

## Normative requirements

Each requirement is its own paragraph, opening with a unique ID anchor at the
very start of the line: `**[XXX-NNN]**` — three uppercase letters (a stable
prefix for this contract, chosen once and never reused by another contract),
a hyphen, three digits. Follow the anchor immediately with RFC 2119 language
(MUST / MUST NOT / SHOULD / SHOULD NOT / MAY).

IDs are permanent once published: never renumber or reuse a retired one, not
even across a major-version bump. `scripts/validate-contracts.mjs` enforces
that every contract (other than this template and README.md) has at least
one anchor, and that no ID collides with one defined anywhere else in the
corpus.

**[XXX-001]** The <subject> MUST <requirement text>.

**[XXX-002]** The <subject> SHOULD <requirement text>, unless <condition>.

**[XXX-003]** The <subject> MAY <optional behavior>.

## Wire shapes

The concrete on-the-wire representation(s) this contract governs — request
and response bodies, message frames, or file formats. One named, fenced
block per shape; mark every field required or optional (with its default).

```json
// <ShapeName>
{
  "<field>": "<type — required>",
  "<optional_field>": "<type — optional, default <value>>"
}
```

## Negotiation

How peers detect and resolve a version mismatch: what each side advertises
(e.g. `<major>.<minor>` in a handshake field), what happens on a major
mismatch (MUST refuse) versus a minor mismatch (SHOULD negotiate down to the
lower shared minor), and how a minor's deprecation is signaled ahead of
removal.

## Error taxonomy

Every error a conformant implementation of this contract can surface, as a
table, so drivers assert on `code` rather than on message text.

| code | meaning | retryable |
|---|---|---|
| `<ERROR_CODE>` | <what triggers it> | <yes/no> |

## Conformance notes

- Traceability map: `conformance/traceability/<slug>-<major>.md` — maps every
  `XXX-NNN` above to the case(s) that exercise it (format:
  `conformance/traceability/README.md`). Every ID this contract defines
  should have a row there — `TBD-wave1` is a valid status, an ID missing
  entirely is not.
- Corpus: `conformance/corpora/<slug>-<major>/` — one JSON case file per
  `case-id` referenced from the traceability map (envelope format:
  `conformance/corpora/README.md`).
- Call out anything intentionally left uncovered here (e.g. timing-dependent
  behavior a static corpus can't express), with the reason — not silently.
