import "@testing-library/jest-dom/vitest";
import { afterEach } from "vitest";
import { cleanup } from "@testing-library/react";

// Unmount and clear the DOM after every test so renders never leak across cases.
afterEach(() => {
  cleanup();
});

// jsdom lacks a handful of layout/pointer APIs that Radix primitives call when
// they open (Select, DropdownMenu, Tooltip, ScrollArea…). Polyfill them as
// no-ops so overlay components can be rendered open in unit tests without
// throwing. These are test-environment shims only — never shipped.
if (typeof window !== "undefined") {
  const proto = window.Element.prototype as unknown as Record<string, unknown>;
  if (!proto.scrollIntoView) proto.scrollIntoView = () => {};
  if (!proto.hasPointerCapture) proto.hasPointerCapture = () => false;
  if (!proto.setPointerCapture) proto.setPointerCapture = () => {};
  if (!proto.releasePointerCapture) proto.releasePointerCapture = () => {};

  if (!window.ResizeObserver) {
    window.ResizeObserver = class {
      observe() {}
      unobserve() {}
      disconnect() {}
    };
  }

  if (!window.matchMedia) {
    window.matchMedia = (query: string) =>
      ({
        matches: false,
        media: query,
        onchange: null,
        addListener: () => {},
        removeListener: () => {},
        addEventListener: () => {},
        removeEventListener: () => {},
        dispatchEvent: () => false,
      }) as unknown as MediaQueryList;
  }

  // Node 25 ships an experimental global `localStorage` (behind
  // --localstorage-file) that shadows jsdom's and is unusable without a backing
  // file. Install a clean, Map-backed Storage so theme-persistence tests are
  // deterministic and independent of the host runtime's webstorage flag.
  const store = new Map<string, string>();
  const memoryStorage = {
    get length() {
      return store.size;
    },
    clear: () => store.clear(),
    getItem: (key: string) => (store.has(key) ? store.get(key)! : null),
    key: (index: number) => Array.from(store.keys())[index] ?? null,
    removeItem: (key: string) => {
      store.delete(key);
    },
    setItem: (key: string, value: string) => {
      store.set(key, String(value));
    },
  } as Storage;
  Object.defineProperty(window, "localStorage", {
    value: memoryStorage,
    configurable: true,
    writable: true,
  });
}
