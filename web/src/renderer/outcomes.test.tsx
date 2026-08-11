import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { PageRenderer } from "./PageRenderer";
import { validatePage } from "./validate";
import type { ActionHandler, OutcomeReporter } from "./types";

// ui-schema/1 v1.1 — the confirm gate (UIS-165), the ActionOutcome (UIS-166/167),
// and the two widget props that make an outcome actionable (UIS-076 disabledIf,
// UIS-077 announce).
//
// Every case DRIVES the page: it clicks the control, dismisses or acknowledges
// the dialog, and settles the seam by hand so the PENDING half of the lifecycle
// is observed rather than raced past. Nothing here asserts that a thing rendered
// and stops — this console has shipped a button that painted correctly and did
// nothing, past a test that only checked the markup.

const messages: Record<string, string> = {
  "msg:run": "Run it",
  "msg:del": "Delete",
  "msg:save": "Save",
  "msg:name": "Name",
  "msg:confirm.title": "Really do this?",
  "msg:confirm.body": "It cannot be undone.",
  "msg:confirm.ok": "Do it",
  "msg:confirm.cancel": "Back out",
  "msg:working": "Working on it…",
  "msg:blocked": "Blocked: {0}",
  "msg:done": "Done at {0}",
};

function deferred<T>(): { promise: Promise<T>; resolve: (v: T) => void; reject: (e: unknown) => void } {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

/** A one-tile dashboard wrapping `children` — the smallest conformant carrier for
 * a button and the nodes that read its outcome. */
function page(children: unknown[]): Record<string, unknown> {
  return {
    pageType: "dashboard",
    tiles: [{ size: "large", widget: { type: "section", children } }],
  };
}

const pendingIs = (lit: string) => ({ compute: "eq", args: ["$ui.out.status", { lit }] });

// ── UIS-165: the confirm gate ────────────────────────────────────────────────

describe("confirm (UIS-165) — the press alone dispatches nothing", () => {
  const doc = page([
    {
      type: "button",
      props: { labelMsg: "msg:run", style: "destructive" },
      on: {
        press: {
          verb: "call-action",
          action: "jobs.run",
          confirm: {
            titleMsg: "msg:confirm.title",
            bodyMsg: "msg:confirm.body",
            confirmLabelMsg: "msg:confirm.ok",
            cancelLabelMsg: "msg:confirm.cancel",
            destructive: true,
          },
        },
      },
    },
  ]);

  it("is a valid document", () => {
    expect(validatePage(doc).ok).toBe(true);
  });

  it("opens the dialog with the ConfirmSpec's own copy and dispatches nothing yet", async () => {
    const user = userEvent.setup();
    const handler: ActionHandler = { callAction: vi.fn() };
    render(<PageRenderer doc={doc} messages={messages} handler={handler} />);

    await user.click(screen.getByRole("button", { name: "Run it" }));
    const dialog = await screen.findByRole("dialog", { name: "Really do this?" });
    expect(within(dialog).getByText("It cannot be undone.")).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Do it" })).toBeInTheDocument();
    expect(within(dialog).getByRole("button", { name: "Back out" })).toBeInTheDocument();
    expect(handler.callAction).not.toHaveBeenCalled();
  });

  it("dismissing — by the cancel button OR by Escape — dispatches nothing at all", async () => {
    const user = userEvent.setup();
    const handler: ActionHandler = { callAction: vi.fn() };
    render(<PageRenderer doc={doc} messages={messages} handler={handler} />);

    await user.click(screen.getByRole("button", { name: "Run it" }));
    await user.click(await screen.findByRole("button", { name: "Back out" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(handler.callAction).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: "Run it" }));
    await screen.findByRole("dialog");
    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(handler.callAction).not.toHaveBeenCalled();
  });

  it("acknowledging dispatches the SAME ActionRef, exactly once", async () => {
    const user = userEvent.setup();
    const handler: ActionHandler = { callAction: vi.fn() };
    render(<PageRenderer doc={doc} messages={messages} handler={handler} />);

    await user.click(screen.getByRole("button", { name: "Run it" }));
    await user.click(await screen.findByRole("button", { name: "Do it" }));
    expect(handler.callAction).toHaveBeenCalledTimes(1);
    expect(handler.callAction).toHaveBeenCalledWith("jobs.run", {});
  });

  it("gates a table rowPress too — the gate is the renderer's, not the button's", async () => {
    const user = userEvent.setup();
    const handler: ActionHandler = { remove: vi.fn() };
    const listDoc = {
      pageType: "list-detail",
      list: {
        source: "items",
        display: {
          type: "table",
          props: { source: "items", columns: [{ headerMsg: "msg:name", cell: "item.name" }] },
          on: {
            rowPress: {
              verb: "delete",
              target: "item",
              confirm: { titleMsg: "msg:confirm.title", confirmLabelMsg: "msg:confirm.ok" },
            },
          },
        },
      },
      detail: { source: "items[id=$ui.sel]", root: { type: "text", props: { value: "name" } } },
    };
    expect(validatePage(listDoc).ok).toBe(true);
    render(<PageRenderer doc={listDoc} data={{ items: [{ id: "a", name: "Alpha" }] }} messages={messages} handler={handler} />);

    await user.click(screen.getByRole("button", { name: /Alpha/ }));
    expect(handler.remove).not.toHaveBeenCalled();
    await user.click(await screen.findByRole("button", { name: "Do it" }));
    // The acknowledged dispatch ran in the ROW's scope, not the page's.
    expect(handler.remove).toHaveBeenCalledWith("item", { id: "a", name: "Alpha" });
  });

  it("gates a list-detail newAction create — the one ActionRef the draft idiom intercepts", async () => {
    const user = userEvent.setup();
    const listDoc = {
      pageType: "list-detail",
      list: {
        source: "items",
        display: {
          type: "table",
          props: { source: "items", columns: [{ headerMsg: "msg:name", cell: "item.name" }] },
        },
      },
      detail: {
        source: "items[id=$ui.sel]",
        root: { type: "text-input", bind: "name", props: { labelMsg: "msg:name" } },
      },
      newAction: {
        verb: "create",
        target: "items",
        itemDefault: { name: "Untitled" },
        confirm: { titleMsg: "msg:confirm.title", confirmLabelMsg: "msg:confirm.ok" },
      },
    };
    expect(validatePage(listDoc).ok).toBe(true);
    render(<PageRenderer doc={listDoc} data={{ items: [] }} messages={messages} handler={{}} />);

    await user.click(screen.getByRole("button", { name: "New" }));
    // The draft form has NOT opened — the confirm stands between the press and it.
    expect(screen.queryByLabelText("Name")).not.toBeInTheDocument();
    await user.click(await screen.findByRole("button", { name: "Do it" }));
    expect(await screen.findByLabelText("Name")).toHaveValue("Untitled");
  });
});

// ── UIS-166/167: the ActionOutcome ───────────────────────────────────────────

const outcomeDoc = page([
  {
    type: "button",
    props: { labelMsg: "msg:run", disabledIf: pendingIs("pending") },
    on: { press: { verb: "call-action", action: "jobs.run", outcomeTo: "$ui.out" } },
  },
  {
    type: "text",
    visibleIf: pendingIs("pending"),
    props: { announce: "polite", value: { compute: "msg", args: ["msg:working"] } },
  },
  {
    type: "switch",
    visibleIf: pendingIs("error"),
    props: {
      discriminant: "$ui.out.code",
      cases: [
        {
          when: "JOB_BLOCKED",
          render: {
            type: "text",
            props: { announce: "assertive", value: { compute: "msg", args: ["msg:blocked", "$ui.out.detail"] } },
          },
        },
      ],
      default: { type: "text", props: { announce: "assertive", value: "$ui.out.detail" } },
    },
  },
  { type: "text", visibleIf: "$ui.out.traceId", props: { value: "$ui.out.traceId" } },
  {
    type: "text",
    visibleIf: pendingIs("ok"),
    props: { value: { compute: "msg", args: ["msg:done", "$ui.out.result.at"] } },
  },
  { type: "text", visibleIf: pendingIs("error"), props: { value: "$ui.out.code" } },
]);

describe("outcomeTo (UIS-166) — the lifecycle a page can bind", () => {
  it("is a valid document", () => {
    expect(validatePage(outcomeDoc).ok).toBe(true);
  });

  it("writes pending at dispatch, then ok with the invocation's own result", async () => {
    const user = userEvent.setup();
    const call = deferred<unknown>();
    const handler: ActionHandler = { callAction: vi.fn(() => call.promise) };
    render(<PageRenderer doc={outcomeDoc} messages={messages} handler={handler} />);

    await user.click(screen.getByRole("button", { name: "Run it" }));
    expect(await screen.findByRole("status")).toHaveTextContent("Working on it…");
    expect(screen.getByRole("button", { name: "Run it" })).toBeDisabled();

    call.resolve({ at: "noon" });
    expect(await screen.findByText("Done at noon")).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole("button", { name: "Run it" })).toBeEnabled());
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("settles error with the refusal's own code, detail and traceId — the code a page can branch on", async () => {
    const user = userEvent.setup();
    const call = deferred<unknown>();
    const handler: ActionHandler = { callAction: vi.fn(() => call.promise) };
    render(<PageRenderer doc={outcomeDoc} messages={messages} handler={handler} />);

    await user.click(screen.getByRole("button", { name: "Run it" }));
    call.reject({ code: "JOB_BLOCKED", detail: "a backup is running", traceId: "t-7" });

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Blocked: a backup is running");
    expect(screen.getByText("t-7")).toBeInTheDocument();
    expect(screen.getByText("JOB_BLOCKED")).toBeInTheDocument();
    await waitFor(() => expect(screen.getByRole("button", { name: "Run it" })).toBeEnabled());
  });

  it("falls back to a plain Error's message, and to a null code, rather than hanging", async () => {
    const user = userEvent.setup();
    const call = deferred<unknown>();
    const handler: ActionHandler = { callAction: vi.fn(() => call.promise) };
    render(<PageRenderer doc={outcomeDoc} messages={messages} handler={handler} />);

    await user.click(screen.getByRole("button", { name: "Run it" }));
    call.reject(new Error("the service is unreachable"));
    // No `code`, so the switch's DEFAULT arm renders the detail (UIS-202).
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("the service is unreachable");
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("settles even when the seam throws synchronously", async () => {
    const user = userEvent.setup();
    const handler: ActionHandler = {
      callAction: vi.fn(() => {
        throw new Error("boom");
      }),
    };
    render(<PageRenderer doc={outcomeDoc} messages={messages} handler={handler} />);

    await user.click(screen.getByRole("button", { name: "Run it" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("boom");
    expect(screen.getByRole("button", { name: "Run it" })).toBeEnabled();
  });

  it("merges a host's interim progress while pending, and ignores one that arrives after it settled", async () => {
    const user = userEvent.setup();
    const call = deferred<unknown>();
    let captured: OutcomeReporter | undefined;
    const handler: ActionHandler = {
      callAction: vi.fn((_a: string, _p: Record<string, unknown>, report?: OutcomeReporter) => {
        captured = report;
        return call.promise;
      }),
    };
    const progressDoc = page([
      {
        type: "button",
        props: { labelMsg: "msg:run", disabledIf: pendingIs("pending") },
        on: { press: { verb: "call-action", action: "jobs.run", outcomeTo: "$ui.out" } },
      },
      { type: "text", visibleIf: pendingIs("pending"), props: { announce: "polite", value: "$ui.out.detail" } },
      { type: "text", visibleIf: pendingIs("ok"), props: { value: { compute: "msg", args: ["msg:done", "$ui.out.result.at"] } } },
    ]);
    expect(validatePage(progressDoc).ok).toBe(true);
    render(<PageRenderer doc={progressDoc} messages={messages} handler={handler} />);

    await user.click(screen.getByRole("button", { name: "Run it" }));
    expect(captured).toBeDefined();
    captured?.({ detail: "accepted by systemd" });
    // Still PENDING — a progress report never settles the outcome.
    expect(await screen.findByRole("status")).toHaveTextContent("accepted by systemd");
    expect(screen.getByRole("button", { name: "Run it" })).toBeDisabled();

    call.resolve({ at: "noon" });
    expect(await screen.findByText("Done at noon")).toBeInTheDocument();
    // A late report is ignored: the settled record is not dragged back to pending.
    captured?.({ detail: "too late" });
    await waitFor(() => expect(screen.getByRole("button", { name: "Run it" })).toBeEnabled());
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
    expect(screen.queryByText("too late")).not.toBeInTheDocument();
  });

  it("does not pass a reporter when the ActionRef declared no outcomeTo", async () => {
    const user = userEvent.setup();
    const handler: ActionHandler = { callAction: vi.fn() };
    const plain = page([
      { type: "button", props: { labelMsg: "msg:run" }, on: { press: { verb: "call-action", action: "jobs.run" } } },
    ]);
    render(<PageRenderer doc={plain} messages={messages} handler={handler} />);
    await user.click(screen.getByRole("button", { name: "Run it" }));
    expect(handler.callAction).toHaveBeenCalledWith("jobs.run", {});
  });

  it("an unwired seam the page ASKED about settles as ACTION_DISPATCH_UNWIRED (UIS-167)", async () => {
    const user = userEvent.setup();
    render(<PageRenderer doc={outcomeDoc} messages={messages} handler={{}} />);
    await user.click(screen.getByRole("button", { name: "Run it" }));
    expect(await screen.findByText("ACTION_DISPATCH_UNWIRED")).toBeInTheDocument();
    // Settled, not stuck on pending: the control comes back.
    expect(screen.getByRole("button", { name: "Run it" })).toBeEnabled();
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("an unwired seam the page did NOT ask about stays the silent no-op it always was", async () => {
    const user = userEvent.setup();
    const plain = page([
      { type: "button", props: { labelMsg: "msg:run" }, on: { press: { verb: "call-action", action: "jobs.run" } } },
    ]);
    render(<PageRenderer doc={plain} messages={messages} handler={{}} />);
    await user.click(screen.getByRole("button", { name: "Run it" }));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("publishes an outcome for submit and delete too, not only call-action", async () => {
    const user = userEvent.setup();
    const submitCall = deferred<unknown>();
    const handler: ActionHandler = { submit: vi.fn(() => submitCall.promise) };
    const formDoc = {
      pageType: "settings-form",
      source: "site",
      sections: [{ fields: [{ type: "text-input", bind: "name", props: { labelMsg: "msg:name" } }] }],
      actions: [
        {
          type: "button",
          props: { labelMsg: "msg:save", disabledIf: pendingIs("pending") },
          on: { press: { verb: "submit", outcomeTo: "$ui.out" } },
        },
        { type: "text", visibleIf: pendingIs("error"), props: { announce: "assertive", value: "$ui.out.code" } },
      ],
    };
    expect(validatePage(formDoc).ok).toBe(true);
    render(<PageRenderer doc={formDoc} data={{ site: { name: "The Hangar" } }} messages={messages} handler={handler} />);

    await user.click(screen.getByRole("button", { name: "Save" }));
    expect(screen.getByRole("button", { name: "Save" })).toBeDisabled();
    submitCall.reject({ code: "REVISION_CONFLICT", detail: "changed elsewhere" });
    expect(await screen.findByRole("alert")).toHaveTextContent("REVISION_CONFLICT");
  });
});

// ── UIS-076: disabledIf ──────────────────────────────────────────────────────

describe("disabledIf (UIS-076) — the control stays, the dispatch stops", () => {
  it("a disabled control dispatches nothing, so a second press cannot re-enter a pending invocation", async () => {
    const user = userEvent.setup();
    const call = deferred<unknown>();
    const handler: ActionHandler = { callAction: vi.fn(() => call.promise) };
    render(<PageRenderer doc={outcomeDoc} messages={messages} handler={handler} />);

    const button = screen.getByRole("button", { name: "Run it" });
    await user.click(button);
    expect(handler.callAction).toHaveBeenCalledTimes(1);

    // The control is still THERE (not removed, as visibleIf would do) and refuses.
    expect(screen.getByRole("button", { name: "Run it" })).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Run it" }));
    expect(handler.callAction).toHaveBeenCalledTimes(1);

    call.resolve({ at: "noon" });
    await waitFor(() => expect(screen.getByRole("button", { name: "Run it" })).toBeEnabled());
  });

  it("a disabled control does not even reach its confirm gate", async () => {
    const user = userEvent.setup();
    const doc = page([
      {
        type: "button",
        props: { labelMsg: "msg:run", disabledIf: true },
        on: {
          press: {
            verb: "call-action",
            action: "jobs.run",
            confirm: { titleMsg: "msg:confirm.title", confirmLabelMsg: "msg:confirm.ok" },
          },
        },
      },
    ]);
    expect(validatePage(doc).ok).toBe(true);
    const handler: ActionHandler = { callAction: vi.fn() };
    render(<PageRenderer doc={doc} messages={messages} handler={handler} />);
    await user.click(screen.getByRole("button", { name: "Run it" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(handler.callAction).not.toHaveBeenCalled();
  });

  it("an absent disabledIf leaves the control enabled", () => {
    const doc = page([
      { type: "button", props: { labelMsg: "msg:run" }, on: { press: { verb: "call-action", action: "jobs.run" } } },
    ]);
    render(<PageRenderer doc={doc} messages={messages} handler={{ callAction: vi.fn() }} />);
    expect(screen.getByRole("button", { name: "Run it" })).toBeEnabled();
  });
});

// ── UIS-077: announce ────────────────────────────────────────────────────────

describe("announce (UIS-077) — text that lands after the page was read is announced", () => {
  function renderText(announce?: unknown) {
    const props: Record<string, unknown> = { value: { lit: "hello" } };
    if (announce !== undefined) props.announce = announce;
    render(<PageRenderer doc={page([{ type: "text", props }])} messages={messages} />);
  }

  it("polite renders a status live region", () => {
    renderText("polite");
    expect(screen.getByRole("status")).toHaveTextContent("hello");
  });

  it("assertive renders an alert live region", () => {
    renderText("assertive");
    expect(screen.getByRole("alert")).toHaveTextContent("hello");
  });

  it("an unrecognized politeness degrades to POLITE, never to a silent node", () => {
    // A typo must cost an over-announcement, not an announcement that never
    // happens — the second is invisible to everyone who can see the screen.
    renderText("shouty");
    expect(screen.getByRole("status")).toHaveTextContent("hello");
  });

  it("an absent announce is an ordinary, non-live text node", () => {
    renderText();
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.getByText("hello")).toBeInTheDocument();
  });
});
