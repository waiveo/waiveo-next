import js from "@eslint/js";
import globals from "globals";
import reactHooks from "eslint-plugin-react-hooks";
import reactRefresh from "eslint-plugin-react-refresh";
import tseslint from "typescript-eslint";

// The single structural guarantee this config enforces: the widget kit
// (src/components/kit) is the ONLY sanctioned component surface. Pages, routes
// and (later) the ui-schema renderer import from the kit; they may never reach
// into the vendored shadcn base (src/components/ui) directly. The kit and the
// vendored base themselves are exempt — they ARE the layer that wraps `ui`.
const kitOnlyImports = {
  patterns: [
    {
      group: ["@/components/ui", "@/components/ui/*", "**/components/ui", "**/components/ui/*"],
      message:
        "Import UI primitives through the widget kit (@/components/kit), never components/ui directly. The kit is the only sanctioned component surface.",
    },
  ],
};

export default tseslint.config(
  { ignores: ["dist", "node_modules", "coverage"] },
  js.configs.recommended,
  tseslint.configs.recommended,
  {
    files: ["**/*.{ts,tsx}"],
    languageOptions: {
      ecmaVersion: 2022,
      globals: globals.browser,
    },
    plugins: {
      "react-hooks": reactHooks,
      "react-refresh": reactRefresh,
    },
    rules: {
      "react-hooks/rules-of-hooks": "error",
      "react-hooks/exhaustive-deps": "warn",
      "react-refresh/only-export-components": ["warn", { allowConstantExport: true }],
      "no-restricted-imports": ["error", kitOnlyImports],
    },
  },
  // The kit, the vendored base, and the theme infrastructure are the only places
  // allowed to import from components/ui — they ARE the wrapping/base layer, not
  // pages. Everything else must go through the kit.
  {
    files: ["src/components/kit/**", "src/components/ui/**", "src/components/theme/**"],
    rules: { "no-restricted-imports": "off" },
  },
  // Node-context files: the config files at the root and the tooling scripts
  // under scripts/ (e.g. the dev-server SSE proxy probe) run on Node, not in the
  // browser, so they get the Node globals (process, fetch, AbortController, …).
  {
    files: ["*.{js,ts}", "vite.config.ts", "eslint.config.js", "scripts/**/*.{js,mjs,ts}"],
    languageOptions: { globals: globals.node },
  },
  // The Playwright click-through gate (tests/e2e) is DEV-ONLY and runs under the
  // Playwright test runner on Node — it reads the pack zip off disk (fs), sees
  // PW_* env, and drives the browser through the `page` fixture. Give it the Node
  // globals; it is never built into the app bundle (tsconfig `include` is `src`).
  {
    files: ["tests/**/*.{ts,tsx}"],
    languageOptions: { globals: globals.node },
  },
);
