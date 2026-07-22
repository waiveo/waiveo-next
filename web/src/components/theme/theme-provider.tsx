import { useCallback, useEffect, useMemo, useState, type ReactNode } from "react";
import {
  DEFAULT_THEME,
  isTheme,
  STORAGE_KEY,
  ThemeContext,
  type Theme,
  type ThemeContextValue,
} from "./theme-context";

function readStored(): Theme {
  if (typeof window === "undefined") return DEFAULT_THEME;
  try {
    const stored = window.localStorage.getItem(STORAGE_KEY);
    return isTheme(stored) ? stored : DEFAULT_THEME;
  } catch {
    return DEFAULT_THEME;
  }
}

function persist(theme: Theme): void {
  try {
    window.localStorage.setItem(STORAGE_KEY, theme);
  } catch {
    // A blocked/unavailable localStorage must never break theming.
  }
}

/**
 * ThemeProvider owns the console's Dusk/Daybreak theme: it restores the last
 * choice from localStorage (defaulting to Dusk), reflects it as `data-theme` on
 * the document element (which scopes every HORIZON token), and persists changes.
 */
export function ThemeProvider({ children }: { children: ReactNode }) {
  const [theme, setThemeState] = useState<Theme>(readStored);

  useEffect(() => {
    document.documentElement.setAttribute("data-theme", theme);
  }, [theme]);

  const setTheme = useCallback((next: Theme) => {
    persist(next);
    setThemeState(next);
  }, []);

  const toggleTheme = useCallback(() => {
    setThemeState((prev) => {
      const next: Theme = prev === "dark" ? "light" : "dark";
      persist(next);
      return next;
    });
  }, []);

  const value = useMemo<ThemeContextValue>(
    () => ({ theme, setTheme, toggleTheme }),
    [theme, setTheme, toggleTheme],
  );

  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>;
}
