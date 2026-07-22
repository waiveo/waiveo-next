/// <reference types="vitest/config" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "node:path";

// The Go feeder serves the API (/api/v1), the live event stream (/events/v1) and
// the content origin (/content/) on a self-signed HTTPS listener. During `make
// web-dev` the Vite dev server proxies those prefixes to it so the SPA talks to
// the real backend; `secure: false` accepts the feeder's ed25519 self-signed leaf.
const FEEDER = "https://127.0.0.1:7420";
const proxyToFeeder = { target: FEEDER, changeOrigin: true, secure: false };

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: { "@": path.resolve(import.meta.dirname, "src") },
  },
  server: {
    proxy: {
      "/api": proxyToFeeder,
      "/events": proxyToFeeder,
      "/content": proxyToFeeder,
    },
  },
  test: {
    environment: "jsdom",
    setupFiles: ["./src/test/setup.ts"],
    css: true,
    include: ["src/**/*.{test,spec}.{ts,tsx}"],
  },
});
