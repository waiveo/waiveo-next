import { beforeEach, describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ThemeProvider } from "./theme-provider";
import { ThemeToggle } from "./theme-toggle";
import { STORAGE_KEY } from "./theme-context";

function setup() {
  return render(
    <ThemeProvider>
      <ThemeToggle />
    </ThemeProvider>,
  );
}

describe("ThemeProvider + ThemeToggle", () => {
  beforeEach(() => {
    window.localStorage.clear();
    document.documentElement.removeAttribute("data-theme");
  });

  it("defaults to Dusk (dark) with no stored preference", () => {
    setup();
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
  });

  it("restores a persisted Daybreak (light) preference", () => {
    window.localStorage.setItem(STORAGE_KEY, "light");
    setup();
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
  });

  it("gives the toggle a >=44px touch target (wv-touch) like the rest of the shell chrome", () => {
    // ThemeToggle sits in the always-visible shell header beside the 44px
    // hamburger, so it must carry the same wv-touch treatment (size=icon is only
    // 36px on its own).
    setup();
    const toggle = screen.getByRole("button", { name: /switch to .* theme/i });
    expect(toggle.className).toContain("wv-touch");
  });

  it("flips data-theme and persists to localStorage on toggle", async () => {
    const user = userEvent.setup();
    setup();

    // Starts dark → the toggle is labeled with the theme it switches TO.
    const toDaybreak = screen.getByRole("button", { name: /switch to daybreak \(light\) theme/i });
    await user.click(toDaybreak);

    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
    expect(window.localStorage.getItem(STORAGE_KEY)).toBe("light");

    // Now light → label flips to switch back to Dusk.
    const toDusk = screen.getByRole("button", { name: /switch to dusk \(dark\) theme/i });
    await user.click(toDusk);

    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
    expect(window.localStorage.getItem(STORAGE_KEY)).toBe("dark");
  });

  it("says what the sun/moon glyph does — on hover AND on keyboard focus", async () => {
    // The one icon-only control on every page of the console. It carried a
    // native `title`, which shows for a hovering mouse and for nobody else, so
    // a keyboard user met an unexplained glyph. It is now the kit Tooltip.
    const user = userEvent.setup();
    setup();
    const toggle = screen.getByRole("button", { name: /switch to daybreak \(light\) theme/i });

    await user.hover(toggle);
    expect(await screen.findByRole("tooltip")).toHaveTextContent(
      "Switch to Daybreak (light) theme",
    );
    await user.keyboard("{Escape}");

    toggle.focus();
    expect(await screen.findByRole("tooltip")).toHaveTextContent(
      "Switch to Daybreak (light) theme",
    );
  });

  it("still toggles when it is wrapped in a tooltip trigger", async () => {
    // A trigger wrapping a live button is where an onClick goes missing.
    const user = userEvent.setup();
    setup();
    await user.click(screen.getByRole("button", { name: /switch to daybreak \(light\) theme/i }));
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
  });

  it("names the toggle by its OWN label, never by the tip", () => {
    // If the tip became the accessible name, the control would be nameless
    // whenever it is closed — which is nearly always.
    setup();
    const toggle = screen.getByRole("button", { name: /switch to daybreak \(light\) theme/i });
    expect(toggle).toHaveAttribute("aria-label", "Switch to Daybreak (light) theme");
  });
});
