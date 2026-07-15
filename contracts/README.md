# Contracts

Each contract is a versioned document; the header format and requirement-ID
rules are enforced by `scripts/validate-contracts.mjs`. Start a new contract
from [`TEMPLATE.md`](TEMPLATE.md) — it shows the required shape: the
`**Contract:**`, `**Version:**`, `**Status:**` header lines directly under
the title, followed by Scope, Definitions, ID'd normative requirements, wire
shapes, negotiation, error taxonomy, and conformance notes. Contract
documents are the sole normative source for their domain.
