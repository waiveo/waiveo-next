import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { PageRenderer } from "./PageRenderer";

/**
 * Re-seeding the resource store when a host's data arrives after first paint.
 *
 * The store seeds at mount. For a page whose record is already in hand that is
 * the whole story; for a host that FETCHES after first paint it was not, and
 * three hosts were remounting the renderer to escape it — which throws away the
 * table's search, its sort and the `$ui` selection every time a read resolves.
 *
 * A new `initialResource` identity now re-seeds the store, UNLESS the page has
 * written to the resource since mount.
 *
 * THAT EXCEPTION IS THE WHOLE SAFETY ARGUMENT, so it is tested first and hardest.
 * Without it this change is a data-loss bug: a settings-form an operator is
 * halfway through typing into would be silently reverted the moment a background
 * refetch landed underneath them, and they would have no way to know it happened.
 */

const settingsDoc = {
  pageType: "settings-form",
  source: "site",
  sections: [
    {
      fields: [{ type: "text-input", bind: "displayName", props: { labelMsg: "msg:name" } }],
    },
  ],
  actions: [
    {
      type: "button",
      props: { labelMsg: "msg:save" },
      on: { press: { verb: "submit" } },
    },
  ],
};

const messages = { "msg:name": "Display name", "msg:save": "Save" };

describe("the store re-seeds from a host that fetched late", () => {
  it("takes a NEW resource identity when the page has not been edited", async () => {
    // The read-only case, and the reason the change exists: a host that paints
    // before its fetch resolves must not be stuck on the empty seed.
    const { rerender } = render(
      <PageRenderer doc={settingsDoc} data={{ site: { displayName: "" } }} messages={messages} />,
    );
    expect(screen.getByLabelText("Display name")).toHaveValue("");

    rerender(
      <PageRenderer doc={settingsDoc} data={{ site: { displayName: "The Hangar" } }} messages={messages} />,
    );
    await waitFor(() => expect(screen.getByLabelText("Display name")).toHaveValue("The Hangar"));
  });

  it("DOES NOT clobber an operator's unsaved edit when a refetch lands", async () => {
    // The guard. If this ever fails, the change must be reverted rather than
    // patched: silently reverting someone's typing is worse than the stale-seed
    // problem it was meant to fix, because the stale seed is at least visible.
    const user = userEvent.setup();
    const { rerender } = render(
      <PageRenderer doc={settingsDoc} data={{ site: { displayName: "" } }} messages={messages} />,
    );

    await user.type(screen.getByLabelText("Display name"), "Half-typed name");
    expect(screen.getByLabelText("Display name")).toHaveValue("Half-typed name");

    // A background read resolves with the server's older value.
    rerender(
      <PageRenderer doc={settingsDoc} data={{ site: { displayName: "Whatever the server had" } }} messages={messages} />,
    );

    // The edit stands. Asserted after a settle so a late effect cannot sneak the
    // revert in after the synchronous check passes.
    await new Promise((r) => setTimeout(r, 0));
    expect(screen.getByLabelText("Display name")).toHaveValue("Half-typed name");
  });

  it("stays put when the host re-renders with the SAME identity", async () => {
    // Identity, not deep equality: a host that rebuilds an equal object every
    // render must not re-seed on every render. Here the same object is passed
    // twice with an edit in between — the edit survives, which it could not if
    // re-seeding keyed on value.
    const user = userEvent.setup();
    const site = { displayName: "Original" };
    const { rerender } = render(
      <PageRenderer doc={settingsDoc} data={{ site }} messages={messages} />,
    );
    await user.clear(screen.getByLabelText("Display name"));
    await user.type(screen.getByLabelText("Display name"), "Edited");

    rerender(<PageRenderer doc={settingsDoc} data={{ site }} messages={messages} />);
    await new Promise((r) => setTimeout(r, 0));
    expect(screen.getByLabelText("Display name")).toHaveValue("Edited");
  });

  it("does not report a re-seed back to the host as an edit", async () => {
    // `onResourceChange` means "the operator changed the record". A re-seed is
    // the host's OWN value arriving, so reporting it would make any host that
    // treats the seam as unsaved-changes wrong every time its fetch resolved.
    const onResourceChange = vi.fn();
    const { rerender } = render(
      <PageRenderer
        doc={settingsDoc}
        data={{ site: { displayName: "" } }}
        messages={messages}
        onResourceChange={onResourceChange}
      />,
    );
    rerender(
      <PageRenderer
        doc={settingsDoc}
        data={{ site: { displayName: "Arrived" } }}
        messages={messages}
        onResourceChange={onResourceChange}
      />,
    );
    await waitFor(() => expect(screen.getByLabelText("Display name")).toHaveValue("Arrived"));
    expect(onResourceChange).not.toHaveBeenCalled();
  });

  it("still reports a REAL edit to the host", async () => {
    // The other half of the case above: suppressing the re-seed must not
    // suppress the thing the seam exists for.
    const user = userEvent.setup();
    const onResourceChange = vi.fn();
    render(
      <PageRenderer
        doc={settingsDoc}
        data={{ site: { displayName: "" } }}
        messages={messages}
        onResourceChange={onResourceChange}
      />,
    );
    await user.type(screen.getByLabelText("Display name"), "x");
    await waitFor(() => expect(onResourceChange).toHaveBeenCalled());
  });
});
