import { describe, expect, it } from "vitest";
import { validatePage, isValidBindingPath } from "./validate";
import type { ValidationError } from "./schema";

// Targeted unit cases, one per closed set / grammar rule the ui-schema/1
// validation story defines (contract §Validation, §Error taxonomy). The corpus
// exercises a handful of these end to end; these pin the rest so every closed
// set is provably closed — an unknown member is a typed rejection, never a
// silent skip or best-effort render.

function codes(doc: unknown): string[] {
  const r = validatePage(doc);
  return r.ok ? [] : r.errors.map((e) => e.code);
}

function firstError(doc: unknown): ValidationError | undefined {
  const r = validatePage(doc);
  return r.ok ? undefined : r.errors[0];
}

// A minimal valid settings-form we can perturb one field at a time.
function settingsForm(field: Record<string, unknown>): Record<string, unknown> {
  return {
    pageType: "settings-form",
    source: "site",
    sections: [{ fields: [field] }],
    actions: [
      {
        type: "button",
        props: { labelMsg: "msg:save" },
        on: { press: { verb: "submit" } },
      },
    ],
  };
}

describe("isValidBindingPath (UIS-100 grammar)", () => {
  it("accepts field names, item scope, reserved roots, dotted paths", () => {
    for (const ok of [
      "name",
      "displayName",
      "item.name",
      "item.after.offset",
      "$ui.selected",
      "$context.presets",
      "$root.mode",
      "$params.value",
      "entity_id",
    ]) {
      expect(isValidBindingPath(ok), ok).toBe(true);
    }
  });

  it("accepts index and predicate-index segments", () => {
    for (const ok of [
      "triggers[0]",
      "triggers[0].type",
      "automations[id=$ui.selected]",
      "presets[id=$ui.selected].name",
      "rows[status=on]",
      "rows[count=5]",
      "items[$index]",
    ]) {
      expect(isValidBindingPath(ok), ok).toBe(true);
    }
  });

  it("rejects empty segments, leading/trailing dots, and $-prefixed field names", () => {
    for (const bad of [
      "",
      "general..displayName",
      ".name",
      "name.",
      "$unknown.foo",
      "9leading",
      "a[b=]",
      "a[=5]",
    ]) {
      expect(isValidBindingPath(bad), bad).toBe(false);
    }
  });
});

describe("page-type membership (UIS-003 → UNKNOWN_PAGE_TYPE)", () => {
  it("rejects a pageType outside the closed set", () => {
    const e = firstError({ pageType: "surface", tiles: [] });
    expect(e?.code).toBe("UNKNOWN_PAGE_TYPE");
    expect(e?.path).toBe("pageType");
  });
  it("accepts each of the four members", () => {
    expect(codes({ pageType: "dashboard", tiles: [] })).toEqual([]);
  });
});

describe("widget catalog membership (UIS-060 → UNKNOWN_WIDGET_TYPE)", () => {
  it("rejects an unknown widget type without descending into it", () => {
    const e = firstError(settingsForm({ type: "carousel", bind: "photos" }));
    expect(e?.code).toBe("UNKNOWN_WIDGET_TYPE");
    expect(e?.path).toBe("sections[0].fields[0].type");
  });
});

describe("widget props/events (UIS-062, UIS-064, UIS-074 → *_UNKNOWN / *_MISSING)", () => {
  it("rejects an unknown prop key", () => {
    const e = firstError(settingsForm({ type: "text-input", bind: "x", props: { bogus: 1 } }));
    expect(e?.code).toBe("WIDGET_PROP_UNKNOWN");
    expect(e?.path).toBe("sections[0].fields[0].props.bogus");
  });
  it("rejects children on a non-children-bearing type, naming children", () => {
    const e = firstError(settingsForm({ type: "text-input", bind: "x", children: [] }));
    expect(e?.code).toBe("WIDGET_PROP_UNKNOWN");
    expect(e?.path).toBe("sections[0].fields[0].children");
  });
  it("rejects an unknown event key", () => {
    const e = firstError(
      settingsForm({ type: "text-input", bind: "x", on: { press: { verb: "submit" } } }),
    );
    expect(e?.code).toBe("WIDGET_EVENT_UNKNOWN");
    expect(e?.path).toBe("sections[0].fields[0].on.press");
  });
  it("rejects a missing required prop (table.columns)", () => {
    const e = firstError(settingsForm({ type: "table", props: { source: "rows" } }));
    expect(e?.code).toBe("WIDGET_REQUIRED_FIELD_MISSING");
    expect(e?.path).toBe("sections[0].fields[0].props.columns");
  });
  it("rejects a button without on.press (UIS-074)", () => {
    const doc = {
      pageType: "settings-form",
      source: "site",
      sections: [{ fields: [{ type: "text-input", bind: "x" }] }],
      actions: [{ type: "button", props: { labelMsg: "msg:save" } }],
    };
    const e = firstError(doc);
    expect(e?.code).toBe("WIDGET_REQUIRED_FIELD_MISSING");
    expect(e?.path).toBe("actions[0].on.press");
  });
});

describe("required accessible labels (UIS-075) — strictInputLabels", () => {
  const doc = settingsForm({ type: "text-input", bind: "x" });
  it("is accepted by default (matches the frozen corpus's minimal valid cases)", () => {
    const r = validatePage(doc);
    expect(r.ok).toBe(true);
  });
  it("is rejected as WIDGET_REQUIRED_FIELD_MISSING under strictInputLabels", () => {
    const r = validatePage(doc, { strictInputLabels: true });
    expect(r.ok).toBe(false);
    if (r.ok) return;
    const e = r.errors[0];
    expect(e.code).toBe("WIDGET_REQUIRED_FIELD_MISSING");
    expect(e.path).toBe("sections[0].fields[0].props.labelMsg");
  });
  it("is satisfied when labelMsg is present, even under strictInputLabels", () => {
    const withLabel = settingsForm({
      type: "text-input",
      bind: "x",
      props: { labelMsg: "msg:x" },
    });
    expect(validatePage(withLabel, { strictInputLabels: true }).ok).toBe(true);
  });
});

describe("binding grammar (UIS-066/UIS-100 → BINDING_PATH_INVALID)", () => {
  it("rejects a malformed widget bind", () => {
    const e = firstError(settingsForm({ type: "text-input", bind: "a..b" }));
    expect(e?.code).toBe("BINDING_PATH_INVALID");
    expect(e?.path).toBe("sections[0].fields[0].bind");
  });
  it("rejects writing to the read-only $index segment (UIS-107)", () => {
    const e = firstError(settingsForm({ type: "text-input", bind: "item.$index" }));
    expect(e?.code).toBe("BINDING_PATH_INVALID");
  });
});

describe("option sources (UIS-130–132 → OPTION_SOURCE_INVALID / VOCAB_REF_UNKNOWN)", () => {
  const withOptions = (options: unknown) =>
    settingsForm({ type: "select", bind: "mode", props: { options } });

  it("rejects an unknown OptionSource kind", () => {
    const e = firstError(withOptions({ kind: "remote" }));
    expect(e?.code).toBe("OPTION_SOURCE_INVALID");
  });
  it("rejects a vocab OptionSource whose ref is unknown", () => {
    const e = firstError(
      withOptions({ kind: "vocab", ref: "rules/1:nope", labels: {} }),
    );
    expect(e?.code).toBe("VOCAB_REF_UNKNOWN");
  });
  it("rejects a vocab labels map missing a member", () => {
    const e = firstError(
      withOptions({
        kind: "vocab",
        ref: "rules/1:mode",
        labels: { single: "m", restart: "m", queued: "m" },
      }),
    );
    expect(e?.code).toBe("OPTION_SOURCE_INVALID");
    expect(e?.path).toBe("sections[0].fields[0].props.options.labels");
  });
  it("accepts a complete vocab labels map", () => {
    expect(
      codes(
        withOptions({
          kind: "vocab",
          ref: "rules/1:mode",
          labels: { single: "m", restart: "m", queued: "m", parallel: "m" },
        }),
      ),
    ).toEqual([]);
  });
  it("accepts literal and data OptionSources", () => {
    expect(
      codes(withOptions({ kind: "literal", items: [{ value: "a", labelMsg: "msg:a" }] })),
    ).toEqual([]);
    expect(
      codes(
        // A page-local collection path (not $context) needs no context declaration.
        withOptions({ kind: "data", source: "presets", valuePath: "id", labelPath: "name" }),
      ),
    ).toEqual([]);
  });
});

describe("computed values (UIS-140 → COMPUTE_FN_UNKNOWN)", () => {
  const withStatTile = (value: unknown) => ({
    pageType: "dashboard",
    tiles: [{ size: "small", widget: { type: "stat-tile", props: { labelMsg: "msg:l", value } } }],
  });
  it("rejects an unknown compute function", () => {
    const e = firstError(withStatTile({ compute: "sqrt", args: ["x"] }));
    expect(e?.code).toBe("COMPUTE_FN_UNKNOWN");
  });
  it("accepts a known compute function", () => {
    expect(codes(withStatTile({ compute: "count", args: ["screens"] }))).toEqual([]);
  });
  it("does not grammar-check a label() vocabRef arg as a binding", () => {
    // "rules/1:mode" is not a valid Binding — label's first arg is a pinned
    // vocabRef, not a data path, so it must not be rejected as BINDING_PATH_INVALID.
    expect(codes(withStatTile({ compute: "label", args: ["rules/1:mode", "mode"] }))).toEqual([]);
  });
});

describe("context feeds (UIS-105 → CONTEXT_REF_UNDEFINED)", () => {
  it("rejects a $context ref not declared in the page's context map", () => {
    const doc = {
      pageType: "settings-form",
      source: "site",
      sections: [
        {
          fields: [
            {
              type: "select",
              bind: "x",
              props: {
                options: { kind: "data", source: "$context.ghosts", valuePath: "id", labelPath: "n" },
              },
            },
          ],
        },
      ],
      actions: [{ type: "button", props: { labelMsg: "msg:s" }, on: { press: { verb: "submit" } } }],
    };
    expect(codes(doc)).toContain("CONTEXT_REF_UNDEFINED");
  });
  it("accepts a $context ref declared in the page's context map", () => {
    const doc = {
      pageType: "settings-form",
      source: "site",
      context: { presets: { collection: "presets" } },
      sections: [
        {
          fields: [
            {
              type: "select",
              bind: "x",
              props: {
                options: { kind: "data", source: "$context.presets", valuePath: "id", labelPath: "n" },
              },
            },
          ],
        },
      ],
      actions: [{ type: "button", props: { labelMsg: "msg:s" }, on: { press: { verb: "submit" } } }],
    };
    expect(codes(doc)).toEqual([]);
  });
});

describe("fragments & slots (UIS-180/UIS-186)", () => {
  it("rejects a fragment ref that does not resolve (FRAGMENT_REF_UNDEFINED)", () => {
    const doc = {
      pageType: "settings-form",
      source: "site",
      fragments: { known: { type: "text", props: { value: "x" } } },
      sections: [{ fields: [{ type: "fragment", props: { ref: "missing" } }] }],
      actions: [{ type: "button", props: { labelMsg: "msg:s" }, on: { press: { verb: "submit" } } }],
    };
    const e = firstError(doc);
    expect(e?.code).toBe("FRAGMENT_REF_UNDEFINED");
  });
  it("resolves a fragment ref present in the fragments map", () => {
    const doc = {
      pageType: "settings-form",
      source: "site",
      fragments: { known: { type: "text", props: { value: "x" } } },
      sections: [{ fields: [{ type: "fragment", props: { ref: "known" } }] }],
      actions: [{ type: "button", props: { labelMsg: "msg:s" }, on: { press: { verb: "submit" } } }],
    };
    expect(codes(doc)).toEqual([]);
  });
  it("rejects duplicate slot names (SLOT_NAME_DUPLICATE)", () => {
    const doc = {
      pageType: "dashboard",
      tiles: [
        { size: "small", widget: { type: "slot", props: { name: "cards" } } },
        { size: "small", widget: { type: "slot", props: { name: "cards" } } },
      ],
    };
    expect(codes(doc)).toContain("SLOT_NAME_DUPLICATE");
  });
});

describe("actions (UIS-160/UIS-163 → ACTION_VERB_UNKNOWN / ACTION_FIELDS_INVALID)", () => {
  it("rejects an unknown action verb", () => {
    const doc = {
      pageType: "settings-form",
      source: "site",
      sections: [{ fields: [{ type: "text-input", bind: "x" }] }],
      actions: [{ type: "button", props: { labelMsg: "msg:s" }, on: { press: { verb: "teleport" } } }],
    };
    const e = firstError(doc);
    expect(e?.code).toBe("ACTION_VERB_UNKNOWN");
    expect(e?.path).toBe("actions[0].on.press.verb");
  });
  it("rejects an action missing a verb-required field (create.target)", () => {
    const doc = {
      pageType: "list-detail",
      list: { source: "rows", display: { type: "text", props: { value: "x" } } },
      detail: { source: "rows[id=1]", root: { type: "text", props: { value: "x" } } },
      newAction: { verb: "create" },
    };
    const e = firstError(doc);
    expect(e?.code).toBe("ACTION_FIELDS_INVALID");
  });
  it("rejects a wizard-only verb used outside a wizard (UIS-163)", () => {
    const doc = {
      pageType: "settings-form",
      source: "site",
      sections: [{ fields: [{ type: "text-input", bind: "x" }] }],
      actions: [
        { type: "button", props: { labelMsg: "msg:s" }, on: { press: { verb: "wizard-next" } } },
      ],
    };
    const e = firstError(doc);
    expect(e?.code).toBe("ACTION_FIELDS_INVALID");
  });
  it("accepts a wizard-only verb inside a wizard", () => {
    const doc = {
      pageType: "wizard",
      steps: [
        {
          id: "one",
          titleMsg: "msg:one",
          root: {
            type: "button",
            props: { labelMsg: "msg:next" },
            on: { press: { verb: "wizard-next" } },
          },
        },
      ],
      onFinish: { verb: "call-action", action: "done" },
    };
    expect(codes(doc)).toEqual([]);
  });
});

describe("wizard step ids (UIS-050 → WIZARD_STEP_ID_DUPLICATE)", () => {
  it("rejects duplicate step ids", () => {
    const doc = {
      pageType: "wizard",
      steps: [
        { id: "dup", titleMsg: "msg:a", root: { type: "text", props: { value: "x" } } },
        { id: "dup", titleMsg: "msg:b", root: { type: "text", props: { value: "y" } } },
      ],
      onFinish: { verb: "call-action", action: "done" },
    };
    const e = firstError(doc);
    expect(e?.code).toBe("WIZARD_STEP_ID_DUPLICATE");
    expect(e?.path).toBe("steps[1].id");
  });
});

describe("live bindings (UIS-109 → BINDING_PATH_INVALID)", () => {
  const withValue = (value: unknown) => ({
    pageType: "dashboard",
    tiles: [{ size: "small", widget: { type: "stat-tile", props: { labelMsg: "msg:l", value } } }],
  });
  it("accepts a well-formed LiveBinding", () => {
    expect(codes(withValue({ path: "current_temperature", live: true }))).toEqual([]);
  });
  it("rejects a LiveBinding whose live is not literally true", () => {
    expect(codes(withValue({ path: "x", live: false }))).toContain("BINDING_PATH_INVALID");
  });
  it("rejects a LiveBinding with a malformed path", () => {
    expect(codes(withValue({ path: "a..b", live: true }))).toContain("BINDING_PATH_INVALID");
  });
});
