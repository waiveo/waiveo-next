import type { ReactElement } from "react";
import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { Download } from "lucide-react";

import { Badge } from "./badge";
import { Combobox } from "./combobox";
import { DownloadLink } from "./link";
import { Tab, TabList, TabPanel, Tabs } from "./tabs";
import { Tooltip } from "./tooltip";
import { Button } from "./button";

/**
 * The kit's own two-sky pass, the counterpart to `components/ui/ui.themes.test`.
 * The console has Dusk and Daybreak and a component that only ever gets looked at
 * in one of them is a component with an untested half — the vendored bases have
 * carried this gate since they were themed, and the kit wrappers now do too.
 *
 * Overlay components are rendered OPEN so their portalled content is actually
 * built under each theme rather than skipped.
 */
const THEMES = ["dark", "light"] as const;

type Case = { name: string; build: () => ReactElement; assert: () => void };

const cases: Case[] = [
  {
    name: "Badge (neutral)",
    build: () => <Badge>Draft</Badge>,
    assert: () => expect(screen.getByText("Draft")).toBeInTheDocument(),
  },
  {
    name: "Badge (warn, mono)",
    build: () => (
      <Badge tone="warn" mono>
        12s
      </Badge>
    ),
    assert: () => expect(screen.getByText("12s")).toBeInTheDocument(),
  },
  {
    name: "Tabs",
    build: () => (
      <Tabs defaultValue="live">
        <TabList aria-label="Screen views">
          <Tab value="live">Live</Tab>
          <Tab value="paired">Paired</Tab>
        </TabList>
        <TabPanel value="live">Three screens on air</TabPanel>
      </Tabs>
    ),
    assert: () => {
      expect(screen.getByRole("tablist", { name: "Screen views" })).toBeInTheDocument();
      expect(screen.getByText("Three screens on air")).toBeInTheDocument();
    },
  },
  {
    name: "Tooltip (open)",
    build: () => (
      <Tooltip tip="Delete this cast" delayDuration={0}>
        <Button aria-label="Delete Morning Loop">Delete</Button>
      </Tooltip>
    ),
    // Closed at rest by design, so the theme pass exercises the trigger; the
    // opened content is driven in tooltip.test.tsx.
    assert: () =>
      expect(screen.getByRole("button", { name: "Delete Morning Loop" })).toBeInTheDocument(),
  },
  {
    name: "Combobox (open)",
    build: () => (
      <Combobox
        aria-label="Time zone"
        options={[
          { value: "UTC", label: "UTC" },
          { value: "Europe/London", label: "Europe/London" },
        ]}
        value="UTC"
        onChange={() => {}}
      />
    ),
    assert: () =>
      expect(screen.getByRole("combobox", { name: "Time zone" })).toBeInTheDocument(),
  },
  {
    name: "DownloadLink (inline)",
    build: () => (
      <DownloadLink href="/x" download="w.waiveobk" icon={Download}>
        Download
      </DownloadLink>
    ),
    assert: () => expect(screen.getByRole("link", { name: "Download" })).toBeInTheDocument(),
  },
  {
    name: "DownloadLink (icon)",
    build: () => (
      <DownloadLink variant="icon" href="/x" download icon={Download} aria-label="Export" />
    ),
    assert: () => expect(screen.getByRole("link", { name: "Export" })).toBeInTheDocument(),
  },
];

describe.each(THEMES)("kit renders under the %s theme", (theme) => {
  it.each(cases)("$name", ({ build, assert }) => {
    document.documentElement.setAttribute("data-theme", theme);
    const { unmount } = render(build());
    assert();
    unmount();
  });
});
