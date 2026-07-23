import { describe, it, expect, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { MemoryRouter } from "react-router";
import { ThemeProvider } from "@/components/theme/theme-provider";
import { AppShell } from "@/shell/app-shell";
import { TRACE_ID, pack, PACK_EN_CATALOG } from "@/api/test-support";

// The shell's Extensions nav lists installed packs' pages, titled from each pack's
// own locale catalog (MAN-060/111) and linking to `/p/{pack}/{path}`. It is a
// distinct nav landmark from the core Primary nav — third-party destinations are
// clearly demarcated — and it simply does not appear when no packs are installed.

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

function jsonBody(body: Parameters<typeof HttpResponse.json>[0]) {
  return HttpResponse.json(body, { headers: { "Trace-Id": TRACE_ID } });
}

function renderShell() {
  return render(
    <ThemeProvider>
      <MemoryRouter initialEntries={["/"]}>
        <AppShell>
          <div>content</div>
        </AppShell>
      </MemoryRouter>
    </ThemeProvider>,
  );
}

describe("Extensions nav", () => {
  it("lists an installed pack's pages with catalog-resolved titles, linking to /p/{pack}/{path}", async () => {
    server.use(
      http.get("*/api/v1/packs", () => jsonBody({ items: [pack()], cursor: null })),
      http.get("*/api/v1/packs/acme/menu-board/messages/en", () => jsonBody(PACK_EN_CATALOG)),
    );
    renderShell();

    // A distinct Extensions landmark (not merged into Primary).
    const ext = await screen.findByRole("navigation", { name: "Extensions" });
    // The pack's display name titles its group…
    expect(within(ext).getByText("Menu Board")).toBeInTheDocument();
    // …and each ui.pages[] entry is a link with its catalog-resolved title + route.
    expect(within(ext).getByRole("link", { name: "Menu Items" })).toHaveAttribute(
      "href",
      "/p/acme/menu-board/menu-items",
    );
    expect(within(ext).getByRole("link", { name: "Settings" })).toHaveAttribute(
      "href",
      "/p/acme/menu-board/settings",
    );

    // The core Primary nav is untouched (the eight console destinations).
    const primary = screen.getAllByRole("navigation", { name: /primary/i })[0];
    expect(within(primary).getAllByRole("link")).toHaveLength(8);
  });

  it("shows no Extensions section when no packs are installed", async () => {
    server.use(http.get("*/api/v1/packs", () => jsonBody({ items: [], cursor: null })));
    renderShell();
    // Let the (empty) packs list settle, then confirm no Extensions landmark.
    await waitFor(() => expect(screen.getByText("content")).toBeInTheDocument());
    expect(screen.queryByRole("navigation", { name: "Extensions" })).not.toBeInTheDocument();
  });
});
