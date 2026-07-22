import { useEffect, useState } from "react";

/**
 * useMediaQuery — subscribe a component to a CSS media query and re-render when
 * it changes. The initial value is read synchronously in the state initializer,
 * so the very first paint is already correct (no flash into the wrong layout);
 * the effect then keeps it live across viewport crossings.
 *
 * This is how the kit's responsive primitives (e.g. DataTable's stacked-card
 * switch below the narrow breakpoint) decide their presentation. It degrades to
 * `false` where `matchMedia` is unavailable (older/headless environments).
 */
function readMatch(query: string): boolean {
  if (typeof window === "undefined" || typeof window.matchMedia !== "function") return false;
  return window.matchMedia(query).matches;
}

export function useMediaQuery(query: string): boolean {
  const [matches, setMatches] = useState(() => readMatch(query));

  useEffect(() => {
    if (typeof window === "undefined" || typeof window.matchMedia !== "function") return;
    const mql = window.matchMedia(query);
    const onChange = () => setMatches(mql.matches);
    // Sync once in case the query changed between render and effect.
    onChange();
    mql.addEventListener?.("change", onChange);
    return () => mql.removeEventListener?.("change", onChange);
  }, [query]);

  return matches;
}
