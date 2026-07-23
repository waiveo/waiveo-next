// ui-schema/1 — the action widget (UIS-070): button.
//
// A button is the one action idiom, rendered through the kit Button. Its
// required `on.press` (UIS-074) carries an ActionRef dispatched through the
// closed verb grammar (actions.tsx). `style` maps onto the kit's button
// variants; `primary` is the one gradient-carrying primary action per view.

import { Button, type ButtonProps } from "@/components/kit";
import { runAction } from "../actions";
import { useRenderer } from "../state";
import type { ActionRef } from "../types";
import type { WidgetProps } from "./common";
import { useWizard } from "./wizard-context";

type Variant = NonNullable<ButtonProps["variant"]>;
const STYLE_VARIANT: Record<string, Variant> = {
  primary: "default",
  secondary: "secondary",
  destructive: "destructive",
};

export function ButtonWidget({ node, scope }: WidgetProps) {
  const ctx = useRenderer();
  const wizard = useWizard();
  const press = node.on?.press as ActionRef | undefined;
  // A `delete` acts on an already-persisted record (its `$root`), which does not
  // exist inside a list-detail create draft (the New idiom, UIS-021): the bound
  // record is the not-yet-saved `$ui.__draft`, carrying no id/revision, so the host
  // remove seam can only no-op on it. Rather than paint a fully-enabled control that
  // silently does nothing, don't render it — a create form offers Save, never
  // Delete. `draftCreateTarget` is set only inside that draft scope, and rides
  // through every nested narrowing (narrowToItem/Fragment spread the scope).
  if (press?.verb === "delete" && scope.draftCreateTarget !== undefined) return null;
  const label = ctx.env.msg(String(node.props?.labelMsg ?? ""));
  const variant = STYLE_VARIANT[String(node.props?.style ?? "secondary")] ?? "secondary";
  return (
    // `.wv-touch` holds the button to the 44px touch-target minimum the responsive
    // contract mandates (the kit Button's `default` size is a 36px `h-9`).
    <Button
      variant={variant}
      className="wv-touch"
      onClick={() => {
        if (press) runAction(press, scope, ctx, wizard);
      }}
    >
      {label}
    </Button>
  );
}
