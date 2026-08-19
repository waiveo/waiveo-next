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
    // The /casts tooltip and /studio shots that used to follow went with those
    // routes in the 2026-08-19 console strip.
  });
}
