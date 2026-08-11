import { describe, it, expect, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { MemoryRouter } from "react-router";
import { ThemeProvider } from "@/components/theme/theme-provider";
import { AppShell } from "@/shell/app-shell";
import { TRACE_ID, pack, packManifest, PACK_EN_CATALOG } from "@/api/test-support";

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

    // The core Primary nav is untouched. What that MEANS is the assertion
    // below: no pack page leaked out of the Extensions landmark into Primary.
    // The count is the weaker proxy — it is the union of every route track's
    // destinations (11 = the original 8, plus Devices, Casts and Media), and it
    // has to be re-agreed every time a track adds a console route, which is why
    // it is no longer the only thing checked here (12 = plus System).
    const primary = screen.getAllByRole("navigation", { name: /primary/i })[0];
    expect(within(primary).getAllByRole("link")).toHaveLength(12);
    expect(
      within(primary)
        .getAllByRole("link")
        .filter((a) => a.getAttribute("href")?.startsWith("/p/")),
    ).toHaveLength(0);
  });

  it("drops a page whose manifest path would traverse out of /p/{pack}/ — no nav landmark escapes the pack", async () => {
    // The manifest engine does not (yet) constrain ui.pages[].path, so a pack can
    // declare a dot-segment path. Concatenated raw into a NavLink `to`, react-router
    // folds `../..` into an arbitrary OTHER console route (an `<a href="/design">`),
    // defeating the Extensions demarcation. Such a page must contribute NO nav entry.
    const evilManifest = packManifest({
      ui: {
        pages: [
          { path: "menu-items", pageType: "list-detail", titleMsg: "msg:page.menuItems.title" },
          { path: "../../../../design", pageType: "list-detail", titleMsg: "msg:page.settings.title" },
        ],
      },
    });
    server.use(
      http.get("*/api/v1/packs", () =>
        jsonBody({ items: [pack({ manifest: evilManifest })], cursor: null }),
      ),
      http.get("*/api/v1/packs/acme/menu-board/messages/en", () => jsonBody(PACK_EN_CATALOG)),
    );
    renderShell();

    const ext = await screen.findByRole("navigation", { name: "Extensions" });
    // The legit page still links, confined to the pack.
    expect(within(ext).getByRole("link", { name: "Menu Items" })).toHaveAttribute(
      "href",
      "/p/acme/menu-board/menu-items",
    );
    // The traversal page contributed nothing: the ONLY Extensions link is the legit
    // one, and no link resolves outside the pack's own `/p/{pack}/` prefix.
    const links = within(ext).getAllByRole("link");
    expect(links).toHaveLength(1);
    for (const link of links) {
      expect(link.getAttribute("href")).toMatch(/^\/p\/acme\/menu-board\//);
    }
  });

  it("survives a pack whose locale catalog carries a NON-string value — humanizes, never blanks the shell", async () => {
    // The install pipeline validates a locale catalog only as well-formed JSON, so a
    // pack can ship messages/en.json with a non-string value under a
    // manifest-referenced key (here pack.displayName → an object). Carried verbatim
    // through toRendererMessages, an unguarded resolveTitle would return that object
    // as `group.title`, which React refuses to render ("Objects are not valid as a
    // React child") — blanking the ENTIRE console (AppShell wraps every route, and
    // there is no ErrorBoundary). resolveTitle must guard the value's type and
    // humanize the reference instead, exactly as the renderer's makeMessageResolver.
    const poisonedCatalog = {
      ...PACK_EN_CATALOG,
      "pack.displayName": { evil: 1 },
    } as unknown as typeof PACK_EN_CATALOG;
    server.use(
      http.get("*/api/v1/packs", () => jsonBody({ items: [pack()], cursor: null })),
      http.get("*/api/v1/packs/acme/menu-board/messages/en", () => jsonBody(poisonedCatalog)),
    );
    renderShell();

    // The shell paints (its content is present) — no blank console.
    expect(await screen.findByText("content")).toBeInTheDocument();
    // The Extensions section still appears; the poisoned pack title degrades to the
    // humanized reference rather than crashing render.
    const ext = await screen.findByRole("navigation", { name: "Extensions" });
    expect(within(ext).getByText("Display Name")).toBeInTheDocument();
    // The pages, whose catalog values ARE valid strings, resolve normally.
    expect(within(ext).getByRole("link", { name: "Menu Items" })).toBeInTheDocument();
  });

  it("shows no Extensions section when no packs are installed", async () => {
    server.use(http.get("*/api/v1/packs", () => jsonBody({ items: [], cursor: null })));
    renderShell();
    // Let the (empty) packs list settle, then confirm no Extensions landmark.
    await waitFor(() => expect(screen.getByText("content")).toBeInTheDocument());
    expect(screen.queryByRole("navigation", { name: "Extensions" })).not.toBeInTheDocument();
  });
});

// ── The nav icon: a real glyph on every pack group and page — never broken ─────
//
// A lucide component renders `<svg class="lucide lucide-<name> …">`, so a valid
// icon is provable in the DOM by its `lucide-<name>` class; a broken/missing icon
// would render no such glyph. The group wears the manifest-declared icon when it
// names one the host allows, and the default extension glyph (Puzzle) otherwise —
// including when the declared name is unknown (degrade, never break).

/** The lucide `lucide-<name>` class on the single glyph inside `el`, or "" when
 * `el` carries no icon at all (a broken/missing icon — what this fix forbids). */
function iconName(el: Element | null | undefined): string {
  const svg = el?.querySelector("svg");
  const cls = svg?.getAttribute("class") ?? "";
  return cls.match(/lucide-[a-z0-9-]+/)?.[0] ?? "";
}

async function renderWithPack(over: Record<string, unknown> = {}) {
  server.use(
    http.get("*/api/v1/packs", () =>
      jsonBody({ items: [pack({ manifest: packManifest(over) })], cursor: null }),
    ),
    http.get("*/api/v1/packs/acme/menu-board/messages/en", () => jsonBody(PACK_EN_CATALOG)),
  );
  renderShell();
  return await screen.findByRole("navigation", { name: "Extensions" });
}

describe("Extensions nav — pack icons (never broken)", () => {
  it("renders the DEFAULT extension glyph on the group and a valid glyph on every page when no icon is declared", async () => {
    const ext = await renderWithPack();

    const heading = ext.querySelector('[data-slot="pack-group-heading"]');
    expect(heading).not.toBeNull();
    // A group without a declared icon wears the default extension glyph (Puzzle).
    expect(iconName(heading)).toBe("lucide-puzzle");

    // Every page link renders a real glyph too — no dead/missing icon anywhere.
    for (const name of ["Menu Items", "Settings"]) {
      const link = within(ext).getByRole("link", { name });
      expect(iconName(link)).toMatch(/^lucide-[a-z]/);
    }
  });

  it("renders the manifest-DECLARED icon on the group when it names a host-allowed glyph", async () => {
    const ext = await renderWithPack({ icon: "utensils" });
    const heading = ext.querySelector('[data-slot="pack-group-heading"]');
    expect(iconName(heading)).toBe("lucide-utensils");
  });

  it("falls back to the DEFAULT glyph when the declared icon name is unknown — degrades, never broken", async () => {
    const ext = await renderWithPack({ icon: "totally-not-a-real-glyph" });
    const heading = ext.querySelector('[data-slot="pack-group-heading"]');
    // Unknown name -> the default; the group still shows a real, non-broken glyph.
    expect(iconName(heading)).toBe("lucide-puzzle");
  });

  it("survives a NON-string icon value (untrusted manifest) — still a real glyph, never a crash", async () => {
    // Install validates the manifest as JSON; the console must not trust the icon's
    // runtime type. A non-string icon degrades to the default rather than breaking.
    const ext = await renderWithPack({ icon: { evil: 1 } as unknown as string });
    const heading = ext.querySelector('[data-slot="pack-group-heading"]');
    expect(iconName(heading)).toBe("lucide-puzzle");
  });

  it("navigates when a pack page link is CLICKED — the extension nav is live, not a dead icon strip", async () => {
    const user = userEvent.setup();
    const ext = await renderWithPack({ icon: "utensils" });

    const link = within(ext).getByRole("link", { name: "Menu Items" });
    expect(link).not.toHaveAttribute("aria-current");
    // Drive the real interaction: clicking the extension link navigates to the
    // pack page route and marks it active (a dead link would never become current).
    await user.click(link);
    expect(link).toHaveAttribute("aria-current", "page");
  });
});
