import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Modal, ConfirmModal } from "./modal";

describe("Modal", () => {
  it("renders a dialog named by its required title, with description and footer", () => {
    render(
      <Modal
        title="Publish schedule"
        description="This goes live on every governed screen."
        open
        footer={<button type="button">Publish</button>}
      >
        <p>Review the changes before publishing.</p>
      </Modal>,
    );
    const dialog = screen.getByRole("dialog", { name: "Publish schedule" });
    expect(dialog).toBeInTheDocument();
    expect(screen.getByText("This goes live on every governed screen.")).toBeInTheDocument();
    expect(screen.getByText("Review the changes before publishing.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Publish" })).toBeInTheDocument();
  });

  it("opens from a trigger and traps focus inside the dialog", async () => {
    const user = userEvent.setup();
    render(
      <Modal title="Rename screen" trigger={<button type="button">Rename</button>}>
        <input aria-label="New name" />
      </Modal>,
    );
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Rename" }));
    const dialog = await screen.findByRole("dialog", { name: "Rename screen" });
    // Radix moves focus into the dialog on open (focus trap).
    expect(dialog.contains(document.activeElement)).toBe(true);
  });
});

describe("ConfirmModal", () => {
  it("fires onConfirm when the confirm action is chosen", async () => {
    const user = userEvent.setup();
    const onConfirm = vi.fn();
    render(
      <ConfirmModal
        title="Delete screen"
        description="This removes The Hangar TV from the fleet."
        confirmLabel="Delete"
        destructive
        onConfirm={onConfirm}
        open
      >
        <p>You can re-add it later.</p>
      </ConfirmModal>,
    );
    await user.click(screen.getByRole("button", { name: "Delete" }));
    expect(onConfirm).toHaveBeenCalledOnce();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeInTheDocument();
  });
});
