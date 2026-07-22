import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { PageHeader } from "./page-header";

describe("PageHeader", () => {
  it("renders the title as the page's h1 in the display face", () => {
    render(<PageHeader title="Screens" />);
    const heading = screen.getByRole("heading", { level: 1, name: "Screens" });
    expect(heading).toBeInTheDocument();
    expect(heading.className).toContain("font-display");
  });

  it("renders description and actions", () => {
    render(
      <PageHeader
        title="Schedules"
        description="Everything on air across your screens."
        actions={<button type="button">New schedule</button>}
      />,
    );
    expect(screen.getByText("Everything on air across your screens.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "New schedule" })).toBeInTheDocument();
  });

  it("hero variant paints the scrimmed hero band via .wv-hero (never raw gradient)", () => {
    const { container } = render(<PageHeader title="Welcome" variant="hero" />);
    const header = container.querySelector('[data-slot="page-header"]')!;
    // The scrim-laying utility, not a raw --grad-hero background, is what carries text.
    expect(header.className).toContain("wv-hero");
    expect(header.className).not.toContain("var(--grad-hero)");
    expect(header.getAttribute("data-variant")).toBe("hero");
  });
});
