import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Upload } from "lucide-react";
// The kit files are exempt from the no-restricted-imports boundary, so the test
// may reach the vendored base to prove the kit Button carries its exact classes.
import { Button as VendoredButton } from "@/components/ui/button";

// The kit re-export is what pages actually import; test through it.
import { Button as KitButton } from "./button";

const VARIANTS = ["default", "secondary", "outline", "ghost", "destructive", "link"] as const;

describe("Button", () => {
  it("renders an accessible button named by its text children", () => {
    render(<KitButton>Publish schedule</KitButton>);
    expect(screen.getByRole("button", { name: "Publish schedule" })).toBeInTheDocument();
  });

  it("defaults to type=button so it never submits a form by accident", () => {
    render(<KitButton>Cancel</KitButton>);
    expect(screen.getByRole("button", { name: "Cancel" })).toHaveAttribute("type", "button");
  });

  it("honours an explicit type=submit for the submitting action", () => {
    render(<KitButton type="submit">Save screen</KitButton>);
    expect(screen.getByRole("button", { name: "Save screen" })).toHaveAttribute("type", "submit");
  });

  it("fires onClick", async () => {
    const user = userEvent.setup();
    const onClick = vi.fn();
    render(<KitButton onClick={onClick}>Preview</KitButton>);
    await user.click(screen.getByRole("button", { name: "Preview" }));
    expect(onClick).toHaveBeenCalledOnce();
  });

  it("renders a leading icon decoratively, so the text carries the name alone", () => {
    render(<KitButton icon={Upload}>Publish schedule</KitButton>);
    const button = screen.getByRole("button", { name: "Publish schedule" });
    const svg = button.querySelector("svg");
    expect(svg).not.toBeNull();
    expect(svg).toHaveAttribute("aria-hidden", "true");
    // The icon must not add a second accessible name.
    expect(screen.queryByRole("img")).not.toBeInTheDocument();
  });

  it("delegates styling to the vendored button — identical classes per variant (no hand-rolled drift)", () => {
    // The regression guard: a gallery/page that re-implemented the variant strings
    // by hand had already dropped `hover:text-accent-foreground` (outline/ghost)
    // and `focus-visible:ring-destructive` (destructive). The kit Button wraps the
    // vendored base, so its rendered classes must be byte-for-byte the vendored
    // control's — it can never silently diverge again.
    for (const variant of VARIANTS) {
      const vendored = render(<VendoredButton variant={variant}>Label</VendoredButton>);
      const vendoredClass = screen.getByRole("button", { name: "Label" }).className;
      vendored.unmount();

      const kit = render(<KitButton variant={variant}>Label</KitButton>);
      const kitClass = screen.getByRole("button", { name: "Label" }).className;
      kit.unmount();

      expect(kitClass).toBe(vendoredClass);
    }
  });

  it("carries the hover/focus classes the hand-rolled copy had dropped", () => {
    const { rerender } = render(<KitButton variant="outline">Preview</KitButton>);
    expect(screen.getByRole("button", { name: "Preview" }).className).toContain(
      "hover:text-accent-foreground",
    );

    rerender(<KitButton variant="ghost">Cancel</KitButton>);
    expect(screen.getByRole("button", { name: "Cancel" }).className).toContain(
      "hover:text-accent-foreground",
    );

    rerender(<KitButton variant="destructive">Remove</KitButton>);
    expect(screen.getByRole("button", { name: "Remove" }).className).toContain(
      "focus-visible:ring-destructive",
    );
  });

  it("asChild renders the caller's element (e.g. a link) with the button styling and its own name", () => {
    render(
      <KitButton asChild>
        <a href="/design">Open the design gallery</a>
      </KitButton>,
    );
    // Rendered as a link, not a nested button.
    const link = screen.getByRole("link", { name: "Open the design gallery" });
    expect(link).toHaveAttribute("href", "/design");
    // It carries the vendored primary styling (the gradient accent surface).
    expect(link.className).toContain("bg-[image:var(--grad-accent)]");
    // asChild must not smuggle a native `type` onto the anchor.
    expect(link).not.toHaveAttribute("type");
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
  });

  it("is exported from the kit barrel (the only sanctioned surface pages reach for)", async () => {
    const kit = await import("./index");
    expect(kit.Button).toBe(KitButton);
  });
});
