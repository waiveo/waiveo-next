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
      settingsForm({
        type: "text-input",
        bind: "x",
        props: { labelMsg: "msg:x" },
        on: { press: { verb: "submit" } },
      }),
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
      sections: [{ fields: [{ type: "text-input", bind: "x", props: { labelMsg: "msg:x" } }] }],
      actions: [{ type: "button", props: { labelMsg: "msg:save" } }],
    };
    const e = firstError(doc);
    expect(e?.code).toBe("WIDGET_REQUIRED_FIELD_MISSING");
    expect(e?.path).toBe("actions[0].on.press");
  });
});

describe("required accessible labels (UIS-075 → WIDGET_REQUIRED_FIELD_MISSING)", () => {
  // UIS-075 is an unconditional MUST: every input-category widget carries labelMsg
  // as a required prop (schema catalog), enforced by the ordinary validatePage(doc)
  // call — no opt-in flag. A missing accessible name is a rejection, not a
  // render-time surprise (UIS-200).
  it("rejects an input widget with no labelMsg by default", () => {
    const r = validatePage(settingsForm({ type: "text-input", bind: "x" }));
    expect(r.ok).toBe(false);
    if (r.ok) return;
    const e = r.errors[0];
    expect(e.code).toBe("WIDGET_REQUIRED_FIELD_MISSING");
    expect(e.path).toBe("sections[0].fields[0].props.labelMsg");
  });
  it("rejects each of the eight input-category types when labelMsg is absent", () => {
    for (const type of [
      "text-input",
      "number-input",
      "duration-input",
      "toggle",
      "select",
      "multi-select",
      "entity-picker",
      "time-of-day",
    ]) {
      // `select`/`multi-select` also need `options`; give it so labelMsg is the sole
      // missing required prop being asserted.
      const props =
        type === "select" || type === "multi-select"
          ? { options: { kind: "literal", items: [{ value: "a", labelMsg: "msg:a" }] } }
          : undefined;
      const codesFor = codes(settingsForm({ type, bind: "x", ...(props ? { props } : {}) }));
      expect(codesFor, type).toContain("WIDGET_REQUIRED_FIELD_MISSING");
    }
  });
  it("accepts an input widget once labelMsg is present", () => {
    const withLabel = settingsForm({ type: "text-input", bind: "x", props: { labelMsg: "msg:x" } });
    expect(validatePage(withLabel).ok).toBe(true);
  });
});

describe("binding grammar (UIS-066/UIS-100 → BINDING_PATH_INVALID)", () => {
  it("rejects a malformed widget bind", () => {
    const e = firstError(settingsForm({ type: "text-input", bind: "a..b", props: { labelMsg: "msg:x" } }));
    expect(e?.code).toBe("BINDING_PATH_INVALID");
    expect(e?.path).toBe("sections[0].fields[0].bind");
  });
  it("rejects writing to the read-only $index segment (UIS-107)", () => {
    const e = firstError(settingsForm({ type: "text-input", bind: "item.$index", props: { labelMsg: "msg:x" } }));
    expect(e?.code).toBe("BINDING_PATH_INVALID");
  });
});

describe("option sources (UIS-130–132 → OPTION_SOURCE_INVALID / VOCAB_REF_UNKNOWN)", () => {
  const withOptions = (options: unknown) =>
    settingsForm({ type: "select", bind: "mode", props: { labelMsg: "msg:mode", options } });

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
  it("rejects a label() whose vocabRef is not a member of the UIS-120 table (VOCAB_REF_UNKNOWN)", () => {
    // label's pinned vocabRef is a closed set (UIS-120/121) — a string outside it
    // fails, never a silent pass.
    expect(codes(withStatTile({ compute: "label", args: ["rules/1:nope", "mode"] }))).toContain(
      "VOCAB_REF_UNKNOWN",
    );
  });
  it("rejects a label() whose vocabRef arg is a non-string (VOCAB_REF_UNKNOWN)", () => {
    // A non-string in the vocabRef position is not a member of the table.
    expect(codes(withStatTile({ compute: "label", args: [5, "mode"] }))).toContain("VOCAB_REF_UNKNOWN");
  });
  it("rejects a label() with args omitted entirely (vocabRef absent → VOCAB_REF_UNKNOWN)", () => {
    expect(codes(withStatTile({ compute: "label" }))).toContain("VOCAB_REF_UNKNOWN");
  });
  it("rejects a label() with an empty args array (vocabRef absent → VOCAB_REF_UNKNOWN)", () => {
    expect(codes(withStatTile({ compute: "label", args: [] }))).toContain("VOCAB_REF_UNKNOWN");
  });
  it("accepts a known formatCurrency() call (UIS-140/143)", () => {
    expect(codes(withStatTile({ compute: "formatCurrency", args: ["price", "USD"] }))).toEqual([]);
  });
  it("does not grammar-check a formatCurrency() currency-code arg as a binding", () => {
    // A currency code is a pinned literal (like label's vocabRef), not a data
    // path — "US Dollar" is not valid Binding syntax but must not be rejected as
    // BINDING_PATH_INVALID for it.
    expect(codes(withStatTile({ compute: "formatCurrency", args: ["price", "US Dollar"] }))).toEqual([]);
  });
  it("still grammar-checks formatCurrency()'s first (numeric) arg as an ordinary Binding", () => {
    expect(codes(withStatTile({ compute: "formatCurrency", args: ["bad path!", "USD"] }))).toContain(
      "BINDING_PATH_INVALID",
    );
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
                labelMsg: "msg:x",
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
                labelMsg: "msg:x",
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
      sections: [{ fields: [{ type: "text-input", bind: "x", props: { labelMsg: "msg:x" } }] }],
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
      sections: [{ fields: [{ type: "text-input", bind: "x", props: { labelMsg: "msg:x" } }] }],
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

describe("create idiom envelope seed fields (UIS-164 → BINDING_PATH_INVALID / ACTION_FIELDS_INVALID)", () => {
  // A minimal valid list-detail we perturb one `newAction` field at a time. The
  // create idiom (UIS-021/164) lets a page seed a new row's universal-envelope
  // fields declaratively — `scopeFrom` (a Binding → scope_node) and `lifecycle`
  // (a literal string → lifecycle_state). Both are OPTIONAL, so a typo or a wrong
  // shape degrades SILENTLY to the host default unless the validator catches it —
  // which is exactly what these cases pin (UIS-200: never accept it unvalidated).
  const listDetail = (newAction: Record<string, unknown>) => ({
    pageType: "list-detail",
    list: { source: "rows", display: { type: "text", props: { value: "x" } } },
    detail: { source: "rows[id=$ui.selected]", root: { type: "text", props: { value: "x" } } },
    newAction,
  });

  it("accepts a create declaring valid scopeFrom + lifecycle seed fields", () => {
    expect(
      codes(
        listDetail({
          verb: "create",
          target: "rows",
          itemDefault: { name: "" },
          scopeFrom: "$root.defaultScope",
          lifecycle: "draft",
        }),
      ),
    ).toEqual([]);
  });

  it("rejects a non-string scopeFrom (a Binding-typed field, UIS-066)", () => {
    const e = firstError(listDetail({ verb: "create", target: "rows", scopeFrom: 42 }));
    expect(e?.code).toBe("BINDING_PATH_INVALID");
    expect(e?.path).toBe("newAction.scopeFrom");
  });

  it("rejects a malformed scopeFrom binding path", () => {
    const e = firstError(listDetail({ verb: "create", target: "rows", scopeFrom: "a..b" }));
    expect(e?.code).toBe("BINDING_PATH_INVALID");
    expect(e?.path).toBe("newAction.scopeFrom");
  });

  it("rejects a non-string lifecycle", () => {
    const e = firstError(listDetail({ verb: "create", target: "rows", lifecycle: true }));
    expect(e?.code).toBe("ACTION_FIELDS_INVALID");
    expect(e?.path).toBe("newAction.lifecycle");
  });

  it("rejects a misspelled/unknown create field (e.g. scopeForm) — never silently accepted (UIS-200)", () => {
    const e = firstError(listDetail({ verb: "create", target: "rows", scopeForm: "$root.defaultScope" }));
    expect(e?.code).toBe("ACTION_FIELDS_INVALID");
    expect(e?.path).toBe("newAction.scopeForm");
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

// Find the first error carrying `code` in a (possibly multi-error) rejection.
function errorFor(doc: unknown, code: string): ValidationError | undefined {
  const r = validatePage(doc);
  return r.ok ? undefined : r.errors.find((e) => e.code === code);
}

// A text-input field carrying its required label, to fill a settings-form's
// non-empty `sections` without introducing an unrelated UIS-075 rejection.
const labeledField = { type: "text-input", bind: "x", props: { labelMsg: "msg:x" } };
const submitButton = { type: "button", props: { labelMsg: "msg:s" }, on: { press: { verb: "submit" } } };
const textWidget = { type: "text", props: { value: "x" } };

describe("page-type structural fields present (UIS-020/030/040/050 → WIDGET_REQUIRED_FIELD_MISSING)", () => {
  it("rejects a list-detail page missing `list` entirely", () => {
    const e = firstError({
      pageType: "list-detail",
      detail: { source: "rows[id=1]", root: textWidget },
    });
    expect(e?.code).toBe("WIDGET_REQUIRED_FIELD_MISSING");
    expect(e?.path).toBe("list");
  });
  it("rejects a list-detail page missing `detail.root`", () => {
    const e = errorFor(
      { pageType: "list-detail", list: { source: "rows", display: textWidget }, detail: { source: "rows[id=1]" } },
      "WIDGET_REQUIRED_FIELD_MISSING",
    );
    expect(e?.path).toBe("detail.root");
  });
  it("rejects a settings-form missing `actions` entirely (UIS-031)", () => {
    const e = firstError({ pageType: "settings-form", source: "site", sections: [{ fields: [labeledField] }] });
    expect(e?.code).toBe("WIDGET_REQUIRED_FIELD_MISSING");
    expect(e?.path).toBe("actions");
  });
  it("rejects a settings-form whose actions never wire a submit (UIS-031)", () => {
    const e = firstError({
      pageType: "settings-form",
      source: "site",
      sections: [{ fields: [labeledField] }],
      actions: [{ type: "button", props: { labelMsg: "msg:x" }, on: { press: { verb: "navigate", to: "/x" } } }],
    });
    expect(e?.code).toBe("WIDGET_REQUIRED_FIELD_MISSING");
    expect(e?.path).toBe("actions");
  });
  it("rejects a settings-form with an empty section fields array (UIS-030)", () => {
    const e = errorFor(
      { pageType: "settings-form", source: "site", sections: [{ fields: [] }], actions: [submitButton] },
      "WIDGET_REQUIRED_FIELD_MISSING",
    );
    expect(e?.path).toBe("sections[0].fields");
  });
  it("rejects a dashboard missing `tiles` (UIS-040)", () => {
    const e = firstError({ pageType: "dashboard" });
    expect(e?.code).toBe("WIDGET_REQUIRED_FIELD_MISSING");
    expect(e?.path).toBe("tiles");
  });
  it("rejects a wizard missing `steps` (UIS-050)", () => {
    const e = firstError({ pageType: "wizard", onFinish: { verb: "call-action", action: "done" } });
    expect(e?.code).toBe("WIDGET_REQUIRED_FIELD_MISSING");
    expect(e?.path).toBe("steps");
  });
  it("rejects a wizard missing `onFinish` (UIS-050)", () => {
    const e = errorFor(
      { pageType: "wizard", steps: [{ id: "a", titleMsg: "msg:a", root: textWidget }] },
      "WIDGET_REQUIRED_FIELD_MISSING",
    );
    expect(e?.path).toBe("onFinish");
  });
  it("rejects a wizard step missing both id and titleMsg (UIS-050)", () => {
    const doc = {
      pageType: "wizard",
      steps: [{ root: textWidget }],
      onFinish: { verb: "call-action", action: "done" },
    };
    const paths = (() => {
      const r = validatePage(doc);
      return r.ok ? [] : r.errors.map((e) => e.path);
    })();
    expect(paths).toContain("steps[0].id");
    expect(paths).toContain("steps[0].titleMsg");
  });
});

describe("root Bindings syntactically validated (UIS-005/023/066 → BINDING_PATH_INVALID)", () => {
  it("rejects a paginated list.source missing `path`", () => {
    const e = firstError({
      pageType: "list-detail",
      list: { source: { paginated: true }, display: textWidget },
      detail: { source: "rows[id=1]", root: textWidget },
    });
    expect(e?.code).toBe("BINDING_PATH_INVALID");
    expect(e?.path).toBe("list.source.path");
  });
  it("rejects a paginated list.source whose `paginated` is not literally true", () => {
    const e = firstError({
      pageType: "list-detail",
      list: { source: { path: "automations", paginated: "yes" }, display: textWidget },
      detail: { source: "rows[id=1]", root: textWidget },
    });
    expect(e?.code).toBe("BINDING_PATH_INVALID");
    expect(e?.path).toBe("list.source.paginated");
  });
  it("accepts a well-formed paginated list.source (UIS-023)", () => {
    expect(
      codes({
        pageType: "list-detail",
        list: { source: { path: "automations", paginated: true, limit: 100 }, display: textWidget },
        detail: { source: "automations[id=$ui.selected]", root: textWidget },
      }),
    ).toEqual([]);
  });
  it("rejects a malformed bare-string list.source", () => {
    const e = firstError({
      pageType: "list-detail",
      list: { source: "a..b", display: textWidget },
      detail: { source: "rows[id=1]", root: textWidget },
    });
    expect(e?.code).toBe("BINDING_PATH_INVALID");
    expect(e?.path).toBe("list.source");
  });
  it("rejects a malformed detail.source predicate", () => {
    const e = errorFor(
      {
        pageType: "list-detail",
        list: { source: "rows", display: textWidget },
        detail: { source: "rows[id=]", root: textWidget },
      },
      "BINDING_PATH_INVALID",
    );
    expect(e?.path).toBe("detail.source");
  });
  it("rejects a malformed settings-form source", () => {
    const e = firstError({
      pageType: "settings-form",
      source: "general..displayName",
      sections: [{ fields: [labeledField] }],
      actions: [submitButton],
    });
    expect(e?.code).toBe("BINDING_PATH_INVALID");
    expect(e?.path).toBe("source");
  });
  it("accepts a REST-ish settings-form source naming a single record", () => {
    expect(
      codes({
        pageType: "settings-form",
        source: "automations/01J8Z3K4N5P6Q7R8S9T0V1W2A1",
        sections: [{ fields: [labeledField] }],
        actions: [submitButton],
      }),
    ).toEqual([]);
  });
  it("rejects a malformed wizard draftSource", () => {
    const e = errorFor(
      {
        pageType: "wizard",
        draftSource: "a..b",
        steps: [{ id: "a", titleMsg: "msg:a", root: textWidget }],
        onFinish: { verb: "call-action", action: "done" },
      },
      "BINDING_PATH_INVALID",
    );
    expect(e?.path).toBe("draftSource");
  });
});

describe("root Bindings honor the $context existence check (UIS-005/023/105 → CONTEXT_REF_UNDEFINED)", () => {
  // UIS-023 endorses `$context.<name>` as a list.source form; UIS-105 is an
  // unconditional MUST with no carve-out for root-Binding positions — an
  // undeclared context feed named at a page's primary source must be rejected,
  // never silently accepted on grammar alone.
  it("rejects an undeclared $context feed on list.source", () => {
    const e = errorFor(
      {
        pageType: "list-detail",
        list: { source: "$context.ghosts", display: textWidget },
        detail: { source: "rows[id=1]", root: textWidget },
      },
      "CONTEXT_REF_UNDEFINED",
    );
    expect(e?.path).toBe("list.source");
  });
  it("rejects an undeclared $context feed on detail.source", () => {
    const e = errorFor(
      {
        pageType: "list-detail",
        list: { source: "rows", display: textWidget },
        detail: { source: "$context.ghosts[id=1]", root: textWidget },
      },
      "CONTEXT_REF_UNDEFINED",
    );
    expect(e?.path).toBe("detail.source");
  });
  it("rejects an undeclared $context feed on a settings-form source", () => {
    const e = errorFor(
      {
        pageType: "settings-form",
        source: "$context.ghosts",
        sections: [{ fields: [labeledField] }],
        actions: [submitButton],
      },
      "CONTEXT_REF_UNDEFINED",
    );
    expect(e?.path).toBe("source");
  });
  it("rejects an undeclared $context feed on a wizard draftSource", () => {
    const e = errorFor(
      {
        pageType: "wizard",
        draftSource: "$context.ghosts",
        steps: [{ id: "a", titleMsg: "msg:a", root: textWidget }],
        onFinish: { verb: "call-action", action: "done" },
      },
      "CONTEXT_REF_UNDEFINED",
    );
    expect(e?.path).toBe("draftSource");
  });
  it("rejects an undeclared $context feed inside a root LiveBinding's path", () => {
    const e = errorFor(
      {
        pageType: "list-detail",
        list: { source: { path: "$context.ghosts", live: true }, display: textWidget },
        detail: { source: "rows[id=1]", root: textWidget },
      },
      "CONTEXT_REF_UNDEFINED",
    );
    expect(e?.path).toBe("list.source.path");
  });
  it("accepts a $context feed on list.source/detail.source once it is declared", () => {
    expect(
      codes({
        pageType: "list-detail",
        context: { ghosts: { collection: "ghosts" } },
        list: { source: "$context.ghosts", display: textWidget },
        detail: { source: "$context.ghosts[id=$ui.selected]", root: textWidget },
      }),
    ).toEqual([]);
  });
});

describe("table.columns / switch.cases sub-fields (UIS-070 → WIDGET_REQUIRED_FIELD_MISSING)", () => {
  it("rejects an empty table.columns array", () => {
    const e = firstError(settingsForm({ type: "table", props: { source: "rows", columns: [] } }));
    expect(e?.code).toBe("WIDGET_REQUIRED_FIELD_MISSING");
    expect(e?.path).toBe("sections[0].fields[0].props.columns");
  });
  it("rejects a table column missing cell", () => {
    const e = errorFor(
      settingsForm({ type: "table", props: { source: "rows", columns: [{ headerMsg: "msg:h" }] } }),
      "WIDGET_REQUIRED_FIELD_MISSING",
    );
    expect(e?.path).toBe("sections[0].fields[0].props.columns[0].cell");
  });
  it("rejects a table column missing headerMsg", () => {
    const e = errorFor(
      settingsForm({ type: "table", props: { source: "rows", columns: [{ cell: "item.x" }] } }),
      "WIDGET_REQUIRED_FIELD_MISSING",
    );
    expect(e?.path).toBe("sections[0].fields[0].props.columns[0].headerMsg");
  });
  it("rejects an empty switch.cases array", () => {
    const e = firstError(settingsForm({ type: "switch", props: { discriminant: "mode", cases: [] } }));
    expect(e?.code).toBe("WIDGET_REQUIRED_FIELD_MISSING");
    expect(e?.path).toBe("sections[0].fields[0].props.cases");
  });
  it("rejects a switch case missing render", () => {
    const e = errorFor(
      settingsForm({ type: "switch", props: { discriminant: "mode", cases: [{ when: "a" }] } }),
      "WIDGET_REQUIRED_FIELD_MISSING",
    );
    expect(e?.path).toBe("sections[0].fields[0].props.cases[0].render");
  });
  it("rejects a switch case missing when", () => {
    const e = errorFor(
      settingsForm({ type: "switch", props: { discriminant: "mode", cases: [{ render: textWidget }] } }),
      "WIDGET_REQUIRED_FIELD_MISSING",
    );
    expect(e?.path).toBe("sections[0].fields[0].props.cases[0].when");
  });
});

describe("dashboard tile.size enum (UIS-040 → WIDGET_REQUIRED_FIELD_MISSING)", () => {
  const tile = (over: Record<string, unknown>) => ({ pageType: "dashboard", tiles: [{ ...over }] });
  it("rejects an out-of-enum tile size", () => {
    const e = firstError(tile({ size: "huge", widget: textWidget }));
    expect(e?.code).toBe("WIDGET_REQUIRED_FIELD_MISSING");
    expect(e?.path).toBe("tiles[0].size");
  });
  it("rejects a tile with no size", () => {
    const e = errorFor(tile({ widget: textWidget }), "WIDGET_REQUIRED_FIELD_MISSING");
    expect(e?.path).toBe("tiles[0].size");
  });
  it("rejects a tile with no widget", () => {
    const e = errorFor(tile({ size: "small" }), "WIDGET_REQUIRED_FIELD_MISSING");
    expect(e?.path).toBe("tiles[0].widget");
  });
  it("accepts each of the three closed sizes", () => {
    for (const size of ["small", "medium", "large"]) {
      expect(codes(tile({ size, widget: textWidget })), size).toEqual([]);
    }
  });
});

describe("ActionRef params walked (UIS-160/108 → BINDING_PATH_INVALID / ACTION_FIELDS_INVALID)", () => {
  it("rejects a malformed Binding string nested in navigate.params", () => {
    const doc = {
      pageType: "settings-form",
      source: "site",
      sections: [{ fields: [labeledField] }],
      actions: [
        submitButton,
        {
          type: "button",
          props: { labelMsg: "msg:go" },
          on: { press: { verb: "navigate", to: "/x/{id}", params: { id: "a..b" } } },
        },
      ],
    };
    const e = errorFor(doc, "BINDING_PATH_INVALID");
    expect(e?.path).toBe("actions[1].on.press.params.id");
  });
  it("rejects a malformed Binding string nested in call-action.params", () => {
    const doc = {
      pageType: "wizard",
      steps: [{ id: "a", titleMsg: "msg:a", root: textWidget }],
      onFinish: { verb: "call-action", action: "done", params: { x: "a..b" } },
    };
    const e = errorFor(doc, "BINDING_PATH_INVALID");
    expect(e?.path).toBe("onFinish.params.x");
  });
  it("rejects a params of the wrong shape (not an object)", () => {
    const doc = {
      pageType: "wizard",
      steps: [{ id: "a", titleMsg: "msg:a", root: textWidget }],
      onFinish: { verb: "call-action", action: "done", params: "nope" },
    };
    const e = errorFor(doc, "ACTION_FIELDS_INVALID");
    expect(e?.path).toBe("onFinish.params");
  });
  it("accepts a well-formed Binding inside params", () => {
    const doc = {
      pageType: "wizard",
      steps: [{ id: "a", titleMsg: "msg:a", root: textWidget }],
      onFinish: { verb: "call-action", action: "done", params: { x: "name", n: 5 } },
    };
    expect(codes(doc)).toEqual([]);
  });
});

describe("repeat widget (UIS-070/107 → WIDGET_REQUIRED_FIELD_MISSING / BINDING_PATH_INVALID)", () => {
  // repeat's `bind` names the array its `itemTemplate` iterates (UIS-070 bind
  // shape `array`, UIS-107); a repeat with no bound array has nothing to render,
  // so the bind is required, not optional (unlike `fragment`, whose bind rescopes).
  const withRepeat = (repeat: Record<string, unknown>) => ({
    pageType: "dashboard",
    tiles: [{ size: "large", widget: repeat }],
  });
  it("rejects a repeat with no bind (nothing to iterate, UIS-107)", () => {
    const e = errorFor(withRepeat({ type: "repeat", props: { itemTemplate: textWidget } }), "WIDGET_REQUIRED_FIELD_MISSING");
    expect(e?.path).toBe("tiles[0].widget.bind");
  });
  it("rejects a repeat missing its required itemTemplate", () => {
    const e = errorFor(withRepeat({ type: "repeat", bind: "items" }), "WIDGET_REQUIRED_FIELD_MISSING");
    expect(e?.path).toBe("tiles[0].widget.props.itemTemplate");
  });
  it("rejects a malformed repeat bind (BINDING_PATH_INVALID)", () => {
    const e = errorFor(withRepeat({ type: "repeat", bind: "a..b", props: { itemTemplate: textWidget } }), "BINDING_PATH_INVALID");
    expect(e?.path).toBe("tiles[0].widget.bind");
  });
  it("accepts a well-formed repeat: bound array, itemTemplate, and its optional props", () => {
    expect(
      codes(
        withRepeat({
          type: "repeat",
          bind: "triggers",
          props: { itemTemplate: textWidget, itemScope: "item", minItems: 0, maxItems: 5, emptyMsg: "msg:empty" },
        }),
      ),
    ).toEqual([]);
  });
});
