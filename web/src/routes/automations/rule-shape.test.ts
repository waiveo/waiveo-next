import { describe, expect, it } from "vitest";
import {
  hydrateForBuilder,
  normalizeModeMax,
  pruneBuilderScaffolding,
  ruleBodyJson,
  ruleBodyOf,
  ruleSaveBody,
  ruleUpdateFrom,
  type RuleBody,
} from "./rule-shape";
import { automation } from "@/api/test-support";
import type { Automation } from "@/api";

// The two seams the declarative builder cannot cover on its own, unit-tested where
// their invariants live. The route test proves they hold END TO END (a Channel
// field that writes into a params container that was never on the wire); these
// cases pin the exact algebra, including the one property that makes the pair safe
// to run on every load and every save: hydrate ∘ prune is the identity on any rule
// that has no empty scaffolding container of its own.

function body(over: Partial<RuleBody> = {}): RuleBody {
  return { mode: "single", max: null, triggers: [], conditions: [], actions: [], ...over };
}

describe("hydrateForBuilder", () => {
  it("seeds the nested containers the builder binds through, per node class", () => {
    const hydrated = hydrateForBuilder(
      body({
        triggers: [{ type: "time", at: "07:00:00" }],
        conditions: [{ type: "state", entity_id: "e1", state: ["on"] }],
        actions: [{ type: "log", message: "hi" }],
      }),
    );
    // A trigger can become `template` (item.expression.expr) without any host code
    // running in between, so every trigger carries the container.
    expect(hydrated.triggers[0]).toEqual({ type: "time", at: "07:00:00", expression: {} });
    expect(hydrated.conditions[0]).toEqual({
      type: "state",
      entity_id: "e1",
      state: ["on"],
      expression: {},
      after: {},
      before: {},
    });
    expect(hydrated.actions[0]).toEqual({ type: "log", message: "hi", params: {} });
  });

  it("never overwrites a container that already has a value of any shape", () => {
    const hydrated = hydrateForBuilder(
      body({
        // A `time` condition's after/before are STRINGS, not the sun condition's
        // {event, offset} objects — the seed must not flatten them to {}.
        conditions: [{ type: "time", after: "18:00:00", before: "23:00:00" }],
        actions: [{ type: "device_command", entity_id: "e1", command: "launch", params: { channel: "dev" } }],
      }),
    );
    expect(hydrated.conditions[0]).toMatchObject({ after: "18:00:00", before: "23:00:00" });
    expect(hydrated.actions[0]).toMatchObject({ params: { channel: "dev" } });
  });

  it("descends condition compositions and a choose action's branches", () => {
    const hydrated = hydrateForBuilder(
      body({
        conditions: [{ and: [{ or: [{ not: { type: "variable", variable: "v", equals: 1 } }] }] }],
        actions: [
          {
            type: "choose",
            branches: [{ condition: { type: "state", entity_id: "e1" }, actions: [{ type: "log", message: "x" }] }],
            default: [{ type: "device_command", entity_id: "e1", command: "home" }],
          },
        ],
      }),
    );
    const leaf = ((hydrated.conditions[0] as Record<string, never[]>).and[0] as Record<string, never[]>).or[0] as Record<
      string,
      Record<string, unknown>
    >;
    expect(leaf.not).toMatchObject({ expression: {}, after: {}, before: {} });
    const choose = hydrated.actions[0] as Record<string, Record<string, unknown>[]>;
    // The composition node itself gets nothing — it has no leaf fields to bind.
    expect(choose).not.toHaveProperty("params");
    expect(choose.branches[0].condition).toMatchObject({ expression: {} });
    expect(choose.branches[0].actions).toEqual([{ type: "log", message: "x", params: {} }]);
    expect(choose.default[0]).toMatchObject({ params: {} });
  });

  it("does not mutate the record it was given", () => {
    const source = body({ actions: [{ type: "log", message: "hi" }] });
    hydrateForBuilder(source);
    expect(source.actions[0]).toEqual({ type: "log", message: "hi" });
  });
});

describe("pruneBuilderScaffolding", () => {
  it("is the exact inverse of hydrate for a rule with nothing filled in", () => {
    const original = body({
      triggers: [{ type: "sun", event: "sunset", offset: -600 }],
      conditions: [{ and: [{ type: "time", after: "18:00:00" }] }],
      actions: [{ type: "delay", duration_seconds: 5 }],
    });
    expect(pruneBuilderScaffolding(hydrateForBuilder(original))).toEqual(original);
  });

  it("keeps every container the operator actually filled", () => {
    const filled = hydrateForBuilder(body({ actions: [{ type: "device_command", entity_id: "e1", command: "launch" }] }));
    (filled.actions[0] as Record<string, Record<string, string>>).params.channel = "waiveo";
    expect(pruneBuilderScaffolding(filled).actions[0]).toEqual({
      type: "device_command",
      entity_id: "e1",
      command: "launch",
      params: { channel: "waiveo" },
    });
  });

  it("leaves alone any key that is not one of the seeded container names", () => {
    // An empty object somewhere the builder never seeds is the author's own data.
    const kept = body({ triggers: [{ type: "event", event: "x", match: {} }] });
    expect(pruneBuilderScaffolding(kept)).toEqual(kept);
  });
});

describe("normalizeModeMax (RUL-244)", () => {
  it("clears a stale max the moment the mode is not parallel", () => {
    expect(normalizeModeMax(body({ mode: "single", max: 3 })).max).toBeNull();
    expect(normalizeModeMax(body({ mode: "queued", max: 3 })).max).toBeNull();
  });

  it("keeps a parallel rule's max, and does not invent one when it is absent", () => {
    expect(normalizeModeMax(body({ mode: "parallel", max: 4 })).max).toBe(4);
    // No safe default concurrency cap exists to fabricate — the compiler's own
    // MODE_MAX_MISSING is what the operator must see.
    expect(normalizeModeMax(body({ mode: "parallel", max: null })).max).toBeNull();
  });
});

describe("the rule projection", () => {
  it("ruleBodyOf takes the rules/1 vocabulary and nothing from the resource envelope", () => {
    const a = automation({ name: "n", revision: 7 }) as unknown as Automation;
    expect(Object.keys(ruleBodyOf(a)).sort()).toEqual(["actions", "conditions", "max", "mode", "triggers"]);
    expect(JSON.parse(ruleBodyJson(a))).toEqual(ruleBodyOf(a));
  });

  it("ruleUpdateFrom takes only the rule keys present, and refuses a non-object", () => {
    expect(ruleUpdateFrom({ actions: [{ type: "log" }] })).toEqual({ actions: [{ type: "log" }] });
    expect(ruleUpdateFrom({ mode: "restart", nonsense: 1 })).toEqual({ mode: "restart" });
    expect(ruleUpdateFrom([1, 2])).toBeNull();
    expect(ruleUpdateFrom("nope")).toBeNull();
    expect(ruleUpdateFrom(null)).toBeNull();
  });

  it("ruleSaveBody prunes, couples mode/max, and carries the name the builder edits", () => {
    const loaded = automation({ name: "Open at dawn", mode: "parallel", max: 2 }) as unknown as Automation;
    const edited = hydrateForBuilder(loaded);
    expect(ruleSaveBody(edited)).toEqual({
      name: "Open at dawn",
      mode: "parallel",
      max: 2,
      // The rule that went in, not the hydrated copy the builder held.
      triggers: loaded.triggers,
      conditions: [],
      actions: loaded.actions,
    });
    // …and the scaffolding is gone: the hydrated copy carried `expression: {}` on
    // its trigger, the saved body does not.
    expect(JSON.stringify(ruleSaveBody(edited))).not.toContain("expression");
  });
});
