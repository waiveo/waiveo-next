# Automation Semantics

**Contract:** rules/1
**Version:** 1.0
**Status:** draft

## Scope

rules/1 defines the complete automation vocabulary — every trigger, condition, action, and mode a rule may use — and the evaluation semantics that make a rule's behavior fully determined: value comparison, the expression grammar, first-observation handling, flap suppression, per-device-class state matching, calendar/clock edge cases, misfire handling, and how a compiled rule behaves across a restart or a desired-state update. It also defines which execution class (edge or app) each vocabulary member belongs to and the rule-level classification formula built from those classes. This is the sole normative source for automation behavior; any other document describing automation MUST defer to this one.

- In scope: the closed trigger/condition/action/mode vocabulary and each member's execution class; entity targeting shared by triggers and device-affecting actions; typed value comparison and number parsing; the closed expression grammar and its filter list; first-observation semantics per trigger kind; flap suppression; per-device-class state matching (semantic groups, state-change vs. attribute-only-change); DST and high-latitude calendar semantics; misfire policy; in-flight hold/delay behavior across a restart; time evaluation under an untrusted clock; generation-swap cancellation; compile-time closure over app state; rule-template (blueprint) substitution semantics.
- Out of scope: the device-class registry's own content — state vocabularies, semantic-group membership, and typed attribute declarations are declared there and referenced here by name only (a separate companion document); the platform's label-selector grammar's own syntax (referenced here by name only, as `manifest/1`'s `notify` verb family reference already establishes the pattern); the compiled-generation transport, signing, and cross-tier version-skew mechanics (`relay/1`); the workflow state-machine's own execution semantics (a separate concern; this contract defines only the action that starts one); the platform's scheduling/dayparting system and its preset-batch/schedule data-model rows (a separate concern; this contract defines only the action that invokes a preset batch, not the row it references); notification transport and delivery (`ctx/1`'s `notify` family); the declarative UI grammar an automation-authoring surface renders against (`ui-schema/1`).

## Definitions

- **Entity** — a device-plane object exposing a canonical `state` string and a set of typed attributes, addressed by `entity_id`. Entities belong to a **device class**.
- **Device class** — a named category of entity whose state vocabulary, semantic groups, and typed attribute declarations are declared in the platform's device-class registry (a companion document to this contract, not defined here).
- **Semantic group** — a named set of canonical state values a device class declares as mutually equivalent for matching purposes (e.g., a group named `on` containing several canonical sub-states of "on-ness"). Declared in the device-class registry.
- **Rule** — an authored unit of automation: an ID, one or more triggers, zero or more conditions, one or more actions, and a mode.
- **Trigger**, **condition**, **action**, **mode** — members of this contract's closed vocabulary (Normative requirements).
- **Run** — one execution of a rule's `actions` sequence, started by a single trigger firing whose conditions passed. A rule's `mode` governs what happens when a new firing arrives while a run is already in progress (Modes).
- **Disposition** — the closed outcome a run (or a dropped/preempted firing) resolves to for mode-evaluation purposes: `ran`, `skipped`, or `restarted` (RUL-246).
- **Edge-class** — a rule, or a vocabulary member, evaluable using only LAN-visible entity state and values already closed over at compile time — no dependency on live app connectivity.
- **App-class** — a rule, or a vocabulary member, that requires the app's own runtime (live platform state, full expression power, or an execution mode the edge engine does not implement).
- **Edge engine** — the executor that evaluates edge-class rules.
- **App engine** — the executor that evaluates app-class rules and workflows.
- **Engine restart** — the evaluating engine's process starting to run, whether its first-ever start or any later restart (crash, redeploy, or an operator-initiated restart); this contract treats every such start identically regardless of cause, and uses this one term for it throughout — including where a requirement's own wording says "boot." Not to be confused with a rule's own `restart` mode (Modes, RUL-242), which cancels and restarts a single rule's run without the evaluating engine itself stopping.
- **Generation** — a versioned snapshot of compiled edge rules (and other desired state) produced by compiling authored rules against this contract. The snapshot's transport, signing, and pull mechanics are defined elsewhere (`relay/1`).
- **Monotonic time** — a clock source that only advances and is never adjusted backward by a wall-clock correction; used for every duration-based mechanism in this contract (`for:`, delay, stabilization windows) so that none of them can be shortened or reset by a clock-trust change.
- **Clock trust state** — `trusted` or `untrusted`, as attested by the evaluating engine's environment (the attestation mechanism itself is defined elsewhere, `relay/1`); this contract states only how time-based evaluation behaves under each state.
- **Timestamp** — the wire representation of an absolute instant this contract's expression grammar produces and consumes: an integer number of milliseconds since the Unix epoch (UTC). `now()` (RUL-290) returns one; it is wall-clock-derived and subject to the clock-trust floor (Time under clock trust) — a distinct notion from Monotonic time, which this contract uses only for duration holds (`for`, delay, stabilization windows).
- **Expression** — a value computed at evaluation time by applying zero or more filters, from this contract's closed filter list, to a source value.
- **ULID** — as defined in `manifest/1`: a 26-character Crockford-base32, time-sortable identifier.

## Normative requirements

### Rule classification

**[RUL-001]** A rule's triggers, conditions, and actions MUST each be drawn exclusively from the closed vocabulary this contract defines (Triggers, Conditions, Actions, Modes below). A rule referencing a trigger, condition, action, or mode member not defined here MUST fail compilation with a typed validation error — it MUST NOT be accepted by silently treating it as app-class or by any other fallback; growth of the vocabulary is exclusively a rules/1 minor-version change, never a silent extension by a rule author or a pack.

**[RUL-002]** A rule is **edge-class** if and only if every trigger in its `triggers` array is edge-class, every condition in its `conditions` array (recursively, through any `and`/`or`/`not` composition) is edge-class, every action in its `actions` array is edge-class, and its `mode` is `single` or `restart`. A rule failing any one of these is **app-class**. This is a total function: every closed-vocabulary member's class is fixed by this contract (stated per member below), so classification never depends on rule content the classifier cannot see.

**[RUL-003]** A rule's `triggers` array MAY contain more than one trigger. The rule's action sequence runs once whenever any single trigger in the array fires (logical OR across triggers) — composing a requirement that must hold across multiple signals is expressed through `conditions` (evaluated once a trigger has fired), never by requiring more than one trigger to fire together.

**[RUL-004]** A rule's `conditions` array, when non-empty, is evaluated as an implicit AND: every entry MUST pass for the rule's actions to run. An empty `conditions` array always passes.

**[RUL-005]** A pack-declared trigger macro (`manifest/1` MAN-092, a named human-labeled shorthand resolving to a state/numeric match on a relay-polled capability or to a pack event) MUST be resolved to one of this contract's own trigger kinds — `state`/`numeric` or `event` — before a rule using it is classified; this contract's classifier operates only on that resolved form and has no separate "macro" vocabulary member. A macro resolving to `state`/`numeric` is classified exactly as an ordinary trigger of that kind; a macro resolving to a pack event is classified exactly as an ordinary `event` trigger.

**[RUL-006]** For a rule classified app-class (RUL-002), the classifier MUST additionally expose which specific member(s) of the rule's `triggers`/`conditions`/`actions`/`mode` caused that classification — every trigger, condition, action, or mode value that is itself app-class or app-only, identified by its position within the rule (e.g. `actions[0].default[0]`) — not merely the rule-level `app` verdict alone. This diagnostic MUST be available anywhere a rule's execution class is surfaced (compile-time validation output, a compiled generation's own per-rule entry, CompiledRuleEntry, Wire shapes), never requiring an operator to re-derive it by manually re-checking every member against this contract's per-member class table.

### Entity targeting

**[RUL-010]** Wherever this contract defines a field typed `EntityRef` (a trigger's subject entity, or a device-affecting action's target), the value MUST be exactly one of: a single `entity_id` (ULID); a `selector` string in the platform's label-selector grammar (defined elsewhere); or a bare `device_class` filter (every entity of the named device class). Exactly one of `entity_id` / `selector` / `device_class` MUST be present.

**[RUL-011]** An `EntityRef` that resolves, at evaluation time, to more than one entity MUST be treated as independent per matched entity for a trigger: each matched entity carries its own first-observation state (First-observation semantics), its own `for:`-hold timer (where applicable), and its own from/to evaluation, exactly as if it were the sole subject of its own separate trigger. Any one matched entity's independent firing dispatches the rule's action sequence once, for that occurrence.

**[RUL-012]** An `EntityRef` that resolves to more than one entity on a device-affecting action MUST dispatch the action to each matched entity independently; the action's outcome aggregates per-entity results using the same partial-failure disposition defined for preset-batch actions (Actions: preset batch).

**[RUL-013]** For an edge-classified rule, an `EntityRef`'s matched entity set MUST be resolved once at compile time and frozen into the compiled generation (Compile-time closure) — the edge engine never re-resolves a `selector` or `device_class` filter live. A membership change that would alter the matched set recompiles a new generation.

### Triggers: state

**[RUL-020]** A `state` trigger MUST declare an `EntityRef` (Entity targeting) and MAY declare `from`, `to`, `attribute`, and `for`. It is edge-class. `for`'s meaning depends on whether the trigger is bounded or unscoped (RUL-023): RUL-024 defines it for a bounded trigger, RUL-026 for an unscoped one.

**[RUL-021]** When `attribute` is absent, `from`/`to` compare the entity's canonical state string. Each of `from`/`to`, when present, is either a scalar string or an array of strings: a scalar value MUST be matched via semantic-group expansion (State matching per device class); an array value MUST be matched via exact literal membership only, with no group expansion.

**[RUL-022]** When `attribute` names one of the entity's typed attributes, `from`/`to` compare that attribute's own value using typed equality (Typed value comparison) — semantic-group expansion does not apply to attribute values, only to the state-string comparison of RUL-021.

**[RUL-023]** A `state` trigger's fire condition depends on which of `attribute`/`from`/`to` are present, and MUST follow exactly one of these three rules: (1) **attribute-scoped, bounded** — `attribute` present, at least one of `from`/`to` present: fires only when that attribute's own value transitions matching the declared `from`/`to`, and MUST NOT fire on a tick where only the state string changed; (2) **attribute-scoped, unbounded** — `attribute` present, neither `from` nor `to` present: fires whenever that attribute's value changes to a new value, regardless of the state string; (3) **state-scoped** — `attribute` absent, at least one of `from`/`to` present: fires only on a genuine state-string transition matching the declared `from`/`to`, and MUST NOT fire on a tick whose state string is unchanged even if that unchanged value happens to equal the declared `to`; (4) **unscoped** — `attribute`, `from`, and `to` all absent: fires on any change to the entity, whether the state string or any attribute moved.

**[RUL-024]** `for`, when present, MUST be a non-negative integer number of seconds (0 behaves as if `for` were absent). A trigger declaring `for` MUST NOT fire until the matched condition (RUL-023) has held continuously for at least that many seconds of monotonic time; the hold MUST be evaluated on monotonic time only, never on wall-clock time, so that a clock-trust transition can neither shorten nor extend it. If the held condition stops holding before `for` elapses, the pending fire MUST be discarded without dispatching the rule. This paragraph defines `for` for a **bounded** trigger (RUL-023 cases 1–3, and every `numeric` trigger, RUL-033) — one with a `to`, `from`, or bound declared, whose matched condition is a level that can hold or stop holding; RUL-026 defines `for` for an **unscoped** `state` trigger (RUL-023 case 4), which has no such level to hold.

**[RUL-025]** State-diffing for a `state` trigger MUST compare against the entity's durable last-recorded value; an entity's first observation after an engine restart, after the entity is newly enrolled, or after a qualifying new generation is applied, MUST NOT be treated as an absent prior value (the fuller rule, including which generation-apply cases qualify, is stated in First-observation semantics, RUL-300/RUL-303, which reuses Generation-swap semantics' RUL-380/381 changed/unchanged test).

**[RUL-026]** On an **unscoped** `state` trigger (RUL-023 case 4: `attribute`, `from`, and `to` all absent) declaring `for`, the held condition RUL-024 refers to has no target value to hold — there is nothing to check the entity against beyond "did it change." `for` on an unscoped trigger instead means: once a qualifying change occurs, the trigger fires only if no further qualifying change to the entity occurs for the full declared duration of monotonic time — a settle/debounce, keyed to the absence of further change rather than to a value holding steady. A further qualifying change arriving before `for` elapses restarts the debounce window from that new change; this is not a fourth `for` mechanism, it is RUL-024's own "discard the pending fire and re-arm" behavior, applied to the unscoped case's different notion of what breaks the hold.

### Triggers: numeric

**[RUL-030]** A `numeric` trigger MUST declare an `EntityRef` and MAY declare `attribute`, `above`, `below`, and `for`; at least one of `above`/`below` MUST be present, each a literal JSON number (never an expression). It is edge-class.

**[RUL-031]** The compared value is the entity's canonical state string when `attribute` is absent, or the named attribute's value when present, parsed per the one number-parsing rule (Number parsing). A value that fails to parse as a number MUST be treated as not satisfying either bound, on every observation, without raising an evaluation error.

**[RUL-032]** A value **satisfies** the trigger when it is strictly greater than `above` (if declared) and strictly less than `below` (if declared); when both bounds are declared, both MUST hold simultaneously — the value strictly between them, with both endpoints themselves excluded.

**[RUL-033]** On an entity's first-ever observation for this trigger (no prior parsed value), the trigger fires immediately if the current value satisfies RUL-032 — a level check, not a crossing. On every subsequent observation, the trigger fires only on a **crossing**: the immediately preceding parsed value did not satisfy RUL-032 and the current one does. A `for` field (RUL-024's mechanism, reused here identically) delays the fire until the satisfying condition has held continuously for the declared duration.

### Triggers: time

**[RUL-040]** A `time` trigger MUST declare `at` as a local time-of-day string `HH:MM:SS` (24-hour), evaluated against the owning scope node's effective timezone. It is edge-class. It carries no `EntityRef`.

**[RUL-041]** A `time` trigger fires once at each local occurrence of `at`. DST and misfire behavior are governed by the DST semantics and Misfire policy sections respectively, not restated here.

### Triggers: time_pattern

**[RUL-050]** A `time_pattern` trigger MAY declare `hours`, `minutes`, and `seconds`; at least one MUST be present. Each declared field is either a non-negative integer (an exact match on that clock component) or a string of the form `/N` (`N` a positive integer; matches when the clock component is evenly divisible by `N`). An omitted field matches every value of that component. It is edge-class and carries no `EntityRef`.

**[RUL-051]** A `time_pattern` trigger fires at every local instant satisfying all of its declared fields simultaneously.

### Triggers: sun

**[RUL-060]** A `sun` trigger MUST declare `event` (`sunrise` or `sunset`) and MAY declare `offset` (an integer number of seconds, positive or negative, applied to the computed event instant). It is edge-class and carries no `EntityRef`; it is evaluated against the owning scope node's effective latitude/longitude.

**[RUL-061]** On a calendar date at a latitude where the declared `event` does not occur (a polar day or polar night at that scope node), the trigger MUST NOT fire for that date — the same non-firing, non-substituted treatment DST semantics gives a skipped local time, applied here for the same reason: the platform must not synthesize a firing instant the physical event never reached.

### Triggers: template (app-coupled)

**[RUL-070]** A `template` trigger MUST declare `expression`, an Expression (Expression grammar and filters) whose result is interpreted as a boolean. It is app-class unconditionally — it is never evaluated for edge eligibility, since its purpose is expression power the edge grammar restriction (Expression grammar and filters) does not offer.

**[RUL-071]** A `template` trigger fires when its expression's result transitions from falsy (or evaluation failure) to truthy, re-evaluated on every change to any value the expression reads; it MUST NOT fire on a re-evaluation that remains truthy.

### Triggers: event (app-coupled)

**[RUL-080]** An `event` trigger MUST declare `event`, the durable event name to match: either a pack-declared name from that pack's `contributes.automation.events` (`manifest/1` MAN-090) or a platform-reserved unnamespaced name (e.g., a variable-change event; Compile-time closure notes why a variable-gated *condition* stays edge-class while firing *on* a variable change requires this trigger). It MAY declare `match`, a set of exact-value constraints against top-level fields of the event's payload. It is app-class unconditionally.

**[RUL-081]** An `event` trigger fires once per matching durable event delivered, evaluating `match` (if present) against that event's payload; an event whose payload does not satisfy every `match` constraint does not fire the trigger.

### Triggers: webhook/ingest (app-coupled)

**[RUL-090]** A `webhook` trigger fires on a durable event whose origin is the platform's ingest surface (defined elsewhere), addressed by that event's durable ID rather than by the originating route. It is app-class unconditionally and carries no `EntityRef`.

**[RUL-091]** A `webhook` trigger MAY declare `match` with the same semantics as RUL-081, evaluated against the ingest event's payload.

### Conditions: composition and cross-entity evaluability

**[RUL-100]** A condition is either a **composition** — `and` (an array of two or more conditions, all must pass), `or` (an array of two or more conditions, at least one must pass), or `not` (exactly one nested condition, inverted) — or a **leaf**: `state`, `numeric`, `time`, `sun`, `variable`, or `template` (each defined below). A composition's own execution class is edge-class if and only if every condition it contains is edge-class; a composition is otherwise app-class.

**[RUL-101]** A leaf condition of type `state` or `numeric` MUST declare its own `EntityRef` (Entity targeting); it is evaluable against **any** relay-visible entity, not only the entity that fired the rule's trigger. This is unrestricted at the structured-condition level; the corresponding restriction to the triggering entity applies only within the expression grammar (Expression grammar and filters), never to a structured condition's own `EntityRef`.

### Conditions: state

**[RUL-110]** A `state` condition MUST declare an `EntityRef` and a `state` value (scalar or array, same matching rule as a state trigger's `to`, RUL-021), and MAY declare `attribute`. It passes when the referenced entity's current state (or, when `attribute` is present, that attribute's current value) matches, using the identical matching function a `state` trigger uses for RUL-021/RUL-022 — trigger and condition matching MUST NOT diverge, being two call sites of one shared function. It is edge-class.

### Conditions: numeric

**[RUL-120]** A `numeric` condition MUST declare an `EntityRef` and at least one of `above`/`below`, and MAY declare `attribute`, using the identical parsing (Number parsing) and satisfaction rule (RUL-032) a `numeric` trigger uses. It passes when the current value satisfies the declared bounds; it performs a level check only — it has no crossing concept, since a condition is evaluated at a point in time, not observed over a sequence. It is edge-class.

### Conditions: time

**[RUL-130]** A `time` condition MUST declare at least one of `after`/`before`, each a local time-of-day string `HH:MM:SS`. It passes when the current local time falls within the declared bound(s) (both, when both are present, form an inclusive range; a range whose `after` is later than `before` wraps past local midnight). It is edge-class and carries no `EntityRef`.

### Conditions: sun

**[RUL-140]** A `sun` condition MUST declare `after` or `before` (or both) as `{event, offset?}` pairs with the same shape as a `sun` trigger's `event`/`offset`. It passes when the current instant falls within the declared bound(s), computed the same way as RUL-130's local-time range. A date on which a referenced `event` does not occur (RUL-061) MUST be resolved the same way: the condition uses the nearest defined occurrence rather than treating the day as unbounded. This is a deliberate asymmetry with a `sun` trigger's own polar-date handling (RUL-061), not an inconsistency: a trigger has no reasonable instant to synthesize and fire at, while a condition bounds the current instant against a nearest-occurrence fallback instead of needing to invent a firing moment. It is edge-class and carries no `EntityRef`.

### Conditions: variable

**[RUL-150]** A `variable` condition MUST declare `variable` (name) and a comparison (`equals`, `above`, or `below`, with a literal value). It is edge-class: for an edge-classified rule, the variable's value observed at compile time MUST be substituted into the compiled generation as a constant (Compile-time closure) — the edge engine never performs a live variable lookup to evaluate this condition.

### Conditions: template

**[RUL-151]** A `template` condition MUST declare `expression`, an Expression (Expression grammar and filters) whose result is interpreted as a boolean. It is app-class unconditionally, for the same reason as a `template` trigger (RUL-070) — it is never evaluated for edge eligibility, since its purpose is expression power the edge grammar restriction (Expression grammar and filters) does not offer.

**[RUL-152]** A `template` condition passes when its expression's result is truthy at the instant the condition is evaluated — a level check on the current result, not a transition: unlike a `template` trigger (RUL-071), which fires only on a falsy-to-truthy transition, a `template` condition is re-evaluated fresh every time its containing rule's conditions are checked, with no memory of a prior result. An expression that fails to evaluate MUST fail the condition (not-matched/false) per Expression grammar and filters (RUL-284), rather than pass or raise.

### Actions: device command

**[RUL-160]** A `device_command` action MUST declare an `EntityRef` (Entity targeting), a `command` name (drawn from that entity's device class's command vocabulary, declared in the device-class registry), and MAY declare `params`. It is edge-class.

**[RUL-161]** A single-entity `device_command` dispatch is atomic pass/fail; a multi-entity dispatch (RUL-012) aggregates per-entity pass/fail into the same partial-failure disposition as a preset-batch action (Actions: preset batch).

### Actions: preset batch

**[RUL-170]** A `preset_batch` action MUST declare `preset_id` (a reference to a platform-owned preset row; the row itself, and its own device-command list, are declared elsewhere — this contract defines only the action that invokes one). It is edge-class.

**[RUL-171]** Every device command in the referenced preset's list MUST be attempted independently: a failing command MUST NOT prevent the remaining commands in the same batch from being attempted, and a preset-batch action's own failure MUST NOT halt the rest of its rule's `actions` sequence.

**[RUL-172]** A preset-batch action's outcome MUST be one of `complete` (every command succeeded), `partial` (at least one succeeded and at least one failed), or `failed` (none succeeded), accompanied by a per-command result list (PresetBatchOutcome, Wire shapes). A multi-entity `device_command` dispatch (RUL-012, RUL-161) uses this identical three-value outcome and per-target result shape.

**[RUL-173]** For an edge-classified rule, the referenced preset's command list MUST be resolved once at compile time and frozen into the compiled generation (Compile-time closure); a preset edited after compilation does not retroactively affect an already-compiled generation.

### Actions: choose

**[RUL-180]** A `choose` action MUST declare `branches`, a fixed, finite array of `{condition, actions}` pairs, each `condition` any Condition (Conditions) and each `actions` a non-empty array of actions; it MAY declare `default`, an array of actions. `branches` MUST be a statically declared array at authoring time — no dynamically generated branch count and no branch whose `actions` recurses into the same `choose` action.

**[RUL-181]** At evaluation, `choose` evaluates its `branches` in array order and executes the `actions` of the first branch whose `condition` passes, then stops — later branches are not evaluated. If no branch's condition passes and `default` is present, `default`'s actions execute. If no branch matches and `default` is absent, the action performs no work.

**[RUL-182]** A `choose` action is edge-class if and only if every branch's `condition` and every branch's (and `default`'s) `actions` are edge-class; a single app-class branch or the `default` list makes the entire `choose` action app-class, which in turn makes its containing rule app-class (RUL-002).

### Actions: delay

**[RUL-190]** A `delay` action MUST declare `duration_seconds`, a non-negative integer (0 behaves as an immediate no-op delay). It is edge-class. The delay's remaining time MUST be tracked on monotonic time only.

### Actions: log

**[RUL-200]** A `log` action MUST declare `message`, an Expression (Expression grammar and filters) evaluated to a string, and MAY declare `level` (`info`, `warning`, or `error`; default `info`). It is edge-class.

### Actions: notify (app-coupled)

**[RUL-210]** A `notify` action MUST declare `template`, `recipients_selector`, and `params`, carrying the identical shape as the `notify.send` verb (`ctx/1` CTX-080) — this action is the automation-authored path to that same platform-owned delivery mechanism, not a second one. It is app-class unconditionally.

### Actions: variable write (app-coupled)

**[RUL-220]** A `variable_write` action MUST declare `variable` (name) and `value` (an Expression). It is app-class unconditionally — the edge engine never performs a live write, since a variable's value is only ever closed over as a frozen constant on the edge side (Compile-time closure).

### Actions: workflow start (app-coupled)

**[RUL-230]** A `workflow_start` action MUST declare `workflow_id` and MAY declare `params`. It is app-class unconditionally. This contract defines only the action that starts a workflow run; the workflow's own state-machine execution semantics are a separate concern.

### Actions: pack action

**[RUL-231]** A `pack_action` action MUST declare `action` (a publisher-qualified pack action name matching a `contributes.automation.actions` entry, `manifest/1` MAN-091) and MAY declare `params`. Its execution class is read directly from that entry's `execution` field: `relay-command` is edge-class, `app-service` is app-class.

**[RUL-232]** A `pack_action` whose referenced entry is `execution: relay-command` MUST be dispatched directly as a device command, never through the pack's runtime action handler (`ctx/1` CTX-112 states this same routing exception from the host side); one whose entry is `execution: app-service` MUST be dispatched through the pack's runtime action handler exactly as a management-API invocation of the same action would be (`ctx/1` CTX-110).

### Modes

**[RUL-240]** A rule's `mode` MUST be exactly one of `single`, `restart`, `queued`, or `parallel`. `single` and `restart` are edge-class; `queued` and `parallel` are app-only — a rule declaring either forces the whole rule app-class (RUL-002) regardless of how its triggers, conditions, and actions would otherwise classify, because only the app engine implements queued or concurrent run management.

**[RUL-241]** `single` (the default when `mode` is omitted): while a run of this rule is in progress, a new trigger firing for the same rule MUST be dropped — no new run starts — and MUST be recorded with a `skipped` disposition (RunDisposition, Wire shapes).

**[RUL-242]** `restart`: a new trigger firing for the same rule while a run is in progress MUST cancel that in-progress run, discarding any pending `for`-hold or `delay` it was waiting on, and start a fresh run from the beginning of the action sequence, using the new trigger's own context; the fresh run's own timers (any `for`-hold or `delay` it subsequently reaches) start at zero, never inheriting elapsed time from the canceled run. The canceled run's disposition MUST be recorded as `restarted`. This is a distinct mechanism from an engine restart (In-flight holds across restart): here, the rule's own mode causes the cancellation; the evaluating engine keeps running throughout.

**[RUL-243]** `queued` (app-only): a new trigger firing while a run is in progress MUST be enqueued and run to completion, in firing order, once every earlier queued run for this rule completes; no firing is dropped.

**[RUL-244]** `parallel` (app-only): a new trigger firing while a run is in progress starts a new, independent concurrent run, up to a declared `max` (a positive integer; REQUIRED when `mode` is `parallel`). A firing that would exceed `max` concurrently active runs MUST be dropped and recorded with a `skipped` disposition, identically to `single`'s overflow handling. Declaring `max` with a non-null value under any `mode` other than `parallel` MUST fail compilation as a typed validation error (`MODE_MAX_NOT_APPLICABLE`) rather than being silently ignored; a `max: null` alongside a non-`parallel` mode (as the Rule wire shape, Wire shapes, always shows regardless of mode) is the field's normal absence, not a declaration of it.

*draft-note: RUL-244's overflow-drops-rather-than-queues choice is a proposed default for review, not dictated by the source vocabulary — an overflow-queues alternative is equally defensible; flagged here rather than silently picked.*

**[RUL-245]** rules/1 defines no automatic exact-repeat refire-suppression window beyond `for` (Flap suppression) and the mode-level disposition rules above (RUL-241–244); there is no additional per-rule cooldown/dedup-window mechanism in this version. An author needing to suppress rapid repeated identical firings composes it explicitly, typically with `for`.

**[RUL-246]** Every trigger firing that reaches mode evaluation MUST resolve to exactly one mode disposition from the closed set `ran`, `skipped`, `restarted` (RunDisposition, Wire shapes): `ran` — a new run started normally (`queued`'s eventual run and each `parallel` run also record `ran`); `skipped` — dropped per `single`'s or `parallel`'s overflow handling (RUL-241, RUL-244); `restarted` — the run it preempted per `restart` (RUL-242). A firing whose origin is a caught-up misfired occurrence (RUL-355) carries this same mode disposition **plus** an orthogonal `misfire_caught: true` marker — misfire catch-up is a fact about how the firing arose, not a fourth mode outcome.

### Rule templates (blueprint substitution)

**[RUL-250]** A rule instantiated from a rule template MUST bind the template's parameters into the resulting rule's trigger/condition/action field positions as literal data substitution only — no template-language evaluation pass (conditionals, loops, or string interpolation at the template layer) runs over the rule's structure. The bound result MUST be a rule that independently validates against this contract exactly as an authored-from-scratch rule would.

**[RUL-251]** Execution-class classification (RUL-002) MUST run on each template-instantiated rule individually, after parameter binding — never once at the template's own definition. Two instances of the same template bound with different parameters MAY differ in execution class.

### Typed value comparison

**[RUL-260]** Every value this contract's vocabulary compares (a `state`/`numeric` trigger or condition's bound, a `variable` condition's comparison value, an action's typed field) carries a type — string, number, boolean, or the entity attribute's own declared type from the device-class registry. A comparison between values of different types MUST NOT silently coerce.

**[RUL-261]** When a type mismatch is knowable at compile time — a numeric bound (`above`/`below`) declared against an attribute the device-class registry types as non-numeric, for example — it MUST fail compilation as a typed validation error.

**[RUL-262]** When a type mismatch is only knowable at evaluation time (a dynamically typed or untyped attribute), the comparison MUST evaluate as not-matching/false rather than raising an evaluation error or coercing.

**[RUL-263]** String comparison (outside semantic-group expansion, State matching per device class) is exact and case-sensitive.

### Number parsing

**[RUL-270]** Exactly one parsing rule governs every numeric comparison and every `int`/`float` filter evaluation in both engines: (1) a JSON number is used as-is; (2) a string is accepted only if it matches the grammar `-?[0-9]+(\.[0-9]+)?` — an optional leading minus, one or more digits, an optional decimal point followed by one or more digits; no leading `+`, no exponent notation, no leading or trailing whitespace, no thousands separators, no hex/octal/binary prefixes, and no `Infinity`/`NaN` literals; (3) every other input — boolean, null, an object or array, or a string not matching the grammar above — is not-a-number.

**[RUL-271]** A not-a-number result (RUL-270 case 3) MUST fail closed at every call site: a numeric trigger/condition treats it as not satisfying its bounds (RUL-031); an `int`/`float` filter treats it as an expression evaluation failure (Expression grammar and filters).

### Expression grammar and filters

**[RUL-280]** An Expression is either a JSON literal (string, number, boolean, or null) used as-is, or an object `{"expr": "<pipeline>"}` where `<pipeline>` is a string of the form `source (| filter(args...))*` — a source, optionally followed by one or more pipe-separated filter applications, each drawing from the closed filter list (RUL-290).

*draft-note: `<pipeline>`'s concrete string grammar above is a proposed default for review — the source vocabulary requires "a closed expression grammar" with a fixed filter list but does not itself dictate a concrete pipeline syntax.*

**[RUL-281]** A source is one of: a literal; `state(entity_id)`; `attr(entity_id, name)`; or `now()`. `entity_id` in a source MUST be a literal entity ID string — a source's entity reference is never itself an `EntityRef` selector or device-class filter (Entity targeting governs trigger/action subjects; an expression's source is always a single, specific entity).

**[RUL-282]** For an edge-classified rule, every `state(entity_id)`/`attr(entity_id, name)` source within any Expression the rule uses MUST reference the entity that is the subject of the rule's own trigger (for a multi-entity `EntityRef`, RUL-011's currently-firing matched entity) — no other entity ID is permitted, and the expression grammar offers no loop or recursion construct. This restriction does not apply to a `template` trigger/condition (RUL-070, RUL-151, both app-class unconditionally) or to any other app-class evaluation context, where an Expression's sources may reference any relay-visible entity.

**[RUL-283]** The filter list (RUL-290) is exactly one shared list between edge and app evaluation; edge and app never diverge on which filters exist or what they compute — the only axis of restriction between them is the entity-reference scope of RUL-282.

**[RUL-284]** An Expression that fails to evaluate — an unresolvable entity or attribute reference, a filter argument outside its accepted domain, or a number-parse failure (RUL-271) — MUST cause the containing trigger/condition to evaluate as not-matched/false (fail closed), and MUST be recorded for operator visibility (the record's own shape is out of scope of this contract) rather than silently discarded, except as RUL-285 states for a failure `default` contains.

**[RUL-285]** The `value` `default` (RUL-290) consumes is the prior pipeline stage's own output (RUL-292) — the source and any filters preceding `default` in the pipeline. When evaluating that upstream stage fails for any of RUL-284's reasons (an unresolvable entity/attribute reference, a filter argument outside its accepted domain, or a number-parse failure), `default` contains the failure and yields its own `fallback` argument in place of propagating it: the pipeline continues evaluating normally from that fallback value, the containing trigger/condition does not fail closed on account of it, and the contained failure MUST NOT itself be recorded as an evaluation failure under RUL-284. `default` is the sole exception RUL-284 admits; every other filter and source failure fails closed exactly as RUL-284 states.

**[RUL-290]** The closed filter list is exactly:

| filter | signature | behavior |
|---|---|---|
| `state` | `state(entity_id) -> string` | the referenced entity's current canonical state string |
| `attr` | `attr(entity_id, name) -> value` | the referenced entity's current value for the named attribute, typed per the device-class registry |
| `default` | `default(value, fallback) -> value` | `fallback` when the upstream `value` is null/absent or fails to evaluate (RUL-285's contained-failure exception), else `value` |
| `upper` | `upper(string) -> string` | uppercased |
| `lower` | `lower(string) -> string` | lowercased |
| `trim` | `trim(string) -> string` | leading/trailing whitespace removed |
| `round` | `round(number, precision=0) -> number` | rounded to `precision` decimal places, half rounds up (ties round toward positive infinity) |
| `abs` | `abs(number) -> number` | absolute value |
| `int` | `int(value) -> number` | parsed per Number parsing, truncated toward zero; fails per RUL-271 |
| `float` | `float(value) -> number` | parsed per Number parsing; fails per RUL-271 |
| `now` | `now() -> timestamp` | the evaluating engine's current time, as a Timestamp (edge: subject to the clock-trust floor, Time under clock trust) |
| `elapsed` | `elapsed(timestamp) -> number` | seconds between the given Timestamp and `now()` |
| `duration` | `duration(value, unit) -> number` | `value` expressed in `unit` (RUL-291), normalized to seconds |
| `convert` | `convert(value, from_unit, to_unit) -> number` | `value` converted between two units of the same unit class (RUL-291) |

**[RUL-291]** `duration` and `convert`'s `unit` arguments are drawn from a closed unit-class list; v1 defines exactly one class, **duration units** (`ms`, `s`, `min`, `h`). `convert` MUST fail (RUL-284) if `from_unit` and `to_unit` are not members of the same unit class.

*draft-note: whether v1 needs additional unit classes beyond duration (e.g., data size) is not established by the source vocabulary beyond "unit-conversion helpers the signage corpus needs" — proposed default above is duration-only, extensible as an additive rules/1 minor.*

**[RUL-292]** `state`/`attr`/`now` are the only filters usable as a pipeline's source (RUL-281); every other filter in RUL-290 MUST appear only after a source in a pipeline, consuming the prior stage's output as its first argument.

### First-observation semantics

**[RUL-300]** A `state` trigger's first observation of its subject entity — the first observation after an engine restart, after that entity is newly enrolled, or after a new generation is applied that qualifies under RUL-303 for this trigger — MUST NOT be treated as a transition, even when the observed value equals the trigger's declared `to`. The comparison MUST be made against the entity's durable last-recorded value (never an absent/reset baseline), so that a first post-restart observation which merely reconfirms an already-durable, unchanged value produces no firing. This first-observation state is keyed per (trigger, entity), never per entity alone (RUL-304).

**[RUL-301]** State and numeric triggers MUST NOT dispatch their rule's actions until a **stabilization window** has elapsed after the evaluating engine reaches a defined readiness point (an engine restart, entity enrollment, or generation-apply, as applicable). The window MUST be bounded, config-visible, and fallback-timed — a maximum wait applies even if the readiness signal the window is keyed to never arrives, so the gate cannot wedge open indefinitely.

*draft-note: RUL-301 requires the mechanism (bounded, config-visible, fallback-timed); concrete duration defaults are not fixed by this contract version and are proposed for review separately — they are not carried over from any prior implementation's figures as binding defaults.*

**[RUL-302]** A `numeric` trigger's first-ever observation of its subject (RUL-033) is exempted from RUL-300's transition-suppression rule by design: it performs a level check against the current value rather than suppressing, because a crossing requires a prior value to cross from, which does not yet exist. This is a deliberate asymmetry with RUL-300, not an inconsistency: a discrete state has no well-defined "currently past the line" reading on first sight, while a numeric threshold does.

**[RUL-303]** A generation-apply resets a trigger's first-observation baseline (RUL-300) only for a trigger that is new in the applied generation or whose own compiled structure or closed-over values changed from the prior generation — the identical changed/unchanged test RUL-381 defines at the rule level, applied here at the individual trigger's level. A trigger that is unchanged across the generation swap carries its first-observation state forward exactly as if no generation had been applied: the entity's durable last-recorded value from before the swap remains what its next observation is diffed against. A routine, unrelated edit elsewhere in the same generation — a variable write, a preset edit, a selector-membership change affecting a different rule — MUST NOT reset first-observation for, or suppress a genuine subsequent transition on, a trigger the edit does not itself touch.

**[RUL-304]** First-observation state (RUL-300) is keyed per (trigger, entity) pair, not per entity alone. This generalizes RUL-011's rule — that one trigger's `EntityRef` matching several entities gives each matched entity independent first-observation state — to the ordinary case of several triggers, in the same rule or different rules, watching one common entity: each such trigger carries its own first-observation state for that entity, independent of every other trigger's.

### Flap suppression

**[RUL-310]** `for` (RUL-024, reused by numeric triggers per RUL-033) is the sole normative duration-hold trigger primitive in this contract: it prevents a trigger from firing until its matched condition has held continuously for a declared span of monotonic time.

**[RUL-311]** `for` and mode-level disposition handling (RUL-241–245) are distinct, complementary mechanisms and MUST NOT be conflated: `for` governs whether a trigger fires at all (a hold-before-firing gate evaluated before any action runs), while mode governs what happens when a rule that has already started a run receives another firing while busy. Neither mechanism substitutes for the other.

### State matching per device class

**[RUL-320]** State matching — a state trigger's `from`/`to` (RUL-021), a state condition's `state` (RUL-110), and any other point in this contract comparing a scalar or array against an entity's canonical state string — MUST be evaluated by one shared matching algorithm, reused identically by every call site; the algorithm MUST NOT be reimplemented per call site with independently drifting behavior.

**[RUL-321]** The shared matching algorithm: a **scalar** expected value first attempts an exact string match against the actual state; failing that, it attempts membership in the actual state's device class's semantic groups (built-in groups first, then any extension-registered group for that device class, in that order — an extension-registered group MUST NOT be permitted to shadow or override a built-in group's membership). An **array** expected value matches only via exact literal membership — semantic-group expansion never applies to an array.

**[RUL-322]** Semantic groups MUST be evaluated live at match time, never snapshotted at an engine restart or at compile time — a group registered or extended after an engine restart is visible to every subsequent match.

### Attribute-change vs. state-change

**[RUL-330]** Every entity observation carries two independent facts: whether the canonical state string changed, and whether any attribute's value changed. A `state` trigger or `state` condition's fire/pass behavior with respect to this distinction is governed exactly by RUL-023's four-way rule (attribute-scoped bounded/unbounded, state-scoped, unscoped) — restated here as the general principle: **state-scoped** matching (`attribute` absent, `from`/`to` present) MUST NOT be satisfied by an attribute-only change, regardless of what the unchanged state string equals; **unscoped** matching (`attribute`, `from`, `to` all absent) MUST be satisfied by either kind of change.

### DST semantics

**[RUL-340]** `time` and `time_pattern` triggers (Triggers: time, Triggers: time_pattern) are evaluated as local wall-clock instants converted to an absolute instant via the owning scope node's effective timezone.

**[RUL-341]** On a calendar date where a daylight-saving transition removes a local time from existence (a "spring forward" gap), a trigger whose declared local time falls inside the removed range MUST NOT fire for that date — it is treated as not having occurred, never as firing early, late, or at the transition boundary.

**[RUL-342]** On a calendar date where a daylight-saving transition repeats a local time (a "fall back" duplication), a trigger whose declared local time falls inside the duplicated range MUST fire exactly once for that date, keyed to the **first** absolute occurrence of that local time — never twice.

*draft-note: RUL-341/RUL-342 are proposed defaults for review (skip-the-gap, fire-once-on-first-occurrence) — the source vocabulary requires that DST skipped/repeated local times be given normative semantics but does not itself dictate which resolution.*

*draft-note: RUL-340–342 are written against a trigger with a single declared local time (`time`, RUL-040/041); `time_pattern` (RUL-050/051) recurs, so one calendar date can nominally place several of its occurrences inside a single DST transition's affected range. The proposed generalization, not yet normative: on a spring-forward date, every nominal `time_pattern` occurrence whose local time falls inside the removed range does not fire, exactly as RUL-341 already states for a single occurrence, applied individually to each one; on a fall-back date, every nominal `time_pattern` occurrence whose local time falls inside the duplicated range fires exactly once, each keyed to its own first absolute instant, exactly as RUL-342 already states for a single occurrence, applied individually to each one.*

### Misfire policy

**[RUL-350]** `misfire` is a per-trigger enum with values `catch_up_once`, `skip`, or `fire_each`, applicable to `time`, `time_pattern`, and `sun` triggers. It governs what happens to an occurrence the evaluating engine was unable to evaluate at its scheduled instant (e.g., the engine was offline, applying a new generation across that instant, or the clock was untrusted across that instant, RUL-371). The enum and its default-selection rule (RUL-354) apply identically regardless of whether the containing rule is edge-class (evaluated by the edge engine) or app-class (evaluated by the app engine) — one policy, evaluated consistently by whichever engine a given rule compiles to.

**[RUL-351]** `skip`: a missed occurrence is never fired after the fact.

**[RUL-352]** `catch_up_once`: exactly one occurrence fires once evaluation resumes, collapsing any number of missed occurrences into a single firing, regardless of how many were actually missed.

**[RUL-353]** `fire_each`: every missed occurrence dispatches a firing attempt once evaluation resumes, in original chronological order, each independently subject to full mode evaluation (Modes; RUL-355) — under a busy `single` or `restart` mode, an earlier occurrence's dispatch can still cause a later occurrence in the same catch-up batch to resolve `skipped`, or to itself be preempted and resolve `restarted`, exactly as any other pair of close-together firings would. `fire_each` guarantees a mode-evaluated dispatch attempt per missed occurrence, not a completed run per occurrence. `fire_each` has no default trigger kind (RUL-354) and MUST be explicitly declared to take effect.

**[RUL-354]** Every `time`, `time_pattern`, and `sun` trigger in this contract's own vocabulary represents a single momentary instant, not an ongoing represented state; absent an explicit `misfire` declaration, every trigger of these kinds defaults to `skip` — a missed one-shot does not fire late. (A recurring scheduled *state*, as distinct from an instantaneous trigger, is a separate platform concern outside this contract's vocabulary and may default oppositely; that default is not this contract's to state.)

**[RUL-355]** A caught-up misfired trigger (`catch_up_once` or `fire_each`) dispatches its rule exactly as a normal firing would, including full mode evaluation (Modes) — a caught-up fire arriving while another run of the same rule is in progress resolves to `skipped`/`restarted`/queued/parallel-run per the rule's declared mode like any other firing (RUL-246), and MUST additionally carry `misfire_caught: true` on its recorded disposition (RunDisposition, Wire shapes), marking that this particular firing's origin was a misfire catch-up rather than a live observation.

### In-flight holds across restart

**[RUL-360]** An in-progress rule run does not survive an engine restart: no partial run state (including its position in the `actions` sequence) is reconstructed after restart. What this rule states is narrower and applies once the same rule's trigger/hold conditions occur again post-restart: any `for`-hold (RUL-024) or `delay` action (RUL-190) that a subsequent run encounters MUST count from zero, never seeded from a pre-restart elapsed value — a hold begins counting only once its underlying condition freshly re-matches after restart, and a delay begins counting only when a (new) run reaches it.

**[RUL-361]** Every timed wait affected by RUL-360 MUST be evaluated on monotonic time only, both before and after an engine restart, so that an engine restart cannot be used to shorten (or a clock-trust transition to lengthen) a hold or delay's effective duration.

**[RUL-362]** RUL-360's reset is distinct from misfire (Misfire policy): a hold or delay interrupted mid-count by an engine restart is an in-progress action interrupted, not a missed scheduled occurrence — it never itself carries the `misfire_caught` marker (RUL-246).

### Time under clock trust

**[RUL-370]** While the evaluating engine's clock trust state is `untrusted`, `time`, `time_pattern`, and `sun` triggers and conditions MUST evaluate against the engine's persisted best-known time floor (defined elsewhere) rather than an unverified wall clock — never firing based on a clock reading earlier than the floor.

**[RUL-371]** When the engine's clock trust state transitions from `untrusted` to `trusted`, every time-based trigger and condition MUST be re-evaluated immediately against the newly trusted time, rather than waiting for its next naturally scheduled tick. Any `time`, `time_pattern`, or `sun` occurrence whose scheduled instant fell within the untrusted window MUST be treated as a missed occurrence governed by that trigger's declared `misfire` policy (Misfire policy, RUL-350) — the same category of handling as an occurrence missed while the engine was offline — rather than silently dropped outside the `misfire` policy or fired as an ordinary live tick.

### Generation-swap semantics

**[RUL-380]** When a new compiled generation is applied, an in-flight run of a rule that is **changed or removed** in the new generation MUST be canceled; an in-flight run of a rule that is unchanged between generations MUST continue uninterrupted.

**[RUL-381]** "Changed," for RUL-380's purpose, means the rule's own compiled trigger/condition/action/mode structure differs from the prior generation, or any value closed over into it (Compile-time closure) differs — a rule whose compiled structure and every closed-over value are identical across two generations is not "changed" merely because the generation's own version number advanced, and its in-flight run is not canceled.

### Compile-time closure

**[RUL-390]** For every edge-classified rule, the following are resolved once at compile time and frozen into the compiled generation, never re-resolved live by the edge engine: every `variable` condition's comparison value (the variable's value as observed at compile time, RUL-150); every `EntityRef` selector or device-class filter's matched entity set (RUL-013); and every `preset_batch` action's referenced command list (RUL-173).

**[RUL-391]** While operating offline (unable to reach the app), the edge engine MUST continue evaluating every closed-over value from RUL-390 exactly as frozen — stale-but-defined, never treated as unknown or as cause to stop evaluating the rule.

**[RUL-392]** A variable write, a selector/device-class membership change, or a preset edit MUST cause a new generation to be compiled, incorporating the new closed-over values; per RUL-380/RUL-381, only rules whose own closed-over values actually changed have their in-flight runs canceled by that recompilation.

**[RUL-393]** An action's `params` field (`device_command` RUL-160, `pack_action` RUL-231, `notify` RUL-210, `workflow_start` RUL-230) accepts, per value, either a literal or an Expression (Expression grammar and filters). Unlike RUL-390's compile-time closures, an Expression inside `params` is **live-evaluated** at the moment the action runs, never frozen into the compiled generation — a deliberate exception to RUL-390's freeze list, not an oversight: RUL-390 enumerates exactly three things frozen at compile time (a `variable` condition's comparison value, an `EntityRef`'s matched entity set, a `preset_batch`'s command list), and an action's `params` Expression is not among them. For an edge-classified rule, a `params` Expression remains subject to the same entity-reference scope restriction as every other edge-side Expression (RUL-282) — it may source only the rule's own triggering entity, never an arbitrary one.

## Wire shapes

```json
// Rule (authored top-level object)
{
  "id": "01J8Z3K4N5P6Q7R8S9T0V1W2Z1",
  "mode": "single",
  "max": null,
  "triggers": [
    { "type": "state", "entity_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Z2", "from": ["off", "standby"], "to": ["on"] }
  ],
  "conditions": [],
  "actions": [
    { "type": "device_command", "entity_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Z2", "command": "launch", "params": { "channel": "dev" } }
  ]
}
```

```json
// EntityRef (embedded in triggers and device-affecting actions — exactly one of the three)
{ "entity_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Z2" }
```

```json
// EntityRef (selector form — illustrative; the label-selector grammar is out of scope here (Scope) and referenced only by name, the same treatment the device-class registry gets)
{ "selector": "label==lobby-screens" }
```

```json
// EntityRef (device-class filter form)
{ "device_class": "media-player" }
```

```json
// StateTrigger — attribute-scoped, bounded (RUL-023 case 1)
{
  "type": "state",
  "entity_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Z2",
  "attribute": "volume",
  "from": 20,
  "to": 25,
  "for": 5
}
```

```json
// NumericTrigger
{
  "type": "numeric",
  "entity_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Z3",
  "attribute": "cpu_temp",
  "above": 80,
  "for": 60
}
```

```json
// TimeTrigger
{ "type": "time", "at": "08:00:00", "misfire": "catch_up_once" }
```

```json
// TimePatternTrigger (fires every 15 minutes, on the minute)
{ "type": "time_pattern", "minutes": "/15", "seconds": 0 }
```

```json
// SunTrigger
{ "type": "sun", "event": "sunset", "offset": -900 }
```

```json
// TemplateTrigger (app-coupled)
{ "type": "template", "expression": { "expr": "state('media_player.lobby') | upper" } }
```

```json
// EventTrigger (app-coupled)
{ "type": "event", "event": "acme/weather-widget.forecast_updated", "match": { "severity": "high" } }
```

```json
// WebhookTrigger (app-coupled)
{ "type": "webhook", "match": { "source": "front-desk-kiosk" } }
```

```json
// Condition — and/or/not composition wrapping leaf conditions
{
  "and": [
    { "type": "state", "entity_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Z4", "state": "on" },
    { "not": { "type": "state", "entity_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Z2", "state": "playing" } },
    { "type": "sun", "after": { "event": "sunset" } }
  ]
}
```

```json
// VariableCondition
{ "type": "variable", "variable": "guest_mode", "equals": false }
```

```json
// TemplateCondition (app-coupled)
{ "type": "template", "expression": { "expr": "attr('01J8Z3K4N5P6Q7R8S9T0V1W2Z3', 'battery_ok') | default(true)" } }
```

```json
// PresetBatchAction
{ "type": "preset_batch", "preset_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Z5" }
```

```json
// PresetBatchOutcome (also the shape a multi-entity device_command dispatch aggregates into)
{
  "outcome": "partial",
  "results": [
    { "target": "01J8Z3K4N5P6Q7R8S9T0V1W2Z2", "command": "power_on", "ok": true },
    { "target": "01J8Z3K4N5P6Q7R8S9T0V1W2Z6", "command": "power_on", "ok": false, "error": "unreachable" }
  ]
}
```

```json
// ChooseAction
{
  "type": "choose",
  "branches": [
    {
      "condition": { "type": "state", "entity_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Z2", "state": "playing" },
      "actions": [{ "type": "log", "message": "already playing" }]
    }
  ],
  "default": [
    { "type": "device_command", "entity_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Z2", "command": "launch" }
  ]
}
```

```json
// DelayAction
{ "type": "delay", "duration_seconds": 30 }
```

```json
// LogAction
{ "type": "log", "message": { "expr": "state('media_player.lobby')" }, "level": "info" }
```

```json
// NotifyAction (app-coupled — same shape as ctx/1's notify.send, CTX-080)
{ "type": "notify", "template": "screen-offline", "recipients_selector": "role==owner", "params": { "screen": "lobby" } }
```

```json
// VariableWriteAction (app-coupled)
{ "type": "variable_write", "variable": "guest_mode", "value": true }
```

```json
// WorkflowStartAction (app-coupled)
{ "type": "workflow_start", "workflow_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Z7", "params": {} }
```

```json
// RunDisposition (the outcome of one trigger firing against a rule's mode; carried by telemetry defined elsewhere)
{
  "rule_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Z1",
  "disposition": "skipped",
  "mode": "single",
  "misfire_caught": false
}
```

```json
// CompiledRuleEntry (illustrative — a generation's per-rule classification + frozen closure, RUL-390)
{
  "rule_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Z1",
  "execution_class": "edge",
  "closed_over": {
    "variables": { "guest_mode": false },
    "selectors": { "label==lobby-screens": ["01J8Z3K4N5P6Q7R8S9T0V1W2Z2", "01J8Z3K4N5P6Q7R8S9T0V1W2Z6"] },
    "preset_batches": { "01J8Z3K4N5P6Q7R8S9T0V1W2Z5": [{ "entity_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Z2", "command": "power_on" }] }
  }
}
```

```json
// CompiledRuleEntry — app-class case (illustrative; app_class_reasons is RUL-006's per-member "why", present when execution_class is app)
{
  "rule_id": "01J8Z3K4N5P6Q7R8S9T0V1W2Z8",
  "execution_class": "app",
  "app_class_reasons": [
    { "field": "actions[0].default[0]", "type": "notify", "reason": "notify is app-class unconditionally (RUL-210)" },
    { "field": "mode", "value": "queued", "reason": "queued is app-only (RUL-240)" }
  ],
  "closed_over": null
}
```

## Negotiation

rules/1 has no live wire handshake of its own; it governs the authored rule shape a compiler validates and classifies, and the compiled generation that results (Compile-time closure, Generation-swap semantics). Version compatibility is expressed at the vocabulary level: adding a new trigger/condition/action/mode member, or a new filter to RUL-290, is an additive rules/1 minor; removing a member, narrowing an existing member's accepted shape, or changing an existing member's evaluation semantics is a rules/1 major. A compiled generation MUST carry the rules/1 minor version it was compiled against; an executor asked to evaluate a generation compiled against a rules/1 major it does not implement MUST refuse rather than evaluate it under a mismatched vocabulary — the wire-level mechanics of that refusal (and any N−1 tolerance) belong to the generation's own transport contract (`relay/1`); rules/1's role ends at declaring the version tag every compiled generation carries.

## Error taxonomy

| code | meaning | retryable |
|---|---|---|
| `UNKNOWN_VOCABULARY_MEMBER` | A trigger/condition/action `type`, or a rule's `mode`, is not in this contract's closed vocabulary. | no |
| `UNKNOWN_FILTER` | An Expression pipeline references a filter not in RUL-290. | no |
| `EDGE_EXPRESSION_CROSS_ENTITY_REFERENCE` | An edge-classified rule's Expression sources an entity other than its trigger's subject (RUL-282). | no |
| `ENTITY_REF_AMBIGUOUS` | An `EntityRef` declares more than one of `entity_id`/`selector`/`device_class`, or none. | no |
| `CHOOSE_BRANCH_RECURSION` | A `choose` action's branch actions recurse into the same `choose` action (RUL-180). | no |
| `TYPE_MISMATCH_STATIC` | A numeric or typed comparison is declared against an attribute whose registry-declared type cannot satisfy it (RUL-261). | no |
| `TEMPLATE_PARAM_UNBOUND` | A rule-template instantiation leaves a declared parameter position unbound (RUL-250). | no |
| `MODE_MAX_MISSING` | `mode: "parallel"` is declared without `max` (RUL-244). | no |
| `MODE_MAX_NOT_APPLICABLE` | `max` is declared with a non-null value while `mode` is not `parallel` (RUL-244). | no |
| `PRESET_NOT_FOUND` | A `preset_batch` action's `preset_id` does not resolve to an existing preset row at compile time. | no |

## Conformance notes

- Traceability map: `conformance/traceability/rules-1.md` — maps every `RUL-NNN` above to the case(s) that exercise it.
- Corpus: `conformance/corpora/rules-1/` — one JSON case file per `case-id` referenced from the traceability map; several cases are recast from a sanitized field-trace corpus (an informative seed, not an oracle for this contract's semantics — every case's `expected` outcome is derived from the normative text above, not replayed from a prior implementation).
- The device-class registry's own content (state vocabularies, semantic-group membership, typed attribute declarations) is host/companion-document configuration, not enumerated by this contract; corpus cases use a small fixed fixture registry documented beside the corpus, the same convention `manifest/1`'s corpus notes establish.
- Timing-dependent behavior (`for`-holds, stabilization windows, delay durations) is exercised against an injectable/fake clock in the driver harness, not wall-clock sleeps in a static corpus — the JSON cases here assert sequencing and outcome, not elapsed real time.
- The platform's label-selector grammar's own concrete syntax is out of scope; corpus cases exercising `EntityRef` selector resolution treat the resolved entity set as a given input rather than parsing a selector string.
