# Device Class Registry

**Contract:** device-class-registry/1
**Version:** 1.0
**Status:** review

## Scope

device-class-registry/1 defines the format every device class's registry entry MUST satisfy: its closed state vocabulary, its named semantic groups over that vocabulary, its typed attribute declarations, and its command vocabulary. This is the data `rules/1`'s compiler and matching function resolve a `device_class`, `attribute`, or `command` reference against, and the data a device contribution in a pack's manifest (`manifest/1`) names. The registry is a versioned data document consumed by those compilers and functions — it is never executable code. This contract also normatively pins the complete initial content of every built-in device class the platform ships; the built-in `media-player` class's content is defined below (The media-player class).

- In scope: the registry's overall structure and the distinction between built-in and extension-registered entries; a class entry's state-vocabulary, semantic-group, typed-attribute, and command-vocabulary field grammars; the precedence and no-shadowing rules governing extension registration, at both the class-identifier level and the semantic-group level; the built-in `media-player` class's complete initial content.
- Out of scope: the automation vocabulary that consumes a `device_class`, `attribute`, or `command` reference, and the entity/`EntityRef` model itself (`rules/1`); the pack-manifest fields that declare a device contribution or resolve an automation trigger macro against this registry (`manifest/1`); the pack↔host runtime verb a pack's own logic uses to register an extension class or group at runtime (`ctx/1`); the device-plane wire transport that dispatches a resolved command to a physical device, and the driver implementation mapping a command or attribute to a specific device's native protocol (`relay/1`); device-row identity, discovery matching, and per-device credential storage (a separate concern); operator-facing surfacing of a device-plane health condition (`ctx/1`'s `health.report` and related, a separate concern).

## Definitions

- **Entity** — as defined in `rules/1`: a device-plane object exposing a canonical `state` string and a set of typed attributes, addressed by `entity_id`. Every entity belongs to exactly one device class.
- **Device class** — a named category of entity. This contract defines the format of the data a device class publishes: its state vocabulary, semantic groups, typed attribute declarations, and command vocabulary.
- **Registry** — the complete collection of device class entries a host resolves a `device_class` reference against; a single data document (Wire shapes).
- **Class entry** — one device class's own published data: its `origin`, `states`, `unknown_state_fallback`, `semantic_groups`, `attributes`, and `commands` (Wire shapes, `ClassEntry`).
- **Origin** — whether a class entry, or a semantic group registered onto one, was shipped by the platform (`built-in`) or registered afterward by an extension (`extension-registered`).
- **Canonical state** — the single state-vocabulary member that best describes an entity at a point in time; the value `rules/1` compares against a trigger or condition's `from`/`to`.
- **Semantic group** — a named set of a device class's own canonical state values, declared mutually equivalent for scalar `from`/`to` matching purposes (Semantic groups).
- **Typed attribute declaration** — a device class's description of one attribute an entity of that class MAY carry: its name, value type, unit, nullability, and change-emission class (Typed attribute declarations).
- **Change-emission class** — a typed attribute's declared participation in an entity observation's aggregate any-attribute-changed signal (`rules/1` RUL-330); a `significant` attribute contributes to that signal, a `cosmetic` one is excluded from it (Typed attribute declarations).
- **Command declaration** — a device class's description of one command an entity of that class accepts: its name and typed parameter list (Command vocabulary).
- **Snake identifier** — a string matching `^[a-z][a-z0-9_]*$`: lowercase ASCII letters, digits, and underscores, starting with a letter. Used for every state, semantic-group, attribute, and command name this contract defines.
- **Class identifier** — a string matching `^[a-z][a-z0-9-]*$`: lowercase ASCII letters, digits, and hyphens, starting with a letter — the same grammar `manifest/1` uses for a pack's own `id` (MAN-001), and the exact form `rules/1`'s `device_class` filter carries on the wire (RUL-010).

## Normative requirements

### Registry model

**[REG-001]** The registry MUST be represented as a structured data document, never as executable code; every field this contract defines is data for a consumer — `rules/1`'s compiler and matching function, chiefly — to interpret under this contract's rules, not an instruction for that consumer to execute.

**[REG-002]** The registry's top-level shape MUST be an object carrying a `device_classes` key, mapping each device class's Class identifier to that class's ClassEntry (Wire shapes).

### Device class identity

**[REG-010]** A device class's key in `device_classes` MUST be a Class identifier, and MUST be unique within the registry.

**[REG-011]** A ClassEntry MUST declare `origin` as exactly one of `built-in` or `extension-registered`.

**[REG-012]** A registration attempt for a new extension-registered class whose identifier already exists in the registry — whether the existing entry is `built-in` or itself `extension-registered` — MUST be rejected outright; the existing entry MUST NOT be shadowed, overridden, or merged with the rejected attempt's content.

### State vocabulary

**[REG-020]** A ClassEntry MUST declare `states` as a non-empty array of unique Snake identifiers — the class's complete, closed set of canonical states.

**[REG-021]** A driver that classifies an entity's raw device-reported value into this class's canonical `state` MUST implement that classification as a whitelist: each explicitly recognized raw value maps to its designated member of `states`, and every other raw value — including one the driver has never seen before — MUST map to the class's `unknown_state_fallback` (REG-022). A classifier MUST NOT be implemented as a blacklist that maps a set of known-bad raw values away from a permissive default state.

**[REG-022]** A ClassEntry MUST declare `unknown_state_fallback` as one member of its own `states` — the canonical state a driver classification (REG-021) resolves to for a raw value it does not recognize.

### Semantic groups

**[REG-030]** A ClassEntry MUST declare `semantic_groups` as an object (possibly empty) mapping each group's Snake identifier name to a non-empty array of that group's member states. Every member value MUST be present in the same ClassEntry's own `states` (REG-020); a group referencing a value outside its class's state vocabulary MUST fail validation.

**[REG-031]** A group name MUST be unique for its class regardless of origin: a registration attempt for a group name that already exists for that class — whether the existing group is built-in or itself extension-registered — MUST be rejected outright, never merged into the existing group's membership and never permitted to redefine it. An extension author who needs a related-but-different set of states MUST register it under a new, distinctly named group.

**[REG-032]** The state-matching algorithm that consumes semantic groups (`rules/1` RUL-321) MUST consult a class's built-in groups before any of its extension-registered groups, in that order. Per REG-031's uniqueness rule no group at either origin can already share another's name, but this ordering is restated here as the registry-side half of RUL-321's own requirement.

**[REG-033]** Semantic group membership MUST be evaluated live by every consumer at match time, never snapshotted at compiler or engine start — a group registered after a consumer starts MUST be visible to that consumer's very next match. This is the registry-side obligation `rules/1` RUL-322 depends on.

### Typed attribute declarations

**[REG-040]** A ClassEntry MUST declare `attributes` as an array (possibly empty) of typed attribute declarations, each `{name, type, unit, nullable, change_emission}`. `name` MUST be a Snake identifier, unique within the class's `attributes`.

**[REG-041]** `type` MUST be exactly one of `string`, `number`, `boolean`, `enum`. When `type` is `enum`, the declaration MUST also carry `values`, a non-empty array of unique strings enumerating the attribute's valid values; when `type` is anything else, `values` MUST be absent. This is the type `rules/1`'s typed value comparison (RUL-260) and its static type-mismatch validation (RUL-261) resolve against.

**[REG-042]** `unit` MAY be declared only when `type` is `number`; a declaration with any other `type` MUST omit `unit` or carry it as `null`. When present, `unit` is a free-form string naming the value's physical or measurement unit; this contract does not enumerate a closed unit vocabulary.

**[REG-043]** `nullable` is a boolean, default `false` when absent, stating whether the attribute's value MAY be absent/`null` on an otherwise-populated entity of this class.

**[REG-044]** `change_emission` MUST be exactly one of `significant` or `cosmetic` (Change-emission class). A change to a `significant` attribute's value alone, with the entity's canonical state string unchanged, MUST cause that observation to report an attribute-only change; a change to a `cosmetic` attribute's value alone, with the canonical state string unchanged, MUST NOT.

**[REG-045]** `change_emission` governs only the aggregate any-attribute-changed signal REG-044 describes. An attribute-scoped trigger or condition naming a specific attribute directly (`rules/1` RUL-022/RUL-023) MUST evaluate that attribute's own value transition regardless of its declared `change_emission` — a `cosmetic` classification MUST NOT be used to suppress a match a rule author explicitly scoped to that attribute by name. This registry is the canonical source of `change_emission`: `rules/1`'s aggregate any-attribute-changed signal reads this classification directly rather than restating it (`rules/1` RUL-330), so the two contracts cannot diverge on which attribute changes contribute to that signal.

### Command vocabulary

**[REG-050]** A ClassEntry MUST declare `commands` as an array (possibly empty) of command declarations, each `{name, params}`. `name` MUST be a Snake identifier, unique within the class's `commands`.

**[REG-051]** `params` MUST be an array — empty when the command takes none, never omitted — of `{name, type, required}`, where `name` is a Snake identifier unique within that command's own `params`, `type` uses the same closed vocabulary as Typed attribute declarations (REG-041, including the `enum`/`values` extension), and `required` is a boolean.

**[REG-052]** A `device_command` action's `command` value (`rules/1` RUL-160) MUST resolve to a `name` declared in the target entity's device class's `commands`; an unresolved command name MUST fail as a compile-time validation error, never as a silent no-op or a runtime-only failure.

**[REG-053]** The mapping from a declared command (and its supplied `params`) to the operation actually issued to a physical device is driver-specific and out of scope for this contract (`relay/1`); this contract fixes only the command's name and typed parameter shape.

### The media-player class

**[REG-060]** The registry MUST include a built-in ClassEntry whose Class identifier is `media-player`.

**[REG-061]** The `media-player` class's `states` MUST be exactly the following:

| state | meaning |
|---|---|
| `on` | Powered on; no foreground content is running and no idle surface has taken over — the class's powered-on baseline. |
| `playing` | Powered on; foreground content is actively presenting. |
| `paused` | Powered on; foreground content is loaded but playback is suspended. |
| `buffering` | Powered on; foreground content is loading or stalled, before or during playback. |
| `idle` | Powered on; an idle/attract surface (e.g. a screensaver) has taken over the foreground in place of any running content. |
| `off` | Not powered on. |
| `standby` | A driver-distinguishable low-power condition short of `off` — for a driver that can tell "not fully off" apart from "fully off," rather than collapsing both to `off`. |
| `unavailable` | The driver could not reach the entity's device at all, so none of the above could be observed. |

**[REG-062]** The `media-player` class's `unknown_state_fallback` MUST be `off` (State vocabulary, REG-021/REG-022): a raw driver value this class's driver does not recognize resolves to `off`, never to `on` or any other member.

**[REG-063]** The `media-player` class's `semantic_groups` MUST be exactly:

| group | members |
|---|---|
| `on` | `on`, `playing`, `idle`, `paused`, `buffering` |
| `off` | `off`, `standby`, `unavailable` |

**[REG-064]** The `media-player` class's `attributes` MUST be exactly:

| name | type | change_emission | meaning |
|---|---|---|---|
| `active_app` | `string`, nullable | `significant` | Display label of the foregrounded app/channel, or the powered-on baseline surface's own label when nothing is foregrounded. |
| `active_app_id` | `string`, nullable | `significant` | The foregrounded app/channel's driver-assigned identifier — what an automation compares against a known identifier to tell the platform's own player app apart from any other app. |
| `app_type` | `enum` (`app`, `screensaver`, `home`, `menu`), nullable | `significant` | The foreground surface's category, as the driver classifies it — separates a running app from the idle/screensaver surface and from the powered-on-baseline surface. |
| `power_mode` | `string`, nullable | `significant` | The device's raw, driver-reported power sub-mode, ahead of classification into a canonical `state` — the distinction between a genuinely full power-on and a low-power sub-mode a driver's polling can still observe. Classifying this raw value into `state` is itself subject to State vocabulary's whitelist requirement (REG-021). |
| `is_screensaver` | `boolean` | `significant` | Whether the idle/attract surface is presently the foreground content. |
| `app_version` | `string`, nullable | `cosmetic` | The foregrounded app's own version string, where the driver can read one. |

**[REG-065]** The `active_app_id` and `app_type` attributes (REG-064) MUST jointly be sufficient to express, via an attribute-scoped trigger or condition (`rules/1` RUL-022), the distinction between the platform's own player app foregrounded, a different app foregrounded, and no app foregrounded — no additional derived attribute is declared for this purpose.

**[REG-066]** The `media-player` class's `commands` MUST be exactly:

| name | params | meaning |
|---|---|---|
| `launch` | `channel` (`string`, required) | Foreground the named app/channel. |
| `home` | none | Return to the powered-on-baseline surface. |
| `keypress` | `key` (`string`, required) | Send one discrete remote-control key event; the accepted key vocabulary is driver-defined. |
| `power` | `state` (`enum` — `on`, `off`, required) | Set the device's gross power state. |

## Wire shapes

```json
// ClassEntry (the media-player built-in entry)
{
  "origin": "built-in",
  "states": ["on", "playing", "paused", "buffering", "idle", "off", "standby", "unavailable"],
  "unknown_state_fallback": "off",
  "semantic_groups": {
    "on": ["on", "playing", "idle", "paused", "buffering"],
    "off": ["off", "standby", "unavailable"]
  },
  "attributes": [
    { "name": "active_app", "type": "string", "nullable": true, "change_emission": "significant" },
    { "name": "active_app_id", "type": "string", "nullable": true, "change_emission": "significant" },
    { "name": "app_type", "type": "enum", "values": ["app", "screensaver", "home", "menu"], "nullable": true, "change_emission": "significant" },
    { "name": "power_mode", "type": "string", "nullable": true, "change_emission": "significant" },
    { "name": "is_screensaver", "type": "boolean", "nullable": false, "change_emission": "significant" },
    { "name": "app_version", "type": "string", "nullable": true, "change_emission": "cosmetic" }
  ],
  "commands": [
    { "name": "launch", "params": [{ "name": "channel", "type": "string", "required": true }] },
    { "name": "home", "params": [] },
    { "name": "keypress", "params": [{ "name": "key", "type": "string", "required": true }] },
    { "name": "power", "params": [{ "name": "state", "type": "enum", "values": ["on", "off"], "required": true }] }
  ]
}
```

```json
// Registry (top-level document shape, one entry shown)
{
  "device_classes": {
    "media-player": { "...": "a ClassEntry, shown above" }
  }
}
```

## Negotiation

The registry has no live wire handshake of its own; `rules/1`'s compiler and matching function, and any other host-side consumer resolving a `device_class` reference, read it directly. Compatibility is expressed at two independent levels:

- **This contract's own schema** — the field grammar a ClassEntry, semantic group, typed attribute declaration, or command declaration MUST satisfy (Normative requirements, above The media-player class). Adding an optional field, or a new closed `type`/`change_emission` value, is a device-class-registry/1 minor. Removing a field, narrowing an existing field's accepted shape, or changing an existing field's evaluation semantics is a major.
- **A built-in class's own pinned content** (The media-player class) — adding a new state, semantic group, attribute, or command to a built-in class is a device-class-registry/1 minor, mirroring `rules/1`'s own closed-vocabulary growth policy (RUL-001: growth is exclusively a minor-version change, never a silent extension). Removing, renaming, or redefining one of a built-in class's existing members is a major.

An extension-registered class, or an extension-registered semantic group added to an existing class, carries no version tag of its own beyond the registering pack's own manifest version (`manifest/1`); the registry stores it as live data (State vocabulary, Semantic groups), not as content this contract pins.

## Error taxonomy

| code | meaning | retryable |
|---|---|---|
| `DEVICE_CLASS_UNKNOWN` | A `device_class` reference does not resolve to any entry in `device_classes`. | no |
| `CLASS_IDENTIFIER_INVALID` | A class's key in `device_classes` is not a valid Class identifier. | no |
| `SNAKE_IDENTIFIER_INVALID` | A state, semantic-group, attribute, command, or command-param name is not a valid Snake identifier (REG-020/030/040/050/051). | no |
| `CLASS_IDENTIFIER_COLLISION` | An extension-registered class's identifier collides with an existing entry's identifier (REG-012). | no |
| `ORIGIN_INVALID` | A ClassEntry's `origin` is neither `built-in` nor `extension-registered` (REG-011). | no |
| `STATE_LIST_EMPTY` | A ClassEntry declares zero `states`. | no |
| `UNKNOWN_STATE_FALLBACK_INVALID` | `unknown_state_fallback` is missing, or is not a member of the same ClassEntry's own `states`. | no |
| `GROUP_MEMBER_NOT_IN_VOCABULARY` | A `semantic_groups` entry's member value is not present in the same ClassEntry's own `states` (REG-030). | no |
| `GROUP_NAME_COLLISION` | A group registration's name already exists for that class (REG-031). | no |
| `ATTRIBUTE_NAME_COLLISION` | Two entries in the same ClassEntry's `attributes` share a `name`. | no |
| `ATTRIBUTE_TYPE_INVALID` | An attribute's `type` is outside the closed vocabulary (REG-041), `type: "enum"` is declared without a non-empty `values`, or `values` is present with any other `type`. | no |
| `CHANGE_EMISSION_INVALID` | An attribute's `change_emission` is neither `significant` nor `cosmetic` (REG-044). | no |
| `ATTRIBUTE_UNIT_NOT_APPLICABLE` | `unit` is declared on an attribute whose `type` is not `number` (REG-042). | no |
| `COMMAND_NAME_COLLISION` | Two entries in the same ClassEntry's `commands` share a `name`. | no |
| `COMMAND_UNKNOWN` | A `device_command` action's `command` does not resolve to any entry in the target entity's class's `commands` (REG-052). | no |
| `COMMAND_PARAM_TYPE_INVALID` | A command's declared param uses a `type` outside the closed vocabulary, or misuses `values` the same way `ATTRIBUTE_TYPE_INVALID` does. | no |

## Conformance notes

- Traceability map: `conformance/traceability/device-class-registry.md` — maps every `REG-NNN` above to the case(s) that exercise it.
- Corpus: `conformance/corpora/device-class-registry/` — one case today, the built-in `media-player` class's complete entry, validated against this contract's own structural rules end to end.
- Cross-contract alignment: this corpus's `media-player` `semantic_groups` are authored to match `rules/1`'s own RUL-321 corpus fixture exactly (`conformance/corpora/rules-1/RUL-321-scalar-expands-array-exact.json`). Neither corpus imports the other's file; the two are kept in lockstep by hand, the same way `rules/1`'s own conformance notes describe its fixture registry as "a small fixed fixture registry documented beside the corpus."
- An extension-registered class's or group's actual runtime content is, by definition, not enumerable by this contract; corpus cases for that path exercise only the structural acceptance/rejection rules above (Error taxonomy), never a specific third-party pack's real vocabulary — the same convention `manifest/1`'s and `rules/1`'s corpus notes establish.
