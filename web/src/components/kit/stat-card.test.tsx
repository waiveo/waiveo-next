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
});
