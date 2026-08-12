import { test, expect, signIn, csrfHeader } from "./support/console-session";
import type { APIRequestContext, Page } from "@playwright/test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { randomUUID } from "node:crypto";

/**
 * The click-through gate — the anti-regression this suite exists for. The whole
 * reason the dead New button shipped is that 500+ unit tests and the Go e2e all
 * asserted RENDERING/validation/data-flow and NONE pressed a control through to an
 * effect. This spec drives the REAL console the feeder serves and presses the
 * actual buttons: New -> fill -> Save (a new row must appear in the table AND
 * exist via the pack-data API), select -> edit -> Save (the change must persist),
 * Delete (the row must be gone from the table AND the API), and every core nav
 * item (its page heading must appear). A control that renders but does nothing —
 * the exact failure mode a render-only test misses — fails this gate.
 *
 * The stack (feeder + relay + the embedded SPA) is up already, managed by
 * `make web-e2e`. The `baseURL` + `ignoreHTTPSErrors` in playwright.config.ts point
 * at the feeder's self-signed HTTPS origin, same-origin for both the browser and
 * the `page.request` context this file cross-checks the API with.
 *
 * Every test here begins by signing in through the real login form: the console is
 * behind the SessionGate (every api/1 route is authenticated, SEC-005), so an
 * anonymous navigation lands on /login rather than on the page under test. The
 * owner credential that sign-in uses is established by the suite's shared
 * bootstrap (support/console-session.ts), which claims the box on a fresh one and
 * reuses the credential on a box an earlier run already claimed — so this spec and
 * its sibling work in either order and against a box in either state.
 */

const PACK_ID = "waiveo/menu-board";
const MENU_PAGE = "/p/waiveo/menu-board/menu-items";
const ROWS_API = "/api/v1/packs/waiveo/menu-board/data/menu_items";

// A distinctive, regex-safe name so a table/row/detail match is unambiguous. The
// stack is freshly seeded each `make web-e2e` run (an empty store), so a fixed name
// is deterministic; it carries no secret and is not a real menu item.
const ITEM_NAME = "E2E Flat White";

// NOTE (cold-open create): the create test below drives New → fill → Save with NO
// test-provisioned scope. A pack row must attach to a scope_node (MAN-051), and a
// stock `make web-e2e` seeds ONLY a Demo Site (kind=site) whose org ancestor is a
// virtual boundary — there is NO kind=org row. The app must resolve that site as the
// create target on its own (org → site → any). This spec deliberately does NOT
// provision an org: if the app cannot resolve a scope from the stock seed, the create
// fails here — which is exactly the regression this gate must catch.

// Install the in-repo example pack over the REAL install endpoint if it is not already
// present. The zip bytes come from `make example-pack` (one source of truth,
// examples/packs); its path is handed in via PW_PACK_ZIP, with a repo-relative fallback.
//
// It runs over the signed-in operator's own session, so the pack is installed by the
// same principal the UI then acts as — and the install being a POST is why it carries
// the CSRF echo the console's api client would normally add.
async function ensurePackInstalled(page: Page): Promise<void> {
  const api = page.request;
  const res = await api.get("/api/v1/packs");
  expect(res.ok(), `list packs: ${res.status()}`).toBeTruthy();
  const body = (await res.json()) as { items?: Array<{ id?: string }> };
  if ((body.items ?? []).some((p) => p.id === PACK_ID)) return;
  const zipPath = process.env.PW_PACK_ZIP ?? resolve(process.cwd(), "..", ".dev", "menu-board.pack.zip");
  const zip = readFileSync(zipPath);
  const installed = await api.post("/api/v1/packs", {
    headers: {
      "Content-Type": "application/zip",
      "Idempotency-Key": randomUUID(),
      ...(await csrfHeader(page)),
    },
    data: zip,
  });
  // 201 fresh install or 200 reinstall-in-place are both success.
  expect([200, 201], `install pack: ${installed.status()} ${await installed.text()}`).toContain(
    installed.status(),
  );
}

// The current menu_items rows straight off the pack-data api/1 surface — the ground
// truth every UI assertion is cross-checked against (the row is real, queryable data).
async function apiRows(api: APIRequestContext): Promise<Array<Record<string, unknown>>> {
  const res = await api.get(ROWS_API);
  expect(res.ok(), `list rows: ${res.status()}`).toBeTruthy();
  const body = (await res.json()) as { items?: Array<Record<string, unknown>> };
  return body.items ?? [];
}

// The seeded org root's scope-node id straight off the scope-nodes api — the create
// target the app must resolve on its OWN (org → site → any, MAN-051) with nothing
// test-provisioned. A stock `make dev-up` seeds exactly one org, because a scope
// node's parent has to resolve (DAT-002) and the seed's site names one; assert it is
// present so a rootless seed fails loudly here rather than surfacing opaquely
// downstream.
async function defaultScopeNodeId(api: APIRequestContext): Promise<string> {
  const res = await api.get("/api/v1/scope-nodes");
  expect(res.ok(), `list scope nodes: ${res.status()}`).toBeTruthy();
  const body = (await res.json()) as { items?: Array<{ id?: string; kind?: string }> };
  const org = (body.items ?? []).find((n) => n.kind === "org");
  expect(org?.id, "stock make dev-up seeds an org root (kind=org)").toBeTruthy();
  return org!.id!;
}

// The create → edit → delete chain is STATEFUL over one row; serial so a failure stops
// the chain (a corrupted-state cascade of red would obscure the first real failure).
test.describe.serial("installed pack list-detail — real create / edit / delete clicks", () => {
  test("New enters a blank draft; Save adds a row to the table AND the pack-data API", async ({ page }) => {
    const api = await signIn(page);
    await ensurePackInstalled(page);

    await page.goto(MENU_PAGE);
    await expect(page.getByRole("heading", { level: 1, name: "Menu Items" })).toBeVisible();

    const list = page.getByRole("region", { name: "List" });
    const detail = page.getByRole("region", { name: "Detail" });

    // Press New -> the detail switches to a BLANK create draft (the headline fix: New
    // used to do nothing / keep the prior item). Fill it and Save through the real
    // create path.
    await list.getByRole("button", { name: "New" }).click();
    const nameInput = detail.getByLabel("Item name");
    await expect(nameInput).toHaveValue("");
    await nameInput.fill(ITEM_NAME);
    await detail.getByLabel("Price").fill("4.5");
    await detail.getByRole("button", { name: "Save changes" }).click();

    // The new row appears in the table, its price formatted as currency (UIS-143).
    const table = page.getByRole("table", { name: "Menu items" });
    await expect(table.getByText(ITEM_NAME)).toBeVisible();
    await expect(table.getByText("$4.50")).toBeVisible();

    // And it truly persisted — present via the pack-data API with the typed price.
    await expect
      .poll(async () => (await apiRows(api)).find((r) => r.name === ITEM_NAME)?.price)
      .toBe(4.5);

    // The app resolved the create scope on its OWN, with nothing test-provisioned:
    // the row attaches to the deployment ROOT a stock `make dev-up` guarantees — the
    // org → site → any resolution (MAN-051), which now lands on its first preference
    // because the seed inserts the org root its site names. This pins the actual fix:
    // the scope is neither null (which would have refused with "no scope to attach
    // records to yet") nor a node further down the tree.
    const rootScopeNode = await defaultScopeNodeId(api);
    await expect
      .poll(async () => (await apiRows(api)).find((r) => r.name === ITEM_NAME)?.scope_node)
      .toBe(rootScopeNode);
  });

  test("selecting the row, editing Price, and Save persists the change", async ({ page }) => {
    const api = await signIn(page);
    await ensurePackInstalled(page);

    await page.goto(MENU_PAGE);
    const table = page.getByRole("table", { name: "Menu items" });
    await expect(table.getByText(ITEM_NAME)).toBeVisible();

    // Select the row (whole-row press, UIS-070) -> its values load into the detail form.
    await table.getByRole("button", { name: new RegExp(ITEM_NAME) }).click();
    const detail = page.getByRole("region", { name: "Detail" });
    await expect(detail.getByLabel("Item name")).toHaveValue(ITEM_NAME);

    // Change a field and Save -> the update rides the standard optimistic-concurrency path.
    await detail.getByLabel("Price").fill("6.25");
    await detail.getByRole("button", { name: "Save changes" }).click();

    await expect(table.getByText("$6.25")).toBeVisible();
    await expect(table.getByText("$4.50")).toHaveCount(0);
    await expect.poll(async () => (await apiRows(api)).find((r) => r.name === ITEM_NAME)?.price).toBe(6.25);
  });

  test("Delete removes the row from the table AND the pack-data API", async ({ page }) => {
    const api = await signIn(page);
    await ensurePackInstalled(page);

    await page.goto(MENU_PAGE);
    const table = page.getByRole("table", { name: "Menu items" });
    await table.getByRole("button", { name: new RegExp(ITEM_NAME) }).click();

    const detail = page.getByRole("region", { name: "Detail" });
    await detail.getByRole("button", { name: "Delete item" }).click();

    await expect(page.getByText(ITEM_NAME)).toHaveCount(0);
    await expect.poll(async () => (await apiRows(api)).some((r) => r.name === ITEM_NAME)).toBe(false);
  });
});

// Every core destination: click the real nav link, assert the page's own h1 heading.
// A nav item that renders but routes nowhere (or a page that never paints its header)
// fails here.
test("core navigation — each nav item routes to its page heading", async ({ page }) => {
  // Signing in lands on the overview, which is where the nav walk starts — and its
  // Overview heading assertion is the first destination already proven.
  await signIn(page);
  const nav = page.getByRole("navigation", { name: "Primary" });

  // DERIVED from the rail, never enumerated. The previous version hard-coded ten
  // destinations and rotted: it still asked for a "Content" link that became
  // "/upload" merges ago, so Playwright waited 30s for a link that cannot exist
  // and the suite carried a standing red. Worse than the red itself, that taught
  // everyone — me included — to wave the whole spec through as "known
  // pre-existing", which is exactly how a real failure gets missed. It also knew
  // nothing of Devices, Roku, Widgets, Variables, Extensions or Settings, all of
  // which shipped after it was written.
  //
  // It asserts that SOME h1 paints, not which one. That is the property this test
  // is actually for — its own header says "a nav item that renders but routes
  // nowhere (or a page that never paints its header) fails here" — and pinning
  // copy is what made it a maintenance burden rather than a safety net. A page
  // whose heading text matters is pinned by its own suite.
  const labels = await nav.getByRole("link").evaluateAll((els) =>
    els.map((el) => (el.textContent ?? "").trim()).filter((t) => t.length > 0),
  );
  expect(labels.length).toBeGreaterThan(8);

  for (const label of labels) {
    await nav.getByRole("link", { name: label, exact: true }).click();
    await expect(page.getByRole("heading", { level: 1 }).first()).toBeVisible();
  }
});