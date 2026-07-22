import { describe, expect, it } from "vitest";
import {
  collectVocabLabels,
  evalBindingExpr,
  formatDuration,
  makeMessageResolver,
  resolveOptions,
  resolvePath,
  resolvePathWithLoc,
  resolveWriteLocation,
  toDisplay,
  type RenderEnv,
  type RenderScope,
} from "./bindings";

// The corpus is the oracle: the predicate-index (UIS-101) and vocab-option
// (UIS-132) cases are loaded straight from the frozen corpus, never copied.
interface CorpusCase {
  case_id: string;
  input: Record<string, unknown>;
  expected: Record<string, unknown>;
}
const modules = import.meta.glob<CorpusCase>("../../../conformance/corpora/ui-schema-1/*.json", {
  eager: true,
  import: "default",
});
const corpus = Object.fromEntries(
  Object.values(modules).map((c) => [c.case_id, c]),
) as Record<string, CorpusCase>;

const env: RenderEnv = { msg: makeMessageResolver(), locale: "en-US", vocabLabels: {} };

function scopeOver(data: Record<string, unknown>): RenderScope {
  return {
    root: data,
    current: data,
    currentPath: [],
    currentTree: "resource",
    ui: (data.$ui as Record<string, unknown>) ?? {},
    context: {},
  };
}

describe("bindings — data paths (UIS-100/103–107)", () => {
  const data = {
    site: { displayName: "The Hangar", quietHours: true, tags: ["lobby", "cafe"] },
    rows: [
      { id: "a", name: "Alpha" },
      { id: "b", name: "Beta" },
    ],
    $ui: { selected: "b" },
  };

  it("resolves an unprefixed path against the current scope", () => {
    const scope: RenderScope = { ...scopeOver(data), current: data.site };
    expect(resolvePath("displayName", scope)).toBe("The Hangar");
    expect(resolvePath("quietHours", scope)).toBe(true);
  });

  it("resolves $root regardless of a narrowed current scope (UIS-103)", () => {
    const scope: RenderScope = { ...scopeOver(data), current: data.site };
    expect(resolvePath("$root.rows[0].name", scope)).toBe("Alpha");
  });

  it("resolves $ui ephemeral state (UIS-104)", () => {
    expect(resolvePath("$ui.selected", scopeOver(data))).toBe("b");
  });

  it("resolves $context feeds (UIS-105/151)", () => {
    const scope: RenderScope = { ...scopeOver(data), context: { presets: data.rows } };
    expect(resolvePath("$context.presets[1].name", scope)).toBe("Beta");
  });

  it("resolves an array index and $index inside an item scope (UIS-107)", () => {
    // Inside a repeat item the renderer narrows `current` to the element, and the
    // itemScope name addresses that same element with `.$index` for its position.
    const scope: RenderScope = {
      ...scopeOver(data),
      current: data.rows[1],
      item: { name: "item", value: data.rows[1], index: 1, arrayPath: ["rows"], arrayTree: "resource" },
    };
    expect(resolvePath("item.name", scope)).toBe("Beta");
    expect(resolvePath("item.$index", scope)).toBe(1);
    // An unprefixed segment also resolves against the item element (UIS-107).
    expect(resolvePath("name", scope)).toBe("Beta");
  });

  it("resolves a bare array index outside an item scope (UIS-100)", () => {
    expect(resolvePath("rows[0].name", scopeOver(data))).toBe("Alpha");
  });
});

describe("bindings — predicate-index (UIS-101, corpus oracle)", () => {
  const c = corpus["UIS-101-valid-predicate-index-binding"];
  const data = c.input.data as Record<string, unknown>;
  const binding = c.input.binding as string;

  it("selects the array element whose field equals the resolved predicate value", () => {
    const resolved = resolvePath(binding, scopeOver(data));
    expect(resolved).toEqual(c.expected.resolved);
  });

  it("resolves to null when the predicate matches zero elements", () => {
    const missed = { ...data, $ui: { selected: "does-not-exist" } };
    expect(resolvePath(binding, scopeOver(missed))).toBeNull();
  });

  it("locates the matched element for an editable root scope (UIS-005)", () => {
    const { value, loc } = resolvePathWithLoc(binding, scopeOver(data));
    expect(value).toEqual(c.expected.resolved);
    expect(loc).toEqual(["automations", 1]);
  });
});

describe("bindings — computed values (UIS-140)", () => {
  const data = {
    screens: [{}, {}, {}],
    automations: [{ id: "x" }],
    empty: [],
    obj: { or: [], type: "state" },
    mode: "parallel",
    created_at: "2026-07-22T10:00:00Z",
    price: 1234.5,
  };
  const scope = scopeOver(data);

  const compute = (compute: string, args: unknown[]) => evalBindingExpr({ compute, args }, scope, env);

  it("count / isEmpty over a bound array", () => {
    expect(compute("count", ["screens"])).toBe(3);
    expect(compute("count", ["missing"])).toBe(0);
    expect(compute("isEmpty", ["empty"])).toBe(true);
    expect(compute("isEmpty", ["screens"])).toBe(false);
  });

  it("eq with a {lit} string escape and a Binding operand (UIS-108)", () => {
    expect(compute("eq", ["mode", { lit: "parallel" }])).toBe(true);
    expect(compute("eq", ["mode", { lit: "single" }])).toBe(false);
  });

  it("not / and / or", () => {
    expect(compute("not", [{ lit: false }])).toBe(true);
    expect(compute("and", [{ lit: true }, "mode"])).toBe(true);
    expect(compute("or", ["empty", "mode"])).toBe(true);
  });

  it("join over a Binding array with a {lit} separator", () => {
    const scope2 = scopeOver({ tags: ["a", "b", "c"] });
    expect(evalBindingExpr({ compute: "join", args: ["tags", { lit: ", " }] }, scope2, env)).toBe("a, b, c");
  });

  it("firstKey discriminates a union by present key (UIS-142)", () => {
    expect(compute("firstKey", ["obj", ["and", "or", "not", "type"]])).toBe("or");
  });

  it("formatNumber / formatDate are locale-formatted (UIS-143)", () => {
    expect(compute("formatNumber", ["price"])).toBe("1,234.5");
    // A medium en-US date rendering of the ISO value (locale-formatted, not raw).
    expect(String(compute("formatDate", ["created_at"]))).toMatch(/2026/);
    expect(String(compute("formatDate", ["created_at"]))).not.toBe(data.created_at);
  });

  it("label resolves a value against a collected vocab label map (UIS-140)", () => {
    const withLabels: RenderEnv = {
      ...env,
      vocabLabels: { "rules/1:mode": { parallel: "msg:mode.parallel" } },
    };
    expect(evalBindingExpr({ compute: "label", args: ["rules/1:mode", "mode"] }, scope, withLabels)).toBe("Parallel");
  });
});

describe("bindings — formatDuration", () => {
  it("renders whole seconds as h/m/s, omitting zero leading units", () => {
    expect(formatDuration(0)).toBe("0s");
    expect(formatDuration(90)).toBe("1m 30s");
    expect(formatDuration(3661)).toBe("1h 1m 1s");
  });
});

describe("bindings — option sources (UIS-130–133, corpus oracle)", () => {
  it("resolves a complete vocab OptionSource in the vocabRef's declared order (UIS-132)", () => {
    const c = corpus["UIS-132-valid-vocab-option-source"];
    const doc = c.input as Record<string, unknown>;
    const sections = doc.sections as Array<{ fields: Array<{ props: { options: unknown } }> }>;
    const optionSource = sections[0].fields[0].props.options;
    const envWithLabels: RenderEnv = { ...env, vocabLabels: collectVocabLabels(doc) };
    const opts = resolveOptions(optionSource, scopeOver({}), envWithLabels);
    expect(opts.map((o) => o.value)).toEqual(["single", "restart", "queued", "parallel"]);
    expect(opts.map((o) => o.label)).toEqual(["Single", "Restart", "Queued", "Parallel"]);
  });

  it("resolves a data OptionSource against a context feed (UIS-133)", () => {
    const scope: RenderScope = {
      ...scopeOver({}),
      context: { presets: [{ id: "p1", name: "Morning" }, { id: "p2", name: "Evening" }] },
    };
    const opts = resolveOptions(
      { kind: "data", source: "$context.presets", valuePath: "id", labelPath: "name" },
      scope,
      env,
    );
    expect(opts).toEqual([
      { value: "p1", label: "Morning" },
      { value: "p2", label: "Evening" },
    ]);
  });
});

describe("bindings — write locations (UIS-065/104/160)", () => {
  it("locates an unprefixed input write within the current scope", () => {
    const scope: RenderScope = {
      root: {},
      current: {},
      currentPath: ["site"],
      currentTree: "resource",
      ui: {},
      context: {},
    };
    expect(resolveWriteLocation("displayName", scope)).toEqual({ tree: "resource", loc: ["site", "displayName"] });
  });

  it("locates a $ui set target regardless of the current tree (UIS-104)", () => {
    const scope: RenderScope = {
      root: {},
      current: {},
      currentPath: ["draft"],
      currentTree: "ui",
      ui: {},
      context: {},
    };
    expect(resolveWriteLocation("$ui.selected", scope)).toEqual({ tree: "ui", loc: ["selected"] });
  });

  it("refuses a read-only $context write target (UIS-152)", () => {
    expect(resolveWriteLocation("$context.presets", scopeOver({}))).toBeNull();
  });
});

describe("bindings — toDisplay", () => {
  it("renders primitives verbatim and structures as JSON", () => {
    expect(toDisplay("hi")).toBe("hi");
    expect(toDisplay(42)).toBe("42");
    expect(toDisplay(null)).toBe("");
    expect(toDisplay({ a: 1 })).toBe('{"a":1}');
  });
});
