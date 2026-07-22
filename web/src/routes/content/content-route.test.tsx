import { describe, it, expect, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { ThemeProvider } from "@/components/theme/theme-provider";
import ContentRoute from "./content-route";
import { TRACE_ID, problem } from "@/api/test-support";

function renderContent() {
  return render(
    <ThemeProvider>
      <ContentRoute />
    </ThemeProvider>,
  );
}

// Content is a first-party page (the sanctioned file-upload grammar gap): a
// drop-zone / picker POSTs bytes to /api/v1/content and the returned
// content-addressed ref lands in a session library with a copy action. These
// tests drive the picker and the drop path, the copy affordance, and the Problem
// surface, over the same-origin relative client (msw wildcard patterns).

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => {
  server.resetHandlers();
  setViewport(false);
  window.localStorage.clear();
});
afterAll(() => server.close());

function setViewport(narrow: boolean) {
  window.matchMedia = ((query: string) =>
    ({
      matches: /max-width/.test(query) ? narrow : !narrow,
      media: query,
      onchange: null,
      addEventListener: () => {},
      removeEventListener: () => {},
      addListener: () => {},
      removeListener: () => {},
      dispatchEvent: () => false,
    }) as unknown as MediaQueryList) as unknown as typeof window.matchMedia;
}

function uploadOk(assetRef: string) {
  return http.post("*/api/v1/content", () =>
    HttpResponse.json(
      { asset_ref: assetRef, url: `/content/${assetRef.replace("sha256:", "")}` },
      { status: 201, headers: { "Trace-Id": TRACE_ID } },
    ),
  );
}

describe("ContentRoute — upload + library", () => {
  it("shows an empty state before anything is uploaded", () => {
    renderContent();
    expect(screen.getByText(/nothing uploaded yet/i)).toBeInTheDocument();
  });

  it("uploads a picked file (carrying an Idempotency-Key) and lists its content ref", async () => {
    let key: string | null = null;
    server.use(
      http.post("*/api/v1/content", ({ request }) => {
        key = request.headers.get("Idempotency-Key");
        return HttpResponse.json(
          { asset_ref: "sha256:abc123", url: "/content/abc123" },
          { status: 201, headers: { "Trace-Id": TRACE_ID } },
        );
      }),
    );

    const user = userEvent.setup();
    renderContent();
    const input = screen.getByLabelText("Choose files");
    await user.upload(input, new File(["hello"], "poster.png", { type: "image/png" }));

    expect(await screen.findByText("poster.png")).toBeInTheDocument();
    expect(screen.getByText("sha256:abc123")).toBeInTheDocument();
    // A deck-gradient placeholder poster stands in for the (content-addressed) thumb.
    expect(document.querySelector('[data-slot="content-thumb"]')).not.toBeNull();
    expect(key).toMatch(/^[0-9a-f-]{36}$/i);
  });

  it("uploads a dropped file", async () => {
    server.use(uploadOk("sha256:dropped"));
    renderContent();
    const dropzone = screen.getByRole("button", { name: /upload content/i });
    const file = new File(["x"], "banner.jpg", { type: "image/jpeg" });
    fireEvent.drop(dropzone, { dataTransfer: { files: [file], types: ["Files"] } });
    expect(await screen.findByText("banner.jpg")).toBeInTheDocument();
    expect(screen.getByText("sha256:dropped")).toBeInTheDocument();
  });

  it("copies an asset's content reference to the clipboard", async () => {
    server.use(uploadOk("sha256:copyme"));
    const user = userEvent.setup();
    renderContent();
    await user.upload(screen.getByLabelText("Choose files"), new File(["x"], "clip.png", { type: "image/png" }));
    await screen.findByText("clip.png");

    await user.click(screen.getByRole("button", { name: /copy content reference for clip\.png/i }));
    expect(await screen.findByText("Copied")).toBeInTheDocument();
    expect(await navigator.clipboard.readText()).toBe("sha256:copyme");
  });

  it("surfaces a 422 Problem's field message inline", async () => {
    server.use(
      http.post("*/api/v1/content", () =>
        problem(422, "VALIDATION_FAILED", "The upload was rejected.", {
          errors: [{ field: "file", code: "UNSUPPORTED_MEDIA_TYPE", message: "Unsupported media type." }],
        }),
      ),
    );
    const user = userEvent.setup();
    renderContent();
    await user.upload(screen.getByLabelText("Choose files"), new File(["x"], "bad.exe", { type: "" }));

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(/unsupported media type/i);
    // The upload failed, so the library stays empty.
    expect(screen.getByText(/nothing uploaded yet/i)).toBeInTheDocument();
  });
});

describe("ContentRoute — responsive at 360px", () => {
  it("keeps the upload control a >=44px touch target and renders a card without a wide table", async () => {
    setViewport(true);
    server.use(uploadOk("sha256:narrow"));
    renderContent();

    const dropzone = screen.getByRole("button", { name: /upload content/i });
    expect(dropzone.className).toContain("wv-touch");

    fireEvent.drop(dropzone, {
      dataTransfer: { files: [new File(["x"], "phone.png", { type: "image/png" })], types: ["Files"] },
    });
    await screen.findByText("phone.png");
    // No <table> anywhere — nothing to force horizontal page scroll at 360px.
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
    expect(document.querySelector('[data-slot="content-thumb"]')).not.toBeNull();
  });
});
