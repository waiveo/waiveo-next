import { describe, expect, it, vi } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { PageRenderer } from "../PageRenderer";

/**
 * UIS-071a / UIS-078 / UIS-079 — the three constructs that made a DECISION
 * COLUMN authorable in `ui-schema/1`.
 *
 * The gap these close was measured, not imagined: `plans/2026-08-21-renderer-
 * gaps-from-the-discovery-console.md` derived them from six iterations of
 * hand-written React on the Discovery console, where "every row carries a
 * decision — Adopt / Ignore inline" (Discovery spec §7) could not be expressed
 * in the format the same spec requires the page be authored in. A `cell` is a
 * BindingExpr; it renders a value, and a value cannot be pressed.
 *
 * These tests drive the RENDERER, not the validator. The corpus
 * (`UIS-071a-*.json`) already proves the documents validate; validation only
 * proves a page is not refused, which is exactly the bar a dead button clears —
 * so what is asserted here is that the subtree paints, that it sees the right
 * ROW, and that pressing it dispatches with that row's `item` in scope.
 */

const messages: Record<string, string> = {
  "msg:col.name": "Name",
  "msg:col.status": "Status",
  "msg:col.decision": "Decision",
  "msg:col.adopt": "Adopt",
  "msg:col.statusHelp": "Nothing has scanned this device yet",
  "msg:detail.empty": "Nothing selected",
};

interface Row extends Record<string, unknown> {
  id: string;
  name: string;
  status: string;
  status_tone: string;
  adopted: boolean;
}

const rows: Row[] = [
  { id: "d1", name: "Zulu", status: "Discovered", status_tone: "warning", adopted: false },
  { id: "d2", name: "Alpha", status: "Adopted", status_tone: "positive", adopted: true },
  { id: "d3", name: "Mike", status: "Ignored", status_tone: "neutral", adopted: false },
];

function docWithColumns(columns: unknown[]) {
  return {
    pageType: "list-detail",
    list: {
      source: "devices",
      display: { type: "table", id: "Devices", props: { source: "devices", columns } },
    },
    detail: {
      source: "devices[id=$ui.selected]",
      emptyMsg: "msg:detail.empty",
      root: { type: "text", props: { value: "id" } },
    },
  };
}

const NAME_COL = { headerMsg: "msg:col.name", cell: "item.name" };

const DECISION_COL = {
  headerMsg: "msg:col.decision",
  cellWidget: {
    type: "button",
    props: { labelMsg: "msg:col.adopt", disabledIf: "item.adopted" },
    on: {
      press: { verb: "call-action", action: "adopt-device", params: { device_id: "item.id" } },
    },
  },
};

const STATUS_COL = {
  headerMsg: "msg:col.status",
  cellValue: "item.status",
  cellWidget: {
    type: "badge",
    props: {
      value: "item.status",
      toneFrom: "item.status_tone",
      titleMsg: "msg:col.statusHelp",
    },
  },
};

/** The body rows of the table, in render order. */
function bodyRows(): HTMLElement[] {
  return within(screen.getByRole("table", { name: "Devices" }))
    .getAllByRole("row")
    .slice(1);
}

describe("UIS-071a — a cellWidget column renders a widget subtree per row", () => {
  it("paints one control PER ROW, each seeing its own item", () => {
    render(
      <PageRenderer
        doc={docWithColumns([NAME_COL, DECISION_COL])}
        data={{ devices: rows }}
        messages={messages}
      />,
    );

    const buttons = screen.getAllByRole("button", { name: "Adopt" });
    expect(buttons).toHaveLength(3);

    // `disabledIf: "item.adopted"` is per-row, which is the whole claim: a
    // subtree that saw the PAGE scope instead of the row would disable all
    // three or none. Alpha is the adopted one.
    const disabled = bodyRows().map((r) =>
      within(r).getByRole("button", { name: "Adopt" }).hasAttribute("disabled"),
    );
    expect(disabled).toEqual([false, true, false]);
  });

  it("dispatches with THAT row's item in scope, not the first row's", async () => {
    const user = userEvent.setup();
    const callAction = vi.fn();
    render(
      <PageRenderer
        doc={docWithColumns([NAME_COL, DECISION_COL])}
        data={{ devices: rows }}
        messages={messages}
        handler={{ callAction }}
      />,
    );

    // Press the THIRD row's button. A subtree wired to the wrong row would send
    // d1 and still look correct on screen — the failure this asserts against.
    await user.click(within(bodyRows()[2]!).getByRole("button", { name: "Adopt" }));

    expect(callAction).toHaveBeenCalledTimes(1);
    expect(callAction.mock.calls[0]?.[0]).toBe("adopt-device");
    expect(callAction.mock.calls[0]?.[1]).toMatchObject({ device_id: "d3" });
  });
});

describe("UIS-078 — badge tone comes from the row", () => {
  it("gives each row its OWN tone", () => {
    render(
      <PageRenderer
        doc={docWithColumns([NAME_COL, STATUS_COL])}
        data={{ devices: rows }}
        messages={messages}
      />,
    );
    const tones = bodyRows().map((r) =>
      within(r).getByText(/Discovered|Adopted|Ignored/).closest("[data-slot='status-badge']")
        ?.getAttribute("data-status"),
    );
    // warning → warn, positive → ok, neutral → off (the catalog's own mapping).
    expect(tones).toEqual(["warn", "ok", "off"]);
  });

  it("degrades an UNKNOWN tone to neutral rather than asserting a false one", () => {
    render(
      <PageRenderer
        doc={docWithColumns([NAME_COL, STATUS_COL])}
        data={{ devices: [{ ...rows[0]!, status_tone: "chartreuse" }] }}
        messages={messages}
      />,
    );
    expect(
      screen.getByText("Discovered").closest("[data-slot='status-badge']"),
    ).toHaveAttribute("data-status", "off");
  });

  it("prefers toneFrom over a static tone declared beside it", () => {
    const col = {
      ...STATUS_COL,
      cellWidget: {
        type: "badge",
        props: { value: "item.status", tone: "critical", toneFrom: "item.status_tone" },
      },
    };
    render(
      <PageRenderer
        doc={docWithColumns([NAME_COL, col])}
        data={{ devices: [rows[1]!] }}
        messages={messages}
      />,
    );
    // `critical` would be "error". The row says positive → ok.
    expect(screen.getByText("Adopted").closest("[data-slot='status-badge']")).toHaveAttribute(
      "data-status",
      "ok",
    );
  });
});

describe("UIS-079 — titleMsg carries the explanation", () => {
  it("resolves through the message catalog", () => {
    render(
      <PageRenderer
        doc={docWithColumns([NAME_COL, STATUS_COL])}
        data={{ devices: [rows[0]!] }}
        messages={messages}
      />,
    );
    expect(screen.getByText("Discovered").closest("[data-slot='status-badge']")).toHaveAttribute(
      "title",
      "Nothing has scanned this device yet",
    );
  });

  it("stays ABSENT when undeclared — never an empty title attribute", () => {
    // This must exercise a widget that GOES THROUGH the titleMsg path and
    // declares none. Asserting on a plain `cell` column would pass no matter
    // what `titleText` returns, since a value cell never calls it — a test that
    // watches the wrong element is the failure mode this file exists to avoid.
    const bare = {
      headerMsg: "msg:col.status",
      cellWidget: { type: "text", props: { value: "item.status" } },
    };
    render(
      <PageRenderer
        doc={docWithColumns([NAME_COL, bare])}
        data={{ devices: [rows[0]!] }}
        messages={messages}
      />,
    );
    const painted = screen.getByText("Discovered");
    expect(painted).toHaveAttribute("data-slot", "widget-text");
    expect(painted.hasAttribute("title")).toBe(false);
  });
});

describe("UIS-071a — sorting and search over a widget column", () => {
  it("sorts a widget column by its cellValue", async () => {
    const user = userEvent.setup();
    render(
      <PageRenderer
        doc={docWithColumns([NAME_COL, STATUS_COL])}
        data={{ devices: rows }}
        messages={messages}
      />,
    );
    const names = () => bodyRows().map((r) => within(r).getAllByRole("cell")[0]?.textContent);
    expect(names()).toEqual(["Zulu", "Alpha", "Mike"]);

    await user.click(screen.getByRole("button", { name: /status/i }));
    // By status: Adopted (Alpha) < Discovered (Zulu) < Ignored (Mike).
    expect(names()).toEqual(["Alpha", "Zulu", "Mike"]);
  });

  it("REFUSES to sort a widget column that declares no cellValue", () => {
    const col = { headerMsg: "msg:col.status", cellWidget: STATUS_COL.cellWidget };
    render(
      <PageRenderer
        doc={docWithColumns([NAME_COL, col])}
        data={{ devices: rows }}
        messages={messages}
      />,
    );
    expect(screen.getByRole("button", { name: /name/i })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /status/i })).not.toBeInTheDocument();
    expect(screen.getByRole("columnheader", { name: /status/i })).not.toHaveAttribute("aria-sort");
  });
});
