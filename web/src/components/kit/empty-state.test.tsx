import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { CalendarClock } from "lucide-react";
import { EmptyState } from "./empty-state";

describe("EmptyState", () => {
  it("renders the title, description and a default decorative icon", () => {
    const { container } = render(
      <EmptyState
        title="Nothing scheduled for this daypart yet"
        description="Add content to light it up."
      />,
    );
    expect(screen.getByText("Nothing scheduled for this daypart yet")).toBeInTheDocument();
    expect(screen.getByText("Add content to light it up.")).toBeInTheDocument();
    expect(container.querySelector("svg")).toHaveAttribute("aria-hidden", "true");
  });

  it("renders a custom icon and an action", () => {
    render(
      <EmptyState
        title="No schedules"
        icon={CalendarClock}
        action={<button type="button">New schedule</button>}
      />,
    );
    expect(screen.getByRole("button", { name: "New schedule" })).toBeInTheDocument();
  });
});
