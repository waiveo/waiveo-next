import { describe, expect, it } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Trash2 } from "lucide-react";
import { Button } from "./button";
import { Tooltip } from "./tooltip";

// A tooltip that only ever renders is worthless — the entire value is in what a
// pointer and a keyboard make it do. Every test here is a gesture.

function Subject({ tip = "Delete this cast" }: { tip?: string }) {
  return (
    <Tooltip tip={tip}>
      <Button size="icon" variant="ghost" icon={Trash2} aria-label="Delete Morning Loop" />
    </Tooltip>
  );
}

describe("Tooltip", () => {
  it("is closed until something asks for it", () => {
    render(<Subject />);
    expect(screen.queryByRole("tooltip")).toBeNull();
  });

  it("opens on hover", async () => {
    const user = userEvent.setup();
    render(<Subject />);
    await user.hover(screen.getByRole("button", { name: "Delete Morning Loop" }));
    expect(await screen.findByRole("tooltip")).toHaveTextContent("Delete this cast");
  });

  it("opens on KEYBOARD FOCUS — the half a native `title` never had", async () => {
    // This is the reason the console's `title=` attributes are being replaced.
    // A `title` appears for a hovering mouse and for nobody else: a keyboard
    // user meeting an unlabelled glyph had no way to learn what it did.
    const user = userEvent.setup();
    render(<Subject />);
    await user.tab();
    expect(screen.getByRole("button", { name: "Delete Morning Loop" })).toHaveFocus();
    expect(await screen.findByRole("tooltip")).toHaveTextContent("Delete this cast");
  });

  it("is dismissible with Escape while the pointer stays put (WCAG 1.4.13)", async () => {
    // A tip that cannot be got rid of is a tip covering the thing underneath it.
    // Escape must close it WITHOUT moving the pointer or the focus, which is the
    // case the success criterion is actually about.
    const user = userEvent.setup();
    render(<Subject />);
    const button = screen.getByRole("button", { name: "Delete Morning Loop" });
    await user.hover(button);
    await screen.findByRole("tooltip");
    await user.keyboard("{Escape}");
    await waitFor(() => expect(screen.queryByRole("tooltip")).toBeNull());
  });

  it("closes when focus leaves the trigger", async () => {
    const user = userEvent.setup();
    render(
      <>
        <Subject />
        <button type="button">Elsewhere</button>
      </>,
    );
    await user.tab();
    await screen.findByRole("tooltip");
    await user.tab();
    await waitFor(() => expect(screen.queryByRole("tooltip")).toBeNull());
  });

  it("describes the trigger without renaming it", async () => {
    // The contract the docstring states: a tip is a DESCRIPTION. If it became the
    // trigger's accessible NAME, the control would be nameless whenever the tip
    // is closed — which is nearly always.
    const user = userEvent.setup();
    render(<Subject tip="Removes it from every schedule" />);
    const button = screen.getByRole("button", { name: "Delete Morning Loop" });
    await user.hover(button);
    await screen.findByRole("tooltip");
    // Still named by its own aria-label, not by the tip.
    expect(screen.getByRole("button", { name: "Delete Morning Loop" })).toBe(button);
    const describedBy = button.getAttribute("aria-describedby");
    expect(describedBy).toBeTruthy();
    expect(document.getElementById(describedBy as string)).toHaveTextContent(
      "Removes it from every schedule",
    );
  });

  it("still passes the trigger's own click through", async () => {
    // Wrapping a button in a trigger is exactly the change that could silently
    // swallow its onClick — the dead-button failure this console has shipped
    // before.
    const user = userEvent.setup();
    let pressed = 0;
    render(
      <Tooltip tip="Delete">
        <Button
          size="icon"
          variant="ghost"
          icon={Trash2}
          aria-label="Delete Morning Loop"
          onClick={() => {
            pressed += 1;
          }}
        />
      </Tooltip>,
    );
    await user.click(screen.getByRole("button", { name: "Delete Morning Loop" }));
    expect(pressed).toBe(1);
  });
});
