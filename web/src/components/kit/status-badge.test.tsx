import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { StatusBadge, type Status } from "./status-badge";

const STATES: Status[] = ["ok", "warn", "error", "off", "pending"];

describe("StatusBadge", () => {
  it.each(STATES)("renders the %s state with its visible label", (status) => {
    render(<StatusBadge status={status}>{status}</StatusBadge>);
    const badge = screen.getByText(status);
    expect(badge).toBeInTheDocument();
    expect(badge).toHaveAttribute("data-status", status);
  });

  it("reserves the ok/green lane for ok only (green token nowhere else)", () => {
    const { container: okC } = render(<StatusBadge status="ok">Live</StatusBadge>);
    expect(okC.querySelector('[data-slot="status-badge"]')?.className).toContain("var(--wv-ok)");

    for (const status of ["warn", "error", "off", "pending"] as Status[]) {
      const { container } = render(<StatusBadge status={status}>{status}</StatusBadge>);
      expect(container.querySelector('[data-slot="status-badge"]')?.className).not.toContain(
        "var(--wv-ok)",
      );
    }
  });

  it("carries a decorative icon (the text label carries the meaning)", () => {
    const { container } = render(<StatusBadge status="error">Offline</StatusBadge>);
    expect(container.querySelector("svg")).toHaveAttribute("aria-hidden", "true");
  });
});
