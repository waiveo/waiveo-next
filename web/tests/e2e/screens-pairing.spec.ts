import { test, expect, request as pwRequest, type APIRequestContext } from "@playwright/test";
import { existsSync, readFileSync, writeFileSync, mkdirSync } from "node:fs";
import { resolve } from "node:path";

/**
 * The operator pairing click-through: drives the REAL console the feeder
 * serves — sign-in form included — through the whole app-side half of screen
 * pairing, and cross-checks every UI claim against the live API:
 *
 *   1. Establish a real owner credential through the real flow: redeem the
 *      first-boot setup grant (the code the feeder persisted on disk,
 *      security-model SEC-120's "printed or on-screen" presentation) via
 *      POST /auth/setup, once; later runs sign in with the same credential.
 *   2. Sign in through the actual login form (no cookie injection).
 *   3. Screens page → "Pair a new screen" → name + placement → "Create & get
 *      code": the screen identity row must exist via /api/v1/screens and the
 *      pairing code must be ON SCREEN with its expiry.
 *   4. The new row appears in the Displays table; its "Pairing code" button
 *      issues a fresh, DIFFERENT code (each issue mints a new one-time grant).
 *
 * Both captured codes are written under ../.dev/ for the player-side probe
 * (`go run ./scripts/pairprobe`), which redeems them against the relay and
 * proves the redeemed screen_id IS the row id this console created — the
 * cross-id-space join the whole flow exists to establish.
 *
 * The stack (feeder + relay + embedded SPA) is up already, exactly as the
 * sibling spec assumes.
 */

const BASE_URL = process.env.PW_BASE_URL ?? "https://127.0.0.1:7420";
const DEV_DIR = resolve(process.cwd(), "..", ".dev");
const SETUP_CODE_PATH = resolve(DEV_DIR, "feeder-auth", "setup-code.txt");

// Dev-lab-only credential this spec claims the freshly-installed workspace
// with (first run) and signs in with (every run). Not a real secret: the
// stack is a loopback dev instance whose whole auth store is reset with .dev.
const OWNER_ID = "e2e-owner";
const OWNER_PASSWORD = "e2e-owner-password-1";

const SCREEN_NAME = `E2E Paired Screen ${Date.now()}`;

let api: APIRequestContext;

// Redeem the on-disk setup code if the workspace is still unclaimed; a claimed
// workspace (GRANT_ALREADY_REDEEMED / no code on disk) signs in instead.
async function ensureOwnerCredential(): Promise<void> {
  if (!existsSync(SETUP_CODE_PATH)) return; // claimed in an earlier boot; login below
  const code = readFileSync(SETUP_CODE_PATH, "utf8").trim();
  const res = await api.post("/api/v1/auth/setup", {
    data: { code, identifier: OWNER_ID, password: OWNER_PASSWORD },
  });
  // 201 = this run claimed it; 403 GRANT_ALREADY_REDEEMED = an earlier run did.
  if (res.status() !== 201 && res.status() !== 403) {
    throw new Error(`auth/setup: ${res.status()} ${await res.text()}`);
  }
}

test.beforeAll(async () => {
  api = await pwRequest.newContext({ baseURL: BASE_URL, ignoreHTTPSErrors: true });
  await ensureOwnerCredential();
});

test.afterAll(async () => {
  await api.dispose();
});

test("pair a new screen end to end: create, code on screen, re-issue", async ({ page }) => {
  // ---- Sign in through the REAL login form. --------------------------------
  await page.goto("/login");
  await page.getByLabel("Identifier").fill(OWNER_ID);
  await page.getByLabel("Password").fill(OWNER_PASSWORD);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("heading", { level: 1, name: "Overview" })).toBeVisible();

  // The browser session's cookies back the API cross-checks below.
  const authed = page.request;

  // ---- Screens → Pair a new screen. ----------------------------------------
  await page.getByRole("navigation", { name: "Primary" }).getByRole("link", { name: "Screens", exact: true }).click();
  await expect(page.getByRole("heading", { level: 1, name: "Screens" })).toBeVisible();

  await page.getByRole("button", { name: "Pair a new screen" }).click();
  const formDialog = page.getByRole("dialog", { name: "Pair a new screen" });
  await formDialog.getByLabel("Screen name").fill(SCREEN_NAME);
  // The stock seed carries one site (Demo Site); with a single candidate the
  // select defaults to it — assert it is offered rather than assuming.
  await expect(formDialog.getByLabel("Placed under")).toBeVisible();
  await formDialog.getByRole("button", { name: "Create & get code" }).click();

  // ---- The code is ON SCREEN, with expiry and the honest no-tick note. -----
  const codeDialog = page.getByRole("dialog", { name: `Pairing code — ${SCREEN_NAME}` });
  await expect(codeDialog).toBeVisible();
  const code1 = (await codeDialog.getByTestId("pairing-code").textContent())?.trim() ?? "";
  expect(code1, "a pairing code is displayed").toMatch(/^[A-Z2-7]{5}(-[A-Z2-7]{1,5})+$/);
  await expect(codeDialog.getByText(/expires at/i)).toBeVisible();
  await expect(codeDialog.getByText(/not yet told when a code is redeemed/i)).toBeVisible();

  // ---- The row truly exists via the API, and the UI's claim matches. -------
  const screensRes = await authed.get("/api/v1/screens");
  expect(screensRes.ok(), `list screens: ${screensRes.status()}`).toBeTruthy();
  const screens = ((await screensRes.json()) as { items?: Array<{ id?: string; name?: string }> }).items ?? [];
  const row = screens.find((s) => s.name === SCREEN_NAME);
  expect(row?.id, "the created screen row exists in /api/v1/screens").toBeTruthy();

  await codeDialog.getByRole("button", { name: "Done" }).click();
  const table = page.getByRole("table", { name: "Displays" });
  await expect(table.getByText(SCREEN_NAME)).toBeVisible();

  // ---- Re-issue: a fresh one-time grant, so a fresh (different) code. ------
  const rowLocator = table.getByRole("row").filter({ hasText: SCREEN_NAME });
  await rowLocator.getByRole("button", { name: "Pairing code" }).click();
  await expect(codeDialog).toBeVisible();
  const code2 = (await codeDialog.getByTestId("pairing-code").textContent())?.trim() ?? "";
  expect(code2).toMatch(/^[A-Z2-7]{5}(-[A-Z2-7]{1,5})+$/);
  expect(code2, "each issuance mints a fresh one-time grant").not.toBe(code1);
  await codeDialog.getByRole("button", { name: "Done" }).click();

  // ---- Hand both codes (and the row id) to the player-side probe. ----------
  mkdirSync(DEV_DIR, { recursive: true });
  writeFileSync(resolve(DEV_DIR, "pairing-clickthrough.json"), JSON.stringify({
    screen_row_id: row!.id,
    screen_name: SCREEN_NAME,
    code1,
    code2,
  }, null, 2));
});
