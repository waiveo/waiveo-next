import { createContext, useContext } from "react";

// HORIZON is dual-mode by identity: Dusk (dark) is the default; Daybreak (light)
// is a first-class theme. The chosen theme is reflected as `data-theme` on the
// document element, which is what scopes every token in theme.css.
export type Theme = "dark" | "light";

export const DEFAULT_THEME: Theme = "dark";
export const STORAGE_KEY = "waiveo-theme";

export function isTheme(value: unknown): value is Theme {
  return value === "dark" || value === "light";
}

export interface ThemeContextValue {
  theme: Theme;
  setTheme: (theme: Theme) => void;
  toggleTheme: () => void;
}

export const ThemeContext = createContext<ThemeContextValue | undefined>(undefined);

export function useTheme(): ThemeContextValue {
  const ctx = useContext(ThemeContext);
  if (!ctx) {
    throw new Error("useTheme must be used within a <ThemeProvider>");
  }
  return ctx;
}
