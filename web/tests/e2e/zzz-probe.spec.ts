import { test, signIn } from "./support/console-session";

test("probe tab state", async ({ page }) => {
  await signIn(page);
  await page.goto("/pages");
  await page.getByRole("tab", { name: "Dashboard" }).click();
  await page.waitForTimeout(500);
  const info = await page.evaluate(() =>
    Array.from(document.querySelectorAll('[role="tab"]')).map((t) => {
      const cs = getComputedStyle(t);
      return {
        text: (t.textContent ?? "").trim(),
        state: t.getAttribute("data-state"),
        selected: t.getAttribute("aria-selected"),
        bg: cs.backgroundColor,
        color: cs.color,
        shadow: cs.boxShadow,
      };
    }),
  );
  // eslint-disable-next-line no-console
  console.log("TABS:", JSON.stringify(info, null, 1));
  const listBg = await page.evaluate(
    () => getComputedStyle(document.querySelector('[role="tablist"]') as Element).backgroundColor,
  );
  // eslint-disable-next-line no-console
  console.log("LIST BG:", listBg);
});
