/// <reference types="node" />
import { existsSync, readFileSync } from "node:fs";
import { resolve } from "node:path";
import { describe, it, expect, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { MemoryRouter } from "react-router";
import { vi } from "vitest";
import { ThemeProvider } from "@/components/theme/theme-provider";
import { SessionGate } from "@/auth/session-gate";
import type { Role, SessionSummary, WaiveoApi } from "@/api";
import { AppShell } from "@/shell/app-shell";
import ExtensionsRoute from "./extensions-route";
import { TRACE_ID, ULID_A, ULID_B, PACK_ID, pack, packManifest, PACK_EN_CATALOG, problem } from "@/api/test-support";

// The Extensions console, driven by clicking it.
//
// Every capability is asserted through a real interaction against a mock feeder
// that answers with the SHAPES the Go handlers emit — including their refusals,
// which is the half this programme keeps getting wrong. A surface that accepts an
// uninstall it never performs, or offers an update the box can only refuse, would
// pass a rendering test and fail here.

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

function jsonBody(body: Parameters<typeof HttpResponse.json>[0]) {
  return HttpResponse.json(body, { headers: { "Trace-Id": TRACE_ID } });
}

/** One install record as the api/1 surface serves it (MKT-094/094a). */
function installRecord(over: Record<string, unknown> = {}) {
  return {
    id: ULID_A,
    pack_id: PACK_ID,
    resolved_version: "1.0.0",
    trust_channel: "community",
    source: "https://registry.example/index.json",
    stale_source: false,
    content_digest: "sha256:aa11bb22",
    key_id: "publisher-key-1",
    artifact_digest: "sha256:cc33dd44",
    installed_at: 1_753_000_000_000,
    ...over,
  };
}

/** A feeder whose installed-pack set is MUTABLE, so an install/uninstall is
 * observable in the next list exactly as it is on a real box. */
function mockFeeder(options: {
  packs?: Record<string, unknown>[];
  records?: Record<string, unknown>[];
  catalog?: Record<string, string> | null;
  /** What GET .../update reports (MKT-095). Defaults to "nothing waiting", so
   * every pre-existing case keeps describing a box that is current. */
  availability?: Record<string, unknown> | null;
  /** What GET /packs/catalog serves (MKT-096). Named `marketplace`, not
   * `catalog`: this fixture already had a `catalog` meaning the pack's LOCALE
   * catalog, and two options one letter apart in meaning is how a test ends up
   * asserting against the wrong mock. */
  marketplace?: Record<string, unknown>[] | null;
} = {}) {
  const state = {
    packs: options.packs ?? [],
    records: options.records ?? [installRecord()],
  };
  const catalog = options.catalog === undefined ? PACK_EN_CATALOG : options.catalog;
  server.use(
    http.get("*/api/v1/packs", () => jsonBody({ items: state.packs, cursor: null })),
    http.get("*/api/v1/packs/:publisher/:name/messages/:locale", () =>
      catalog === null ? problem(404, "NOT_FOUND", "No such locale.") : jsonBody(catalog),
    ),
    http.get("*/api/v1/packs/:publisher/:name/installs", () =>
      jsonBody({ items: state.records, cursor: null }),
    ),
    // The catalog (MKT-096). Its literal segment must not be read as a
    // publisher, which is the same collision the server's route ordering
    // guards against.
    http.get("*/api/v1/packs/catalog", () =>
      options.marketplace === null
        ? problem(503, "UNAVAILABLE", "No registry sources answered.")
        : jsonBody({ sources: options.marketplace ?? [] }),
    ),
    // The availability REPORT (MKT-095). Distinct from the POST on the same
    // path: if this were missing the console would silently render its
    // could-not-read branch for every pack, and every assertion below about
    // update state would pass against a page that had asked nothing.
    http.get("*/api/v1/packs/:publisher/:name/update", () =>
      options.availability === null
        ? problem(503, "UNAVAILABLE", "The registry source did not answer.")
        : jsonBody(
            options.availability ?? {
              action: "unchanged",
              id: PACK_ID,
              from_version: "1.0.0",
              to_version: "1.0.0",
              trust_channel: "community",
              source: "registry",
            },
          ),
    ),
  );
  return state;
}

function renderRoute() {
  return render(
    <ThemeProvider>
      <MemoryRouter initialEntries={["/extensions"]}>
        <ExtensionsRoute />
      </MemoryRouter>
    </ThemeProvider>,
  );
}

/** The card for one pack — every per-pack assertion is scoped to it, so a control
 * belonging to a DIFFERENT pack can never satisfy one. */
async function packCard(id: string): Promise<HTMLElement> {
  return await waitFor(() => {
    const el = document.querySelector(`[data-slot="pack-card"][data-pack-id="${id}"]`);
    if (!el) throw new Error(`no card for ${id} yet`);
    return el as HTMLElement;
  });
}

describe("Extensions console — seeing what is installed", () => {
  it("lists each installed pack with its id, version, pages and collections", async () => {
    mockFeeder({ packs: [pack()] });
    renderRoute();

    const card = await packCard(PACK_ID);
    expect(within(card).getByText("Menu Board")).toBeInTheDocument();
    expect(within(card).getByText(PACK_ID)).toBeInTheDocument();
    expect(within(card).getByText("v1.0.0")).toBeInTheDocument();
    // Its pages are real destinations, under the pack's own namespace.
    expect(within(card).getByRole("link", { name: /Menu Items/ })).toHaveAttribute(
      "href",
      `/p/${PACK_ID}/menu-items`,
    );
    expect(within(card).getByText(/menu_items/)).toBeInTheDocument();
    // Its provenance is named: which source, and which key vouched for the bytes.
    expect(within(card).getByText(/publisher-key-1/)).toBeInTheDocument();
  });

  it("names the build command in the empty state instead of an encouraging sentence", async () => {
    mockFeeder({ packs: [] });
    renderRoute();
    expect(await screen.findByText("No extensions installed")).toBeInTheDocument();
    expect(screen.getByText(/make example-pack/)).toBeInTheDocument();
  });

  it("reports the box being unreachable rather than showing an empty, healthy-looking list", async () => {
    server.use(http.get("*/api/v1/packs", () => problem(500, "INTERNAL", "An unexpected server error occurred.")));
    renderRoute();
    // Scoped to the INSTALLED region, which is what this test is about. The
    // page now has a second, independently-failing region (the catalog reads a
    // different endpoint and a registry can be down while the box is fine), so
    // an unscoped alert query asserts "the page has exactly one problem" —
    // which was never the claim.
    const installed = await screen.findByRole("region", { name: /installed/i });
    expect(await within(installed).findByRole("alert")).toHaveTextContent(/could not be read/i);
    expect(screen.queryByText("No extensions installed")).not.toBeInTheDocument();
  });

  it("shows the install history — every version applied and the key that vouched for it", async () => {
    mockFeeder({
      packs: [pack()],
      records: [
        installRecord({ id: ULID_A, resolved_version: "1.0.0" }),
        installRecord({ id: ULID_B, resolved_version: "1.1.0", key_id: "publisher-key-2" }),
      ],
    });
    renderRoute();
    const card = await packCard(PACK_ID);
    await userEvent.click(within(card).getByRole("button", { name: /Show the install history/ }));

    const items = within(card).getAllByRole("listitem");
    const history = items.filter((li) => li.textContent?.includes("publisher-key"));
    // Newest first, and the newest is badged as what is running.
    expect(history[0]).toHaveTextContent("1.1.0");
    expect(history[0]).toHaveTextContent(/Current/i);
    expect(history[1]).toHaveTextContent("1.0.0");
  });
});

describe("Extensions console — installing", () => {
  it("uploads a chosen pack file to the install endpoint and says it is live", async () => {
    const state = mockFeeder({ packs: [] });
    let contentType: string | null = null;
    let bytes = 0;
    server.use(
      http.post("*/api/v1/packs", async ({ request }) => {
        contentType = request.headers.get("Content-Type");
        bytes = (await request.arrayBuffer()).byteLength;
        state.packs = [pack()];
        return HttpResponse.json(
          { id: PACK_ID, version: "1.0.0", pages: ["menu-items", "settings"], collections: ["menu_items"], locales: ["en"] },
          { status: 201, headers: { "Trace-Id": TRACE_ID } },
        );
      }),
    );
    renderRoute();
    await screen.findByText("No extensions installed");

    const file = new File([new Uint8Array([0x50, 0x4b, 0x03, 0x04])], "menu-board.pack.zip", {
      type: "application/zip",
    });
    await userEvent.upload(screen.getByLabelText(/Pack artifact/), file);
    await userEvent.click(screen.getByRole("button", { name: /Install the chosen pack file/ }));

    // The bytes actually left, as an artifact upload.
    await waitFor(() => expect(bytes).toBe(4));
    expect(contentType).toBe("application/zip");
    // The outcome says what happened AND that nothing restarted — the whole claim.
    expect(await screen.findByText(/Installed acme\/menu-board 1\.0\.0/)).toHaveTextContent(
      /not restarted/i,
    );
    // And the list re-read: the pack is on the box now.
    await packCard(PACK_ID);
  });

  it("will not send a marketplace reference until a trust channel is CHOSEN", async () => {
    mockFeeder({ packs: [] });
    renderRoute();
    await screen.findByText("No extensions installed");

    const install = screen.getByRole("button", { name: /Resolve and install this marketplace reference/ });
    expect(install).toBeDisabled();

    // A pack id alone is not enough: the channel is required and is never defaulted
    // — choosing one for the operator would be choosing how much review the pack
    // has had (MKT-060a(b)).
    await userEvent.type(screen.getByLabelText(/Pack id/), PACK_ID);
    expect(install).toBeDisabled();

    const channel = screen.getByLabelText(/Trust channel/) as HTMLSelectElement;
    expect(channel.value).toBe("");
    await userEvent.selectOptions(channel, "community");
    expect(install).toBeEnabled();
  });

  it("resolves a reference with only the members the operator supplied", async () => {
    const state = mockFeeder({ packs: [] });
    let body: Record<string, unknown> | null = null;
    server.use(
      http.post("*/api/v1/packs", async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        state.packs = [pack()];
        return HttpResponse.json(
          { id: PACK_ID, version: "1.0.0", pages: [], collections: [], locales: ["en"] },
          { status: 201, headers: { "Trace-Id": TRACE_ID } },
        );
      }),
    );
    renderRoute();
    await screen.findByText("No extensions installed");

    await userEvent.type(screen.getByLabelText(/Pack id/), PACK_ID);
    await userEvent.selectOptions(screen.getByLabelText(/Trust channel/), "verified");
    await userEvent.type(screen.getByLabelText(/Version/), "1.2.0");
    await userEvent.click(screen.getByRole("button", { name: /Resolve and install this marketplace reference/ }));

    await waitFor(() => expect(body).not.toBeNull());
    // Source was left blank, so it is absent — not an empty string the server's
    // strict decoder would have to interpret.
    expect(body).toEqual({ pack_id: PACK_ID, trust_channel: "verified", version: "1.2.0" });
  });

  it("renders a refused install in full — its sentence, EVERY field violation, its codes and the trace", async () => {
    mockFeeder({ packs: [] });
    server.use(
      http.post("*/api/v1/packs", () =>
        problem(422, "VALIDATION_FAILED", "The pack manifest failed validation.", {
          errors: [
            { field: "capabilities[0]", code: "UNKNOWN_CAPABILITY", message: "capability \"net.raw\" is not a manifest/1 capability" },
            { field: "resources.memory", code: "RESOURCE_BELOW_FLOOR", message: "memory 4 is below the floor" },
          ],
        }),
      ),
    );
    renderRoute();
    await screen.findByText("No extensions installed");

    await userEvent.upload(
      screen.getByLabelText(/Pack artifact/),
      new File([new Uint8Array([1])], "bad.pack.zip", { type: "application/zip" }),
    );
    await userEvent.click(screen.getByRole("button", { name: /Install the chosen pack file/ }));

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("The pack manifest failed validation.");
    // BOTH violations, not just the first — a manifest refusal names everything at once.
    expect(alert).toHaveTextContent("capabilities[0]: UNKNOWN_CAPABILITY");
    expect(alert).toHaveTextContent("resources.memory: RESOURCE_BELOW_FLOOR");
    expect(alert).toHaveTextContent(`trace ${TRACE_ID}`);
    // And nothing pretends it worked.
    expect(screen.getByText("No extensions installed")).toBeInTheDocument();
  });
});

describe("Extensions console — updating", () => {
  it("checks the pinned channel and reports that nothing changed, without claiming an update", async () => {
    mockFeeder({ packs: [pack()] });
    server.use(
      http.post("*/api/v1/packs/:publisher/:name/update", () =>
        jsonBody({ action: "unchanged", id: PACK_ID, from_version: "1.0.0", to_version: "1.0.0" }),
      ),
    );
    renderRoute();
    const card = await packCard(PACK_ID);
    await userEvent.click(within(card).getByRole("button", { name: /Check .* for updates/ }));
    expect(await within(card).findByText(/Already current at 1\.0\.0/)).toBeInTheDocument();
  });

  it("applies an update and re-reads, so the version on screen is the one installed", async () => {
    const state = mockFeeder({ packs: [pack()] });
    server.use(
      http.post("*/api/v1/packs/:publisher/:name/update", () => {
        state.packs = [pack({ version: "1.1.0", revision: 2 })];
        return jsonBody({ action: "updated", id: PACK_ID, from_version: "1.0.0", to_version: "1.1.0", pages: ["menu-items"] });
      }),
    );
    renderRoute();
    const card = await packCard(PACK_ID);
    await userEvent.click(within(card).getByRole("button", { name: /Check .* for updates/ }));

    expect(await within(card).findByText(/Updated 1\.0\.0 → 1\.1\.0/)).toBeInTheDocument();
    await waitFor(() => expect(within(card).getByText("v1.1.0")).toBeInTheDocument());
  });

  it("explains a REVERT as a publisher withdrawal, not as a failed update", async () => {
    const state = mockFeeder({ packs: [pack({ version: "1.2.0" })] });
    server.use(
      http.post("*/api/v1/packs/:publisher/:name/update", () => {
        state.packs = [pack({ version: "1.1.0", revision: 3 })];
        return jsonBody({ action: "reverted", id: PACK_ID, from_version: "1.2.0", to_version: "1.1.0" });
      }),
    );
    renderRoute();
    const card = await packCard(PACK_ID);
    await userEvent.click(within(card).getByRole("button", { name: /Check .* for updates/ }));
    expect(await within(card).findByText(/withdrawn by its publisher/)).toBeInTheDocument();
  });

  it("does NOT offer an update check for a directly-installed pack, and says why", async () => {
    // MKT-094a: no trust channel pinned means the box has nothing to re-resolve
    // against and refuses to guess. An enabled button here would be a control whose
    // only possible outcome is a refusal.
    mockFeeder({ packs: [pack()], records: [installRecord({ trust_channel: null, artifact_digest: null, source: "direct" })] });
    renderRoute();
    const card = await packCard(PACK_ID);
    expect(within(card).getByRole("button", { name: /Check .* for updates/ })).toBeDisabled();
    expect(within(card).getByText(/no trust channel pinned/i)).toBeInTheDocument();
    expect(within(card).getByText(/Direct install/i)).toBeInTheDocument();
  });

  it("surfaces a refused update check verbatim instead of a generic failure", async () => {
    mockFeeder({ packs: [pack()] });
    server.use(
      http.post("*/api/v1/packs/:publisher/:name/update", () =>
        problem(422, "VALIDATION_FAILED", "the channel pointer named a lower version than this box has already resolved", {
          errors: [{ field: "artifact", code: "POINTER_ROLLBACK_REJECTED", message: "1.0.0 < 1.1.0" }],
        }),
      ),
    );
    renderRoute();
    const card = await packCard(PACK_ID);
    await userEvent.click(within(card).getByRole("button", { name: /Check .* for updates/ }));
    const alert = await within(card).findByRole("alert");
    expect(alert).toHaveTextContent("POINTER_ROLLBACK_REJECTED");
    expect(alert).toHaveTextContent(/lower version/);
  });
});

describe("Extensions console — removing", () => {
  it("confirms, sends a FRESH If-Match, and the pack is gone from the list", async () => {
    // The list was read at revision 7; by the time the operator confirms, the pack
    // is at 9 (an update landed elsewhere). The delete must be conditioned on the
    // revision read AT REMOVAL — a stale precondition from page load would 412
    // against a box that is perfectly willing to remove it.
    const state = mockFeeder({ packs: [pack({ revision: 7 })] });
    let ifMatch: string | null = null;
    server.use(
      http.get("*/api/v1/packs/:publisher/:name", () =>
        HttpResponse.json(pack({ revision: 9 }), { headers: { "Trace-Id": TRACE_ID, ETag: '"9"' } }),
      ),
      http.delete("*/api/v1/packs/:publisher/:name", ({ request }) => {
        ifMatch = request.headers.get("If-Match");
        state.packs = [];
        return new HttpResponse(null, { status: 204, headers: { "Trace-Id": TRACE_ID } });
      }),
    );
    renderRoute();
    const card = await packCard(PACK_ID);
    await userEvent.click(within(card).getByRole("button", { name: `Uninstall ${PACK_ID}` }));
    // The confirm is the console's one dialog idiom, so it portals out of the card.
    expect(await screen.findByRole("dialog")).toHaveTextContent(`Uninstall ${PACK_ID}?`);
    await userEvent.click(screen.getByRole("button", { name: "Uninstall permanently" }));

    // The precondition is the revision read at the moment of removal, not one
    // captured when the page loaded.
    await waitFor(() => expect(ifMatch).toBe('"9"'));
    expect(await screen.findByText("No extensions installed")).toBeInTheDocument();
    // And the operator is told what happened, even though the card it happened to
    // no longer exists.
    expect(
      screen.getByText(/removed — its pages, its rows and its install records went with it/),
    ).toBeInTheDocument();
  });

  it("does not remove anything until the removal is CONFIRMED", async () => {
    mockFeeder({ packs: [pack()] });
    let deletes = 0;
    server.use(
      http.get("*/api/v1/packs/:publisher/:name", () =>
        HttpResponse.json(pack(), { headers: { "Trace-Id": TRACE_ID, ETag: '"1"' } }),
      ),
      http.delete("*/api/v1/packs/:publisher/:name", () => {
        deletes += 1;
        return new HttpResponse(null, { status: 204, headers: { "Trace-Id": TRACE_ID } });
      }),
    );
    renderRoute();
    const card = await packCard(PACK_ID);
    await userEvent.click(within(card).getByRole("button", { name: `Uninstall ${PACK_ID}` }));
    // The confirmation names what goes with the pack, and cancelling sends nothing.
    expect(await screen.findByRole("dialog")).toHaveTextContent(/install history are removed together/i);
    await userEvent.click(screen.getByRole("button", { name: "Keep it" }));
    expect(deletes).toBe(0);
    await packCard(PACK_ID);
  });

  it("REFUSES a required pack loudly — the box's own sentence, its code, and no second invitation", async () => {
    // The required-pack roster is deployment configuration and no api/1 route
    // publishes it, so required status can only be LEARNED from this refusal. The
    // legacy console solved that by hiding the button with no explanation; an
    // operator who cannot remove a pack and is told nothing is the same defect
    // seen from the other side.
    mockFeeder({ packs: [pack()] });
    server.use(
      http.get("*/api/v1/packs/:publisher/:name", () =>
        HttpResponse.json(pack(), { headers: { "Trace-Id": TRACE_ID, ETag: '"1"' } }),
      ),
      http.delete("*/api/v1/packs/:publisher/:name", () =>
        problem(
          422,
          "VALIDATION_FAILED",
          "acme/menu-board is a required pack on this deployment (floor version 1.0.0) and cannot be uninstalled.",
          { errors: [{ field: "pack", code: "REQUIRED_PACK_FLOOR", message: "floor 1.0.0" }] },
        ),
      ),
    );
    renderRoute();
    const card = await packCard(PACK_ID);
    await userEvent.click(within(card).getByRole("button", { name: `Uninstall ${PACK_ID}` }));
    await userEvent.click(await screen.findByRole("button", { name: "Uninstall permanently" }));

    const alert = await within(card).findByRole("alert");
    expect(alert).toHaveTextContent("required pack on this deployment (floor version 1.0.0)");
    expect(alert).toHaveTextContent("REQUIRED_PACK_FLOOR");
    // The pack is still there, is now marked Required, and the control stops
    // inviting a retry it already knows the answer to.
    expect(within(card).getByText("Required")).toBeInTheDocument();
    expect(within(card).getByRole("button", { name: `Uninstall ${PACK_ID}` })).toBeDisabled();
  });

  it("surfaces a stale-precondition conflict rather than retrying the delete", async () => {
    mockFeeder({ packs: [pack({ revision: 2 })] });
    let deletes = 0;
    server.use(
      http.get("*/api/v1/packs/:publisher/:name", () =>
        HttpResponse.json(pack({ revision: 2 }), { headers: { "Trace-Id": TRACE_ID, ETag: '"2"' } }),
      ),
      http.delete("*/api/v1/packs/:publisher/:name", () => {
        deletes += 1;
        return problem(412, "REVISION_CONFLICT", "The pack was modified concurrently.", {
          current_revision: 3,
        });
      }),
    );
    renderRoute();
    const card = await packCard(PACK_ID);
    await userEvent.click(within(card).getByRole("button", { name: `Uninstall ${PACK_ID}` }));
    await userEvent.click(await screen.findByRole("button", { name: "Uninstall permanently" }));

    expect(await within(card).findByRole("alert")).toHaveTextContent("modified concurrently");
    expect(deletes).toBe(1);
    await packCard(PACK_ID);
  });
});

describe("Extensions console — honest health", () => {
  it("NAMES a declared page whose path escapes the pack's namespace, which the nav drops silently", async () => {
    mockFeeder({
      packs: [
        pack({
          manifest: packManifest({
            ui: {
              pages: [
                { path: "../../design", pageType: "list-detail", titleMsg: "msg:page.menuItems.title" },
                { path: "settings", pageType: "settings-form", titleMsg: "msg:page.settings.title" },
              ],
            },
          }),
        }),
      ],
    });
    renderRoute();
    const card = await packCard(PACK_ID);
    expect(within(card).getByText(/1 declared page is unreachable/)).toBeInTheDocument();
    expect(within(card).getByText("../../design")).toBeInTheDocument();
    // The reachable one is still a working destination.
    expect(within(card).getByRole("link", { name: /Settings/ })).toHaveAttribute(
      "href",
      `/p/${PACK_ID}/settings`,
    );
  });

  it("says when a pack's own message catalog did not resolve its display strings", async () => {
    mockFeeder({ packs: [pack()], catalog: null });
    renderRoute();
    const card = await packCard(PACK_ID);
    expect(within(card).getByText(/display strings did not resolve/i)).toBeInTheDocument();
    // The pack is still listed and still usable — a missing catalog degrades, it
    // does not hide the pack.
    expect(within(card).getByText(PACK_ID)).toBeInTheDocument();
  });

  it("says when the install history could not be read, instead of implying a direct install", async () => {
    server.use(
      http.get("*/api/v1/packs", () => jsonBody({ items: [pack()], cursor: null })),
      http.get("*/api/v1/packs/:publisher/:name/messages/:locale", () => jsonBody(PACK_EN_CATALOG)),
      http.get("*/api/v1/packs/:publisher/:name/installs", () =>
        problem(500, "INTERNAL", "An unexpected server error occurred."),
      ),
    );
    renderRoute();
    const card = await packCard(PACK_ID);
    expect(within(card).getByText(/An unexpected server error occurred/)).toBeInTheDocument();
    expect(within(card).getByRole("button", { name: /Check .* for updates/ })).toBeDisabled();
  });
});

describe("Extensions console — live, in this console", () => {
  it("an installed pack's pages appear in the shell nav with NO reload", async () => {
    // "Installs live" has to be true of the console the operator is looking at,
    // not only of the box. Without the change signal the rail would keep showing
    // the pre-install world until the page was reloaded — a restart, from where
    // the operator sits.
    const state = mockFeeder({ packs: [] });
    server.use(
      http.post("*/api/v1/packs", () => {
        state.packs = [pack()];
        return HttpResponse.json(
          { id: PACK_ID, version: "1.0.0", pages: ["menu-items", "settings"], collections: ["menu_items"], locales: ["en"] },
          { status: 201, headers: { "Trace-Id": TRACE_ID } },
        );
      }),
    );

    render(
      <ThemeProvider>
        <MemoryRouter initialEntries={["/extensions"]}>
          <AppShell>
            <ExtensionsRoute />
          </AppShell>
        </MemoryRouter>
      </ThemeProvider>,
    );

    await screen.findByText("No extensions installed");
    // Nothing installed: there is no Extensions landmark at all.
    expect(screen.queryByRole("navigation", { name: "Extensions" })).not.toBeInTheDocument();

    await userEvent.upload(
      screen.getByLabelText(/Pack artifact/),
      new File([new Uint8Array([0x50, 0x4b])], "menu-board.pack.zip", { type: "application/zip" }),
    );
    await userEvent.click(screen.getByRole("button", { name: /Install the chosen pack file/ }));

    const ext = await screen.findByRole("navigation", { name: "Extensions" });
    expect(within(ext).getByRole("link", { name: "Menu Items" })).toHaveAttribute(
      "href",
      `/p/${PACK_ID}/menu-items`,
    );
  });

  it("an uninstalled pack's pages LEAVE the shell nav with no reload either", async () => {
    const state = mockFeeder({ packs: [pack()] });
    server.use(
      http.get("*/api/v1/packs/:publisher/:name", () =>
        HttpResponse.json(pack(), { headers: { "Trace-Id": TRACE_ID, ETag: '"1"' } }),
      ),
      http.delete("*/api/v1/packs/:publisher/:name", () => {
        state.packs = [];
        return new HttpResponse(null, { status: 204, headers: { "Trace-Id": TRACE_ID } });
      }),
    );

    render(
      <ThemeProvider>
        <MemoryRouter initialEntries={["/extensions"]}>
          <AppShell>
            <ExtensionsRoute />
          </AppShell>
        </MemoryRouter>
      </ThemeProvider>,
    );

    await screen.findByRole("navigation", { name: "Extensions" });
    const card = await packCard(PACK_ID);
    await userEvent.click(within(card).getByRole("button", { name: `Uninstall ${PACK_ID}` }));
    await userEvent.click(await screen.findByRole("button", { name: "Uninstall permanently" }));

    await waitFor(() =>
      expect(screen.queryByRole("navigation", { name: "Extensions" })).not.toBeInTheDocument(),
    );
  });
});

// ── The living proof: the REAL in-repo example pack ─────────────────────────
//
// examples/packs/menu-board is what `make example-pack` signs into the zip an
// operator installs on real hardware, and what `make dev` installs over this same
// endpoint. Reading it off disk means this console is asserted against the pack
// the platform actually ships — if the shipped manifest drifts out of what this
// page can present, this fails.

function repoRoot(): string {
  const cwd = process.cwd();
  for (const candidate of [resolve(cwd, ".."), cwd]) {
    if (existsSync(resolve(candidate, "examples/packs/menu-board/manifest.json"))) return candidate;
  }
  throw new Error(`cannot locate the example pack from cwd ${cwd}`);
}

const realManifest = JSON.parse(
  readFileSync(resolve(repoRoot(), "examples/packs/menu-board/manifest.json"), "utf8"),
) as Record<string, unknown>;
const realEnCatalog = JSON.parse(
  readFileSync(resolve(repoRoot(), "examples/packs/menu-board/messages/en.json"), "utf8"),
) as Record<string, string>;

describe("Extensions console — the real example pack, installed end to end", () => {
  it("installs examples/packs/menu-board and presents the REAL manifest's pages, collections and provenance", async () => {
    const REAL_ID = realManifest.id as string;
    const state: { packs: Record<string, unknown>[] } = { packs: [] };
    server.use(
      http.get("*/api/v1/packs", () => jsonBody({ items: state.packs, cursor: null })),
      http.get("*/api/v1/packs/waiveo/menu-board/messages/:locale", () => jsonBody(realEnCatalog)),
      http.get("*/api/v1/packs/waiveo/menu-board/installs", () =>
        jsonBody({
          items: [installRecord({ pack_id: REAL_ID, trust_channel: "first-party", source: "official" })],
          cursor: null,
        }),
      ),
      http.post("*/api/v1/packs", () => {
        state.packs = [
          {
            id: REAL_ID,
            revision: 1,
            version: realManifest.version,
            data_model_version: 1,
            created_at: 1_753_000_000_000,
            updated_at: 1_753_000_000_000,
            manifest: realManifest,
          },
        ];
        return HttpResponse.json(
          {
            id: REAL_ID,
            version: realManifest.version,
            pages: ["menu-items", "settings"],
            collections: ["menu_items"],
            locales: ["en"],
          },
          { status: 201, headers: { "Trace-Id": TRACE_ID } },
        );
      }),
    );

    renderRoute();
    await screen.findByText("No extensions installed");
    await userEvent.upload(
      screen.getByLabelText(/Pack artifact/),
      new File([new Uint8Array([0x50, 0x4b, 0x03, 0x04])], "menu-board.pack.zip", {
        type: "application/zip",
      }),
    );
    await userEvent.click(screen.getByRole("button", { name: /Install the chosen pack file/ }));

    const card = await packCard(REAL_ID);
    // The title comes from the pack's OWN catalog, resolved through the same
    // MAN-110/111 path the nav uses — never a hand-written label.
    expect(within(card).getByText(realEnCatalog["pack.displayName"])).toBeInTheDocument();
    expect(within(card).getByText(`v${realManifest.version as string}`)).toBeInTheDocument();
    // Both real pages are reachable destinations, and the real collection is named.
    expect(within(card).getByRole("link", { name: new RegExp(realEnCatalog["page.menuItems.title"]) })).toHaveAttribute(
      "href",
      `/p/${REAL_ID}/menu-items`,
    );
    expect(within(card).getByRole("link", { name: new RegExp(realEnCatalog["page.settings.title"]) })).toHaveAttribute(
      "href",
      `/p/${REAL_ID}/settings`,
    );
    expect(within(card).getByText(/menu_items/)).toBeInTheDocument();
    // Nothing about the real pack is reported as broken.
    expect(within(card).queryByText(/unreachable/i)).not.toBeInTheDocument();
    expect(within(card).queryByText(/did not resolve/i)).not.toBeInTheDocument();
  });
});

// ---------------------------------------------------------------------------
// MKT-095 in the console: telling an operator an update is waiting, BEFORE they
// press anything. Until the report endpoint existed the page could offer only a
// button whose outcome was unknown until pressed.
// ---------------------------------------------------------------------------

it("says an update is waiting, counts it, and offers to take it", async () => {
  mockFeeder({
    packs: [pack()],
    availability: {
      action: "updated",
      id: PACK_ID,
      from_version: "1.0.0",
      to_version: "2.0.0",
      trust_channel: "community",
      source: "registry",
    },
  });
  renderRoute();

  const card = await packCard(PACK_ID);
  expect(within(card).getByText(/Update available — 2\.0\.0/)).toBeInTheDocument();
  // The report must not read as though it applied anything.
  expect(within(card).getByText(/Nothing has been applied/)).toBeInTheDocument();
  // The action offered changes from "look" to "take", because now the page
  // knows the answer looking would give.
  expect(within(card).getByRole("button", { name: /Update .* to 2\.0\.0/ })).toBeInTheDocument();

  // Asserted through the tile's own HINT rather than by walking up to the
  // value node: the hint states the meaning ("a newer version is published;
  // nothing applied"), so this survives a layout change and fails on a
  // behaviour change, which is the right way round.
  expect(await screen.findByText(/a newer version is published/)).toBeInTheDocument();
  expect(screen.queryByText("Withdrawn")).not.toBeInTheDocument();
});

// The one that matters most, and the reason `waiting` and `tone` are separate
// fields: a withdrawn running version is a FAULT, not an upgrade. Counting it
// under "Updates waiting" would file the most urgent thing this endpoint can
// report under the least urgent word on the page.
it("does not count a withdrawn running version as an available update", async () => {
  mockFeeder({
    packs: [pack()],
    availability: {
      action: "reverted",
      id: PACK_ID,
      from_version: "2.0.0",
      to_version: "1.0.0",
      trust_channel: "community",
      source: "registry",
    },
  });
  renderRoute();

  const card = await packCard(PACK_ID);
  expect(within(card).getByText(/Running version withdrawn/)).toBeInTheDocument();
  // And the button says what pressing it would DO — a rollback, not an upgrade.
  expect(within(card).getByText("Roll back now")).toBeInTheDocument();
  expect(within(card).queryByText("Check for updates")).not.toBeInTheDocument();

  // Nothing is "waiting" — the tally says every tracked pack is current, which
  // is true: there is no newer version, there is a broken one.
  expect(await screen.findByText(/every tracked pack is current/)).toBeInTheDocument();
  // And the fault gets its own tile, which only renders when there is one.
  expect(await screen.findByText("Withdrawn")).toBeInTheDocument();
});

// A source that cannot be reached is not evidence that nothing is waiting, so
// the page must not render its "up to date" branch from a failed lookup.
it("reports a failed availability read rather than claiming the pack is current", async () => {
  mockFeeder({ packs: [pack()], availability: null });
  renderRoute();

  const card = await packCard(PACK_ID);
  expect(within(card).getByText(/Could not read the channel/)).toBeInTheDocument();
  expect(within(card).queryByText("Up to date")).not.toBeInTheDocument();
});

// ---------------------------------------------------------------------------
// MKT-096 in the console: an operator can SEE what the sources offer and pick
// from it, instead of having to already know a publisher/name/channel to type.
// ---------------------------------------------------------------------------

const CATALOG_ENTRY = {
  id: "acme/menu-board",
  version: "2.0.0",
  kind: "pack",
  source: "example-registry",
  trust_channel: "community",
  status: "active",
};

it("lists what a source offers, attributed to that source", async () => {
  mockFeeder({
    packs: [],
    marketplace: [{ source: "example-registry", trust_channel: "community", entries: [CATALOG_ENTRY] }],
  });
  renderRoute();

  const region = await screen.findByRole("region", { name: /browse the catalog/i });
  expect(within(region).getByText("acme/menu-board")).toBeInTheDocument();
  // Attribution is the point, not decoration: source order is a resolution
  // preference and never a trust decision, so an operator has to see which
  // source served what.
  expect(within(region).getByRole("heading", { name: "example-registry" })).toBeInTheDocument();
  expect(
    within(region).getByRole("button", { name: /Install acme\/menu-board 2\.0\.0 from example-registry/ }),
  ).toBeInTheDocument();
});

// The distinction the whole section rests on: an unreachable registry is not an
// empty catalog, and rendering the first as the second tells an operator
// nothing is on offer when the truth is that nobody asked successfully.
it("reports an unreadable source instead of showing it as offering nothing", async () => {
  mockFeeder({
    packs: [],
    marketplace: [
      {
        source: "example-registry",
        trust_channel: "community",
        entries: [],
        unavailable: "the source did not answer",
      },
    ],
  });
  renderRoute();

  const region = await screen.findByRole("region", { name: /browse the catalog/i });
  expect(within(region).getByRole("alert")).toHaveTextContent(/could not be read/i);
  expect(within(region).queryByText(/answered and offers nothing/)).not.toBeInTheDocument();
});

// An index is untrusted transport, so its channel is a bare string that may name
// anything. The console checks it against the channels it implements rather than
// sending a value it can already see is wrong.
it("refuses to install an entry naming a trust channel it does not implement", async () => {
  mockFeeder({
    packs: [],
    marketplace: [
      {
        source: "example-registry",
        trust_channel: "community",
        entries: [{ ...CATALOG_ENTRY, trust_channel: "totally-made-up" }],
      },
    ],
  });
  renderRoute();

  const region = await screen.findByRole("region", { name: /browse the catalog/i });
  await userEvent.click(within(region).getByRole("button", { name: /Install acme\/menu-board/ }));
  expect(await within(region).findByText(/does not implement/)).toBeInTheDocument();
});

// Installing from a listing goes through the ordinary resolve-and-verify path —
// which is exactly why picking from an untrusted catalog is safe.
it("installs the exact reference a catalog row names", async () => {
  let installed: Record<string, unknown> | null = null;
  mockFeeder({
    packs: [],
    marketplace: [{ source: "example-registry", trust_channel: "community", entries: [CATALOG_ENTRY] }],
  });
  server.use(
    http.post("*/api/v1/packs", async ({ request }) => {
      installed = (await request.json()) as Record<string, unknown>;
      return jsonBody({ id: "acme/menu-board", version: "2.0.0", pages: [], collections: [], locales: [] });
    }),
  );
  renderRoute();

  const region = await screen.findByRole("region", { name: /browse the catalog/i });
  await userEvent.click(within(region).getByRole("button", { name: /Install acme\/menu-board 2\.0\.0/ }));

  await waitFor(() => expect(installed).not.toBeNull());
  // The EXACT entry, not a channel-following resolve: the operator picked a
  // version off a list and that is the version they get.
  expect(installed).toMatchObject({
    pack_id: "acme/menu-board",
    trust_channel: "community",
    source: "example-registry",
    version: "2.0.0",
  });
});

// ---------------------------------------------------------------------------
// MKT-097 in the console: the reversible alternative to uninstalling.
// ---------------------------------------------------------------------------

it("turns a pack off through the real control, and says the data survived", async () => {
  let sent: Record<string, unknown> | null = null;
  mockFeeder({ packs: [pack()] });
  server.use(
    http.put("*/api/v1/packs/acme/menu-board/enabled", async ({ request }) => {
      sent = (await request.json()) as Record<string, unknown>;
      return jsonBody({ id: "acme/menu-board", enabled: false });
    }),
  );
  renderRoute();

  const card = await packCard(PACK_ID);
  await userEvent.click(within(card).getByRole("button", { name: /Disable acme\/menu-board, keeping its data/ }));

  await waitFor(() => expect(sent).toEqual({ enabled: false }));
  // The outcome has to say what SURVIVED. "Disabled" alone does not tell an
  // operator whether their rows are gone, and that is the one thing they need
  // before using this instead of uninstalling.
  expect(await within(card).findByText(/data, install history and the pack itself are untouched/i)).toBeInTheDocument();
});

it("shows a disabled pack as off, and offers to enable it", async () => {
  mockFeeder({ packs: [pack({ enabled: false })] });
  renderRoute();

  const card = await packCard(PACK_ID);
  expect(within(card).getByText("Disabled")).toBeInTheDocument();
  // Stated in full on the card, not left to a badge.
  expect(within(card).getByText(/This extension is turned off/)).toBeInTheDocument();
  expect(within(card).getByText(/enabling it puts everything back/i)).toBeInTheDocument();
  expect(within(card).getByRole("button", { name: /Enable acme\/menu-board/ })).toBeInTheDocument();
  expect(within(card).queryByRole("button", { name: /Disable/ })).not.toBeInTheDocument();
});

// The refusal a required pack gets, surfaced rather than swallowed: a control
// that fails silently is worse than one that is absent.
it("reports the refusal when a required pack cannot be disabled", async () => {
  mockFeeder({ packs: [pack()] });
  server.use(
    http.put("*/api/v1/packs/acme/menu-board/enabled", () =>
      problem(422, "REQUIRED_PACK_FLOOR", "acme/menu-board is a required pack on this deployment (floor 1.0.0)."),
    ),
  );
  renderRoute();

  const card = await packCard(PACK_ID);
  await userEvent.click(within(card).getByRole("button", { name: /Disable acme\/menu-board/ }));
  expect(await within(card).findByText(/required pack on this deployment/i)).toBeInTheDocument();
});

// Rolling back to a version this box actually ran (MKT-060a(c)).
//
// MKT-050 refuses a channel pointer that moves backward, and its only exception
// fires when the running version has been YANKED — which never happens to a
// version that is merely broken for this deployment. An explicit version pin is
// deliberately exempt, so this is the operator's way back, and until now it
// existed only for someone willing to hand-type a marketplace reference.
it("offers a way back to an earlier version, using that install's own channel and source", async () => {
  let sent: Record<string, unknown> | null = null;
  mockFeeder({
    packs: [pack({ version: "2.0.0" })],
    records: [
      installRecord({ id: ULID_A, resolved_version: "1.0.0" }),
      installRecord({ id: ULID_B, resolved_version: "2.0.0" }),
    ],
  });
  server.use(
    http.post("*/api/v1/packs", async ({ request }) => {
      sent = (await request.json()) as Record<string, unknown>;
      return jsonBody({ id: PACK_ID, version: "1.0.0", pages: [], collections: [], locales: [] });
    }),
  );
  renderRoute();

  const card = await packCard(PACK_ID);
  await userEvent.click(within(card).getByRole("button", { name: /install history/i }));

  // Offered on the EARLIER version, never on the one already running.
  expect(within(card).getByRole("button", { name: /Go back to version 1\.0\.0/ })).toBeInTheDocument();
  expect(within(card).queryByRole("button", { name: /Go back to version 2\.0\.0/ })).not.toBeInTheDocument();

  await userEvent.click(within(card).getByRole("button", { name: /Go back to version 1\.0\.0/ }));
  await waitFor(() => expect(sent).not.toBeNull());
  // The EXACT version, pinned — not a channel-following resolve, which MKT-050
  // would refuse for going backward.
  expect(sent).toMatchObject({ pack_id: PACK_ID, version: "1.0.0" });
});

// The role sweep (SEC-010). Every pack LIFECYCLE route is gated server-side on
// admin at the workspace root; the console mirrors that threshold so an
// operator is not offered work the box will refuse.
describe("what a non-admin is offered", () => {
  function renderAs(role: Role) {
    const session: SessionSummary = {
      principal_id: "01J8Z3K4N5P6Q7R8S9T0V1W2P1",
      kind: "user",
      role,
      aal: "standard",
      session_id: "01J8Z3K4N5P6Q7R8S9T0V1W2S1",
    };
    const api = {
      auth: { login: vi.fn(), logout: vi.fn(), session: vi.fn(async () => session), claim: vi.fn() },
    } as unknown as WaiveoApi;
    return render(
      <ThemeProvider>
        <MemoryRouter initialEntries={["/extensions"]}>
          <SessionGate api={api}>
            <ExtensionsRoute />
          </SessionGate>
        </MemoryRouter>
      </ThemeProvider>,
    );
  }

  it("tells an operator they can look but not change, and disables the verbs", async () => {
    mockFeeder({ packs: [pack()] });
    renderAs("operator");

    // Said once, at the top — six dead controls with no explanation reads as a
    // broken page.
    expect(await screen.findByText(/you can look, but not change/i)).toBeInTheDocument();

    const card = await packCard(PACK_ID);
    expect(within(card).getByRole("button", { name: /Uninstall/ })).toBeDisabled();
    expect(within(card).getByRole("button", { name: /Disable acme\/menu-board/ })).toBeDisabled();
    // Reading is untouched: the pack, its version and its history are all still
    // there. Only the verbs that change the deployment are withheld.
    expect(within(card).getByText(PACK_ID)).toBeInTheDocument();
    expect(within(card).getByRole("button", { name: /install history/i })).toBeEnabled();
  });

  it("leaves an admin every verb", async () => {
    mockFeeder({ packs: [pack()] });
    renderAs("admin");

    const card = await packCard(PACK_ID);
    expect(screen.queryByText(/you can look, but not change/i)).not.toBeInTheDocument();
    expect(within(card).getByRole("button", { name: /Uninstall/ })).toBeEnabled();
    expect(within(card).getByRole("button", { name: /Disable acme\/menu-board/ })).toBeEnabled();
  });
});
