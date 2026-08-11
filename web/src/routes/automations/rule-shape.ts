// The rules/1 rule body, as the declarative rule BUILDER needs to see it.
//
// The Automations page authors a rule through the ui-schema/1 renderer: every
// trigger/condition/action field is an ordinary `bind` into the automation record
// the renderer holds, and Save hands that record back through the `submit` seam.
// That gives the round-trip its most important property for free — the builder
// edits the REAL objects in place, so a member it has no editor for (a `choose`
// action's branches, an `event` trigger's `match` map, a key from a newer rules/1)
// rides through an edit untouched. Nothing is rebuilt from a model the builder
// understands, so nothing the builder does not understand can be dropped.
//
// Two seams that property does NOT give for free live here.
//
// # 1. Nested bind targets need their container to exist (hydrate/prune)
//
// A ui-schema Binding resolves a WRITE location by walking the record; the walk
// bails the moment it steps onto a missing intermediate (bindings.ts
// `resolvePathWithLoc` returns `loc: null` once `cursor == null`), and a write to
// a null location is silently dropped. So a `text-input` bound to
// `item.params.channel` on a `device_command` that carries no `params` key is a
// control that accepts typing and performs nothing — precisely the dead-end this
// codebase keeps re-growing.
//
// The fix is symmetric and total: `hydrateForBuilder` seeds an empty object at
// every nested container the builder binds THROUGH, and `pruneBuilderScaffolding`
// removes any of those same containers that is still an empty object when the rule
// is saved. Seeding is per node CLASS (trigger / condition / leaf action), not per
// `type`, because the operator changes an item's `type` inside the builder — a
// `log` action re-typed to `device_command` must find its `params` container
// already there, since no host code runs between the kind select's write and the
// channel field's. The cost is a transient `{}` on nodes that will never use it,
// and prune erases exactly those again before the PATCH.
//
// Round-trip fidelity: the ONLY wire difference this can produce is dropping a
// container the server itself sent as an empty object (`"params": {}`), which is
// semantically identical to its absence in rules/1 (RUL-160/RUL-231 `params` is
// MAY-declared; an empty map declares nothing). Any container with content in it
// is kept verbatim.
//
// # 2. mode/max is a coupled pair the compiler rejects out of step (RUL-244)
//
// `parallel` REQUIRES `max`; every other mode requires its ABSENCE. The builder
// shows `max` only under `parallel` (`visibleIf`), so switching parallel → single
// leaves a stale `max` behind in the record that the compiler would reject with
// MODE_MAX_NOT_APPLICABLE. `normalizeModeMax` clears it on the way out. The other
// half — `parallel` with no `max` — is deliberately NOT invented here: there is no
// safe default concurrency cap to fabricate, so the compiler's own message is what
// the operator sees.

import type { Automation, AutomationUpdate } from "@/api";

/** The rules/1 vocabulary keys of an automation — the authored rule itself, as
 * distinct from the api/1 resource envelope (identity, placement, revision). */
export interface RuleBody {
  mode: Automation["mode"];
  max: Automation["max"];
  triggers: Automation["triggers"];
  conditions: Automation["conditions"];
  actions: Automation["actions"];
}

type Node = Record<string, unknown>;

function isPlainObject(v: unknown): v is Node {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

function isEmptyObject(v: unknown): boolean {
  return isPlainObject(v) && Object.keys(v).length === 0;
}

function nodesOf(v: unknown): Node[] {
  return Array.isArray(v) ? v.filter(isPlainObject) : [];
}

/** Deep structural clone, so hydration never mutates the caller's record (the one
 * the route also holds as its own last-read server state). */
function clone<T>(v: T): T {
  return JSON.parse(JSON.stringify(v)) as T;
}

// The nested containers the builder document binds THROUGH, per node class. Each
// name is a key some `bind` path in page.uis.json crosses on its way to a leaf
// field — keep these two in step: a bind path with a new intermediate and no entry
// here is a control that types into nothing.
const TRIGGER_CONTAINERS = ["expression"] as const; // template: item.expression.expr
const CONDITION_CONTAINERS = ["expression", "after", "before"] as const; // template + sun bounds
const ACTION_CONTAINERS = ["params"] as const; // device_command: item.params.<name>

/** Seed `keys` on `node` with an empty object wherever the key is absent. An
 * existing value of ANY shape is left alone — a `time` condition's `after` is the
 * string `"08:00:00"`, and a write through it (`after.event`, after a switch to
 * `sun`) resolves fine because the intermediate merely has to exist. */
function seed(node: Node, keys: readonly string[]): void {
  for (const key of keys) {
    if (!(key in node)) node[key] = {};
  }
}

/** Drop any of `keys` still holding an empty object — the exact inverse of `seed`
 * for a container the operator never filled in. */
function unseed(node: Node, keys: readonly string[]): void {
  for (const key of keys) {
    if (isEmptyObject(node[key])) delete node[key];
  }
}

/** Walk a condition tree (leaf, or an `and`/`or`/`not` composition, RUL-100),
 * applying `visitLeaf` to every leaf. Compositions carry no bindable leaf fields
 * of their own — only their children do. */
function walkConditions(nodes: Node[], visitLeaf: (leaf: Node) => void): void {
  for (const node of nodes) {
    if (Array.isArray(node.and)) walkConditions(nodesOf(node.and), visitLeaf);
    else if (Array.isArray(node.or)) walkConditions(nodesOf(node.or), visitLeaf);
    else if (isPlainObject(node.not)) walkConditions([node.not], visitLeaf);
    else visitLeaf(node);
  }
}

/** Walk an action list, applying `visitLeaf` to every leaf action and descending a
 * `choose` action's branches (their guard conditions AND their actions) plus its
 * `default` list (RUL-180) — a nested action is edited by the same JSON view, but
 * it must survive hydrate/prune with the same fidelity as a top-level one. */
function walkActions(nodes: Node[], visitLeaf: (leaf: Node) => void, visitCondition: (leaf: Node) => void): void {
  for (const node of nodes) {
    if (node.type === "choose") {
      for (const branch of nodesOf(node.branches)) {
        if (isPlainObject(branch.condition)) walkConditions([branch.condition], visitCondition);
        walkActions(nodesOf(branch.actions), visitLeaf, visitCondition);
      }
      walkActions(nodesOf(node.default), visitLeaf, visitCondition);
      continue;
    }
    visitLeaf(node);
  }
}

/**
 * A copy of `automation` whose nested builder-bind containers all exist, ready to
 * be handed to the renderer as bound data. See this module's header for why the
 * seeding is per node class rather than per `type`.
 */
export function hydrateForBuilder<T extends { triggers: unknown; conditions: unknown; actions: unknown }>(
  automation: T,
): T {
  const copy = clone(automation);
  for (const trigger of nodesOf(copy.triggers)) seed(trigger, TRIGGER_CONTAINERS);
  walkConditions(nodesOf(copy.conditions), (leaf) => seed(leaf, CONDITION_CONTAINERS));
  walkActions(
    nodesOf(copy.actions),
    (leaf) => seed(leaf, ACTION_CONTAINERS),
    (leaf) => seed(leaf, CONDITION_CONTAINERS),
  );
  return copy;
}

/**
 * The inverse of `hydrateForBuilder` over a rule body: every seeded container the
 * operator left empty is removed again, so the PATCH carries the rule the operator
 * authored and not the builder's scaffolding. Returns a new body; the input is
 * untouched.
 */
export function pruneBuilderScaffolding(body: RuleBody): RuleBody {
  const copy = clone(body);
  for (const trigger of nodesOf(copy.triggers)) unseed(trigger, TRIGGER_CONTAINERS);
  walkConditions(nodesOf(copy.conditions), (leaf) => unseed(leaf, CONDITION_CONTAINERS));
  walkActions(
    nodesOf(copy.actions),
    (leaf) => unseed(leaf, ACTION_CONTAINERS),
    (leaf) => unseed(leaf, CONDITION_CONTAINERS),
  );
  return copy;
}

/**
 * Apply RUL-244's mode/max coupling to a rule body about to be saved: any mode but
 * `parallel` carries a null `max`. A `parallel` rule missing its `max` is left
 * alone on purpose — see this module's header.
 */
export function normalizeModeMax(body: RuleBody): RuleBody {
  return body.mode === "parallel" ? body : { ...body, max: null };
}

/** The rules/1 projection of an automation — what the JSON escape hatch shows and
 * what a save sends. Identity/placement/revision are NOT the rule. */
export function ruleBodyOf(a: Automation): RuleBody {
  return {
    mode: a.mode,
    max: a.max ?? null,
    triggers: a.triggers,
    conditions: a.conditions,
    actions: a.actions,
  };
}

/** The same projection, pretty-printed for the raw-JSON escape hatch. */
export function ruleBodyJson(a: Automation): string {
  return JSON.stringify(ruleBodyOf(a), null, 2);
}

/**
 * Read a rule body out of arbitrary parsed JSON (the escape hatch's input), taking
 * only the rules/1 vocabulary keys that are present. Returns null when the value is
 * not a JSON object — an array or a scalar is not a rule.
 *
 * Only PRESENT keys are copied: pasting `{"actions": [...]}` alone edits the
 * actions and leaves the rest of the rule as the builder holds it.
 */
export function ruleUpdateFrom(parsed: unknown): AutomationUpdate | null {
  if (!isPlainObject(parsed)) return null;
  const patch: AutomationUpdate = {};
  // Cast through the (required, non-optional) Automation field types so no
  // assigned value carries `undefined` — exactOptionalPropertyTypes forbids
  // assigning an explicit undefined to AutomationUpdate's optional keys.
  if ("mode" in parsed) patch.mode = parsed.mode as Automation["mode"];
  if ("max" in parsed) patch.max = parsed.max as Automation["max"];
  if ("triggers" in parsed) patch.triggers = parsed.triggers as Automation["triggers"];
  if ("conditions" in parsed) patch.conditions = parsed.conditions as Automation["conditions"];
  if ("actions" in parsed) patch.actions = parsed.actions as Automation["actions"];
  return patch;
}

/**
 * The rule PATCH body for a record the builder just handed back through `submit`:
 * the rules/1 projection, with the builder's scaffolding pruned and RUL-244's
 * mode/max coupling applied. `name` rides along because the builder edits it in the
 * same form.
 */
export function ruleSaveBody(resource: Automation): AutomationUpdate {
  const body = normalizeModeMax(pruneBuilderScaffolding(ruleBodyOf(resource)));
  return { name: resource.name, ...body };
}
