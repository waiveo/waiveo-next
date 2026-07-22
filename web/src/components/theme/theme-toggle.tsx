import { Moon, Sun } from "lucide-react";
import { Button } from "@/components/ui/button";
import { useTheme } from "./theme-context";

/**
 * The Dusk/Daybreak switch. It dogfoods the vendored Button and always carries an
 * accessible label naming the theme it switches to (the a11y contract: every
 * interactive control is labeled).
 */
export function ThemeToggle() {
  const { theme, toggleTheme } = useTheme();
  const target = theme === "dark" ? "Daybreak (light)" : "Dusk (dark)";
  const label = `Switch to ${target} theme`;

  return (
    <Button
      type="button"
      variant="outline"
      size="icon"
      onClick={toggleTheme}
      aria-label={label}
      title={label}
      className="wv-touch"
    >
      {theme === "dark" ? <Sun aria-hidden="true" /> : <Moon aria-hidden="true" />}
    </Button>
  );
}
