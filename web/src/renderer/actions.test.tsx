import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { PageRenderer } from "./PageRenderer";
import { validatePage } from "./validate";
import type { ActionHandler } from "./types";

// The closed action-verb grammar (UIS-160), driven through real widget presses.
// Every synthetic document below is a valid ui-schema/1 page (asserted up front),
// so the renderer paints it rather than the rejection panel — the action path is
// what's under test, not validation.

const messages: Record<string, string> = {
  "msg:name": "Name",
  "msg:new": "New",
  "msg:del": "Delete",
  "msg:open": "Open",
  "msg:save": "Save",
  "msg:add": "Add tag",
  "msg:remove": "Remove",
};

describe("actions — create + delete against the api-client seam", () => {
  const doc = {
    pageType: "list-detail",
    list: {
      source: "items",
      display: {
        type: "table",
        props: { source: "items", columns: [{ headerMsg: "msg:name", cell: "item.name" }] },
        on: { rowPress: { verb: "set", target: "$ui.sel", value: "item.id" } },
      },
    },
    detail: {
      source: "items[id=$ui.sel]",
      root: {
        type: "section",
        children: [
          { type: "button", props: { labelMsg: "msg:save" }, on: { press: { verb: "submit" } } },
          { type: "button", props: { labelMsg: "msg:del" }, on: { press: { verb: "delete", target: "$root" } } },
        ],
      },
    },
    newAction: { verb: "create", target: "items", itemDefault: { name: "Untitled" } },
  };

  it("is a valid document", () => {
    expect(validatePage(doc).ok).toBe(true);
  });

  it("New enters a create draft; the draft's Save dispatches handler.create (UIS-021/160)", async () => {
    const user = userEvent.setup();
    const handler: ActionHandler = { create: vi.fn() };
    render(<PageRenderer doc={doc} data={{ items: [{ id: "a", name: "Alpha" }] }} messages={messages} handler={handler} />);
    // The New click enters a draft — it no longer fires create itself…
    await user.click(screen.getByRole("button", { name: "New" }));
    expect(handler.create).not.toHaveBeenCalled();
    // …Save commits the draft through the create path, seeded from itemDefault.
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(handler.create).toHaveBeenCalledWith("items", { name: "Untitled" });
  });

  it("`delete` dispatches to handler.remove with the resolved target (UIS-160)", async () => {
    const user = userEvent.setup();
    const handler: ActionHandler = { remove: vi.fn() };
    render(<PageRenderer doc={doc} data={{ items: [{ id: "a", name: "Alpha" }] }} messages={messages} handler={handler} />);
    await user.click(screen.getByRole("button", { name: /Alpha/ }));
    await user.click(await screen.findByRole("button", { name: "Delete" }));
    expect(handler.remove).toHaveBeenCalledWith("$root", { id: "a", name: "Alpha" });
  });
});

describe("actions — navigate substitutes params into the path template (UIS-160)", () => {
  const doc = {
    pageType: "settings-form",
    source: "site",
    sections: [{ fields: [{ type: "text-input", bind: "name", props: { labelMsg: "msg:name" } }] }],
    actions: [
      { type: "button", props: { labelMsg: "msg:open" }, on: { press: { verb: "navigate", to: "/screens/{id}", params: { id: "screenId" } } } },
      { type: "button", props: { labelMsg: "msg:save" }, on: { press: { verb: "submit" } } },
    ],
  };

  it("is a valid document", () => {
    expect(validatePage(doc).ok).toBe(true);
  });

  it("resolves a Binding param and substitutes it into `to`", async () => {
    const user = userEvent.setup();
    const handler: ActionHandler = { navigate: vi.fn() };
    render(
      <PageRenderer doc={doc} data={{ site: { name: "The Hangar", screenId: "01J8Z" } }} messages={messages} handler={handler} />,
    );
    await user.click(screen.getByRole("button", { name: "Open" }));
    expect(handler.navigate).toHaveBeenCalledWith("/screens/01J8Z", { id: "01J8Z" });
  });
});

describe("actions — repeat-add / repeat-remove mutate the in-memory array (UIS-162)", () => {
  const doc = {
    pageType: "settings-form",
    source: "doc",
    sections: [
      {
        fields: [
          {
            type: "repeat",
            bind: "tags",
            props: {
              itemTemplate: {
                type: "section",
                children: [
                  { type: "text", props: { value: "item.label" } },
                  { type: "button", props: { labelMsg: "msg:remove" }, on: { press: { verb: "repeat-remove", target: "item" } } },
                ],
              },
            },
          },
          { type: "button", props: { labelMsg: "msg:add" }, on: { press: { verb: "repeat-add", target: "tags", itemDefault: { label: "New tag" } } } },
        ],
      },
    ],
    actions: [{ type: "button", props: { labelMsg: "msg:save" }, on: { press: { verb: "submit" } } }],
  };

  it("is a valid document", () => {
    expect(validatePage(doc).ok).toBe(true);
  });

  it("appends a cloned itemDefault and removes an item by its own iteration context", async () => {
    const user = userEvent.setup();
    render(<PageRenderer doc={doc} data={{ doc: { tags: [{ label: "alpha" }, { label: "beta" }] } }} messages={messages} />);

    expect(screen.getByText("alpha")).toBeInTheDocument();
    expect(screen.getByText("beta")).toBeInTheDocument();

    // repeat-add appends a third row seeded from itemDefault.
    await user.click(screen.getByRole("button", { name: "Add tag" }));
    expect(screen.getByText("New tag")).toBeInTheDocument();

    // repeat-remove on the first row removes exactly that element (no path walk).
    await user.click(screen.getAllByRole("button", { name: "Remove" })[0]);
    expect(screen.queryByText("alpha")).not.toBeInTheDocument();
    expect(screen.getByText("beta")).toBeInTheDocument();
  });
});

describe("actions — submit persists the RESOLVED bound record, not the whole data root (UIS-160/005)", () => {
  const doc = {
    pageType: "list-detail",
    list: {
      source: "items",
      display: {
        type: "table",
        props: { source: "items", columns: [{ headerMsg: "msg:name", cell: "item.name" }] },
        on: { rowPress: { verb: "set", target: "$ui.sel", value: "item.id" } },
      },
    },
    detail: {
      source: "items[id=$ui.sel]",
      root: {
        type: "section",
        children: [
          { type: "text-input", bind: "name", props: { labelMsg: "msg:name" } },
          { type: "button", props: { labelMsg: "msg:save" }, on: { press: { verb: "submit" } } },
        ],
      },
    },
  };

  it("is a valid document", () => {
    expect(validatePage(doc).ok).toBe(true);
  });

  it("hands handler.submit the one selected record — never the whole items collection", async () => {
    const user = userEvent.setup();
    const handler: ActionHandler = { submit: vi.fn() };
    render(
      <PageRenderer
        doc={doc}
        data={{ items: [{ id: "a", name: "Alpha" }, { id: "b", name: "Beta" }] }}
        messages={messages}
        handler={handler}
      />,
    );
    // Select Alpha, then Save. submit's default target is detail.source; its
    // RESOLVED value (the single record) is the payload — Beta must never leak.
    await user.click(screen.getByRole("button", { name: /Alpha/ }));
    await user.click(await screen.findByRole("button", { name: "Save" }));
    expect(handler.submit).toHaveBeenCalledWith("items[id=$ui.sel]", { id: "a", name: "Alpha" });
    expect(handler.submit).not.toHaveBeenCalledWith("items[id=$ui.sel]", expect.objectContaining({ items: expect.anything() }));
  });
});

describe("state — a write DESCENDS through a container, never through a scalar", () => {
  // The regression: `setIn` cloned its intermediate with `{...base}`. When the
  // record legitimately held a STRING there — a `time` condition's
  // `after: "18:00:00"`, whose leaf was just re-typed to `sun`, making the very
  // next write target `after.event` — the spread character-spread the string and
  // the record became `{"0":"1","1":"8",…,"event":"sunset"}`. The API accepted
  // that body and the evaluator then read the condition as false forever, with no
  // error on any surface. A scalar cannot hold members, so a write through one
  // REPLACES it.
  const doc = {
    pageType: "settings-form",
    source: "cond",
    sections: [
      {
        fields: [
          {
            type: "select",
            bind: "after.event",
            props: {
              labelMsg: "msg:event",
              options: { kind: "literal", items: [{ value: "sunset", labelMsg: "msg:sunset" }] },
            },
          },
        ],
      },
    ],
    actions: [{ type: "button", props: { labelMsg: "msg:save" }, on: { press: { verb: "submit" } } }],
  };

  it("is a valid document", () => {
    expect(validatePage(doc).ok).toBe(true);
  });

  it("replaces a scalar intermediate with a fresh container instead of spreading it", async () => {
    const user = userEvent.setup();
    const handler: ActionHandler = { submit: vi.fn() };
    render(
      <PageRenderer doc={doc} data={{ cond: { after: "18:00:00" } }} messages={messages} handler={handler} />,
    );
    await user.selectOptions(screen.getByLabelText("Event"), "sunset");
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(handler.submit).toHaveBeenCalledWith("cond", { after: { event: "sunset" } });
  });

  it("still descends through a real container without discarding its siblings", async () => {
    const user = userEvent.setup();
    const handler: ActionHandler = { submit: vi.fn() };
    render(
      <PageRenderer
        doc={doc}
        data={{ cond: { after: { offset: -600 } } }}
        messages={messages}
        handler={handler}
      />,
    );
    await user.selectOptions(screen.getByLabelText("Event"), "sunset");
    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(handler.submit).toHaveBeenCalledWith("cond", { after: { offset: -600, event: "sunset" } });
  });
});
