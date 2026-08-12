import type { ComponentProps, ReactNode } from "react";
import {
  Tabs as UITabs,
  TabsContent as UITabsContent,
  TabsList as UITabsList,
  TabsTrigger as UITabsTrigger,
} from "@/components/ui/tabs";

/**
 * Tabs — one set of mutually exclusive panels, and the console's only sanctioned
 * way to build one.
 *
 * # Why this is not a row of buttons
 *
 * A hand-rolled strip of `<button aria-current="page">` looks identical and is a
 * different control. `aria-current="page"` says "this is the page you are on",
 * which is a claim about NAVIGATION — the URL did not change and there is no
 * page to be on, so a screen reader is told something untrue. A real tab set
 * announces "tab 2 of 4, selected", ties each tab to the panel it reveals with
 * `aria-controls`, and — the part no button row ever reproduces — makes the
 * whole strip ONE tab stop with Left/Right/Home/End moving between tabs inside
 * it. On a button row, a keyboard user tabs through every option to reach the
 * content; here they press Tab once.
 *
 * # The label is required at the type level
 *
 * A `tablist` with no accessible name announces as "tab list" and nothing else,
 * which on a page with two of them is unusable. The kit's standing discipline —
 * every interactive widget carries an accessible label, and `Checkbox` enforces
 * it in the type rather than in a comment — applies here to the LIST, not to the
 * individual tabs: a `Tab`'s visible text is already its name, and demanding a
 * second one would produce the double-labelling it is meant to prevent.
 *
 * # Tabs are not navigation
 *
 * Use these for panels of the same subject that share a page and a URL. A set of
 * destinations with their own addresses is a nav (`NavDrawer`, the app rail) —
 * tabs would make each one unlinkable and unbookmarkable. And a set of stacked
 * sections an operator reads TOGETHER (health beside logs) is not a tab set
 * either: tabs hide everything but one panel, which is a cost, not a feature.
 */
export type TabsProps = ComponentProps<typeof UITabs>;

export function Tabs(props: TabsProps) {
  return <UITabs {...props} />;
}

type TabListBase = Omit<ComponentProps<typeof UITabsList>, "aria-label" | "aria-labelledby">;

/** A `tablist` MUST be named — by `aria-label`, or by `aria-labelledby` when a
 * visible heading already says what the tabs are for. Neither is a valid state,
 * and the union refuses it before it can render. */
export type TabListProps = TabListBase &
  (
    | { "aria-label": string; "aria-labelledby"?: string }
    | { "aria-labelledby": string; "aria-label"?: string }
  );

export function TabList(props: TabListProps) {
  return <UITabsList {...props} />;
}

export interface TabProps extends Omit<ComponentProps<typeof UITabsTrigger>, "children"> {
  /** The panel this tab reveals. */
  value: string;
  /** The tab's visible text — which IS its accessible name, so it is required. */
  children: ReactNode;
}

export function Tab({ children, ...rest }: TabProps) {
  return <UITabsTrigger {...rest}>{children}</UITabsTrigger>;
}

export type TabPanelProps = ComponentProps<typeof UITabsContent>;

export function TabPanel(props: TabPanelProps) {
  return <UITabsContent {...props} />;
}
