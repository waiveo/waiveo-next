import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { Activity } from "lucide-react";
import { StatCard } from "./stat-card";

describe("StatCard", () => {
  it("renders the label and value", () => {
    render(<StatCard label="Screens live" value={12} />);
    expect(screen.getByText("Screens live")).toBeInTheDocument();
    expect(screen.getByText("12")).toBeInTheDocument();
  });

  it("sets the value in the mono face with tabular numerals", () => {
    render(<StatCard label="Devices online" value={7} />);
    const value = screen.getByText("7");
    expect(value.className).toContain("font-mono");
    expect(value.className).toContain("wv-tnum");
  });

  it("renders an optional decorative icon and a hint", () => {
    const { container } = render(
      <StatCard label="Automations" value={3} icon={Activity} hint="2 fired today" />,
    );
    expect(container.querySelector("svg")).toHaveAttribute("aria-hidden", "true");
    expect(screen.getByText("2 fired today")).toBeInTheDocument();
  });

  it("hardens against horizontal overflow when squeezed into a narrow grid cell", () => {
    // Regression: as a grid item StatCard defaults to min-width:auto, so its
    // min-content (the uppercase label word) can outgrow a narrow track and leak
    // page-level horizontal scroll (jsdom can't compute this — the class contract
    // is the guard). The card must be able to shrink (min-w-0), the label must be
    // able to break/shrink rather than push out, and the icon must not be crushed.
    const { container } = render(<StatCard label="Automations" value={3} icon={Activity} />);
    const card = container.querySelector('[data-slot="stat-card"]') as HTMLElement;
    expect(card.className).toContain("min-w-0");
    const label = screen.getByText("Automations");
    expect(label.className).toContain("min-w-0");
    expect(label.className).toContain("break-words");
    const icon = container.querySelector("svg") as SVGElement;
    expect(icon.getAttribute("class")).toContain("shrink-0");
  });
});
