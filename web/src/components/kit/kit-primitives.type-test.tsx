/**
 * Type-level tests (checked by `tsc --noEmit`, not run by vitest) for the a11y
 * contracts the primitives added with this kit expansion enforce IN THE COMPILER
 * rather than in a docstring — the same guarantee `button.type-test.tsx` gives
 * the Button and the `Checkbox` wrapper gives a table's row boxes.
 *
 * Each `@ts-expect-error` below is the test. If a union were loosened so the bad
 * call type-checked, the directive would become unused and `tsc` would fail
 * here — so this file failing to compile IS the guarantee.
 */
import { Download } from "lucide-react";
import { Combobox } from "./combobox";
import { DownloadLink } from "./link";
import { Tab, TabList, TabPanel, Tabs } from "./tabs";

const ZONES = [{ value: "UTC", label: "UTC" }];

// ── TabList: a tablist MUST be named ────────────────────────────────────────

export function TabsLabelledByAria() {
  return (
    <Tabs defaultValue="a">
      <TabList aria-label="Screen views">
        <Tab value="a">Live</Tab>
      </TabList>
      <TabPanel value="a">panel</TabPanel>
    </Tabs>
  );
}

export function TabsLabelledByHeading() {
  return (
    <Tabs defaultValue="a">
      <TabList aria-labelledby="views-heading">
        <Tab value="a">Live</Tab>
      </TabList>
      <TabPanel value="a">panel</TabPanel>
    </Tabs>
  );
}

export function NamelessTabList() {
  return (
    <Tabs defaultValue="a">
      {/* @ts-expect-error — a TabList requires aria-label or aria-labelledby. */}
      <TabList>
        <Tab value="a">Live</Tab>
      </TabList>
      <TabPanel value="a">panel</TabPanel>
    </Tabs>
  );
}

// ── Combobox: the trigger's name cannot come from its own content ───────────

export function ComboboxLabelledByAria() {
  return <Combobox aria-label="Time zone" options={ZONES} value="" onChange={() => {}} />;
}

export function NamelessCombobox() {
  // @ts-expect-error — a Combobox requires aria-label or aria-labelledby.
  return <Combobox options={ZONES} value="" onChange={() => {}} />;
}

// ── DownloadLink: a name, and a `download` that is never optional ───────────

export function DownloadLinkWithText() {
  return (
    <DownloadLink href="/x" download="w.waiveobk">
      Download
    </DownloadLink>
  );
}

export function DownloadLinkIconOnly() {
  return <DownloadLink variant="icon" href="/x" download icon={Download} aria-label="Export" />;
}

export function NamelessDownloadLink() {
  // @ts-expect-error — an icon-only DownloadLink requires an aria-label.
  return <DownloadLink variant="icon" href="/x" download icon={Download} />;
}

export function NavigatingLink() {
  // @ts-expect-error — `download` is required; without it the file replaces the console.
  return <DownloadLink href="/x">Download</DownloadLink>;
}
