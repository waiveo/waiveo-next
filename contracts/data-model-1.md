# Data Model

**Contract:** data-model/1
**Version:** 1.0
**Status:** review

## Scope

data-model/1 defines the platform's core relational data model: the scope-node tree every device, screen, and platform resource attaches to; the IANA timezone and geographic-coordinate columns that make dayparting and sun-based evaluation independent of any evaluating machine's own local clock or locale; and the scheduling core's row schemas — playlist, schedule, daypart, validity-window, fallback, and preset-batch — that express what a screen should be showing, and whether its display should be powered, at any given instant. This is the sole normative source for these row shapes; every other contract that projects one of them over a wire, or reads one of their columns, treats this contract as authoritative for the underlying row's shape and validation.

- In scope: the scope-node tree's `kind` vocabulary, parent/child hierarchy, and referential-integrity rules; the resource-row column baseline every row this contract defines carries; the org node's `account_state` and entitlements-document slot, and the self-hosted one-org/one-site invariant; the `tz`/`lat`/`long` columns and their site-default/override resolution rule; the playlist, schedule, daypart, validity-window, fallback, and preset-batch row schemas; the schedule applicability cascade down the scope-node tree, the strict cross-schedule precedence order, and the per-instant layering of dayparts and fallbacks it produces, terminating in a defined terminal default; the platform-ownership invariant barring a pack-owned scheduler row; the display-power dimension and its projection onto `player/1`'s playback lease; the schedule-side `misfire` default per row kind.
- Out of scope: the automation vocabulary that consumes a scope node's effective timezone, and the `misfire` enum's own values and evaluation semantics (`rules/1`); the compiled desired-state transport, generation numbering, and idempotent-apply mechanics that carry this contract's rows to a relay (`relay/1`); a player's own lease-adoption, rendering, and recovery-suppression behavior (`player/1`); the management-API surface, error shape, optimistic concurrency, pagination, and label-selector grammar that expose these rows over HTTP (`api/1`); the durable-event envelope and schema catalog that reports a change to one of these rows (`events/1`); a pack's own manifest declarations, including its playable-content contribution (`manifest/1`); a device class's own state/attribute/command vocabulary (`device-class-registry`); content resolution, asset signing, and fetch mechanics for a resolved playlist item.

## Definitions

- **ULID** — as defined in `manifest/1`: a 26-character Crockford-base32, time-sortable identifier.
- **Timestamp** — as defined in `rules/1`: an integer number of milliseconds since the Unix epoch (UTC).
- **Scope node** — a node in this contract's own tree (Scope-node tree); the placement every resource row this contract or another contract defines carries, and the subject-placement `events/1`'s durable-event envelope carries (`events/1` EVT-012).
- **Kind** — a scope node's own closed classification: `org`, `site`, `group`, or `screen` (Scope-node tree).
- **Site** — as used in `relay/1`: the scope node a relay is bound to at enrollment. This contract defines the `site`-kind scope node `relay/1`'s own Site definition names.
- **Screen** — as used in `relay/1` and `player/1`: a scope-node-attached display. This contract does not define a screen's own identity, pairing, or rendering behavior; it defines only that a screen, like a device, attaches to a scope node of any kind (Scope-node tree).
- **Device** — as defined in `relay/1`: a physical or virtual thing a relay controls, exposing one or more entities. This contract does not redefine device identity; it defines only that a device attaches to a scope node of any kind.
- **Entity** — as defined in `rules/1`: a device-plane object exposing a canonical state and typed attributes, addressed by `entity_id`. A preset-batch row's command list (Scheduling core: preset batch) targets entities by this same identifier.
- **Account state** — the org node's own closed lifecycle classification (Org node: account state and entitlements).
- **Scheduling core** — collectively, the six row kinds this contract defines: playlist, schedule, daypart, validity window, fallback, and preset batch (a "scene") — the platform-owned rows that express what a scope node's screen(s) should show and whether their displays should be powered.
- **Display power** — the closed `on`/`off`/`blank` dimension a daypart or fallback row carries, stating whether a screen's display should be powered and, if powered, whether it should show content (Display power and the playback lease).

## Normative requirements

### Scope-node tree

**[DAT-001]** A scope node MUST be `{id, kind, parent_id, name, external_id?, labels?, revision, created_at, updated_at}` at minimum (ScopeNode, Wire shapes), plus the additional columns Org node: account state and entitlements and Time as data define. `kind` MUST be exactly one of `org`, `site`, `group`, or `screen` — the platform's complete, closed scope-node kind vocabulary; a value outside it MUST be rejected (`SCOPE_NODE_KIND_INVALID`, Error taxonomy). `group` is the generic grouping kind (for instance, a floor, wing, or department — an "area"), carrying no meaning beyond its place in the tree.

**[DAT-002]** `parent_id` MUST be null if and only if `kind` is `org`; every non-`org` scope node's `parent_id` MUST reference an existing scope node (a violation rejected `SCOPE_NODE_PARENT_INVALID`, Error taxonomy). A conformant scope-node tree MUST contain exactly one `org`-kind node, which MUST be its own root — no node's ancestor chain reaches a second `org`-kind node (a second `org` node rejected `SCOPE_NODE_MULTIPLE_ORG`, Error taxonomy).

**[DAT-003]** A scope node's `kind` MUST be a permitted child of its `parent_id`'s own `kind`, per the closed table below; a creation or re-parenting request violating it MUST be rejected (`SCOPE_NODE_PARENT_INVALID`, Error taxonomy).

| parent kind | permitted child kind(s) |
|---|---|
| *(none — tree root)* | `org` |
| `org` | `site` |
| `site` | `group`, `screen` |
| `group` | `group`, `screen` |
| `screen` | *(none — `screen` is a leaf kind)* |

**[DAT-004]** A device row (`relay/1` Device) and a screen's own identity row (`relay/1`/`player/1` Screen) each carry the ID of the scope node they are placed under, and MAY be placed at a scope node of any `kind` — an `org`, `site`, `group`, or `screen`-kind node alike. Neither this contract nor any other assumes a device or a screen resource is placed only at a `screen`-kind node; a deployment MAY, for instance, place a device directly under a `site`-kind node with no intervening `group`.

### Resource-row baseline

**[DAT-005]** Every row this contract defines — a scope node (Scope-node tree) and every scheduling-core row (Scheduling core: playlist, Scheduling core: schedule, Scheduling core: validity window, Scheduling core: daypart, Scheduling core: fallback, Scheduling core: preset batch) — is a `resource` in `api/1`'s own sense (`api/1` Definitions) and MUST carry `api/1`'s resource-row baseline: an `id` (ULID) unique among rows of its own type, a `revision` (integer, `api/1` API-020) for optimistic concurrency, and MAY carry an `external_id` (`api/1` API-100–104) and `labels` conforming to `api/1`'s label-selector key/value grammar (`api/1` API-042). A preset-batch row's own identity field is `preset_id`, not `id` (Scheduling core: preset batch) — the sole exception, kept for byte-exact continuity with the field name `rules/1` RUL-170 already fixes for a reference to it.

**[DAT-006]** Every row this contract defines other than a scope node itself MUST carry `scope_node`: the `id` of the scope node (Scope-node tree) it is placed under, in the same field name and role `relay/1`'s `site_binding` (`relay/1` REL-036), `events/1`'s durable-event envelope (`events/1` EVT-010/EVT-012), and `api/1`'s own `scope_node` selector term (`api/1` API-044) already use for this identical concept.

**[DAT-007]** A daypart or validity-window row's own `scope_node` MUST equal its owning schedule row's own `scope_node` — carried directly on each row per DAT-006, rather than requiring a consumer to join through `schedule_id` to place it in the tree. This is what lets `relay/1`'s own `schedule` snapshot section carry this scheduling core "keyed by scope node" for every row kind alike (`relay/1` REL-065), not only for the schedule row itself.

**[DAT-008]** `created_at` and `updated_at` MUST each be a Timestamp; `updated_at` MUST be greater than or equal to `created_at` and MUST advance on every write that changes a row's own fields (`revision`'s own increment, `api/1` API-020, is the authoritative change signal — `updated_at` is a convenience column, not a second one).

### Org node: account state and entitlements

**[DAT-010]** An `org`-kind scope node MUST carry `account_state`, exactly one of `trial`, `active`, `suspended`, `closed`, or `purged` (ScopeNode, Wire shapes); a non-`org`-kind scope node MUST NOT carry this field. A create or update request violating either half of this rule MUST be rejected (`SCOPE_NODE_ACCOUNT_STATE_INVALID`, Error taxonomy).

| value | meaning |
|---|---|
| `trial` | Provisioned and operating, under a time- or feature-limited evaluation posture. |
| `active` | Provisioned and operating under this contract's ordinary, unrestricted posture. |
| `suspended` | Provisioned but administratively restricted; distinct from any deletion. |
| `closed` | Deliberately deactivated by its own operator; the workspace's data is retained. |
| `purged` | Terminal: the workspace's data has been destroyed. |

**[DAT-011]** This contract fixes only `account_state`'s closed vocabulary and which single scope node carries it; which capabilities each value gates or restricts elsewhere in the platform is out of this contract's own scope.

**[DAT-012]** A `purged` `account_state` MAY be reached as a consequence of a data-subject delete operation (`api/1` API-121–122, which in turn triggers `security-model.md` SEC-121's key-material destruction); this contract does not itself define that operation's trigger or mechanics, only that `purged` is the terminal member of its own vocabulary a workspace's org node reaches once it has run.

**[DAT-013]** An `org`-kind scope node MUST carry `entitlements`: an object, present (possibly `{}`) whenever `kind` is `org`, and MUST be absent on every non-`org`-kind node; a create or update request violating either half MUST be rejected (`SCOPE_NODE_ENTITLEMENTS_INVALID`, Error taxonomy). This contract reserves the field and fixes only its presence and placement; the entitlements document's own internal schema is defined elsewhere.

**[DAT-014]** A conformant scope-node tree MAY contain exactly one `site`-kind node beneath its single `org`-kind node. This is not a distinct schema, a tier flag, or a special-cased code path — every requirement in this contract applies identically whether an org node has one site beneath it or many; a self-hosted deployment is simply the case where it has exactly one.

### Referential integrity, attachment, and deletion

**[DAT-020]** A scope node carrying at least one other scope node whose `parent_id` references it MUST NOT be deleted; the request MUST be rejected (`SCOPE_NODE_NOT_EMPTY`, Error taxonomy) until every child is first deleted or re-parented.

**[DAT-021]** A scope node referenced by any row's own `scope_node` (DAT-006) — a device, a screen, or a scheduling-core row alike — MUST NOT be deleted while that reference exists; the request MUST be rejected (`SCOPE_NODE_IN_USE`, Error taxonomy) until every such row is first deleted or re-placed under a different scope node.

**[DAT-022]** The tree's single `org`-kind node (DAT-002) MUST NOT be deletable by any ordinary write this contract or `api/1` defines, independent of DAT-020's own child-emptiness test — deleting it would destroy the tree's own root identity, not merely one node in it. A delete request targeting it MUST be rejected (`SCOPE_NODE_ORG_UNDELETABLE`, Error taxonomy).

### Time as data

**[DAT-030]** A scope node MAY carry `tz` (an IANA Time Zone Database identifier, e.g. `America/Denver`), `lat`, and `long` (numbers, WGS84 decimal degrees) — the columns `rules/1`'s time-based and sun-based evaluation reads as "the owning scope node's effective timezone" (`rules/1` RUL-340) and "effective latitude/longitude" (`rules/1` RUL-060), and the columns `relay/1`'s `hello` site binding and `site_effective` snapshot carry for a given site (`relay/1` REL-036, REL-066).

**[DAT-031]** A `site`-kind scope node MUST declare non-null `tz`, `lat`, and `long`; a create or update request leaving any of the three null on a `site`-kind node MUST be rejected (`SCOPE_NODE_GEO_REQUIRED`, Error taxonomy).

**[DAT-032]** A `group`- or `screen`-kind scope node MAY declare non-null `tz`, `lat`, and `long` as its own override; an `org`-kind node MUST NOT declare any of the three (a node with no site-rooted geographic identity of its own).

**[DAT-033]** A scope node's **effective** `tz`/`lat`/`long` — the value any consumer (dayparting, a sun trigger or condition, a relay's own `hello`) reads — MUST be resolved as: the node's own `tz`/`lat`/`long` when it declares them non-null (DAT-032); otherwise, walking `parent_id` toward the root, the nearest ancestor `site`-kind node's own `tz`/`lat`/`long` (DAT-031). All three columns resolve together, from the same node, as one unit — a consumer MUST NOT mix an overriding node's own `tz` with an ancestor site's `lat`/`long`, or vice versa.

**[DAT-034]** DAT-002's tree shape guarantees every `group`- or `screen`-kind node has at least one `site`-kind ancestor, and DAT-031 guarantees that ancestor's `tz`/`lat`/`long` are non-null — so DAT-033's resolution always terminates at an actual scope node's own declared value. This contract defines no further terminal default beyond it: a tree in which resolution cannot terminate is malformed under DAT-002/DAT-031 and MUST be surfaced as a validation error. In particular, a consumer MUST NOT substitute the evaluating machine's own OS locale, OS timezone, or any other box-local setting for an unresolved `tz`/`lat`/`long` — DAT-033 has no fallback path that reaches box-local state at all.

### Scheduling core: playlist

**[DAT-040]** A playlist row MUST be `{id, scope_node, name, items, external_id?, labels?, revision, created_at, updated_at}` (Playlist, Wire shapes) — the Resource-row baseline (DAT-005–008) plus `name` (string) and `items`.

**[DAT-041]** `items` MUST be an array (possibly empty) of `{source, asset_ref?, pack_id?, content_id?, duration_seconds?}`, in play order. `source` MUST be exactly one of `asset` or `playable`. When `source` is `asset`, `asset_ref` MUST be present (a content-addressed `sha256:` URI, the same form `player/1`'s own Content reference uses, `player/1` PLY-083); when `source` is `playable`, `pack_id` and `content_id` MUST both be present, `content_id` naming one pack's own `contributes.playable` entry (`manifest/1` MAN-080). An item MUST NOT declare both an `asset_ref` and a `pack_id`/`content_id` pair, and MUST NOT declare neither.

**[DAT-042]** `duration_seconds`, when present on an `items` entry, overrides whatever duration that item would otherwise resolve to; when absent, an `asset` item's duration is source-driven and a `playable` item's duration follows its own declared `durationSemantics` (`manifest/1` MAN-080) — this contract does not itself resolve either, only defines the override column.

### Scheduling core: schedule

**[DAT-050]** A schedule row MUST be `{id, scope_node, name, fallback_id?, priority?, misfire?, external_id?, labels?, revision, created_at, updated_at}` (Schedule, Wire shapes) — the Resource-row baseline (DAT-005–008) plus `name`, an optional `fallback_id` (referencing a Fallback row's own `id`), an optional `priority` (an integer, default `0`; higher wins — the primary key of the cross-schedule precedence order, DAT-053), and an optional `misfire` (Schedule-side misfire default).

**[DAT-051]** A schedule is **applicable to** a scope node `N` when its own `scope_node` (DAT-006) is `N` itself or any ancestor of `N` on the `parent_id` chain to the org root (Scope-node tree, DAT-002). A node's governing scheduling state (Dayparting evaluation) MUST be resolved from the full set of schedules applicable to it — those attached directly at `N` and those attached at any ancestor — not only from schedules attached directly at `N`. This is the same ancestor-walk cascade shape Time as data already defines for the `tz`/`lat`/`long` columns (DAT-033), applied to schedule attachment: a site-wide base schedule governs every screen beneath it. It differs from DAT-033 in resolution, not in shape — where DAT-033 lets the nearest declaring node simply override, here every applicable schedule composes and their dayparts and fallbacks layer under the strict precedence order (DAT-053). Applicability is evaluated relay-side against `N`'s own effective `tz` (DAT-110), exactly as dayparting itself is.

**[DAT-052]** A schedule row is **in force** at a given instant if it carries no Validity-window row, or if at least one of its Validity-window rows contains that instant (Scheduling core: validity window); a schedule with at least one Validity-window row and none of them containing the current instant is not in force.

**[DAT-053]** When more than one schedule is applicable to a scope node `N` (DAT-051), the applicable schedules are ranked by a strict total **precedence order**, highest first: (1) by `priority` (DAT-050), higher winning; (2) among equal `priority`, by **specificity** — the schedule whose `scope_node` is nearest `N` on the `parent_id` chain (smallest ancestor-distance, `N` itself the nearest) winning; (3) among equal `priority` and equal `scope_node`, by lowest schedule `id` (ULID) winning. Because `id` is unique among schedule rows (DAT-005), the third key never ties, so this is a strict total order over any set of applicable schedules — no two are ever equally ranked, and cross-schedule resolution (Dayparting evaluation) is therefore always deterministic.

### Scheduling core: validity window

**[DAT-060]** A validity-window row MUST be `{id, schedule_id, scope_node, starts_at, ends_at, revision, created_at, updated_at}` (ValidityWindow, Wire shapes) — `schedule_id` referencing its owning Schedule row's own `id`, and `scope_node` per DAT-007.

**[DAT-061]** `starts_at` and `ends_at` MUST each be a Timestamp or `null` — `null` meaning, respectively, no lower or no upper bound. When both are non-null, `ends_at` MUST be strictly greater than `starts_at`; a row violating this MUST be rejected (`VALIDITY_WINDOW_RANGE_INVALID`, Error taxonomy).

**[DAT-062]** A validity-window row carries no `misfire` field (Schedule-side misfire default): a `misfire` policy's applicability is to an occurrence at a trigger's own scheduled instant (`rules/1` RUL-350), and a validity window has none to miss — Schedule in force (DAT-052) is evaluated as a continuous predicate against the current instant, fresh, whenever it is read.

### Scheduling core: daypart

**[DAT-070]** A daypart row MUST be `{id, schedule_id, scope_node, days_of_week, start_time, end_time, display_power, playlist_id?, preset_batch_id?, misfire?, name?, revision, created_at, updated_at}` (Daypart, Wire shapes) — `schedule_id` referencing its owning Schedule row's own `id`, and `scope_node` per DAT-007.

**[DAT-071]** `days_of_week` MUST be a non-empty array of unique integers `0`–`6` (`0` = Sunday). `start_time` and `end_time` MUST each be a local time-of-day string `HH:MM:SS`, the same lexical format `rules/1`'s own `time` trigger uses for a local time-of-day (`rules/1` RUL-040) — reused here as a format only, not a reference to that trigger's own evaluation semantics.

**[DAT-072]** `end_time` less than or equal to `start_time` MUST be interpreted as a window that wraps past local midnight, ending on the following calendar day — for instance, `start_time: "22:00:00"`, `end_time: "06:00:00"` denotes a nightly window from 22:00 to 06:00.

**[DAT-073]** Two daypart rows belonging to the same `schedule_id` MUST NOT overlap — a create or update request that would produce an overlap MUST be rejected (`DAYPART_OVERLAP`, Error taxonomy). Overlap is defined over each daypart's full `(weekday, time-of-day)` coverage, evaluated in the schedule's own effective `tz` (all of a schedule's dayparts share one `scope_node`, DAT-007, hence one effective `tz`, DAT-033). Each daypart expands into a set of half-open `(weekday, [from, until))` segments — the `until` boundary exclusive — as follows: a non-wrapping daypart (`end_time` greater than `start_time`) expands to `{ (d, [start_time, end_time)) : d ∈ days_of_week }`; a daypart that wraps past local midnight (`end_time` less than or equal to `start_time`, DAT-072) expands to `{ (d, [start_time, 24:00:00)) : d ∈ days_of_week } ∪ { ((d + 1) mod 7, [00:00:00, end_time)) : d ∈ days_of_week }`, its post-midnight tail carried onto the following weekday. Two dayparts of the same `schedule_id` overlap, and MUST be rejected, if and only if some segment of the one and some segment of the other share a weekday **and** have intersecting half-open time-of-day ranges. Because a wrapping daypart's tail is placed on `(d + 1) mod 7`, a wrap whose tail runs into a range a sibling daypart holds on that next weekday is an overlap even when the two dayparts' `days_of_week` arrays are disjoint. A schedule's dayparts thus partition time, they do not layer over one another.

**[DAT-074]** `display_power` MUST be exactly one of `on`, `off`, or `blank` (Display power and the playback lease) — the state a screen's display should be in while this daypart holds.

**[DAT-075]** `playlist_id`, when present, MUST reference a Playlist row's own `id` (Scheduling core: playlist) — the content this daypart shows while `display_power` is `on`. `preset_batch_id`, when present, MUST reference a Preset-batch row's own `preset_id` (Scheduling core: preset batch) — invoked once at the instant this daypart becomes the currently-holding daypart for its `scope_node`.

*Note: this version intentionally leaves `playlist_id` presence unconstrained against `display_power` — an `off`/`blank` daypart may carry a `playlist_id` and an `on` daypart may omit one; no validation rule ties the two fields together.*

**[DAT-076]** A daypart row's own effective `misfire` (Schedule-side misfire default), when it does not declare one itself, is its owning schedule's own `misfire`; when neither declares one, it is `catch_up_once`.

**[DAT-077]** Because a schedule's own dayparts never overlap (DAT-073), each schedule has **at most one** currently holding daypart (DAT-110) at any instant `t`. This single-holding-daypart-per-schedule property is the precondition that makes cross-schedule resolution well-defined: when several schedules are applicable to a node (DAT-051), each contributes at most one candidate daypart at `t`, and the node-level winner among those candidates is chosen by the precedence order (DAT-053, DAT-111) — never an ambiguous within-schedule contest of a schedule against itself.

### Scheduling core: fallback

**[DAT-080]** A fallback row MUST be `{id, scope_node, name, display_power, playlist_id?, external_id?, labels?, revision, created_at, updated_at}` (Fallback, Wire shapes) — the Resource-row baseline (DAT-005–008) plus `name`, `display_power` (Display power and the playback lease), and an optional `playlist_id` referencing a Playlist row's own `id`.

**[DAT-081]** A fallback row is what a `scope_node` resolves to whenever no daypart is currently holding for it (Dayparting evaluation, DAT-111) — the resolution-order default beneath its schedules' dayparts. Where more than one schedule is applicable to the node (DAT-051), which schedule's `fallback_id` supplies that fallback is the layered selection DAT-117 defines; where exactly one schedule is applicable, it is simply that schedule's own `fallback_id`. A fallback is never a fetch-failure substitute for content a player already holds but cannot refresh, which is `player/1`'s own, distinct, player-local concern (`player/1` PLY-087).

**[DAT-082]** A fallback row carries no `misfire` field, for the same reason a validity-window row does not (DAT-062): it is a resolution-order default, not a scheduled occurrence.

### Scheduling core: preset batch

**[DAT-090]** A preset-batch row MUST be `{preset_id, scope_node, name, commands, last_outcome?, external_id?, labels?, revision, created_at, updated_at}` (PresetBatch, Wire shapes). Its own identity field is `preset_id` (Resource-row baseline, DAT-005) — the exact field name a `preset_batch` action's own `preset_id` (`rules/1` RUL-170) resolves against.

**[DAT-091]** `commands` MUST be a non-empty array of `{entity_id, command, params?}` — a row submitted with zero commands MUST be rejected (`PRESET_BATCH_COMMANDS_EMPTY`, Error taxonomy). `entity_id` (Entity) is the target entity, `command` a name drawn from that entity's device class's own command vocabulary, and `params` its typed parameters, resolved against the device-class registry exactly as a `device_command` action's own fields are (`device-class-registry` REG-050–052; `rules/1` RUL-160). This is the "device-command list" `rules/1` RUL-170 defers to this contract.

**[DAT-092]** Every command in `commands` MUST be attempted independently on invocation, and a preset-batch row's own invocation outcome MUST be recorded as `last_outcome: {outcome, results, evaluated_at}` once at least one invocation has occurred (`null` beforehand) — `outcome` exactly one of `complete`, `partial`, or `failed`, byte-exact with `rules/1`'s own preset-batch action outcome vocabulary (`rules/1` RUL-172), and `results` an array of `{target, command, ok, error?}`, one entry per attempted command, in the identical shape `rules/1`'s own PresetBatchOutcome uses (`rules/1` Wire shapes). This contract does not restate RUL-171/RUL-172's own independent-attempt or outcome-classification behavior — a preset-batch row's invocation, wherever it originates (a daypart transition, DAT-075; a `preset_batch` action, `rules/1` RUL-170), is that same behavior, referenced once here rather than redefined per caller.

**[DAT-093]** `last_outcome` records only the most recently completed invocation; this contract defines no durable history of prior invocations as part of the row itself — a durable record of each invocation belongs to `events/1`'s own event catalog, not this row.

**[DAT-094]** A preset-batch row invoked by a daypart transition (DAT-075) has no `misfire` field of its own; its dispatch timing follows the triggering daypart's own effective `misfire` (DAT-076). A preset-batch row invoked by a `rules/1` `preset_batch` action fired from a `time`, `time_pattern`, or `sun` trigger (`rules/1` RUL-170) instead follows that trigger's own declared or defaulted `misfire` (`rules/1` RUL-350–354), entirely unchanged by this contract — that path does not run through this contract's own dayparting evaluation at all (Dayparting evaluation).

### Platform ownership

**[DAT-100]** None of the six scheduling-core row kinds (Scheduling core) carries a field naming a pack as that row's own owner or author — every playlist, schedule, daypart, validity-window, fallback, and preset-batch row is platform-owned. This is distinct from a playlist item's own `pack_id` (DAT-041), which names the pack a specific item's content comes from, not the playlist row's own owner.

**[DAT-101]** A create or update request on any of the six row kinds MUST be rejected outright (`SCHEDULER_ROW_PACK_OWNED`, Error taxonomy) if it supplies any row-level pack-identifying field — for instance, an `owner_pack` or a row-level `pack_id` on a schedule, daypart, validity-window, fallback, or preset-batch row, or on a playlist row itself (as distinct from a `pack_id` legitimately nested inside one of that playlist's own `items` entries, DAT-041). None of this contract's own schemas (Scheduling core) declare a row-level pack-identifying field in the first place; this rule bars a nonconformant write from introducing one.

**[DAT-102]** A pack's sole route to contributing schedulable content is a manifest `contributes.playable` declaration (`manifest/1` MAN-080), consumed only as a Playlist item's `source: "playable"` reference (DAT-041) — a pack never authors, owns, or ships a playlist, schedule, daypart, validity-window, fallback, or preset-batch row itself; there is no pack-owned scheduler.

### Dayparting evaluation

**[DAT-110]** A schedule's currently holding daypart at an instant `t` is the one daypart of that schedule (at most one, DAT-077) whose `days_of_week`/`start_time`/`end_time` (DAT-071–072) contains `t`. That per-schedule holding daypart — and, when several schedules are applicable, the node-level winner among them (DAT-111) — MUST be evaluated relay-side, against the owning scope node's own effective `tz` (Time as data, DAT-033), exactly as `rules/1`'s own time-based and sun-based evaluation is (`rules/1` RUL-340, RUL-060). The schedules considered are those applicable to the node — attached at it or any ancestor (DAT-051) — and in force at `t` (DAT-052); the node-level result is derived relay-side from the scheduling-core rows `relay/1`'s own `schedule` desired-state section carries, keyed by scope node (`relay/1` REL-065).

**[DAT-111]** A scope node `N`'s **currently holding daypart** at instant `t` — the daypart whose `display_power` and content drive `N`'s Lease (Display power and the playback lease, DAT-113–115) — is resolved by layering the schedules applicable to `N` (DAT-051) that are in force at `t` (DAT-052): each such schedule contributes its own holding daypart at `t`, if it has one (DAT-110), and `N`'s winning daypart is the one contributed by the **highest-precedence** such schedule (DAT-053). Layering is evaluated per instant: where the highest-precedence applicable in-force schedule has a holding daypart at `t`, that daypart wins; where it has none at `t`, the next schedule in precedence order that does have one shows through, and so on down the order. Because the precedence order is a strict total order (DAT-053) and each schedule contributes at most one candidate (DAT-077), exactly one winning daypart — or none — is determined at every instant, deterministically.

**[DAT-117]** When no schedule applicable to `N` (DAT-051) and in force at `t` (DAT-052) has a holding daypart at `t` (DAT-111 selects none), `N`'s Lease resolves to a **fallback** (Scheduling core: fallback, DAT-081) rather than a daypart: specifically the fallback named by the `fallback_id` (DAT-050) of the highest-precedence applicable in-force schedule that declares one, taken in the precedence order of DAT-053 and falling through each schedule that declares no `fallback_id`. A schedule declaring no `fallback_id` contributes no fallback and is skipped; the first in precedence order that declares one supplies the resolved fallback. This is the multi-schedule generalization of DAT-081's single-schedule "resolves to its fallback when no daypart holds" — the fallback layer resolves by the same strict precedence order (DAT-053) the daypart layer does. The daypart layer is resolved across **all** applicable in-force schedules before any fallback is considered; a lower-precedence schedule's holding daypart therefore outranks a higher-precedence schedule's fallback, which is reached only when no applicable in-force schedule has a holding daypart at `t` at all.

**[DAT-118]** When no schedule is applicable to `N` and in force at `t` (DAT-051, DAT-052), or every such schedule both lacks a holding daypart at `t` (DAT-111) and declares no `fallback_id` (DAT-117), `N`'s Lease resolves to a **terminal default** of `display: blank` (`player/1` PLY-093) with no content — powered on, showing nothing, distinct from off. The scheduling core asserts this empty-but-defined state itself; it never leaves `N`'s state unresolved and never falls back to any box-local or player-local content the way a player's own fetch-failure handling would (that failure mode is `player/1`'s own distinct concern, `player/1` PLY-087, not this resolution). This mirrors Time as data's own no-box-local-fallback discipline (DAT-034): scheduling resolution always terminates at a state this contract defines.

### Display power and the playback lease

**[DAT-112]** `display_power` (DAT-074, DAT-080) is the platform's own record of whether a screen's display should be powered and, if so, whether it should show content — distinct from, and the source of, what `player/1`'s Lease `display` field (`player/1` PLY-093) projects to a given player.

**[DAT-113]** A currently holding daypart (DAT-110), or a fallback (DAT-081) resolved in its absence, whose `display_power` is `on` projects to a Lease `display` of `content` (`player/1` PLY-093), content sourced from its own `playlist_id` (DAT-075, DAT-080) where present.

**[DAT-114]** A currently holding daypart, or a resolved fallback, whose `display_power` is `blank` projects to a Lease `display` of `blank` (`player/1` PLY-093) — powered on, showing nothing.

**[DAT-115]** A currently holding daypart, or a resolved fallback, whose `display_power` is `off` projects to a Lease `display` of `blank` (`player/1` PLY-093) as well — `player/1`'s own `display` vocabulary has no third, device-power-off value (`player/1` PLY-093 is explicit that `blank` does not itself request any device-level power-off). An actual device-level power-off is realized separately, as a device command (`device-class-registry` REG-066's `power` command, `state: "off"`) dispatched through whatever preset-batch a transition into this daypart invokes (DAT-075) — not through the Lease at all.

**[DAT-116]** `display_power`'s own three-value distinction between `off` and `blank` is preserved as platform state even though both project to the identical Lease `display: "blank"` (DAT-114–115) — this is what lets a consumer distinguish an intentionally powered-down or blanked screen from one that is simply down. `player/1` PLY-155 already suppresses its own screen-liveness recovery for a `blank`-display Lease; `player/1` PLY-156 already suppresses it identically for a screen the display-power schedule most recently commanded `off`, deferring the row shape it honors generically — it does not name this contract. Daypart (DAT-070–076) and Fallback (DAT-080–082) are that deferred row shape.

### Schedule-side misfire default

**[DAT-120]** `misfire` (Scheduling core: schedule, DAT-050; Scheduling core: daypart, DAT-070) MUST be exactly one of `catch_up_once`, `skip`, or `fire_each` — the identical closed vocabulary `rules/1` RUL-350 defines, byte-exact; this contract adds no fourth value and changes none of the three's own meaning (`rules/1` RUL-351–353).

**[DAT-121]** A schedule or daypart row's own `misfire`, absent an explicit declaration, defaults to `catch_up_once` (DAT-076; the recurring-state opposite default `rules/1` RUL-354 reserves to this contract, DAT-122) — on resuming evaluation after any gap (an offline relay, a generation apply spanning the gap, an untrusted clock, `rules/1` RUL-350), a scope node's currently holding daypart (DAT-110) is resolved fresh against the current instant and its `display_power`/content applied immediately, collapsing any number of missed transitions into the single, currently-correct state — never left showing a stale daypart's state until its own next natural boundary.

**[DAT-122]** This default is deliberately the opposite of `rules/1` RUL-354's own default for a `time`, `time_pattern`, or `sun` trigger (`skip`) — RUL-354 itself carves out exactly this exception, reserving a recurring scheduled state's own default as a separate platform concern outside its own vocabulary that may default oppositely. Schedule and daypart rows are that concern; RUL-354's own `skip` default is otherwise unchanged, and continues to govern every `time`/`time_pattern`/`sun` trigger this contract does not itself define a row for — including a `preset_batch` action fired from one (DAT-094).

**[DAT-123]** Validity-window and fallback rows carry no `misfire` field at all (DAT-062, DAT-082); the two-way split above (DAT-121–122) is exhaustive over every row kind this contract defines that could carry one.

## Wire shapes

```json
// ScopeNode — org (the tree root; account_state and entitlements are org-only)
{
  "id": "01JRGNDE171HFE865V215DE1CT",
  "kind": "org",
  "parent_id": null,
  "name": "Acme Signage",
  "account_state": "active",
  "entitlements": {},
  "revision": 3,
  "created_at": 1752537600000,
  "updated_at": 1752537600000
}
```

```json
// ScopeNode — site (tz/lat/long required and non-null)
{
  "id": "01JSTENDE1EWH0AVNH9DN65R6P",
  "kind": "site",
  "parent_id": "01JRGNDE171HFE865V215DE1CT",
  "name": "Denver Flagship",
  "tz": "America/Denver",
  "lat": 39.7392,
  "long": -104.9903,
  "revision": 1,
  "created_at": 1752537600000,
  "updated_at": 1752537600000
}
```

```json
// ScopeNode — screen, overriding its site's tz/lat/long (DAT-032/DAT-033)
{
  "id": "01JSCRNVERRDEF4W6305FATZYD",
  "kind": "screen",
  "parent_id": "01JSTENDE1EWH0AVNH9DN65R6P",
  "name": "Times Square Kiosk",
  "tz": "America/New_York",
  "lat": 40.7128,
  "long": -74.0060,
  "revision": 1,
  "created_at": 1752537600000,
  "updated_at": 1752537600000
}
```

```json
// Playlist
{
  "id": "01JPAYST15TMGDMFGS8KXM40X6",
  "scope_node": "01JGRPNDE1PG2X7R5JQC42EJ5E",
  "name": "Lobby Rotation",
  "items": [
    { "source": "asset", "asset_ref": "sha256:e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85", "duration_seconds": 10 },
    { "source": "playable", "pack_id": "acme-weather", "content_id": "forecast-strip" }
  ],
  "revision": 2,
  "created_at": 1752537600000,
  "updated_at": 1752537600000
}
```

```json
// Schedule
{
  "id": "01JSCHED1S3AR0RGXJVZ9CJD33",
  "scope_node": "01JGRPNDE1PG2X7R5JQC42EJ5E",
  "name": "Weekday Lobby",
  "fallback_id": "01JFABACK169HJDNDGZG35VH20",
  "priority": 0,
  "misfire": "catch_up_once",
  "revision": 1,
  "created_at": 1752537600000,
  "updated_at": 1752537600000
}
```

```json
// ValidityWindow — open-ended (no ends_at)
{
  "id": "01JVAWN14DG8P4FQJAWK0K68G7",
  "schedule_id": "01JSCHED1S3AR0RGXJVZ9CJD33",
  "scope_node": "01JGRPNDE1PG2X7R5JQC42EJ5E",
  "starts_at": 1752537600000,
  "ends_at": null,
  "revision": 1,
  "created_at": 1752537600000,
  "updated_at": 1752537600000
}
```

```json
// Daypart — overnight display_power: off window (wraps midnight, DAT-072)
{
  "id": "01JDAYPART1M33YA35B44FS7F2",
  "schedule_id": "01JSCHED1S3AR0RGXJVZ9CJD33",
  "scope_node": "01JGRPNDE1PG2X7R5JQC42EJ5E",
  "name": "Overnight",
  "days_of_week": [0, 1, 2, 3, 4, 5, 6],
  "start_time": "22:00:00",
  "end_time": "06:00:00",
  "display_power": "off",
  "playlist_id": null,
  "preset_batch_id": "01JPRESET1N8GAWV07492Q9V82",
  "revision": 1,
  "created_at": 1752537600000,
  "updated_at": 1752537600000
}
```

```json
// Fallback
{
  "id": "01JFABACK169HJDNDGZG35VH20",
  "scope_node": "01JGRPNDE1PG2X7R5JQC42EJ5E",
  "name": "Default Lobby Content",
  "display_power": "on",
  "playlist_id": "01JPAYST15TMGDMFGS8KXM40X6",
  "revision": 1,
  "created_at": 1752537600000,
  "updated_at": 1752537600000
}
```

```json
// PresetBatch — a partial-failure last_outcome (vocabulary byte-exact with rules/1 RUL-172)
{
  "preset_id": "01JPRESET1N8GAWV07492Q9V82",
  "scope_node": "01JGRPNDE1PG2X7R5JQC42EJ5E",
  "name": "Power Down Lobby Devices",
  "commands": [
    { "entity_id": "01JENTTYAKQ2PDF6PT9FABT1BN", "command": "power", "params": { "state": "off" } },
    { "entity_id": "01JENTTYBTFHA6R2YECXPKEE1C", "command": "power", "params": { "state": "off" } }
  ],
  "last_outcome": {
    "outcome": "partial",
    "results": [
      { "target": "01JENTTYAKQ2PDF6PT9FABT1BN", "command": "power", "ok": true },
      { "target": "01JENTTYBTFHA6R2YECXPKEE1C", "command": "power", "ok": false, "error": "unreachable" }
    ],
    "evaluated_at": 1752537600000
  },
  "revision": 4,
  "created_at": 1752537600000,
  "updated_at": 1752537600000
}
```

## Negotiation

This contract has no live wire handshake of its own; `api/1`'s CRUD operations and `relay/1`'s desired-state snapshot (`relay/1` REL-065) carry these rows to their consumers, each under those contracts' own version-negotiation mechanics. Compatibility is expressed at this contract's own schema level:

- Adding a new optional column, a new member of a vocabulary this contract itself owns (`kind`, `account_state`, `display_power`, a `PlaylistItem.source` value — `misfire`'s own membership remains `rules/1`'s to grow, RUL-001), or a new scheduling-core row kind is a data-model/1 minor.
- Removing a column, narrowing an existing column's accepted values, changing an existing column's evaluation semantics (Time as data's resolution rule, DAT-052's in-force test, Dayparting evaluation, Display power and the playback lease), or changing the parent-kind table (DAT-003) is a major.

## Error taxonomy

| code | meaning | retryable |
|---|---|---|
| `SCOPE_NODE_KIND_INVALID` | A scope node's `kind` is not one of `org`, `site`, `group`, `screen` (DAT-001). | no |
| `SCOPE_NODE_PARENT_INVALID` | A scope node's `kind` is not a permitted child of its `parent_id`'s own `kind` (DAT-003), or `parent_id` is null on a non-`org` node or non-null on an `org` node (DAT-002). | no |
| `SCOPE_NODE_MULTIPLE_ORG` | A scope-node tree contains more than one `org`-kind node, or a node's ancestor chain reaches a second `org`-kind node (DAT-002). | no |
| `SCOPE_NODE_GEO_REQUIRED` | A `site`-kind scope node's `tz`/`lat`/`long` is missing or null (DAT-031). | no |
| `SCOPE_NODE_ACCOUNT_STATE_INVALID` | `account_state` is missing or invalid on an `org`-kind node, or present on a non-`org`-kind node (DAT-010). | no |
| `SCOPE_NODE_ENTITLEMENTS_INVALID` | `entitlements` is missing on an `org`-kind node, or present on a non-`org`-kind node (DAT-013). | no |
| `SCOPE_NODE_NOT_EMPTY` | Deletion attempted on a scope node with at least one child scope node (DAT-020). | no |
| `SCOPE_NODE_IN_USE` | Deletion attempted on a scope node still referenced by another row's `scope_node` (DAT-021). | no |
| `SCOPE_NODE_ORG_UNDELETABLE` | Deletion attempted on the tree's single `org`-kind node (DAT-022). | no |
| `VALIDITY_WINDOW_RANGE_INVALID` | A validity-window row's `ends_at` is not strictly greater than its `starts_at` when both are non-null (DAT-061). | no |
| `DAYPART_OVERLAP` | Two daypart rows under the same `schedule_id` declare an overlapping range (DAT-073). | no |
| `SCHEDULER_ROW_PACK_OWNED` | A create or update request supplied a row-level pack-identifying field on a scheduling-core row (DAT-101). | no |
| `PRESET_BATCH_COMMANDS_EMPTY` | A preset-batch row's `commands` array is empty (DAT-091). | no |

## Conformance notes

- Traceability map: `conformance/traceability/data-model-1.md` — maps every `DAT-NNN` above to the case(s) that exercise it.
- Corpus: `conformance/corpora/data-model-1/` — seed cases covering the scope-node tree and its referential-integrity/attachment rules, the org node's `account_state` vocabulary, the `tz`/`lat`/`long` override-over-default resolution rule, a `display_power: off` daypart and its Lease projection, a preset-batch partial-failure outcome, the schedule-side `misfire` default by row kind, a rejected pack-owned scheduler row, and — for schedule composition — an ancestor-cascade governance case, a nearer/higher-`priority` schedule winning the precedence order, a per-instant layered-daypart resolution, a layered fallback selected down the precedence order, a rejected within-schedule daypart overlap, and the terminal `display: blank` default when nothing is applicable.
- Schedule applicability (DAT-051), the cross-schedule precedence order (DAT-053), and its daypart/fallback layering and terminal default (DAT-111, DAT-117, DAT-118) are now defined and exercised by corpus cases; the resolution these cases assert is authoring-time-deterministic (a fixed set of rows evaluated at a fixed instant), while the relay-side derivation of a compiled timeline from them remains `relay/1`'s own concern (below).
- The relay-side compiled/resolved dayparting timeline a relay derives by evaluating Daypart rows against a scope node's effective `tz` (Dayparting evaluation) is `relay/1`'s own desired-state content (`relay/1` REL-065), not a row shape this contract defines — no corpus case here exercises that derivation itself, only the authoring-time rows it is derived from.
