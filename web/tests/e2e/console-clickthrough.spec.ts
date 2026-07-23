import { test, expect, request as pwRequest, type APIRequestContext } from "@playwright/test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { randomUUID } from "node:crypto";

/**
 * The click-through gate (Wave 4b, Task 5) — the anti-regression this wave exists
 * for. The whole reason the dead New button shipped is that 500+ unit tests and the
 * Go e2e all asserted RENDERING/validation/data-flow and NONE pressed a control
 * through to an effect. This spec drives the REAL console the feeder serves and
 * presses the actual buttons: New -> fill -> Save (a new row must appear in the
 * table AND exist via the pack-data API), select -> edit -> Save (the change must
 * persist), Delete (the row must be gone from the table AND the API), and every core
 * nav item (its page heading must appear). A control that renders but does nothing —
 * the exact failure mode a render-only test misses — fails this gate.
 *
 * The stack (feeder + relay + the embedded SPA) is up already, managed by
 * `make web-e2e`. The `baseURL` + `ignoreHTTPSErrors` in playwright.config.ts point
 * at the feeder's self-signed HTTPS origin, same-origin for both the browser and the
 * `APIRequestContext` this file opens for its API assertions and setup.
 */

const BASE_URL = process.env.PW_BASE_URL ?? "https://127.0.0.1:7420";
const PACK_ID = "waiveo/menu-board";
const MENU_PAGE = "/p/waiveo/menu-board/menu-items";
const ROWS_API = "/api/v1/packs/waiveo/menu-board/data/menu_items";

// A distinctive, regex-safe name so a table/row/detail match is unambiguous. The
// stack is freshly seeded each `make web-e2e` run (an empty store), so a fixed name
// is deterministic; it carries no secret and is not a real menu item.
const ITEM_NAME = "E2E Flat White";

let api: APIRequestContext;

// A pack row must attach to a scope_node (MAN-051). A real deployment carries an org
// root; the dev seed materializes only a Demo Site (its org ancestor id is a virtual
// boundary, never inserted), so `selector=kind=org` is empty on a fresh stack and the
// create control has no scope to attach to. Provision an org root over the API if one
// is absent — the SAME "prepare the data precondition, then drive the control" pattern
// as installing the pack if absent — so the gate exercises the create CONTROL, not a
// seed gap. (Flagged for a proper seed follow-up; a pack row attaches to any scope
// node, so this makes the out-of-box create flow work.)
async function ensureOrgScope(): Promise<void> {
  const res = await api.get("/api/v1/scope-nodes", { params: { selector: "kind=org" } });
  expect(res.ok(), `list org scope nodes: ${res.status()}`).toBeTruthy();
  const body = (await res.json()) as { items?: unknown[] };
  if ((body.items ?? []).length > 0) return;
  const created = await api.post("/api/v1/scope-nodes", {
    headers: { "Idempotency-Key": randomUUID() },
    data: { kind: "org", name: "E2E Org Root", parent_id: null },
  });
  expect(created.status(), `create org scope node: ${await created.text()}`).toBe(201);
}

// Install the in-repo example pack over the REAL install endpoint if it is not already
// present. The zip bytes come from `make example-pack` (one source of truth,
// examples/packs); its path is handed in via PW_PACK_ZIP, with a repo-relative fallback.
async function ensurePackInstalled(): Promise<void> {
  const res = await api.get("/api/v1/packs");
  expect(res.ok(), `list packs: ${res.status()}`).toBeTruthy();
  const body = (await res.json()) as { items?: Array<{ id?: string }> };
  if ((body.items ?? []).some((p) => p.id === PACK_ID)) return;
  const zipPath = process.env.PW_PACK_ZIP ?? resolve(process.cwd(), "..", ".dev", "menu-board.pack.zip");
  const zip = readFileSync(zipPath);
  const installed = await api.post("/api/v1/packs", {
    headers: { "Content-Type": "application/zip", "Idempotency-Key": randomUUID() },
    data: zip,
  });
  // 201 fresh install or 200 reinstall-in-place are both success.
  expect([200, 201], `install pack: ${installed.status()} ${await installed.text()}`).toContain(
    installed.status(),
  );
}

// The current menu_items rows straight off the pack-data api/1 surface — the ground
// truth every UI assertion is cross-checked against (the row is real, queryable data).
async function apiRows(): Promise<Array<Record<string, unknown>>> {
  const res = await api.get(ROWS_API);
  expect(res.ok(), `list rows: ${res.status()}`).toBeTruthy();
  const body = (await res.json()) as { items?: Array<Record<string, unknown>> };
  return body.items ?? [];
}

test.beforeAll(async () => {
  api = await pwRequest.newContext({ baseURL: BASE_URL, ignoreHTTPSErrors: true });
  await ensurePackInstalled();
  await ensureOrgScope();
});

test.afterAll(async () => {
  await api.dispose();
});

// The create → edit → delete chain is STATEFUL over one row; serial so a failure stops
// the chain (a corrupted-state cascade of red would obscure the first real failure).
test.describe.serial("installed pack list-detail — real create / edit / delete clicks", () => {
  test("New enters a blank draft; Save adds a row to the table AND the pack-data API", async ({ page }) => {
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
      .poll(async () => (await apiRows()).find((r) => r.name === ITEM_NAME)?.price)
      .toBe(4.5);
  });

  test("selecting the row, editing Price, and Save persists the change", async ({ page }) => {
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
    await expect.poll(async () => (await apiRows()).find((r) => r.name === ITEM_NAME)?.price).toBe(6.25);
  });

  test("Delete removes the row from the table AND the pack-data API", async ({ page }) => {
    await page.goto(MENU_PAGE);
    const table = page.getByRole("table", { name: "Menu items" });
    await table.getByRole("button", { name: new RegExp(ITEM_NAME) }).click();

    const detail = page.getByRole("region", { name: "Detail" });
    await detail.getByRole("button", { name: "Delete item" }).click();

    await expect(page.getByText(ITEM_NAME)).toHaveCount(0);
    await expect.poll(async () => (await apiRows()).some((r) => r.name === ITEM_NAME)).toBe(false);
  });
});

// Every core destination: click the real nav link, assert the page's own h1 heading.
// A nav item that renders but routes nowhere (or a page that never paints its header)
// fails here.
test("core navigation — each nav item routes to its page heading", async ({ page }) => {
  await page.goto("/");
  const nav = page.getByRole("navigation", { name: "Primary" });

  const destinations: Array<[label: string, heading: string]> = [
    ["Screens", "Screens"],
    ["Schedules", "Schedules"],
    ["Content", "Content"],
    ["Automations", "Automations"],
    ["Activity", "Activity"],
    ["Pages", "Declarative pages"],
    ["Design kit", "Waiveo design kit"],
    ["Overview", "Overview"],
  ];

  for (const [label, heading] of destinations) {
    await nav.getByRole("link", { name: label, exact: true }).click();
    await expect(page.getByRole("heading", { level: 1, name: heading })).toBeVisible();
  }
});
