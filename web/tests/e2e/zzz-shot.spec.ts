import { test, expect, signIn } from "./support/console-session";

// TEMPORARY: screenshots of the time-zone picker in both skies. Deleted before commit.
for (const [theme, file] of [
  ["Daybreak", "zone-light"],
  ["Dusk", "zone-dark"],
] as const) {
  test(`zone picker — ${theme}`, async ({ page }) => {
    const api = await signIn(page);
    const res = await api.get("/api/v1/scope-nodes?selector=kind%3Dsite");
    const sites = ((await res.json()) as { items: Array<Record<string, unknown>> }).items;
    // eslint-disable-next-line no-console
    console.log("SITES:", JSON.stringify(sites.map((s) => s.name)));
    expect(sites.length).toBeGreaterThan(0);
    const siteName = String(sites[0].name);

    await page.goto("/settings");
    const toggle = page.getByRole("button", { name: new RegExp(`switch to ${theme}`, "i") });
    if (await toggle.isVisible()) await toggle.click();

    await page.getByRole("table", { name: "Sites" }).getByText(siteName).click();
    await expect(page.getByLabel("Time zone")).toBeVisible();
    await page.screenshot({ path: `../.dev/${file}-closed.png`, fullPage: true });

    await page.getByLabel("Time zone").click();
    await expect(page.getByPlaceholder(/search time zone/i)).toBeVisible();
    await page.waitForTimeout(300);
    await page.screenshot({ path: `../.dev/${file}-open.png` });

    await page.getByPlaceholder(/search time zone/i).fill("tokyo");
    await page.waitForTimeout(300);
    await page.screenshot({ path: `../.dev/${file}-search.png` });
  });
}
