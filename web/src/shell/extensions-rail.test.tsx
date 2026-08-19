import { describe, it, expect, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { MemoryRouter } from "react-router";
import { ThemeProvider } from "@/components/theme/theme-provider";
import { AppShell } from "@/shell/app-shell";
import { TRACE_ID, pack, packManifest, PACK_EN_CATALOG } from "@/api/test-support";

// What the rail does about EXTENSIONS, with a pack actually installed.
//
// Two claims, and the first one is a deletion. The rail used to render a second
// landmark below a divider, resolved live from the installed packs: a titled
// section per pack, with a link per declared page. The owner killed it on
// 2026-08-19 — "the hell is this Discovery policy crap? That doesn't need to be
// there" — and the ruling is general: NO pack gets a nav area of its own, now or
// in future. So the tests that asserted that section existed are replaced by
// tests that assert it does not — with a pack installed, resolved, and declaring
// pages, which is the only arrangement in which its absence means anything.
//
// It is a rail change, not a capability removal: pack pages are still routed and
// still served, and Extensions > Installed links every one of them. THAT door is
// pinned where it lives — routes/extensions/extensions-route.test.tsx clicks the
// link and asserts the route it lands on.
//
// The second claim is what SURVIVES: the "extensions need attention" badge. It
// hangs off the core Extensions group in Primary and never belonged to the
// deleted section, so nothing needed re-homing. Its tests moved here with it,
// unchanged in substance, because the hook swallows a failed lookup by design and
// that same catch will happily swallow a missing mock — without a case that
// asserts the badge is PRESENT, the whole feature passes while asking nothing.

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

/** An installed pack declaring two pages, its catalog, and an update check that
 * reports one WAITING — everything the removed section needed in order to
 * render, plus a settle signal (see `settledSidebar`). */
function mountWithPack(over: Record<string, unknown> = {}) {
  server.use(
    http.get("*/api/v1/extensions", () =>
      jsonBody({ items: [pack({ manifest: packManifest(over) })], cursor: null }),
    ),
    http.get("*/api/v1/extensions/acme/menu-board/messages/en", () => jsonBody(PACK_EN_CATALOG)),
    http.get("*/api/v1/extensions/acme/menu-board/update", () =>
      jsonBody({
        action: "updated",
        id: "acme/menu-board",
        from_version: "1.0.0",
        to_version: "2.0.0",
        trust_channel: "community",
        source: "registry",
      }),
    ),
  );
  renderShell();
}

/**
 * The rail, once the shell has demonstrably SEEN the installed pack.
 *
 * An absence measured too early is worthless: "no pack section" is trivially
 * true of a shell that has not read the pack list yet and renders one a moment
 * later. The badge is the proof — it only appears once /extensions has been
 * listed AND the pack's update endpoint has answered, so a rendered badge means
 * this pack is fully resolved in this console. That is exactly the state in which
 * the removed section used to appear.
 */
async function settledSidebar(): Promise<HTMLElement> {
  await screen.findByText("content");
  await screen.findByRole("button", { name: /Extensions.*1 update waiting/s });
  return document.querySelector('[data-slot="shell-sidebar"]') as HTMLElement;
}

/** Every rail link that points into a pack's own `/p/{publisher}/{name}/`
 * namespace. The href is the test, not the label: a pack titles its own pages,
 * so "Settings" in the rail may legitimately be the core Platform destination. */
function packPageLinks(scope: HTMLElement): string[] {
  return within(scope)
    .getAllByRole("link")
    .map((a) => a.getAttribute("href") ?? "")
    .filter((href) => href.startsWith("/p/"));
}

describe("The rail carries NO pack-contributed section", () => {
  it("gives a resolved, page-declaring pack no section, no heading and no link", async () => {
    mountWithPack();
    const sidebar = await settledSidebar();

    // The landmark is gone outright — not renamed, not emptied.
    expect(screen.queryByRole("navigation", { name: "Extensions" })).not.toBeInTheDocument();
    // The pack's own display name titled that section; nothing in the rail says it.
    expect(within(sidebar).queryByText("Menu Board")).not.toBeInTheDocument();
    // Its pages are not rail destinations, by title…
    expect(within(sidebar).queryByRole("link", { name: "Menu Items" })).not.toBeInTheDocument();
    // …nor under any label at all: no rail link points into a pack's namespace.
    expect(packPageLinks(sidebar)).toEqual([]);
  });

  it("puts NOTHING below the Primary nav — one landmark, no divider, no second area", async () => {
    // Stated structurally rather than by name, so it holds for the next pack
    // section somebody is tempted to add as well as for the one just removed.
    mountWithPack({ icon: "utensils" });
    const sidebar = await settledSidebar();

    expect(within(sidebar).getAllByRole("navigation").map((n) => n.getAttribute("aria-label"))).toEqual([
      "Primary",
    ]);
    // The per-pack heading that section rendered is gone as a construct.
    expect(sidebar.querySelectorAll('[data-slot="pack-group-heading"]')).toHaveLength(0);
  });

  it("keeps the mobile DRAWER in step — a pack gets no section there either", async () => {
    // The drawer renders the same ShellNav. A section restored to one and not the
    // other would be a pack area on a phone and none on a desktop.
    const user = userEvent.setup();
    mountWithPack();
    await settledSidebar();

    await user.click(screen.getByRole("button", { name: /open navigation menu/i }));
    const drawer = await screen.findByRole("dialog");

    // The drawer really is showing the nav (or the absences below prove nothing).
    expect(within(drawer).getByRole("link", { name: "Overview" })).toBeInTheDocument();
    expect(within(drawer).getAllByRole("navigation").map((n) => n.getAttribute("aria-label"))).toEqual(
      ["Primary"],
    );
    expect(packPageLinks(drawer)).toEqual([]);
  });
});

// ---------------------------------------------------------------------------
// The rail's updates badge (MKT-095). The register's row: "an operator is never
// told a pack has a newer version; they must learn it out of band."
//
// It hangs off the CORE Extensions group — the one that discloses /extensions,
// the page that acts on what the badge counts — and it always did. The removal
// of the pack section left it exactly where it was, which is where it belongs:
// re-homing was considered and there was nothing to move.
// ---------------------------------------------------------------------------

describe("The rail's updates badge", () => {
  function availability(over: Record<string, unknown> = {}) {
    return {
      action: "unchanged",
      id: "acme/menu-board",
      from_version: "1.0.0",
      to_version: "1.0.0",
      trust_channel: "community",
      source: "registry",
      ...over,
    };
  }

  function mountWith(report: Record<string, unknown>) {
    server.use(
      http.get("*/api/v1/extensions", () => jsonBody({ items: [pack()], cursor: null })),
      http.get("*/api/v1/extensions/acme/menu-board/messages/en", () => jsonBody(PACK_EN_CATALOG)),
      http.get("*/api/v1/extensions/acme/menu-board/update", () => jsonBody(report)),
    );
    renderShell();
  }

  it("counts a waiting update on the Extensions heading, in words a screen reader can use", async () => {
    mountWith(availability({ action: "updated", to_version: "2.0.0" }));

    // Found through the CONTROL's accessible name, not by hunting a span: the
    // badge is only useful if it reaches the name of the thing it annotates.
    const toggle = await screen.findByRole("button", { name: /Extensions.*1 update waiting/s });
    expect(within(toggle).getByText("1")).toBeInTheDocument();
  });

  it("hangs the badge on the group that discloses the Extensions PAGE", async () => {
    // The badge points somewhere. Asserting WHICH group carries it is what makes
    // "it lives on Extensions > Installed" a checked claim rather than a note —
    // and it is the assertion that would have caught the badge riding the
    // pack-contributed section into deletion, had it ever been attached there.
    mountWith(availability({ action: "updated", to_version: "2.0.0" }));

    const sidebar = await settledSidebar();
    const badge = sidebar.querySelector('[data-slot="nav-group-badge"]');
    expect(badge).not.toBeNull();
    const toggle = badge?.closest('[data-slot="nav-group-toggle"]') as HTMLElement;
    expect(toggle.getAttribute("data-nav-group")).toBe("extensions");

    // …and the region that toggle controls holds the link to /extensions.
    const panel = sidebar.querySelector(`#${toggle.getAttribute("aria-controls")}`) as HTMLElement;
    expect(within(panel).getByRole("link", { name: "Installed" })).toHaveAttribute(
      "href",
      "/extensions",
    );
  });

  it("says 'need attention' when a running version has been withdrawn", async () => {
    mountWith(availability({ action: "reverted", from_version: "2.0.0", to_version: "1.0.0" }));

    // Summed into the same badge deliberately — the rail answers "is there
    // something in Extensions to look at", and the PAGE is where an upgrade is
    // told apart from a fault. But the WORDING has to change, or the badge
    // would call a withdrawn version an available update.
    await screen.findByRole("button", { name: /Extensions.*1 extension needs attention/s });
  });

  it("shows no badge when every tracked pack is current", async () => {
    mountWith(availability());

    const toggle = await screen.findByRole("button", { name: /Extensions/ });
    expect(toggle.querySelector('[data-slot="nav-group-badge"]')).toBeNull();
  });
});
