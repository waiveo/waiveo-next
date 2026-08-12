import { useState } from "react";
import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { Tab, TabList, TabPanel, Tabs } from "./tabs";

// Every assertion here is a gesture or an ARIA relationship, because the whole
// reason this component exists is that a row of buttons LOOKS the same and is a
// different control. "It rendered" would have passed on the button row too.

function Subject() {
  const [value, setValue] = useState("live");
  return (
    <Tabs value={value} onValueChange={setValue}>
      <TabList aria-label="Screen views">
        <Tab value="live">Live</Tab>
        <Tab value="paired">Paired</Tab>
        <Tab value="retired">Retired</Tab>
      </TabList>
      <TabPanel value="live">Three screens on air</TabPanel>
      <TabPanel value="paired">Two awaiting a cast</TabPanel>
      <TabPanel value="retired">Nothing retired</TabPanel>
    </Tabs>
  );
}

describe("Tabs", () => {
  it("names the tablist, so two sets on one page are told apart", () => {
    render(<Subject />);
    expect(screen.getByRole("tablist", { name: "Screen views" })).toBeInTheDocument();
  });

  it("shows only the selected panel, and switches it on click", async () => {
    const user = userEvent.setup();
    render(<Subject />);
    expect(screen.getByText("Three screens on air")).toBeInTheDocument();
    expect(screen.queryByText("Two awaiting a cast")).toBeNull();

    await user.click(screen.getByRole("tab", { name: "Paired" }));
    expect(screen.getByText("Two awaiting a cast")).toBeInTheDocument();
    expect(screen.queryByText("Three screens on air")).toBeNull();
  });

  it("ties each tab to the panel it controls", async () => {
    const user = userEvent.setup();
    render(<Subject />);
    await user.click(screen.getByRole("tab", { name: "Retired" }));
    const tab = screen.getByRole("tab", { name: "Retired" });
    const panel = screen.getByRole("tabpanel");
    expect(tab).toHaveAttribute("aria-controls", panel.id);
    expect(panel).toHaveAttribute("aria-labelledby", tab.id);
  });

  it("marks exactly one tab selected at a time", async () => {
    const user = userEvent.setup();
    render(<Subject />);
    const selected = () =>
      screen.getAllByRole("tab").filter((t) => t.getAttribute("aria-selected") === "true");
    expect(selected().map((t) => t.textContent)).toEqual(["Live"]);
    await user.click(screen.getByRole("tab", { name: "Paired" }));
    expect(selected().map((t) => t.textContent)).toEqual(["Paired"]);
  });

  it("is one tab stop with the arrow keys moving inside it", async () => {
    const user = userEvent.setup();
    render(<Subject />);
    const [live, paired, retired] = screen.getAllByRole("tab");

    live.focus();
    await user.keyboard("{ArrowRight}");
    expect(paired).toHaveFocus();
    await user.keyboard("{ArrowRight}");
    expect(retired).toHaveFocus();
    // Wraps, so a keyboard user never dead-ends at the last option.
    await user.keyboard("{ArrowRight}");
    expect(live).toHaveFocus();

    // Tab leaves the strip; it does not step to the next option.
    await user.tab();
    expect(screen.getAllByRole("tab").some((t) => t === document.activeElement)).toBe(false);
  });

  it("accepts aria-labelledby when a visible heading already names the set", () => {
    render(
      <>
        <h2 id="views-heading">Screen views</h2>
        <Tabs defaultValue="a">
          <TabList aria-labelledby="views-heading">
            <Tab value="a">A</Tab>
          </TabList>
          <TabPanel value="a">panel</TabPanel>
        </Tabs>
      </>,
    );
    expect(screen.getByRole("tablist", { name: "Screen views" })).toBeInTheDocument();
  });
});
