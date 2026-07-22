// ui-schema/1 — the widget-node dispatcher.
//
// Walks one widget node (UIS-060): it gates on `visibleIf` (UIS-063) then
// dispatches on `type` through a closed registry to the mapped kit-rendered
// component. The document is already validated, so `type` is always a catalog
// member; a defensive miss renders nothing rather than an unstyled container
// (closed sets are closed — never a best-effort render).

import type { ReactNode } from "react";
import { useRenderer } from "../state";
import type { WidgetNode } from "../types";
import type { RenderScope } from "../bindings";
import { isVisible, type WidgetProps } from "./common";
import { BadgeWidget, StatTileWidget, TableWidget, TextWidget } from "./display";
import {
  DurationInputWidget,
  EntityPickerWidget,
  MultiSelectWidget,
  NumberInputWidget,
  SelectWidget,
  TextInputWidget,
  TimeOfDayWidget,
  ToggleWidget,
} from "./inputs";
import { ButtonWidget } from "./action";
import { FragmentWidget, RepeatWidget, SectionWidget, SlotWidget, SwitchWidget } from "./structural";

const REGISTRY: Record<string, (props: WidgetProps) => ReactNode> = {
  // structural
  section: SectionWidget,
  repeat: RepeatWidget,
  switch: SwitchWidget,
  fragment: FragmentWidget,
  slot: SlotWidget,
  // display
  text: TextWidget,
  badge: BadgeWidget,
  table: TableWidget,
  "stat-tile": StatTileWidget,
  // input
  "text-input": TextInputWidget,
  "number-input": NumberInputWidget,
  "duration-input": DurationInputWidget,
  "time-of-day": TimeOfDayWidget,
  toggle: ToggleWidget,
  select: SelectWidget,
  "multi-select": MultiSelectWidget,
  "entity-picker": EntityPickerWidget,
  // action
  button: ButtonWidget,
};

export function WidgetNodeView({
  node,
  scope,
  depth = 0,
}: {
  node: WidgetNode;
  scope: RenderScope;
  depth?: number;
}) {
  const { env } = useRenderer();
  if (!isVisible(node, scope, env)) return null;
  const Widget = REGISTRY[node.type];
  if (!Widget) return null;
  return <Widget node={node} scope={scope} depth={depth} />;
}
