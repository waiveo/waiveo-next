import { describe, it, expect, afterEach } from "vitest";
import { renderHook, act } from "@testing-library/react";
import { useMediaQuery } from "./use-media-query";

// A controllable matchMedia double: it records the change listener the hook
// subscribes with so a test can drive a viewport crossing and assert the hook
// re-renders with the new value (the behaviour a responsive component relies on).
function installMatchMedia(initial: boolean) {
  let listener: ((e: unknown) => void) | null = null;
  const mql = {
    matches: initial,
    media: "",
    onchange: null,
    addEventListener: (_type: string, cb: (e: unknown) => void) => {
      listener = cb;
    },
    removeEventListener: () => {
      listener = null;
    },
    addListener: () => {},
    removeListener: () => {},
    dispatchEvent: () => false,
  };
  window.matchMedia = ((q: string) => {
    mql.media = q;
    return mql;
  }) as unknown as typeof window.matchMedia;
  return {
    set(next: boolean) {
      mql.matches = next;
      if (listener) act(() => listener!({} as unknown));
    },
  };
}

afterEach(() => {
  // Restore the setup.ts default (matches:false) so nothing leaks across tests.
  window.matchMedia = ((query: string) =>
    ({
      matches: false,
      media: query,
      onchange: null,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
    }) as unknown as MediaQueryList) as unknown as typeof window.matchMedia;
});

describe("useMediaQuery", () => {
  it("returns the query's current match on mount", () => {
    installMatchMedia(true);
    const { result } = renderHook(() => useMediaQuery("(max-width: 640px)"));
    expect(result.current).toBe(true);
  });

  it("returns false when the query does not match", () => {
    installMatchMedia(false);
    const { result } = renderHook(() => useMediaQuery("(min-width: 1024px)"));
    expect(result.current).toBe(false);
  });

  it("updates when the viewport crosses the query boundary", () => {
    const mm = installMatchMedia(false);
    const { result } = renderHook(() => useMediaQuery("(max-width: 640px)"));
    expect(result.current).toBe(false);
    mm.set(true);
    expect(result.current).toBe(true);
  });
});
