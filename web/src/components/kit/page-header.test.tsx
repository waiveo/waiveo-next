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

  it("hero variant bottom-anchors its content into the protected lower band", () => {
    // --grad-hero-scrim is bottom-weighted (0.62 dark end at the bottom) and the
    // 150deg ramp puts the indigo tail low; HORIZON §4 places the headline there.
    // So the hero band must be taller than its content and pin it to the bottom
    // (justify-end on the mobile column, items-end on the sm row) — NEVER centered
    // over the weakly-scrimmed ember top.
    const { container } = render(<PageHeader title="Welcome" variant="hero" />);
    const header = container.querySelector('[data-slot="page-header"]')!;
    expect(header.className).toContain("justify-end");
    expect(header.className).toContain("sm:items-end");
    // A min-height gives the band a "lower third" for justify-end to anchor into.
    expect(header.className).toMatch(/min-h-\[/);
    // Centering would drop the headline back onto the unprotected ember stop.
    expect(header.className).not.toContain("sm:items-center");
  });

  it("default variant keeps its centered row and never bottom-anchors", () => {
    const { container } = render(<PageHeader title="Screens" actions={<span>a</span>} />);
    const header = container.querySelector('[data-slot="page-header"]')!;
    expect(header.className).toContain("sm:items-center");
    expect(header.className).not.toContain("justify-end");
    expect(header.className).not.toContain("wv-hero");
  });
});
