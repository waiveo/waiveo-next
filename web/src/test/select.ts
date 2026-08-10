// A deterministic `<select>` interaction for tests (dev-only).
//
// Why this exists at all: `findBy*` waits for the ELEMENT, and a `<select>` fed
// by an async fetch exists from the first paint carrying nothing but its
// placeholder `<option>`. So `await user.selectOptions(await screen.findByLabelText(…), id)`
// is a race — it passes whenever the fetch happens to resolve before userEvent
// gets there (a warm module cache) and throws "Unable to find an option" when it
// does not (a cold CI run). The wait has to be on the OPTION, not the control.
//
// It lives in one place rather than being spelled at each call site because the
// failure is invisible when it passes: a test that races and wins looks exactly
// like a correct one, so every new select-driving test would have to rediscover
// the rule. Taking the option's VALUE (not its label) keeps the wait and the
// selection keyed on the same thing, so they cannot drift.

import { screen, waitFor } from "@testing-library/react";
import type { UserEvent } from "@testing-library/user-event";

/**
 * Find the select labelled `label`, WAIT until it actually carries an option
 * whose value is `value`, then select it. Returns the select element.
 */
export async function selectOptionWhenReady(
  user: UserEvent,
  label: string | RegExp,
  value: string,
): Promise<HTMLSelectElement> {
  const select = (await screen.findByLabelText(label)) as HTMLSelectElement;
  await waitFor(() => {
    const values = Array.from(select.options).map((o) => o.value);
    if (!values.includes(value)) {
      throw new Error(
        `select "${String(label)}" has no option ${value} yet (options: ${JSON.stringify(values)})`,
      );
    }
  });
  await user.selectOptions(select, value);
  return select;
}
