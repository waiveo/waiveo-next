// ui-schema/1 — the wizard step controller seam.
//
// The wizard verbs (`wizard-next`/`-back`/`-finish`, UIS-160) drive the enclosing
// wizard's step state, which the wizard PageRenderer owns. It publishes a
// controller here so a `button` (or any event) deep inside a step's subtree can
// invoke the built-in verbs without threading a callback through every scope.

import { createContext, useContext } from "react";
import type { WizardController } from "../types";

export const WizardContext = createContext<WizardController | null>(null);

export function useWizard(): WizardController | undefined {
  return useContext(WizardContext) ?? undefined;
}
