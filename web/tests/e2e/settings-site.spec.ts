import type { Page } from "@playwright/test";
import { test, expect, signIn } from "./support/console-session";

/**
 * The settings surface, driven against the real box.
 *
 * The claim this gate has to defend is the one the whole page exists for: a
 * setting that writes somewhere nothing reads is worse than no setting. So this
 * does not assert that a select rendered. It changes a site's time zone through
 * the real control, reloads the page, and then reads the value back OUT OF THE
 * BOX over api/1 — because the form re-displaying what you typed proves only
 * that React kept state.
 */

/** Pick a zone through the real control.
 *
 * The control is NOT a `<select>`: this browser enumerates ~446 IANA zones,
 * which is past the renderer's SELECT_SEARCH_THRESHOLD, so the `select` widget
 * paints the kit's searchable Combobox. An operator opens it, types enough of
 * the name to find it, and clicks the row — which is the whole reason it was
 * changed, since a native select's type-ahead cannot find `Asia/Tokyo` from
 * "tokyo". */
async function pickZone(page: Page, zone: string): Promise<void> {
  await page.getByLabel("Time zone").click();
  await page.getByPlaceholder(/search time zone/i).fill(zone);
  await page.getByRole("option", { name: zone, exact: true }).click();
  await expect(page.getByLabel("Time zone")).toHaveText(zone);
}

test("changes a site's time zone through the real control and it survives a reload", async ({ page }) => {
  const api = await signIn(page);

  // The stock dev seed carries one site ("Demo Site", America/Chicago). Read it
  // first so the assertions name the record rather than an index.
  const before = await api.get("/api/v1/scope-nodes?selector=kind%3Dsite");
  expect(before.ok(), `list sites: ${before.status()}`).toBeTruthy();
  const sites = ((await before.json()) as { items: Array<Record<string, unknown>> }).items;
  expect(sites.length, "the dev seed provides one site").toBeGreaterThan(0);
  const target = sites[0];
  const siteId = String(target.id);
  const siteName = String(target.name);
  const originalTz = String(target.tz);
  // Move to a zone the site is definitely not already on, so a no-op passes nothing.
  const newTz = originalTz === "Europe/London" ? "Asia/Tokyo" : "Europe/London";

  await page.goto("/settings");
  await expect(page.getByRole("heading", { name: "Settings", level: 1 })).toBeVisible();

  // Reached from the rail, in the Platform group — not typed as a URL.
  await page.goto("/");
  await page.getByRole("link", { name: "Settings", exact: true }).click();
  await expect(page).toHaveURL(/\/settings$/);

  // Select the site, change the zone, save.
  await page.getByRole("table", { name: "Sites" }).getByText(siteName).click();
  const tz = page.getByLabel("Time zone");
  await expect(tz).toBeVisible();
  await expect(tz).toHaveText(originalTz);
  await pickZone(page, newTz);
  await page.getByRole("button", { name: "Save this site" }).click();

  // The page says it landed, naming the zone it landed on.
  await expect(page.getByText(`Saved. This site now keeps local time in ${newTz}.`)).toBeVisible();

  // THE assertion: the BOX holds it, not the form.
  const after = await api.get(`/api/v1/scope-nodes/${siteId}`);
  expect(after.ok(), `read site: ${after.status()}`).toBeTruthy();
  const stored = (await after.json()) as { tz?: string; revision?: number };
  expect(stored.tz).toBe(newTz);

  // …and it survives a full reload of the console, in the list AND the form.
  await page.reload();
  await expect(page.getByRole("table", { name: "Sites" }).getByText(newTz)).toBeVisible();
  await page.getByRole("table", { name: "Sites" }).getByText(siteName).click();
  await expect(page.getByLabel("Time zone")).toHaveText(newTz);

  await page.screenshot({ path: "../.dev/settings-site.png", fullPage: true });

  // Put the seed back, so a second run of this file starts where the first did.
  await pickZone(page, originalTz);
  await page.getByRole("button", { name: "Save this site" }).click();
  await expect(page.getByText(`Saved. This site now keeps local time in ${originalTz}.`)).toBeVisible();
});

test("names the zone the System log's timestamps are in", async ({ page }) => {
  // The other half of the timezone answer. There is no stored setting behind
  // this and there should not be: the process log has no site, so the reader's
  // own zone is the honest one — it just has to SAY so.
  await signIn(page);
  await page.goto("/system");
  const table = page.getByRole("table", { name: /platform log/i });
  await expect(table).toBeVisible();
  await expect(table.getByRole("columnheader", { name: /^Time \(.+\)$/ })).toBeVisible();
  await table.screenshot({ path: "../.dev/system-log-zone.png" });
});

test("refuses to strip a site of the coordinates its sunrise rules need", async ({ page }) => {
  const api = await signIn(page);
  const before = await api.get("/api/v1/scope-nodes?selector=kind%3Dsite");
  const sites = ((await before.json()) as { items: Array<Record<string, unknown>> }).items;
  const target = sites[0];
  const originalLat = target.lat as number;

  await page.goto("/settings");
  await page.getByRole("table", { name: "Sites" }).getByText(String(target.name)).click();
  await page.getByLabel("Latitude").fill("");
  await page.getByRole("button", { name: "Save this site" }).click();

  await expect(page.getByText(/needs a latitude/).first()).toBeVisible();
  // Nothing was written: the box still holds what it held.
  const after = await api.get(`/api/v1/scope-nodes/${String(target.id)}`);
  expect(((await after.json()) as { lat?: number }).lat).toBe(originalLat);
});
