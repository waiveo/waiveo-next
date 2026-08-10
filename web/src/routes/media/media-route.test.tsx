import { describe, it, expect, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { ThemeProvider } from "@/components/theme/theme-provider";
import MediaRoute from "./media-route";
import { contentAsset, problem } from "@/api/test-support";

/**
 * The Media route reads the content origin's own listing — the half that did
 * not exist while upload was write-only. The cases below drive what an operator
 * actually does with it: look at what is on the box, sort by recency, copy a
 * reference, and see a plain explanation when the origin cannot be read.
 */

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => {
  server.resetHandlers();
  window.localStorage.clear();
});
afterAll(() => server.close());

function renderMedia() {
  return render(
    <ThemeProvider>
      <MediaRoute />
    </ThemeProvider>,
  );
}

describe("Media route", () => {
  it("lists the origin's assets newest first, with their size", async () => {
    server.use(
      http.get("*/api/v1/content", () =>
        HttpResponse.json({
          content: [
            contentAsset({ asset_ref: "sha256:0000older", url: "/content/0000older", stored_at: 1_000, size_bytes: 2048 }),
            contentAsset({ asset_ref: "sha256:ffffnewer", url: "/content/ffffnewer", stored_at: 9_000, size_bytes: 1_572_864 }),
          ],
        }),
      ),
    );
    renderMedia();

    const refs = await screen.findAllByTitle(/^sha256:/);
    // The origin serves digest order; the page shows most-recent first because
    // that is what an operator who just uploaded something is looking for.
    expect(refs[0]).toHaveAttribute("title", "sha256:ffffnewer");
    expect(screen.getAllByText("1.5 MB").length).toBeGreaterThan(0);
    expect(screen.getAllByText("2 KB").length).toBeGreaterThan(0);
  });

  it("copies an asset's full reference to the clipboard", async () => {
    server.use(
      http.get("*/api/v1/content", () => HttpResponse.json({ content: [contentAsset()] })),
    );
    // userEvent.setup() installs its own clipboard stub, so it is the one to
    // read back from — a hand-rolled stub defined first would just be replaced.
    const user = userEvent.setup();
    renderMedia();
    await user.click(await screen.findByRole("button", { name: "Copy content reference sha256:aa11bb22cc33" }));
    expect(await screen.findByText("Copied")).toBeInTheDocument();
    // The SHORT form is what the page shows; the FULL ref is what gets copied —
    // a truncated digest pasted into a playlist item would resolve to nothing.
    expect(await navigator.clipboard.readText()).toBe("sha256:aa11bb22cc33");
  });

  it("says the library is empty rather than showing a blank page", async () => {
    server.use(http.get("*/api/v1/content", () => HttpResponse.json({ content: [] })));
    renderMedia();
    expect(await screen.findByText(/media library is empty/i)).toBeInTheDocument();
  });

  it("surfaces a failure to read the origin", async () => {
    server.use(
      http.get("*/api/v1/content", () => problem(503, "INTERNAL", "The origin is unavailable.")),
    );
    renderMedia();
    expect(await screen.findByRole("alert")).toHaveTextContent(/origin is unavailable/i);
  });

  it("falls back to a placeholder for bytes the browser cannot decode", async () => {
    server.use(
      http.get("*/api/v1/content", () =>
        HttpResponse.json({ content: [contentAsset({ asset_ref: "sha256:video", url: "/content/video" })] }),
      ),
    );
    renderMedia();

    const img = await screen.findByRole("presentation");
    // A content-addressed origin knows no MIME type, so "is this an image?" is
    // answered by whether the browser could decode it — and the tile must not
    // be left showing a broken-image glyph when it could not.
    fireEvent.error(img);
    expect(screen.queryByRole("presentation")).toBeNull();
  });
});
