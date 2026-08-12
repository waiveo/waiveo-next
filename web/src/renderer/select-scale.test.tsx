import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { PageRenderer } from "./PageRenderer";
import { SELECT_SEARCH_THRESHOLD } from "./widgets/inputs";

/**
 * A ui-schema/1 `select` over a LONG candidate set.
 *
 * The Settings page offers a site's IANA time zone from a `data`-kind option
 * source (UIS-133) whose feed holds 446 rows, and painted them as one flat
 * `<select>`: no way to reach the bottom but to drag, and no way to type
 * "tokyo" and land on `Asia/Tokyo`, because a native select's type-ahead
 * matches only the leading characters of an option.
 *
 * The fix is a host RENDERING decision, not a document one — the same shape
 * `TableWidget`'s `RENDERER_PAGE_SIZE` already takes, and for the same reason:
 * the `select` widget's props are closed at `labelMsg`/`options`/
 * `placeholderMsg` (UIS-070) and a host may not invent a fourth. So the control
 * is chosen from the SIZE of the candidate set, and these tests pin both sides
 * of that decision — including that the document, the binding and the written
 * value are identical either way.
 */

const doc = {
  pageType: "settings-form",
  source: "draft",
  context: { zones: { collection: "zones" } },
  sections: [
    {
      fields: [
        {
          type: "select",
          bind: "tz",
          props: {
            labelMsg: "msg:tz",
            placeholderMsg: "msg:tz.placeholder",
            options: { kind: "data", source: "$context.zones", valuePath: "id", labelPath: "id" },
          },
        },
      ],
    },
  ],
  actions: [{ type: "button", props: { labelMsg: "msg:save" }, on: { press: { verb: "submit" } } }],
};

const messages = {
  "msg:tz": "Time zone",
  "msg:tz.placeholder": "Choose an IANA time zone",
  "msg:save": "Save",
};

/** `n` distinct, plausible zone ids. */
function zones(n: number): { id: string }[] {
  const real = Intl.supportedValuesOf("timeZone");
  return Array.from({ length: n }, (_, i) => ({ id: real[i] ?? `Etc/GMT+${i}` }));
}

function renderPage(count: number, tz = "", submit = vi.fn()) {
  render(
    <PageRenderer
      doc={doc}
      data={{ draft: { tz }, zones: zones(count) }}
      messages={messages}
      handler={{ submit }}
    />,
  );
  return submit;
}

describe("ui-schema select — how it scales", () => {
  it("stays a native <select> at the threshold", () => {
    // The boundary itself, asserted from the exported constant so the test and
    // the code cannot drift: at exactly the threshold the plain control is kept.
    renderPage(SELECT_SEARCH_THRESHOLD);
    expect(screen.getByLabelText("Time zone").tagName).toBe("SELECT");
  });

  it("becomes a searchable combobox one candidate past it", () => {
    renderPage(SELECT_SEARCH_THRESHOLD + 1);
    const control = screen.getByLabelText("Time zone");
    expect(control.tagName).not.toBe("SELECT");
    expect(control).toHaveAttribute("role", "combobox");
  });

  it("writes the SAME bound value whichever control painted it", async () => {
    // The whole claim behind calling this presentation. A document author, a
    // validator and the api/1 body cannot tell the two apart.
    const user = userEvent.setup();

    const shortSubmit = renderPage(5);
    await user.selectOptions(screen.getByLabelText("Time zone"), "Africa/Accra");
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(shortSubmit).toHaveBeenCalled());
    const fromSelect = shortSubmit.mock.calls[0][1] as { tz: string };

    screen.getByRole("button", { name: "Save" }).remove();
    document.body.innerHTML = "";

    const longSubmit = renderPage(400);
    await user.click(screen.getByLabelText("Time zone"));
    await user.type(await screen.findByPlaceholderText(/search time zone/i), "Africa/Accra");
    await user.click(await screen.findByRole("option", { name: "Africa/Accra" }));
    await user.click(screen.getByRole("button", { name: "Save" }));
    await waitFor(() => expect(longSubmit).toHaveBeenCalled());
    const fromCombobox = longSubmit.mock.calls[0][1] as { tz: string };

    expect(fromSelect.tz).toBe("Africa/Accra");
    expect(fromCombobox.tz).toBe(fromSelect.tz);
  });

  it("writes a NUMERIC candidate back as a number, not as the key the control uses", async () => {
    // A `select`'s bind shape is a JSON scalar (UIS-070) — string, number OR
    // boolean — and a combobox addresses every candidate by a string. The key
    // has to be mapped back to the original scalar, or a numeric field silently
    // becomes a string and the server refuses it on a type it did not have when
    // the same document rendered a native select.
    const user = userEvent.setup();
    const submit = vi.fn();
    const numeric = {
      ...doc,
      context: undefined,
      sections: [
        {
          fields: [
            {
              type: "select",
              bind: "channel",
              props: {
                labelMsg: "msg:tz",
                options: {
                  kind: "literal",
                  items: Array.from({ length: 30 }, (_, i) => ({
                    value: i + 1,
                    labelMsg: `msg:ch.${i + 1}`,
                  })),
                },
              },
            },
          ],
        },
      ],
    };
    const catalog: Record<string, string> = { ...messages };
    for (let i = 1; i <= 30; i += 1) catalog[`msg:ch.${i}`] = `Channel ${i}`;

    render(
      <PageRenderer
        doc={numeric}
        data={{ draft: { channel: null } }}
        messages={catalog}
        handler={{ submit }}
      />,
    );
    expect(screen.getByLabelText("Time zone")).toHaveAttribute("role", "combobox");
    await user.click(screen.getByLabelText("Time zone"));
    await user.type(await screen.findByPlaceholderText(/search time zone/i), "Channel 17");
    await user.click(await screen.findByRole("option", { name: "Channel 17" }));
    await user.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() => expect(submit).toHaveBeenCalled());
    const body = submit.mock.calls[0][1] as { channel: unknown };
    expect(body.channel).toBe(17);
    expect(body.channel).not.toBe("17");
  });

  it("finds a candidate by a word in the MIDDLE of its name", async () => {
    // What the native control could not do, and the reason the threshold exists.
    const user = userEvent.setup();
    renderPage(400);
    await user.click(screen.getByLabelText("Time zone"));
    await user.type(await screen.findByPlaceholderText(/search time zone/i), "tokyo");
    await waitFor(() => expect(screen.getAllByRole("option")).toHaveLength(1));
    expect(screen.getByRole("option", { name: "Asia/Tokyo" })).toBeInTheDocument();
  });

  it("shows the placeholder, and the stored value once there is one", () => {
    const { unmount } = render(
      <PageRenderer
        doc={doc}
        data={{ draft: { tz: "" }, zones: zones(400) }}
        messages={messages}
        handler={{ submit: vi.fn() }}
      />,
    );
    expect(screen.getByLabelText("Time zone")).toHaveTextContent("Choose an IANA time zone");
    unmount();

    renderPage(400, "Pacific/Auckland");
    expect(screen.getByLabelText("Time zone")).toHaveTextContent("Pacific/Auckland");
  });

  it("surfaces a server field error on the long control too", () => {
    // A control swapped in behind a threshold is the classic place for the
    // FormField wiring to be quietly dropped on one branch only.
    render(
      <PageRenderer
        doc={doc}
        data={{ draft: { tz: "" }, zones: zones(400) }}
        messages={messages}
        handler={{ submit: vi.fn() }}
        fieldErrors={{ tz: "not a loadable IANA zone" }}
      />,
    );
    expect(screen.getByLabelText("Time zone")).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByText("not a loadable IANA zone")).toBeInTheDocument();
  });
});
