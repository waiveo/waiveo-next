# Pack Manifest

**Contract:** manifest/1
**Version:** 1.0
**Status:** review

## Scope

manifest/1 defines the declarative document every pack ships at its root: identity, host-compatibility, requested capabilities, network egress, resource limits, the pack's own data model, the UI pages and fragments it contributes, the devices/playables/automation it declares, and the server-side actions it exposes. The host reads this document to render consent prompts, provision the pack's runtime sandbox, register its data collections, and wire its UI and automation surfaces — entirely before any pack code runs.

- In scope: manifest field shapes and validation rules; capability/consent structure; egress allowlist declaration; resource-limit declaration; the universal entity envelope every pack-declared collection uses; UI page/fragment declarations; device, playable, and automation contribution shapes; action declarations.
- Out of scope: the pack ↔ host runtime protocol (`ctx/1`); the signature/trust envelope wrapping a sealed pack artifact (`channel-index/1`) and its distribution/marketplace semantics (`marketplace/1`); the declarative UI grammar itself — page types, widgets, binding grammar (`ui-schema/1`); the automation rule vocabulary (`rules/1`); the device capability/class registry (`device-class-registry/1`); the on-the-wire representation of the verbs a manifest's declarations end up dispatching to (`ctx/1`).

## Definitions

- **Pack** — a sealed, signed artifact containing this manifest plus bundled pack-side logic and locale catalogs.
- **Publisher** — a registered signing identity that owns one or more pack namespaces.
- **ULID** — a 26-character Crockford-base32 identifier, lexicographically sortable by creation time, used for every entity ID this contract or its referencing contracts define.
- **Scope node** — a node in the platform's org → site → group → screen tree; every entity a pack declares attaches to one.
- **Capability** — a named, host-enforced permission a pack must declare to use (e.g. device I/O, an egress class, a resource class).
- **Consent tier** — the review level a capability declaration triggers: silently granted, prompted at install, or granted by virtue of the pack's trust channel.
- **Revision** — a monotonically increasing per-entity version counter used for optimistic concurrency and change detection.
- **Version range** — a string constraining which `major.minor` values of a referenced contract are acceptable; grammar given at MAN-013.

## Normative requirements

### Pack identity

**[MAN-001]** The manifest MUST declare `id` as a string of the form `<publisher>/<name>`, where `publisher` and `name` each match `^[a-z][a-z0-9-]{1,38}$`. `id` is the pack's permanent identity; it MUST NOT change across versions of the same pack.

**[MAN-002]** The manifest MUST declare `version` as a three-component dotted version string (`MAJOR.MINOR.PATCH`, digits only per component). Two artifacts published under the same `id` MUST NOT share a `version`.

**[MAN-003]** The manifest MUST declare `displayName` as a `msg:` locale-catalog reference (never a raw string), resolved against the pack's bundled locale catalogs (MAN-111) at render time.

**[MAN-004]** The manifest MAY declare `description` as a `msg:` locale-catalog reference.

### Compatibility declaration

**[MAN-010]** The manifest MUST declare a `compat` object with at least the keys `ctx` and `renderer`. `compat.ctx` MUST be a version-range string (MAN-013) constraining the `ctx/1` minor versions the pack's bundled logic supports. `compat.renderer` MUST be an array of page-type name strings the pack's UI declarations use; every name MUST be present in the host's declarative-renderer page-type registry at install time, or install MUST be refused with a typed compatibility error.

**[MAN-011]** The manifest MAY declare `compat.relay` as a version-range string constraining the relay/1 minor versions required by any `devices` contribution. A pack with no `devices` block MUST omit `compat.relay`.

**[MAN-012]** The manifest MAY declare `compat.features` as an array of feature-flag strings the pack requires the host to support beyond baseline `compat.ctx`. An unrecognized feature flag MUST cause install to refuse with a typed compatibility error naming the unrecognized flag.

**[MAN-013]** A version-range string MUST match the grammar `range := comparator (" " comparator)*`, `comparator := (">=" | ">" | "<=" | "<" | "=") major "." minor`. Ranges are evaluated as a conjunction (AND) of comparators against a contract's `major.minor` value; a bare `major.minor` with no leading operator is shorthand for `=major.minor`.

*draft-note: the version-range grammar above is a proposed default (npm-range-style, restricted to major.minor precision since contracts version at that granularity) — it is not derived from an existing spec grammar. Confirm before this contract leaves draft.*

### Capabilities & consent

**[MAN-020]** The manifest MUST declare `capabilities` as an array of objects, each with required fields `capability` (string, a name from the host's capability registry), `scope` (string, capability-specific — e.g. a collection name, a device-class name, or `*`), and `reason` (a `msg:` locale-catalog reference explaining, in end-user terms, why the pack needs it). An empty array is valid for a pack that needs no capability grants.

**[MAN-021]** Each entry's `capability` value MUST be one the host's capability registry recognizes at install time; an unrecognized capability MUST refuse install with a typed error naming it, never a silent no-op grant.

**[MAN-022]** The host MUST render an install-time consent prompt from `capabilities` unless the pack's trust channel and namespace place it in a pre-consented baseline; even then, a subsequent version whose `capabilities`, `egress`, `resources`, or `connections` fields differ from the previously granted manifest MUST re-prompt for consent on the changed subset, or park the update pending owner acknowledgment.

**[MAN-023]** Consent MUST be evaluated against the semantic diff of the `capabilities`/`egress`/`resources`/`connections` fields between the previously granted manifest and the new one — never against a raw whole-manifest hash — so that changes outside this subset (e.g. a locale-catalog edit) never trigger re-consent.

**[MAN-024]** A `devices` contribution (see Device contributions) is itself a capability-consent-rendered declaration: the host MUST render it in the same consent flow as `capabilities`, describing the device classes and command scope the pack requests LAN access to.

### Egress declaration

**[MAN-030]** The manifest MUST declare `egress` as an array of allowlist entries, each either a bare hostname (`example.com`), a hostname with a wildcard leftmost label (`*.example.com`), or an IP-literal. An empty array declares no network egress. `egress` MUST NOT contain a bare wildcard (`*`) or a CIDR block wider than a single host.

**[MAN-031]** Every entry in `egress` MUST use `https` as its implied scheme; the manifest format carries no per-entry scheme or port field — the runtime allowlist check is host-and-port-class based, not path-based.

**[MAN-032]** `egress` entries are consumed exclusively by the pack's `http` verb family at runtime (`ctx/1`); a manifest declaring `egress` entries but no runtime use of that family is valid (declared-but-unused is not an error).

### Resource limits

**[MAN-040]** The manifest MUST declare `resources` as an object with the fields `memory` (integer, MiB, the pack process's enforced memory ceiling), `cpuWeight` (integer 1–10000, relative scheduling weight, default 100 if omitted), `storageQuota` (integer, MiB, ceiling on the pack's own data-model storage), and `maxScheduledTimers` (integer, ceiling on concurrently registered schedule timers).

**[MAN-041]** `resources.memory` MUST be enforced by the host as a hard ceiling on the pack's runtime process (not merely advisory); a pack exceeding it MUST be terminated and restarted under the same supervision policy as a crash, never silently throttled without termination.

**[MAN-042]** A pack declaring `resources.memory` below a host-defined minimum floor MUST fail manifest validation at install time (the floor value is host configuration, not part of this contract).

### Data model declaration

**[MAN-050]** The manifest MUST declare `dataModel.version` as a positive integer, incremented whenever any declared collection's shape changes in a way existing rows cannot satisfy unmodified.

**[MAN-051]** The manifest MUST declare `dataModel.collections` as an array of collection objects, each with a `name` (string, unique within the pack) and a `fields` array. Every row of every declared collection carries the **universal entity envelope** in addition to its declared fields: `entity_id` (ULID, assigned by the host at creation, immutable), `revision` (integer, incremented by the host on every write), `lifecycle_state` (one of `draft`, `published`, `archived`), `scope_node` (ULID reference, required), `labels` (array of strings, may be empty), `template_ref` (optional ULID reference to a rule-template instantiation source), `params` (optional object, present only when `template_ref` is present).

**[MAN-052]** A field declaration MAY carry the annotation `role: title` or `role: summary` (at most one field per collection may declare `role: title`), the boolean annotation `searchable`, and the annotation `lifecycle: draft-publish` marking the collection as participating in the `lifecycle_state` draft/publish flow (a collection without this annotation MUST treat every row as `published` and reject writes of any other `lifecycle_state`).

**[MAN-053]** The manifest MAY declare `dataModel.migrations` as an ordered array of migration steps, each naming the `dataModel.version` it upgrades from and to. The host executes migrations declaratively (never pack code) and MUST snapshot the pack's data before applying one. An update whose `dataModel.version` is lower than the currently installed version MUST fail validation.

**[MAN-054]** The manifest MUST declare `retention` as an object keyed by collection name, each value one of `unbounded` or a bounded retention descriptor (`{maxAge}` in days, or `{maxRows}`). A collection with no `retention` entry defaults to `unbounded`, subject to the pack's `resources.storageQuota`.

**[MAN-055]** The manifest MAY declare `connections` as an array of objects `{provider, authType, scopes}` naming external service connections the pack's logic resolves at runtime via the `connections` verb family (`ctx/1`); a `provider`/`authType` pair not registered with the host MUST fail validation at install time.

### UI page declarations

**[MAN-060]** The manifest MUST declare `ui.pages` as an array of page objects, each with `path` (string, unique within the pack, used to compose the pack's route), `pageType` (string, MUST be a member of `compat.renderer`), and `titleMsg` (a `msg:` reference).

**[MAN-061]** A page object MAY declare `fragment: card` with a `sizeHint` (one of `small`, `medium`, `large`), marking it as contributable into another pack's declared slot rather than a standalone route.

**[MAN-062]** The manifest MAY declare `ui.slots` as an array of named slot points `{name, accepts}` (`accepts` an array of page-type strings) that this pack's own pages expose for other packs' `fragment: card` pages to bind into. A binding attempt of a page type not listed in `accepts` MUST fail at render-registration time.

**[MAN-063]** The manifest MAY declare `ui.surfaces` as an array of surface-declaration objects, each `{name, entry}` — `name` a pack-unique identifier for the surface, `entry` the path of exactly one bundled frontend entry point the surface mounts from. A pack that ships a surface (`surface/1` SUR-001) MUST declare it here, and a mount resolves a declared surface to that single `entry`; a declaration whose `entry` names no file in the pack's bundle MUST fail manifest validation. A pack shipping no surface omits `ui.surfaces`.

### Device contributions

**[MAN-070]** The manifest MAY declare `devices` as an array of device-class contribution objects, each `{deviceClass, match, capabilities}` where `deviceClass` names an entry in the host's device-class registry, `match` is an array of discovery-match patterns, and `capabilities` lists the command/entity capabilities requested for that class.

**[MAN-071]** Every `match` pattern MUST use one of the forms `{ssdp: <search-target>}`, `{mdns: <service-type>}`, or `{macOui: <6-hex-digit prefix>}`; an unrecognized form MUST fail manifest validation.

**[MAN-072]** A pack with a non-empty `devices` array MUST declare `compat.relay` (MAN-011).

### Playable contributions

**[MAN-080]** The manifest MAY declare `contributes.playable` as an object `{contentType, durationSemantics, renderHints, contentId}` where `contentType` is one of the content-type enum members `image`, `video`, `html_bundle`, `stream`, `composed` (defined normatively elsewhere as the platform's screen-content model), `durationSemantics` is one of `fixed`, `source-driven`, or `operator-set`, `renderHints` is an opaque object passed through to the render pipeline unvalidated by this contract, and `contentId` is the pack's own identifier scheme for the playable content it exposes (opaque string, unique within the pack).

**[MAN-081]** When `durationSemantics` is `fixed`, the object MUST also declare `durationSeconds` (a positive number); when `source-driven` or `operator-set`, `durationSeconds` MUST be absent.

### Automation contributions

**[MAN-090]** The manifest MAY declare `contributes.automation.events` as an array of `{name, payloadSchema}` — durable events this pack's runtime logic emits, available as automation trigger sources. `name` MUST be unique within the pack and MUST be namespaced as `<pack-id>.<local-name>`.

**[MAN-091]** The manifest MAY declare `contributes.automation.actions` as an array of `{name, fieldsSchema, execution}` where `execution` is one of `relay-command` or `app-service`. Every entry here MUST also appear in `actions` (MAN-100) under the same `name` — `contributes.automation.actions` is the automation-facing view of a subset of the pack's declared actions, not a second registry.

**[MAN-092]** The manifest MAY declare `contributes.automation.triggers` as an array of `{name, msg, matches, paramsSchema}` — named, human-labeled automation trigger macros. `matches` MUST reference a device-class capability and a state or event pattern drawn from the device-class registry; a trigger macro whose `matches` cannot be resolved against a state/event pattern on a relay-polled capability, or against a `contributes.automation.events` entry of this same pack, MUST fail manifest validation.

### Actions

**[MAN-100]** The manifest MUST declare `actions` as an array of objects `{name, paramsSchema, capabilityScope, auditClass, idempotencyClass, automationCallable}`, where `name` is unique within the pack, `paramsSchema` is a JSON Schema object describing accepted parameters, `capabilityScope` names the capability (from `capabilities`) required to invoke it, `auditClass` is one of `read`, `write`, or `privileged`, `idempotencyClass` is one of `safe-to-retry` or `not-idempotent` (MAN-103), and `automationCallable` is a boolean (default `false`).

**[MAN-101]** Every declared action is auto-surfaced by the host at `/api/v1/packs/{pack}/actions/{name}` — its management-API route; the manifest itself carries no route or transport detail beyond `name`. An action invoked via that route, or as an `execution: app-service` automation action (MAN-091), is dispatched to the pack's action handler via `ctx/1` (`actions.invoke`, CTX-110); MAN-104 states the exception for an `execution: relay-command` action invoked by automation.

**[MAN-102]** A `paramsSchema` field MAY use the type `asset-ref` for a parameter representing uploaded binary content; the host performs the upload-to-content-store step itself and passes the pack a content-addressed reference — a pack's action handler MUST NOT receive raw upload bytes through any other params field type.

**[MAN-103]** Every action MUST declare an idempotency class, `safe-to-retry` or `not-idempotent`, alongside `auditClass`; a `not-idempotent` action MUST NOT be automatically replayed by the host's retry or job-recovery machinery.

**[MAN-104]** When automation invokes an action declared `execution: relay-command` (`contributes.automation.actions`, MAN-091), the relay executes it directly as a device command; it MUST NOT be dispatched to the pack's `ctx/1` action handler (MAN-101) for that invocation. An `execution: app-service` action invoked by automation, and any action invoked via the management API, both dispatch through MAN-101 as normal.

### Reserved sections

**[MAN-110]** The manifest MAY declare `drivers`, `sources`, and `diagnostics` as empty or absent in this version; a host implementing manifest/1 MUST accept their absence and MUST NOT reject a manifest for omitting them. Their content shape is reserved for a future manifest/1 minor.

### Locale catalogs

**[MAN-111]** Every `msg:` reference (e.g. `msg:pack.title`) MUST resolve against a JSON file bundled at `messages/<locale>.json` inside the pack artifact, keyed by the reference's suffix. A pack MUST bundle at least `messages/en.json`; a key missing from a non-default locale catalog falls back to the default locale rather than failing render.

## Wire shapes

```json
// PackManifest
{
  "id": "acme/weather-widget",
  "version": "1.2.0",
  "displayName": "msg:pack.displayName",
  "description": "msg:pack.description",
  "compat": {
    "ctx": ">=1.0 <2.0",
    "relay": ">=1.0 <2.0",
    "renderer": ["list-detail", "settings-form", "dashboard"],
    "features": []
  },
  "capabilities": [
    { "capability": "device.read", "scope": "media-player", "reason": "msg:cap.deviceRead" },
    { "capability": "egress.http", "scope": "*", "reason": "msg:cap.egress" }
  ],
  "egress": ["api.weather.example"],
  "resources": {
    "memory": 96,
    "cpuWeight": 100,
    "storageQuota": 64,
    "maxScheduledTimers": 4
  },
  "dataModel": {
    "version": 1,
    "collections": [
      {
        "name": "forecasts",
        "fields": [
          { "name": "location", "type": "string", "role": "title", "searchable": true },
          { "name": "summary", "type": "string", "role": "summary" }
        ]
      }
    ],
    "migrations": []
  },
  "retention": {
    "forecasts": { "maxAge": 30 }
  },
  "connections": [
    { "provider": "weather-api", "authType": "api-key", "scopes": [] }
  ],
  "ui": {
    "pages": [
      { "path": "forecast", "pageType": "dashboard", "titleMsg": "msg:page.forecast.title" }
    ],
    "slots": []
  },
  "devices": [
    { "deviceClass": "media-player", "match": [{ "ssdp": "urn:roku-com:device:player:1" }], "capabilities": ["device.read"] }
  ],
  "contributes": {
    "playable": {
      "contentType": "html_bundle",
      "durationSemantics": "fixed",
      "durationSeconds": 15,
      "renderHints": {},
      "contentId": "forecast-strip"
    },
    "automation": {
      "events": [{ "name": "acme/weather-widget.forecast_updated", "payloadSchema": { "type": "object" } }],
      "actions": [{ "name": "refresh", "fieldsSchema": { "type": "object" }, "execution": "app-service" }],
      "triggers": []
    }
  },
  "actions": [
    { "name": "refresh", "paramsSchema": { "type": "object" }, "capabilityScope": "egress.http", "auditClass": "write", "idempotencyClass": "safe-to-retry", "automationCallable": true }
  ],
  "drivers": [],
  "sources": [],
  "diagnostics": []
}
```

```json
// CapabilityEntry
{
  "capability": "device.read",
  "scope": "media-player",
  "reason": "msg:cap.deviceRead"
}
```

```json
// UniversalEntityEnvelope (present on every row of every declared collection, beside its declared fields)
{
  "entity_id": "01J8Z3K4N5P6Q7R8S9T0V1W2X3",
  "revision": 4,
  "lifecycle_state": "published",
  "scope_node": "01J8Z2Q1M8H8N4T0V1W2X3Y4Z5",
  "labels": ["seasonal", "region-west"],
  "template_ref": null,
  "params": null
}
```

```json
// DeviceContribution
{
  "deviceClass": "media-player",
  "match": [{ "ssdp": "urn:roku-com:device:player:1" }],
  "capabilities": ["device.read"]
}
```

```json
// ActionDeclaration
{
  "name": "refresh",
  "paramsSchema": { "type": "object", "properties": {} },
  "capabilityScope": "egress.http",
  "auditClass": "write",
  "idempotencyClass": "safe-to-retry",
  "automationCallable": true
}
```

```json
// ValidationResult (what a conformant validator produces for a PackManifest input)
{
  "valid": false,
  "errors": [
    { "code": "UNKNOWN_CAPABILITY", "field": "capabilities[0].capability", "message": "capability \"nonexistent.thing\" is not in the host capability registry" }
  ]
}
```

## Negotiation

manifest/1 has no live handshake of its own — negotiation happens at install time and at host-upgrade time, driven by the `compat` block (MAN-010–013):

- At install, the host evaluates `compat.ctx` (and `compat.relay`, if present) against the `major.minor` versions it currently implements. A **major** mismatch — no host-implemented major version satisfies the range — MUST refuse install. A **minor** mismatch where at least one host-implemented minor satisfies the range MUST proceed; the pack runs against whatever minor is negotiated at connection time (`ctx/1` hello/negotiate).
- `compat.renderer` and `compat.features` are checked the same way against the host's page-type registry and feature-flag set; an unrecognized entry in either refuses install (MAN-010, MAN-012).
- On a host upgrade that removes a major version, every installed pack whose `compat.ctx` no longer intersects any host-implemented major is flagged for typed operator-facing surfacing rather than silently disabled; it stops running until updated — the runtime-layer refusal itself is `ctx/1`'s (hello/negotiate), manifest/1's role ends at declaring the range.

## Error taxonomy

| code | meaning | retryable |
|---|---|---|
| `MANIFEST_SCHEMA_INVALID` | The manifest document does not parse or is missing a required field. | no |
| `UNKNOWN_CAPABILITY` | A `capabilities[].capability` value is not in the host's capability registry. | no |
| `UNKNOWN_PAGE_TYPE` | A `compat.renderer` or `ui.pages[].pageType` value is not in the host's page-type registry. | no |
| `UNKNOWN_FEATURE_FLAG` | A `compat.features` entry is not recognized by the host. | no |
| `COMPAT_RANGE_UNSATISFIED` | No host-implemented `major.minor` of the referenced contract satisfies the declared range. | no |
| `EGRESS_ENTRY_INVALID` | An `egress` entry is not a bare host, wildcard-leftmost host, or IP-literal, or is a disallowed wildcard/CIDR. | no |
| `RESOURCE_BELOW_FLOOR` | `resources.memory` (or another resource field) is below the host-configured minimum. | no |
| `UNKNOWN_DEVICE_MATCH_FORM` | A `devices[].match` entry does not match one of the three recognized forms. | no |
| `TRIGGER_UNRESOLVABLE` | A `contributes.automation.triggers[].matches` cannot be resolved against the device-class registry or the pack's own declared events. | no |
| `ACTION_NAME_DUPLICATE` | Two entries in `actions` share a `name`. | no |
| `ACTION_IDEMPOTENCY_CLASS_INVALID` | An `actions[]` entry omits `idempotencyClass`, or its value is not `safe-to-retry`/`not-idempotent`. | no |
| `DATAMODEL_VERSION_REGRESSION` | An update declares a `dataModel.version` lower than the currently installed version. | no |

## Conformance notes

- Traceability map: `conformance/traceability/manifest-1.md` — maps every `MAN-NNN` above to the case(s) that exercise it.
- Corpus: `conformance/corpora/manifest-1/` — one JSON case file per `case-id` referenced from the traceability map.
- The host's capability registry, page-type registry, and device-class registry are host/companion-document configuration, not enumerated by this contract; corpus cases use a small fixed fixture registry documented beside the corpus rather than the real (independently evolving) registries.
- Consent-prompt rendering — the UI a `capabilities`/`egress`/`resources`/`connections` diff produces — is out of scope; only the data feeding it (MAN-020–024) is normative here.
