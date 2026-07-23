import { describe, expect, it, vi } from "vitest";
import { useState } from "react";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { PageRenderer } from "./PageRenderer";
import type { ActionHandler } from "./types";

// The list-detail create idiom (UIS-021): a page's `newAction` (verb `create`)
// no longer fires the create verb on the New click — it enters a DRAFT. New paints
// a blank detail form bound to a fresh in-memory record seeded from `itemDefault`
// plus the universal-envelope defaults the page declares (`lifecycle` →
// lifecycle_state; `scopeFrom` → scope_node); Save persists that draft through the
// SAME create path (UIS-160/161); Cancel/Escape exits. These tests DRIVE the real
// clicks — a New that never opens a form, or a Save that updates instead of
// creates, fails them.

const messages: Record<string, string> = {
  "msg:col.name": "Name",
  "msg:detail.name": "Item name",
  "msg:detail.save": "Save changes",
  "msg:detail.delete": "Delete item",
  "msg:detail.empty": "Select an item to edit it, or add a new one.",
};

const doc = {
  pageType: "list-detail",
  list: {
    source: "menu_items",
    display: {
      type: "table",
      id: "Menu items",
      props: { source: "menu_items", columns: [{ headerMsg: "msg:col.name", cell: "item.name" }] },
      on: { rowPress: { verb: "set", target: "$ui.selected", value: "item.id" } },
    },
  },
  detail: {
    source: "menu_items[id=$ui.selected]",
    emptyMsg: "msg:detail.empty",
    root: {
      type: "section",
      children: [
        { type: "text-input", bind: "name", props: { labelMsg: "msg:detail.name" } },
        { type: "button", props: { labelMsg: "msg:detail.save", style: "primary" }, on: { press: { verb: "submit" } } },
      ],
    },
  },
  // The declarative create idiom: seed the author-owned `name` blank, and supply
  // the universal-envelope `lifecycle_state` from `lifecycle` (a draft-publish
  // collection starts a new row as a draft).
  newAction: { verb: "create", target: "menu_items", itemDefault: { name: "" }, lifecycle: "draft" },
};

// A host harness mirroring the real route seam: `create` appends the new row (with
// a fresh id) and selects it, remounting the renderer against the new data — exactly
// as a management-API create + reload does in production (pack-page-route et al).
function Harness({ onCreate }: { onCreate: (target: string, item: Record<string, unknown>) => void }) {
  const [rows, setRows] = useState<Array<Record<string, unknown>>>([{ id: "A", name: "Cortado" }]);
  const [selected, setSelected] = useState<string | null>(null);
  const [version, setVersion] = useState(0);
  const handler: ActionHandler = {
    create: (target, item) => {
      onCreate(target, item as Record<string, unknown>);
      const id = "NEW1";
      setRows((r) => [...r, { id, ...(item as Record<string, unknown>) }]);
      setSelected(id);
      setVersion((v) => v + 1);
    },
    submit: vi.fn(),
  };
  return (
    <PageRenderer
      key={version}
      doc={doc}
      data={{ menu_items: rows }}
      initialUi={{ selected }}
      messages={messages}
      handler={handler}
    />
  );
}

const detailRegion = () => screen.getByRole("region", { name: "Detail" });

describe("list-detail create idiom (UIS-021) — New enters a draft, Save creates", () => {
  it("New opens a BLANK draft (not the prior item); Save creates a new row that appears and is selected", async () => {
    const user = userEvent.setup();
    const onCreate = vi.fn();
    render(<Harness onCreate={onCreate} />);

    // Select the existing row → the detail form shows its value.
    await user.click(screen.getByRole("button", { name: /Cortado/ }));
    await waitFor(() => expect(within(detailRegion()).getByLabelText("Item name")).toHaveValue("Cortado"));

    // Click New → the detail switches to a BLANK create draft — NOT the prior item.
    await user.click(screen.getByRole("button", { name: "New" }));
    const draftName = within(detailRegion()).getByLabelText("Item name");
    expect(draftName).toHaveValue("");
    // The prior selection is cleared: no input still carries the old record's value.
    expect(screen.queryByDisplayValue("Cortado")).not.toBeInTheDocument();

    // Fill the draft and Save → create dispatches with the typed value AND the
    // page-declared envelope default (lifecycle_state), through the create path.
    await user.type(draftName, "Flat white");
    await user.click(within(detailRegion()).getByRole("button", { name: "Save changes" }));
    expect(onCreate).toHaveBeenCalledTimes(1);
    expect(onCreate).toHaveBeenCalledWith("menu_items", { name: "Flat white", lifecycle_state: "draft" });

    // The new row appears in the list AND becomes the selection (its detail shows).
    const table = await screen.findByRole("table", { name: "Menu items" });
    await waitFor(() => expect(within(table).getByText("Flat white")).toBeInTheDocument());
    await waitFor(() => expect(within(detailRegion()).getByLabelText("Item name")).toHaveValue("Flat white"));
  });

  it("sources the universal-envelope scope_node from a page-declared scopeFrom binding", async () => {
    const user = userEvent.setup();
    const create = vi.fn();
    // The create idiom lets a page declare where the required scope_node comes from
    // (UIS-021): here a binding into the page's own data, resolved into the draft.
    const scopedDoc = {
      ...doc,
      newAction: {
        verb: "create",
        target: "menu_items",
        itemDefault: { name: "" },
        scopeFrom: "$root.defaultScope",
        lifecycle: "draft",
      },
    };
    render(
      <PageRenderer
        doc={scopedDoc}
        data={{ menu_items: [], defaultScope: "01J8Z3K4N5P6Q7R8S9T0V1W2A1" }}
        messages={messages}
        handler={{ create }}
      />,
    );

    await user.click(screen.getByRole("button", { name: "New" }));
    await user.type(within(detailRegion()).getByLabelText("Item name"), "Latte");
    await user.click(within(detailRegion()).getByRole("button", { name: "Save changes" }));

    expect(create).toHaveBeenCalledWith("menu_items", {
      name: "Latte",
      scope_node: "01J8Z3K4N5P6Q7R8S9T0V1W2A1",
      lifecycle_state: "draft",
    });
  });

  it("Cancel exits the draft without creating", async () => {
    const user = userEvent.setup();
    const onCreate = vi.fn();
    render(<Harness onCreate={onCreate} />);

    await user.click(screen.getByRole("button", { name: "New" }));
    expect(within(detailRegion()).getByLabelText("Item name")).toBeInTheDocument();
    await user.click(within(detailRegion()).getByRole("button", { name: "Cancel" }));

    // Back to the empty prompt; nothing was created.
    expect(screen.getByText("Select an item to edit it, or add a new one.")).toBeInTheDocument();
    expect(onCreate).not.toHaveBeenCalled();
  });

  it("Escape exits the draft without creating", async () => {
    const user = userEvent.setup();
    const onCreate = vi.fn();
    render(<Harness onCreate={onCreate} />);

    await user.click(screen.getByRole("button", { name: "New" }));
    await user.type(within(detailRegion()).getByLabelText("Item name"), "abc");
    await user.keyboard("{Escape}");

    await waitFor(() =>
      expect(screen.getByText("Select an item to edit it, or add a new one.")).toBeInTheDocument(),
    );
    expect(onCreate).not.toHaveBeenCalled();
  });
});

describe("list-detail create idiom (UIS-021) — a create draft hides existing-record-only controls", () => {
  // The detail form pairs Save with a Delete (`{verb:"delete", target:"$root"}`) —
  // exactly as the dogfooded core pages and the menu-board example do.
  const docWithDelete = {
    ...doc,
    detail: {
      ...doc.detail,
      root: {
        type: "section",
        children: [
          { type: "text-input", bind: "name", props: { labelMsg: "msg:detail.name" } },
          { type: "button", props: { labelMsg: "msg:detail.save", style: "primary" }, on: { press: { verb: "submit" } } },
          {
            type: "button",
            props: { labelMsg: "msg:detail.delete", style: "destructive" },
            on: { press: { verb: "delete", target: "$root" } },
          },
        ],
      },
    },
  };

  it("Delete is a live control on a selected row but is NOT rendered inside a create draft (no dead button)", async () => {
    const user = userEvent.setup();
    const remove = vi.fn();
    render(
      <PageRenderer
        doc={docWithDelete}
        data={{ menu_items: [{ id: "A", name: "Cortado" }] }}
        messages={messages}
        handler={{ create: vi.fn(), submit: vi.fn(), remove }}
      />,
    );

    // On a selected, persisted row the Delete button is present AND live: a click
    // dispatches the remove verb against the resolved record.
    await user.click(screen.getByRole("button", { name: /Cortado/ }));
    await user.click(within(detailRegion()).getByRole("button", { name: "Delete item" }));
    expect(remove).toHaveBeenCalledWith("$root", { id: "A", name: "Cortado" });

    // Enter a create draft → the not-yet-persisted record has nothing to delete, so
    // the Delete control is not painted at all (never a fully-enabled, silent no-op).
    // Only Save (the create affordance) remains.
    await user.click(screen.getByRole("button", { name: "New" }));
    expect(within(detailRegion()).getByLabelText("Item name")).toHaveValue("");
    expect(within(detailRegion()).queryByRole("button", { name: "Delete item" })).not.toBeInTheDocument();
    expect(within(detailRegion()).getByRole("button", { name: "Save changes" })).toBeInTheDocument();
  });
});
