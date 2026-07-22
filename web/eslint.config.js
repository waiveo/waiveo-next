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
  // The kit and the vendored base are the only places allowed to import from
  // components/ui — they are the wrapping layer.
  {
    files: ["src/components/kit/**", "src/components/ui/**"],
    rules: { "no-restricted-imports": "off" },
  },
  // Node-context config files.
  {
    files: ["*.{js,ts}", "vite.config.ts", "eslint.config.js"],
    languageOptions: { globals: globals.node },
  },
);
