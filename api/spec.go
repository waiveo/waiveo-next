// Package apispec embeds `api/openapi.yaml` — the declared management-API
// surface — so a running server can check a request against the very document
// that declares it.
//
// It exists so there is no second, hand-maintained copy of a declared schema
// inside the server: a copy is a thing that drifts, and a schema the server
// enforces from its own copy stops being the schema the generated clients, the
// docs, and the drift check are all built from. Anything needing the declared
// surface at runtime reads it from here.
package apispec

import _ "embed"

// OpenAPIYAML is `api/openapi.yaml` verbatim. It is the same bytes the codegen
// and lint steps consume, embedded rather than read from disk so a deployed
// binary cannot be separated from the surface it declares.
//
// Callers MUST treat it as read-only; it is package state shared by every
// consumer in the process.
//
//go:embed openapi.yaml
var OpenAPIYAML []byte
