import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { PageRenderer } from "./PageRenderer";
import { validatePage } from "./validate";
import type { ActionHandler } from "./types";
import { FRAGMENT_DEPTH_CEILING } from "./widgets/common";

// THE AUTOMATION-BUILDER FIXTURE DRIVER.
//
// `conformance/fixtures/automation-builder/` is ui-schema/1's go/no-go acceptance
// artifact: a complete, realistic `list-detail` automation-rule editor. Its
// `fixture-lint.mjs` sibling proves every widget type, vocabRef, verb, compute fn
// and Binding path it names is DEFINED in `contracts/ui-schema-1.md` — a
// vocabulary check over the document's text. That is a strictly weaker claim than
// the one the fixture exists to make: that the real renderer ACCEPTS it and paints
// the editor it declares. This file makes that second claim executable.
//
// It reads the fixture and its illustrative `sample-data.json` straight out of the
// repo tree (never a copy — the same files fixture-lint reads) and drives them
// through the SAME renderer entry point the corpus driver uses (`corpus.test.tsx`
// → `validatePage` + `PageRenderer`); there is no second harness. Every expected
// value is derived from the fixture document or the sample record, never from a
// previous render's output.
//
// The fixture is NOT a `case_id` corpus case, so it earns no `covered` row in
// `conformance/traceability/ui-schema-1.md` — that map's coverage rows cite frozen
// corpus cases driven per `conformance/driven-manifest.json`, a separate track this
// file deliberately does not join (see the contract's Conformance notes).

const FIXTURE_DIR = join(
  import.meta.dirname,
  "..",
  "..",
  "..",
  "conformance",
  "fixtures",
  "automation-builder",
);

interface PageDoc {
  detail: { emptyMsg: string; source: string };
  list: { display: { id: string; props: { columns: { headerMsg: string }[] } } };
}

const doc = JSON.parse(
  readFileSync(join(FIXTURE_DIR, "automation-builder.json"), "utf8"),
) as PageDoc & Record<string, unknown>;

interface Automation {
  id: string;
  name: string;
  mode: string;
  max: number;
  /** A `rules/1` trigger inlines its own fields — `entity_id`, `to`, `for`, …
   * — beside `type` (RUL-010); the extra members are what an entity-picker
   * edit must leave alone (UIS-073a). */
  triggers: (Record<string, unknown> & { type: string; entity_id?: string })[];
  conditions: Condition[];
  actions: { type: string }[];
}
type Condition = Record<string, unknown>;

const sample = JSON.parse(readFileSync(join(FIXTURE_DIR, "sample-data.json"), "utf8")) as {
  automations: Automation[];
  presets: { id: string; name: string }[];
};

/** The page's bound data root (UIS-005): the sample record set, minus the file's
 * own explanatory `_comment` key. */
const data: Record<string, unknown> = {
  automations: sample.automations,
  presets: sample.presets,
};
const record = sample.automations[0];

// A catalog for the `msg:` references this file asserts against — every key is a
// reference the FIXTURE itself declares, so the copy is named by the document
// rather than invented here. Anything unlisted humanizes through the renderer's
// own fallback, exactly as it does with no catalog at all.
const messages: Record<string, string> = {
  "msg:automations.list.name": "Name",
  "msg:automations.list.mode": "Mode",
  "msg:automations.list.triggerCount": "Triggers",
  "msg:automations.detail.empty": "Select an automation to edit it.",
  "msg:automations.detail.nameLabel": "Automation name",
  "msg:automations.detail.triggerKindLabel": "Trigger type",
  "msg:automations.detail.triggerEntityLabel": "Trigger entity",
  "msg:automations.detail.conditionKindLabel": "Condition type",
  "msg:automations.detail.actionKindLabel": "Action type",
  "msg:automations.detail.andGroup": "All of",
  "msg:automations.detail.orGroup": "Any of",
  "msg:automations.detail.notGroup": "Not",
  "msg:automations.detail.modeLabel": "Mode",
  "msg:automations.detail.modeMaxLabel": "Max parallel runs",
  "msg:automations.detail.actionPresetLabel": "Preset",
  "msg:automations.detail.save": "Save",
  "msg:vocab.mode.single": "Single",
  "msg:vocab.mode.restart": "Restart",
  "msg:vocab.mode.queued": "Queued",
  "msg:vocab.mode.parallel": "Parallel",
};

function renderFixture(over?: {
  data?: Record<string, unknown>;
  initialUi?: Record<string, unknown>;
  handler?: ActionHandler;
}) {
  return render(
    <PageRenderer
      doc={doc}
      data={over?.data ?? data}
      messages={messages}
      initialUi={over?.initialUi ?? {}}
      {...(over?.handler ? { handler: over.handler } : {})}
    />,
  );
}

/** Walk a `rules/1` condition tree exactly as the fixture's `condition-editor`
 * fragment discriminates it (UIS-142 key-presence: `and` / `or` / `not` /
 * `type`), counting what a correct render must produce: one group heading per
 * group node and one leaf kind-select per `type`-keyed leaf. Derived from the
 * bound DATA, so the expected counts come from the record, not from the DOM. */
function countConditions(nodes: Condition[]): { and: number; or: number; not: number; leaves: number } {
  const total = { and: 0, or: 0, not: 0, leaves: 0 };
  const visit = (node: Condition): void => {
    if (Array.isArray(node.and)) {
      total.and++;
      (node.and as Condition[]).forEach(visit);
    } else if (Array.isArray(node.or)) {
      total.or++;
      (node.or as Condition[]).forEach(visit);
    } else if (node.not && typeof node.not === "object") {
      total.not++;
      visit(node.not as Condition);
    } else if (typeof node.type === "string") {
      total.leaves++;
    }
  };
  nodes.forEach(visit);
  return total;
}

/** Nest `{not: …}` `depth` times around a leaf condition — the recursion the
 * `condition-editor` fragment expresses through its own self-reference. */
function nestedNot(depth: number): Condition {
  let node: Condition = { type: "state", entity_id: record.id, state: "on" };
  for (let i = 0; i < depth; i++) node = { not: node };
  return node;
}

describe("the automation-builder fixture, through the real renderer", () => {
  it("is a conformant ui-schema/1 document (UIS-200)", () => {
    const result = validatePage(doc);
    if (!result.ok) {
      throw new Error(
        `the fixture failed validation: ${result.errors.map((e) => `${e.code} @ ${e.path}`).join(", ")}`,
      );
    }
    expect(result.ok).toBe(true);
  });

  it("paints the list table the document declares, with its computed cells (UIS-020/071/140)", () => {
    renderFixture();
    const table = screen.getByRole("table", { name: doc.list.display.id });
    for (const column of doc.list.display.props.columns) {
      expect(within(table).getByText(messages[column.headerMsg])).toBeInTheDocument();
    }
    // One row per sample automation; `label` resolves the mode through the
    // document's own vocab labels and `count` counts that record's triggers.
    expect(within(table).getByText(record.name)).toBeInTheDocument();
    expect(within(table).getByText(messages[`msg:vocab.mode.${record.mode}`])).toBeInTheDocument();
    expect(within(table).getByText(String(record.triggers.length))).toBeInTheDocument();
  });

  it("shows the declared empty message until a row press selects a record (UIS-021/022/160)", async () => {
    renderFixture();
    expect(screen.getByText(messages[doc.detail.emptyMsg])).toBeInTheDocument();
    // No selection yet, so the detail form is genuinely absent.
    expect(screen.queryByLabelText(messages["msg:automations.detail.nameLabel"])).toBeNull();

    // The fixture's own rowPress writes `$ui.selected` from the pressed row's id;
    // `detail.source`'s predicate-index path then resolves that record (UIS-101).
    await userEvent.click(screen.getByRole("button", { name: new RegExp(record.name) }));

    expect(screen.queryByText(messages[doc.detail.emptyMsg])).toBeNull();
    expect(screen.getByLabelText(messages["msg:automations.detail.nameLabel"])).toHaveValue(
      record.name,
    );
  });

  it("paints one editor per trigger and per action, each on its own kind (UIS-107/142)", () => {
    renderFixture({ initialUi: { selected: record.id } });
    const triggerKind = screen.getAllByLabelText(messages["msg:automations.detail.triggerKindLabel"]);
    expect(triggerKind).toHaveLength(record.triggers.length);
    record.triggers.forEach((trigger, i) => {
      expect(triggerKind[i]).toHaveValue(trigger.type);
    });

    const actionKind = screen.getAllByLabelText(messages["msg:automations.detail.actionKindLabel"]);
    expect(actionKind).toHaveLength(record.actions.length);
    record.actions.forEach((action, i) => {
      expect(actionKind[i]).toHaveValue(action.type);
    });
  });

  it("populates a data OptionSource from the page's context feed (UIS-133/150)", () => {
    renderFixture({ initialUi: { selected: record.id } });
    // The preset_batch arm's select draws its options from `$context.presets`,
    // which the document's `context` map feeds from the `presets` collection.
    const preset = screen.getByLabelText(messages["msg:automations.detail.actionPresetLabel"]);
    for (const p of sample.presets) {
      expect(within(preset).getByRole("option", { name: p.name })).toBeInTheDocument();
    }
  });

  it("descends the whole nested condition tree, including the `not` arm (UIS-182/183)", () => {
    renderFixture({ initialUi: { selected: record.id } });
    const expected = countConditions(record.conditions);
    // Sanity on the fixture's own sample record: it must actually exercise all
    // three group kinds, or this case proves nothing about recursion.
    expect(expected).toMatchObject({ and: 1, or: 1, not: 1 });

    expect(screen.getAllByRole("heading", { name: messages["msg:automations.detail.andGroup"] })).toHaveLength(expected.and);
    expect(screen.getAllByRole("heading", { name: messages["msg:automations.detail.orGroup"] })).toHaveLength(expected.or);
    // The regression this case exists for: a bound `fragment` narrows the item
    // scope (UIS-183), so the `not` arm descends INTO the negated condition and
    // renders its leaf. Without that narrowing the fragment re-reads the same
    // node forever, painting a `not` group at every level until the UIS-182
    // ceiling fires — many headings and a fail-closed alert instead of one
    // heading and a leaf editor.
    expect(screen.getAllByRole("heading", { name: messages["msg:automations.detail.notGroup"] })).toHaveLength(expected.not);
    expect(screen.queryByRole("alert")).toBeNull();

    // Every leaf in the tree — the `not`'s own included — gets a kind select.
    expect(
      screen.getAllByLabelText(messages["msg:automations.detail.conditionKindLabel"]),
    ).toHaveLength(expected.leaves);
  });

  it("gates the parallel-only `max` field on visibleIf (UIS-063)", () => {
    const maxLabel = messages["msg:automations.detail.modeMaxLabel"];
    renderFixture({ initialUi: { selected: record.id } });
    // The sample record's mode IS the one the fixture's visibleIf tests for.
    expect(record.mode).toBe("parallel");
    expect(screen.getByLabelText(maxLabel)).toHaveValue(record.max);
  });

  it("hides the `max` field for a record in any other mode (UIS-063)", () => {
    const single = { ...record, mode: "single" };
    renderFixture({
      data: { ...data, automations: [single] },
      initialUi: { selected: single.id },
    });
    expect(screen.getByLabelText(messages["msg:automations.detail.modeLabel"])).toHaveValue("single");
    expect(screen.queryByLabelText(messages["msg:automations.detail.modeMaxLabel"])).toBeNull();
  });

  // ── The scalar binding shape, end to end (UIS-073a) ─────────────────────────
  //
  // Every entity-picker in this fixture binds an entity id `rules/1` INLINES into
  // the trigger/condition/action object itself (`{"type":"state","entity_id":…,
  // "to":…,"for":…}`, RUL-010) — there is no EntityRef-shaped sub-object in a
  // rules/1 record for UIS-073's object shape to bind to. Each declares
  // `bindShape: "entityId"` and binds the scalar directly.
  //
  // Two things must hold, and the second is the one that matters: the picker must
  // SHOW the record's entity, and editing it must write ONLY that scalar. The
  // alternative once considered — binding the enclosing `item` under the object
  // shape — would have displayed correctly and then erased the trigger's siblings
  // on the first keystroke, because that shape replaces its whole bound value with
  // a freshly-built `{<form key>: …}` (inputs.tsx). So the edit is asserted through
  // the `submit` seam against the WHOLE sample record: anything the write touched
  // beyond the one scalar shows up as an inequality.

  /** The sample record's own triggers that carry an inlined entity id, in array
   * order — the triggers whose editor therefore paints an entity picker. */
  const entityTriggers = record.triggers.filter(
    (t) => typeof (t as Record<string, unknown>).entity_id === "string",
  );

  it("paints each trigger's inlined entity id in its own picker (UIS-073a)", () => {
    renderFixture({ initialUi: { selected: record.id } });
    // The fixture must actually exercise the shape, or this case proves nothing.
    expect(entityTriggers.length).toBeGreaterThan(0);
    const pickers = screen.getAllByLabelText(messages["msg:automations.detail.triggerEntityLabel"]);
    expect(pickers).toHaveLength(entityTriggers.length);
    entityTriggers.forEach((trigger, i) => {
      expect(pickers[i]).toHaveValue(trigger.entity_id);
    });
  });

  it("writes only the scalar it binds, leaving the trigger's siblings intact (UIS-073a)", async () => {
    // An id the record does not already hold anywhere, so a stale render cannot
    // pass by accident.
    const NEXT_ENTITY = "01J8Z3K4N5P6Q7R8S9T0V1W2D7";
    expect(JSON.stringify(record)).not.toContain(NEXT_ENTITY);
    // The trigger being edited genuinely has siblings to lose.
    expect(Object.keys(record.triggers[0]).length).toBeGreaterThan(1);

    const submit = vi.fn();
    renderFixture({ initialUi: { selected: record.id }, handler: { submit } });
    const picker = screen.getAllByLabelText(
      messages["msg:automations.detail.triggerEntityLabel"],
    )[0];

    await userEvent.clear(picker);
    await userEvent.type(picker, NEXT_ENTITY);
    expect(picker).toHaveValue(NEXT_ENTITY);

    // Save persists the bound record through the seam (UIS-160/161), which is how
    // the edited tree — not just the DOM — becomes assertable.
    await userEvent.click(screen.getByRole("button", { name: messages["msg:automations.detail.save"] }));
    expect(submit).toHaveBeenCalledTimes(1);
    const persisted = submit.mock.calls[0][1];

    // The whole record, derived from the sample data: exactly one scalar moved.
    // `type`, `to` and `for` on the edited trigger — and every other trigger,
    // condition and action — are the sample record's own values, untouched.
    expect(persisted).toEqual({
      ...record,
      triggers: [{ ...record.triggers[0], entity_id: NEXT_ENTITY }, ...record.triggers.slice(1)],
    });
  });

  // The oracle must bite: the "no alert" assertion above is only meaningful if a
  // genuinely over-deep tree DOES trip the ceiling. Malformed/cyclic bound data is
  // exactly the case UIS-182 requires to fail closed rather than exhaust the stack.
  it("fails closed at the recursion ceiling on an over-deep condition tree (UIS-182)", () => {
    const deep = { ...record, conditions: [nestedNot(FRAGMENT_DEPTH_CEILING + 8)] };
    renderFixture({
      data: { ...data, automations: [deep] },
      initialUi: { selected: deep.id },
    });
    expect(screen.getByRole("alert")).toBeInTheDocument();
  });
});
