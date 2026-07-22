import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
// Self-hosted brand fonts (OFL) via @fontsource — bundled locally by Vite, so
// the built app makes ZERO external font/CDN requests (the appliance may be
// offline-adjacent, and we do no third-party tracking).
import "@fontsource-variable/bricolage-grotesque";
import "@fontsource-variable/inter";
import "@fontsource-variable/jetbrains-mono";
import App from "@/App";
import "@/index.css";

const rootEl = document.getElementById("root");
if (!rootEl) {
  throw new Error("root element #root not found");
}

createRoot(rootEl).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
