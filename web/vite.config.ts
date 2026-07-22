/// <reference types="vitest/config" />
import { defineConfig, type ProxyOptions } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import path from "node:path";

// The Go feeder serves the API (/api/v1), the live event stream (/events/v1) and
// the content origin (/content/) on a self-signed HTTPS listener. During `make
// web-dev` the Vite dev server proxies those prefixes to it so the SPA talks to
// the real backend; `secure: false` accepts the feeder's self-signed leaf (an
// ECDSA P-256 cert real browsers can handshake — see the feeder's tls package).
const FEEDER = "https://127.0.0.1:7420";
const proxyToFeeder: ProxyOptions = { target: FEEDER, changeOrigin: true, secure: false };

// /events/v1 is Server-Sent Events: the feeder opens the response with the 200 +
// `text/event-stream` headers and then stays SILENT until an event is recorded
// (a fresh subscribe has no backlog). http-proxy-3's outgoing passes only
// setHeader()/set res.statusCode — they never flush — so Node buffers the client
// response's header block until the first body byte, which for an idle stream
// never comes: the browser's EventSource hangs with no `open`, and every event
// bound `live` degrades to its static value forever. Flush the client headers as
// soon as the upstream SSE response arrives so the stream opens immediately; the
// body then pipes normally, live events included. http-proxy copies the headers
// onto `res` synchronously AFTER this event fires, so flush on the next microtask.
// Dev-server only — production serves the SPA embedded in the feeder, no proxy.
const proxyEvents: ProxyOptions = {
  ...proxyToFeeder,
  configure(proxy) {
    proxy.on("proxyRes", (proxyRes, _req, res) => {
      const contentType = proxyRes.headers["content-type"] ?? "";
      if (contentType.includes("text/event-stream")) {
        queueMicrotask(() => {
          if (!res.headersSent && !res.writableEnded) res.flushHeaders();
        });
      }
    });
  },
};

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: { "@": path.resolve(import.meta.dirname, "src") },
  },
  server: {
    proxy: {
      "/api": proxyToFeeder,
      "/events": proxyEvents,
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
