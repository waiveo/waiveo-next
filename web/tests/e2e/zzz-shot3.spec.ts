import { test, expect, signIn } from "./support/console-session";

// TEMPORARY: tooltip screenshots. Deleted before commit.
for (const [theme, sky] of [
  ["Daybreak", "light"],
  ["Dusk", "dark"],
] as const) {
  test(`tooltips — ${theme}`, async ({ page }) => {
    await signIn(page);
    await page.goto("/casts");
    const toggle = page.getByRole("button", { name: new RegExp(`switch to ${theme}`, "i") });
    if (await toggle.isVisible()) await toggle.click();

    // Header chrome: the one icon-only control on every page. Move the pointer
    // away first — Radix suppresses reopening on a trigger the pointer never
    // left after a click.
    await page.mouse.move(600, 700);
    await page.waitForTimeout(150);
    await page.getByRole("button", { name: /switch to .* theme/i }).hover();
    await expect(page.getByRole("tooltip")).toBeVisible();
    await page.waitForTimeout(300);
    await page.screenshot({ path: `../.dev/tooltip-header-${sky}.png`, clip: { x: 1000, y: 0, width: 440, height: 140 } });
    await page.keyboard.press("Escape");

    // A cast row's five glyphs. Make one if the library is empty.
    if (!(await page.getByRole("button", { name: /^duplicate / }).first().isVisible())) {
      await page.getByRole("button", { name: /new cast/i }).first().click();
      const dialog = page.getByRole("dialog");
      await dialog.getByLabel(/name/i).fill("Lobby loop");
      await dialog.getByRole("button", { name: /create|save/i }).click();
      await page.waitForTimeout(1500);
      await page.goto("/casts");
    }

    const dup = page.getByRole("button", { name: /^duplicate /i }).first();
    await expect(dup).toBeVisible();
    await dup.hover();
    await expect(page.getByRole("tooltip")).toBeVisible();
    await page.waitForTimeout(300);
    const box = (await dup.boundingBox())!;
    await page.screenshot({
      path: `../.dev/tooltip-row-${sky}.png`,
      clip: { x: box.x - 340, y: box.y - 110, width: 560, height: 200 },
    });
  });
}
