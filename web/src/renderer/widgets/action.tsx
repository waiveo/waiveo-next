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
  const label = ctx.env.msg(String(node.props?.labelMsg ?? ""));
  const variant = STYLE_VARIANT[String(node.props?.style ?? "secondary")] ?? "secondary";
  const press = node.on?.press as ActionRef | undefined;
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
