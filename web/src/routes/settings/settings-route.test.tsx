import { describe, it, expect, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import { ThemeProvider } from "@/components/theme/theme-provider";
import SettingsRoute, { formatCoords, geoProblems } from "./settings-route";
import settingsPageDoc from "./page.uis.json";
import { validatePage } from "@/renderer/validate";
import { TRACE_ID, ULID_A, ULID_B, ULID_ROOT, scopeNode, ok, problem } from "@/api/test-support";

// These tests DRIVE the page: every behaviour below is a real userEvent gesture
// through the real PageRenderer against a real (mocked) api/1. The one thing
// this page must never do is present a control that appears to configure the box
// and does not — so each assertion is about what left the browser, or about what
// a refusal put on screen, not about what rendered.

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => {
  server.resetHandlers();
  window.localStorage.clear();
});
afterAll(() => server.close());

function renderSettings() {
  return render(
    <ThemeProvider>
      <MemoryRouter>
        <SettingsRoute />
      </MemoryRouter>
    </ThemeProvider>,
  );
}

function page(items: unknown[]) {
  return HttpResponse.json({ items, cursor: null }, { headers: { "Trace-Id": TRACE_ID } });
}

/** A site-kind scope node: the only record this page edits. */
function site(over: Record<string, unknown> = {}) {
  return scopeNode({
    id: ULID_A,
    kind: "site",
    parent_id: ULID_ROOT,
    name: "The Hangar",
    tz: "America/Chicago",
    lat: 41.8781,
    long: -87.6298,
    revision: 1,
    ...over,
  });
}

function siteRow(name: string): HTMLElement {
  const table = screen.getByRole("table", { name: "Sites" });
  return within(table).getByText(name).closest("tr") as HTMLElement;
}

/** Open a site's detail form and wait for it. */
async function openSite(user: ReturnType<typeof userEvent.setup>, name: string) {
  await screen.findByRole("table", { name: "Sites" });
  await user.click(siteRow(name));
  await screen.findByLabelText("Site name");
}

/** The time-zone control.
 *
 * It is NOT a `<select>`: `Intl.supportedValuesOf` enumerates 446 zones, which
 * is past the renderer's SELECT_SEARCH_THRESHOLD, so the `select` widget paints
 * the kit's searchable Combobox instead. The zone that is currently stored reads
 * off the trigger's own text. */
function zoneControl(): HTMLElement {
  return screen.getByLabelText("Time zone");
}

/** Pick a zone the way an operator does: open the picker, type enough of the
 * name to find it among 446 rows, click the row. */
async function pickZone(user: ReturnType<typeof userEvent.setup>, zone: string) {
  await user.click(zoneControl());
  const search = await screen.findByPlaceholderText(/search time zone/i);
  await user.type(search, zone);
  await user.click(await screen.findByRole("option", { name: zone }));
  await waitFor(() => expect(zoneControl()).toHaveTextContent(zone));
}

describe("Settings — the ui-schema document", () => {
  it("its page.uis.json passes validatePage (the same gate an extension page clears)", () => {
    const result = validatePage(settingsPageDoc);
    expect(result).toEqual({ ok: true });
  });

  it("is a list-detail, because the resource is a COLLECTION of sites", () => {
    // `settings-form` binds exactly one record through a static `source` and has
    // no way to choose which — fine for a pack's own singleton settings, wrong
    // for a workspace that may hold several sites.
    expect((settingsPageDoc as { pageType: string }).pageType).toBe("list-detail");
  });

  it("offers no create and no delete: a site's place in the tree is authored on Screens", () => {
    const doc = settingsPageDoc as Record<string, unknown>;
    expect(doc.newAction).toBeUndefined();
    expect(JSON.stringify(doc)).not.toContain('"delete"');
  });
});

describe("Settings — reading what the box is configured with", () => {
  it("asks for site-kind nodes only", async () => {
    let selector: string | null = null;
    server.use(
      http.get("*/api/v1/scope-nodes", ({ request }) => {
        selector = new URL(request.url).searchParams.get("selector");
        return page([site()]);
      }),
    );
    renderSettings();
    await screen.findByRole("table", { name: "Sites" });
    expect(selector).toBe("kind=site");
  });

  it("shows each site's time zone and coordinates, not just its name", async () => {
    server.use(http.get("*/api/v1/scope-nodes", () => page([site()])));
    renderSettings();
    const table = await screen.findByRole("table", { name: "Sites" });
    expect(within(table).getByText("The Hangar")).toBeInTheDocument();
    expect(within(table).getByText("America/Chicago")).toBeInTheDocument();
    expect(within(table).getByText("41.8781, -87.6298")).toBeInTheDocument();
  });

  it("sends an operator with no sites to the page that creates one", async () => {
    server.use(http.get("*/api/v1/scope-nodes", () => page([])));
    renderSettings();
    expect(await screen.findByRole("link", { name: "Screens" })).toHaveAttribute("href", "/screens");
  });

  it("offers the site's STORED zone as a select option, so opening the form cannot change it", async () => {
    // A zone no browser enumerates. If the union in timezones.ts were dropped,
    // the select would open on nothing and Save would rewrite the site's clock.
    server.use(http.get("*/api/v1/scope-nodes", () => page([site({ tz: "Antarctica/Troll" })])));
    const user = userEvent.setup();
    renderSettings();
    await openSite(user, "The Hangar");
    // The trigger shows the stored zone, and the zone is findable in the list.
    expect(zoneControl()).toHaveTextContent("Antarctica/Troll");
    await user.click(zoneControl());
    await user.type(await screen.findByPlaceholderText(/search time zone/i), "Troll");
    expect(await screen.findByRole("option", { name: "Antarctica/Troll" })).toBeInTheDocument();
  });

  it("does not signpost a second restart control — it links to the one that exists", async () => {
    server.use(http.get("*/api/v1/scope-nodes", () => page([site()])));
    renderSettings();
    await screen.findByRole("table", { name: "Sites" });
    expect(screen.getByRole("link", { name: "System" })).toHaveAttribute("href", "/system");
    expect(screen.queryByRole("button", { name: /restart/i })).toBeNull();
  });
});

describe("Settings — saving a site, driven", () => {
  it("PATCHes tz, lat, long and name together under an If-Match, and says what changed", async () => {
    const state = { row: site() };
    let ifMatch: string | null = null;
    let body: Record<string, unknown> | null = null;
    server.use(
      http.get("*/api/v1/scope-nodes", () => page([state.row])),
      http.patch("*/api/v1/scope-nodes/:id", async ({ request }) => {
        ifMatch = request.headers.get("If-Match");
        body = (await request.json()) as Record<string, unknown>;
        state.row = { ...state.row, ...body, revision: 2 };
        return ok(state.row, { revision: 2 });
      }),
    );

    const user = userEvent.setup();
    renderSettings();
    await openSite(user, "The Hangar");

    await pickZone(user, "Europe/London");
    await user.click(screen.getByRole("button", { name: "Save this site" }));

    await screen.findByText("Saved. This site now keeps local time in Europe/London.");
    expect(ifMatch).toBe('"1"');
    // All four together: DAT-033 resolves tz/lat/long as ONE unit, so a patch
    // that moved the clock and left the coordinates would describe a place the
    // site is not in.
    expect(body).toEqual({
      name: "The Hangar",
      tz: "Europe/London",
      lat: 41.8781,
      long: -87.6298,
    });
  });

  it("saves an edited latitude as a number, not as the string the input carries", async () => {
    const state = { row: site() };
    let body: Record<string, unknown> | null = null;
    server.use(
      http.get("*/api/v1/scope-nodes", () => page([state.row])),
      http.patch("*/api/v1/scope-nodes/:id", async ({ request }) => {
        body = (await request.json()) as Record<string, unknown>;
        return ok({ ...state.row, ...body, revision: 2 }, { revision: 2 });
      }),
    );

    const user = userEvent.setup();
    renderSettings();
    await openSite(user, "The Hangar");

    const lat = screen.getByLabelText("Latitude");
    await user.clear(lat);
    await user.type(lat, "51.5");
    await user.click(screen.getByRole("button", { name: "Save this site" }));

    await waitFor(() => expect(body).not.toBeNull());
    expect(body).toMatchObject({ lat: 51.5 });
    expect(typeof (body as unknown as { lat: unknown }).lat).toBe("number");
  });

  // THE revision guard. The renderer seeds its store once, so after a save the
  // form still holds the revision the page LOADED. Without the host's revision
  // map the second consecutive save sends a stale If-Match and is refused 412 —
  // a conflict with nobody.
  it("conditions a SECOND consecutive save on the revision the first one returned", async () => {
    const state = { row: site({ revision: 1 }) };
    const ifMatches: (string | null)[] = [];
    server.use(
      http.get("*/api/v1/scope-nodes", () => page([state.row])),
      http.patch("*/api/v1/scope-nodes/:id", async ({ request }) => {
        ifMatches.push(request.headers.get("If-Match"));
        const patch = (await request.json()) as Record<string, unknown>;
        const next = (state.row.revision as number) + 1;
        state.row = { ...state.row, ...patch, revision: next };
        return ok(state.row, { revision: next });
      }),
    );

    const user = userEvent.setup();
    renderSettings();
    await openSite(user, "The Hangar");

    await pickZone(user, "Europe/London");
    await user.click(screen.getByRole("button", { name: "Save this site" }));
    await screen.findByText("Saved. This site now keeps local time in Europe/London.");

    await pickZone(user, "Asia/Tokyo");
    await user.click(screen.getByRole("button", { name: "Save this site" }));
    await screen.findByText("Saved. This site now keeps local time in Asia/Tokyo.");

    expect(ifMatches).toEqual(['"1"', '"2"']);
  });

  it("does not carry a success message across to another site", async () => {
    const rows = [site({ id: ULID_A, name: "The Hangar" }), site({ id: ULID_B, name: "Warehouse" })];
    server.use(
      http.get("*/api/v1/scope-nodes", () => page(rows)),
      http.patch("*/api/v1/scope-nodes/:id", () => ok({ ...rows[0], revision: 2 }, { revision: 2 })),
    );

    const user = userEvent.setup();
    renderSettings();
    await openSite(user, "The Hangar");
    await user.click(screen.getByRole("button", { name: "Save this site" }));
    await screen.findByText(/^Saved\./);

    await user.click(siteRow("Warehouse"));
    // The outcome slot is a single static `$ui` path, so the record it describes
    // has to be checked before its success line is shown — otherwise Warehouse
    // reads as saved when nothing was written to it.
    await waitFor(() => expect(screen.queryByText(/^Saved\./)).toBeNull());
  });

  it("disables Save while the write is in flight, so one click cannot become two", async () => {
    let patches = 0;
    let release!: () => void;
    const gate = new Promise<void>((r) => {
      release = r;
    });
    server.use(
      http.get("*/api/v1/scope-nodes", () => page([site()])),
      http.patch("*/api/v1/scope-nodes/:id", async () => {
        patches += 1;
        await gate;
        return ok(site({ revision: 2 }), { revision: 2 });
      }),
    );

    const user = userEvent.setup();
    renderSettings();
    await openSite(user, "The Hangar");

    const save = screen.getByRole("button", { name: "Save this site" });
    await user.click(save);
    await waitFor(() => expect(save).toBeDisabled());
    await screen.findByText(/^Saving —/);
    await user.click(save);
    expect(patches).toBe(1);

    release();
    await screen.findByText(/^Saved\./);
    expect(save).not.toBeDisabled();
  });
});

describe("Settings — refusals", () => {
  // THE completeness guard (DAT-031). A site with no clock is a site whose every
  // schedule stops resolving, so the page must refuse locally rather than send a
  // body and let the operator read a schema complaint about a null.
  it("refuses to save a site with a cleared latitude, and sends nothing", async () => {
    let patches = 0;
    server.use(
      http.get("*/api/v1/scope-nodes", () => page([site()])),
      http.patch("*/api/v1/scope-nodes/:id", () => {
        patches += 1;
        return ok(site({ revision: 2 }), { revision: 2 });
      }),
    );

    const user = userEvent.setup();
    renderSettings();
    await openSite(user, "The Hangar");

    await user.clear(screen.getByLabelText("Latitude"));
    await user.click(screen.getByRole("button", { name: "Save this site" }));

    const alerts = await screen.findAllByRole("alert");
    expect(alerts.some((a) => /needs a latitude/.test(a.textContent ?? ""))).toBe(true);
    expect(patches).toBe(0);
    // …and it says so ON the field, not only in the banner.
    expect(screen.getByLabelText("Latitude")).toHaveAttribute("aria-invalid", "true");
  });

  it("keeps the operator's edits and the box's values side by side on a 412", async () => {
    const changed = site({ name: "Renamed elsewhere", tz: "Europe/Paris", revision: 9 });
    server.use(
      http.get("*/api/v1/scope-nodes", () => page([site({ revision: 1 })])),
      http.get("*/api/v1/scope-nodes/:id", () => ok(changed, { revision: 9 })),
      http.patch("*/api/v1/scope-nodes/:id", () =>
        problem(412, "REVISION_CONFLICT", "Modified concurrently.", { current_revision: 9 }),
      ),
    );

    const user = userEvent.setup();
    renderSettings();
    await openSite(user, "The Hangar");
    await pickZone(user, "Asia/Tokyo");
    await user.click(screen.getByRole("button", { name: "Save this site" }));

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Renamed elsewhere");
    expect(alert).toHaveTextContent("Europe/Paris");
    // The operator's own edit is NOT discarded behind their back.
    expect(zoneControl()).toHaveTextContent("Asia/Tokyo");
    expect(screen.getByRole("button", { name: "Reload this site" })).toBeInTheDocument();
  });

  // THE no-overwrite guard (API-022). Adopting the server's revision after a 412
  // would arm the very next Save to silently destroy the other writer's change.
  it("does not adopt the server's revision after a 412 — a retry cannot overwrite", async () => {
    const ifMatches: (string | null)[] = [];
    server.use(
      http.get("*/api/v1/scope-nodes", () => page([site({ revision: 1 })])),
      http.get("*/api/v1/scope-nodes/:id", () => ok(site({ revision: 9 }), { revision: 9 })),
      http.patch("*/api/v1/scope-nodes/:id", ({ request }) => {
        ifMatches.push(request.headers.get("If-Match"));
        return problem(412, "REVISION_CONFLICT", "Modified concurrently.", { current_revision: 9 });
      }),
    );

    const user = userEvent.setup();
    renderSettings();
    await openSite(user, "The Hangar");
    const save = screen.getByRole("button", { name: "Save this site" });
    await user.click(save);
    await screen.findByRole("button", { name: "Reload this site" });
    await user.click(save);

    await waitFor(() => expect(ifMatches).toHaveLength(2));
    expect(ifMatches).toEqual(['"1"', '"1"']);
  });

  it("pins a server 422's per-field message to the field it names", async () => {
    server.use(
      http.get("*/api/v1/scope-nodes", () => page([site()])),
      http.patch("*/api/v1/scope-nodes/:id", () =>
        problem(422, "VALIDATION_FAILED", "The body was rejected.", {
          errors: [{ field: "tz", message: "not a loadable IANA zone", code: "SCOPE_NODE_GEO_REQUIRED" }],
        }),
      ),
    );

    const user = userEvent.setup();
    renderSettings();
    await openSite(user, "The Hangar");
    await user.click(screen.getByRole("button", { name: "Save this site" }));

    expect(await screen.findByText("not a loadable IANA zone")).toBeInTheDocument();
    expect(screen.getByLabelText("Time zone")).toHaveAttribute("aria-invalid", "true");
  });

  it("names the refusal's code and trace id rather than only apologising", async () => {
    server.use(
      http.get("*/api/v1/scope-nodes", () => page([site()])),
      http.patch("*/api/v1/scope-nodes/:id", () =>
        problem(403, "FORBIDDEN", "Only the workspace owner may change a site."),
      ),
    );

    const user = userEvent.setup();
    renderSettings();
    await openSite(user, "The Hangar");
    await user.click(screen.getByRole("button", { name: "Save this site" }));

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Only the workspace owner may change a site.");
    expect(alert).toHaveTextContent("FORBIDDEN");
    expect(alert).toHaveTextContent(TRACE_ID);
  });

  it("clears a refusal when the operator moves to a different site", async () => {
    const rows = [site({ id: ULID_A, name: "The Hangar" }), site({ id: ULID_B, name: "Warehouse" })];
    server.use(
      http.get("*/api/v1/scope-nodes", () => page(rows)),
      http.patch("*/api/v1/scope-nodes/:id", () => problem(403, "FORBIDDEN", "Nope.")),
    );

    const user = userEvent.setup();
    renderSettings();
    await openSite(user, "The Hangar");
    await user.click(screen.getByRole("button", { name: "Save this site" }));
    await screen.findByRole("alert");

    await user.click(siteRow("Warehouse"));
    await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
  });
});

describe("Settings — the pure guards", () => {
  it("geoProblems names every missing half of DAT-031's one unit", () => {
    expect(geoProblems({ name: "S", tz: "UTC", lat: 1, long: 2 })).toEqual({});
    expect(Object.keys(geoProblems({ name: "S", tz: "UTC", lat: null, long: 2 }))).toEqual(["lat"]);
    expect(Object.keys(geoProblems({ name: "S", tz: "", lat: 1, long: 2 }))).toEqual(["tz"]);
    expect(Object.keys(geoProblems({ name: " ", tz: "UTC", lat: 1, long: 2 }))).toEqual(["name"]);
    expect(Object.keys(geoProblems({ name: "S", tz: "UTC", lat: 1, long: null }))).toEqual(["long"]);
  });

  it("geoProblems refuses 0 as missing nowhere — the equator and Greenwich are real places", () => {
    expect(geoProblems({ name: "S", tz: "UTC", lat: 0, long: 0 })).toEqual({});
  });

  it("geoProblems refuses NaN, which a number-input can produce and JSON cannot carry", () => {
    expect(Object.keys(geoProblems({ name: "S", tz: "UTC", lat: Number.NaN, long: 2 }))).toEqual(["lat"]);
  });

  it("formatCoords says nothing rather than printing a half-cleared pair", () => {
    expect(formatCoords(41.8781, -87.6298)).toBe("41.8781, -87.6298");
    expect(formatCoords(null, -87.6298)).toBe("—");
    expect(formatCoords(41.8781, undefined)).toBe("—");
    expect(formatCoords(0, 0)).toBe("0.0000, 0.0000");
  });
});
