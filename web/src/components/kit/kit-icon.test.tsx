import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { Activity } from "lucide-react";
import { KitIcon } from "./kit-icon";

describe("KitIcon", () => {
  it("names a meaningful icon with its required label (role=img)", () => {
    render(<KitIcon icon={Activity} label="Automation firing" />);
    expect(screen.getByRole("img", { name: "Automation firing" })).toBeInTheDocument();
  });

  it("hides a decorative icon from assistive tech", () => {
    const { container } = render(<KitIcon icon={Activity} decorative />);
    const svg = container.querySelector("svg");
    expect(svg).toHaveAttribute("aria-hidden", "true");
    expect(screen.queryByRole("img")).not.toBeInTheDocument();
  });

  it("forwards curated visual props (size)", () => {
    const { container } = render(<KitIcon icon={Activity} label="On air" size={32} />);
    const svg = container.querySelector("svg");
    expect(svg).toHaveAttribute("width", "32");
  });
});
