import { Moon, Sun } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Tooltip } from "@/components/kit";
import { useTheme } from "./theme-context";

/**
 * The Dusk/Daybreak switch. It dogfoods the vendored Button and always carries an
 * accessible label naming the theme it switches to (the a11y contract: every
 * interactive control is labeled).
 *
 * The name is also shown as a kit Tooltip: this is the one icon-only control on
 * EVERY page of the console, and a sun/moon glyph is only obvious once you have
 * already pressed it. It was a native `title`, which shows on a hovering mouse
 * and on nothing else — not on keyboard focus, which is precisely how a
 * keyboard user meets an unlabelled glyph.
 */
export function ThemeToggle() {
  const { theme, toggleTheme } = useTheme();
  const target = theme === "dark" ? "Daybreak (light)" : "Dusk (dark)";
  const label = `Switch to ${target} theme`;

  return (
    <Tooltip tip={label} side="bottom">
      <Button
        type="button"
        variant="outline"
        size="icon"
        onClick={toggleTheme}
        aria-label={label}
        className="wv-touch"
      >
        {theme === "dark" ? <Sun aria-hidden="true" /> : <Moon aria-hidden="true" />}
      </Button>
    </Tooltip>
  );
}
