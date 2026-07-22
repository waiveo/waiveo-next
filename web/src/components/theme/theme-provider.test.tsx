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
});
