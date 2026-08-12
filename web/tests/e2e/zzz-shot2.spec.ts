import { test, expect, signIn } from "./support/console-session";

// TEMPORARY: screenshots of the other adopted surfaces. Deleted before commit.
for (const [theme, sky] of [
  ["Daybreak", "light"],
  ["Dusk", "dark"],
] as const) {
  test(`tabs + tooltips — ${theme}`, async ({ page }) => {
    await signIn(page);
    await page.goto("/pages");
    const toggle = page.getByRole("button", { name: new RegExp(`switch to ${theme}`, "i") });
    if (await toggle.isVisible()) await toggle.click();

    await expect(page.getByRole("tablist", { name: "Page-type demos" })).toBeVisible();
    await page.screenshot({ path: `../.dev/tabs-${sky}.png` });
    await page.getByRole("tab", { name: "Dashboard" }).click();
    await expect(page.getByRole("tab", { name: "Dashboard" })).toHaveAttribute(
      "aria-selected",
      "true",
    );
    // The trigger carries `transition-[color,background-color,box-shadow]` at
    // Tailwind's default 150ms, so a screenshot taken the instant the click
    // lands photographs the OLD pill on the OLD tab. Let it settle.
    await page.waitForTimeout(400);
    await page.screenshot({ path: `../.dev/tabs-${sky}-switched.png` });

    await page.goto("/casts");
    await expect(page.getByRole("heading", { name: "Casts", level: 1 })).toBeVisible();
    const dup = page.getByRole("button", { name: /^duplicate / }).first();
    if (await dup.isVisible()) {
      await dup.hover();
      await expect(page.getByRole("tooltip")).toBeVisible();
      await page.waitForTimeout(200);
      await page.screenshot({ path: `../.dev/tooltip-${sky}.png` });
    }

    await page.goto("/studio");
    await page.waitForTimeout(400);
    await page.screenshot({ path: `../.dev/studio-${sky}.png` });
  });
}
