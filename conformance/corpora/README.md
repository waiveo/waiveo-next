# Corpora

Conformance test vectors, as data — not test code. One directory per
contract, named `<slug>-<major>/` to match the contract's
`**Contract:** <slug>/<major>` header, holding one JSON file per case, named
`<case_id>.json`:

```
conformance/corpora/
  example-1/
    XXX-001-basic.json
    XXX-002-timeout.json
```

Every file is a single JSON envelope:

```json
{
  "case_id": "XXX-001-basic",
  "contract": "example/1",
  "req_ids": ["XXX-001"],
  "description": "One sentence: what this case exercises and why.",
  "input": {},
  "expected": {}
}
```

Fields:

- **case_id** — matches the filename (without `.json`) and a `case-id(s)`
  entry in that contract's traceability map
  (`conformance/traceability/README.md`). Unique across the whole corpus, not
  just within one contract's directory.
- **contract** — the `slug/major` this case belongs to, matching a
  `**Contract:**` header value exactly.
- **req_ids** — every requirement ID (`XXX-NNN`) this case provides coverage
  evidence for. Usually one entry; list more only when a single input/
  expected pair genuinely exercises several requirements at once.
- **description** — human-readable, one sentence: what's being exercised and
  why this case is the one that proves it.
- **input** — the wire-shape payload (see the contract's "Wire shapes"
  section) fed to the implementation under test.
- **expected** — what a conformant implementation must produce for that
  input. Shape mirrors whatever the contract defines as the response/output
  for this kind of input.

Corpora are data only — no executable assertions live here. Per-contract
driver skeletons that consume these files against a running implementation
are a separate, not-yet-built piece of `conformance/`.
