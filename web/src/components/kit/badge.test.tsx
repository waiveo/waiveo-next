import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { Badge } from "./badge";
import { StatusBadge } from "./status-badge";

// A Badge has no behaviour to drive, so these tests are about the two things it
// CAN get wrong and that no eye would catch on a review: painting the wrong tone
// (an emphasis chip that reads as a health state), and drifting into the ok/green
// lane the brand reserves for live/ok.

describe("Badge", () => {
  it("renders its text", () => {
    render(<Badge>Unsaved changes</Badge>);
    expect(screen.getByText("Unsaved changes")).toBeInTheDocument();
  });

  it("paints each tone differently — a tone that resolved to the same chip is a lie", () => {
    const { rerender } = render(<Badge>x</Badge>);
    const seen = new Set<string>();
    for (const tone of ["neutral", "accent", "outline", "warn", "err"] as const) {
      rerender(<Badge tone={tone}>x</Badge>);
      seen.add(screen.getByText("x").className);
    }
    expect(seen.size).toBe(5);
  });

  it("never enters the ok/green lane — that token family is reserved for live/ok", () => {
    // The brand rule this kit is built on: green means a thing is live. A
    // decorative count chip that could be green would make "on air" ambiguous,
    // so no tone may reach --wv-ok. StatusBadge is where green lives.
    const { rerender } = render(<Badge>x</Badge>);
    for (const tone of ["neutral", "accent", "outline", "warn", "err"] as const) {
      rerender(<Badge tone={tone}>x</Badge>);
      expect(screen.getByText("x").className).not.toMatch(/wv-ok/);
    }
    rerender(<StatusBadge status="ok">live</StatusBadge>);
    expect(document.querySelector("[data-slot='status-badge']")?.className).toMatch(/wv-ok/);
  });

  it("sets an identifier in the mono face, and ordinary text in the display face", () => {
    const { rerender } = render(<Badge mono>state.v1</Badge>);
    expect(screen.getByText("state.v1")).toHaveClass("font-mono");
    rerender(<Badge>Draft</Badge>);
    expect(screen.getByText("Draft")).not.toHaveClass("font-mono");
  });

  it("passes a caller's className through rather than dropping it", () => {
    render(<Badge className="tabular-nums">12s</Badge>);
    expect(screen.getByText("12s")).toHaveClass("tabular-nums");
  });
});
