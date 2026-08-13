import {
  expect,
  request as pwRequest,
  test as base,
  type APIRequestContext,
  type Browser,
  type Page,
} from "@playwright/test";
import { existsSync, readFileSync } from "node:fs";
import { resolve } from "node:path";

/**
 * The e2e suite's shared bootstrap: getting a real owner credential onto the box,
 * and signing in with it.
 *
 * Every spec in this directory needs the same two things before it can drive
 * anything. The box must be CLAIMED — a fresh deployment has no owner at all
 * (security-model/1 SEC-120), so there is no credential to sign in with until
 * something redeems the first-boot setup grant. And the browser must hold a
 * session — every api/1 route is authenticated (SEC-005), so the entire console
 * sits behind the SessionGate and an anonymous `page.goto` of any console route
 * lands on /login, not on the page under test.
 *
 * This lives here rather than in a spec because the claim is a property of the
 * BOX, not of a test file. Whichever spec runs first has to claim it and the
 * others must not care, in either order and against a box in either state. A copy
 * per spec would also be two copies of a precondition that has already been wrong
 * once — see ensureOwnerCredential's note on what the code file does and does not
 * mean.
 *
 * # Why a fixture, and not a setup project or a `beforeAll`
 *
 * The claim is wired as an `auto` FIXTURE, so a bootstrap failure is reported
 * against each test that needed it. The alternatives all fail worse:
 *
 *   - A `beforeAll` (what both specs used to do) aborts the rest of the FILE on
 *     one throw: a single unauthenticated request took down one test and left
 *     three reported as "did not run", which is how three of five specs stopped
 *     executing without anything going red on their own behalf.
 *   - A Playwright setup project + saved `storageState` is the idiomatic auth
 *     pattern and is wrong twice here. Its failure marks every dependent test as
 *     "did not run" — the same pathology one level up — and injecting a cookie
 *     jar would replace the suite's only credential-exchange click-through
 *     (screens-pairing signs in through the real form, deliberately) with the
 *     thing that click-through exists to avoid.
 *   - `globalSetup` shares the all-or-nothing failure mode and reports a claim
 *     failure as a run-level crash with no test attributed to it at all.
 *
 * Nothing here gates one spec on another: each test's own bootstrap either works
 * or fails that test.
 */

/** The feeder's self-signed HTTPS origin — the stack `make web-e2e` brings up. */
export const BASE_URL = process.env.PW_BASE_URL ?? "https://127.0.0.1:7420";

/** The repo-local run dir the Makefile's `RUNDIR` writes: the feeder's auth state
 * (including its printed setup code) and the artifacts specs hand to Go probes.
 *
 * Resolved relative to the CURRENT WORKING DIRECTORY, which is `web/` under
 * `npm --prefix web run e2e` — the only supported way in, and what `make web-e2e`
 * does. Run Playwright from the repo root instead and this silently becomes
 * `<repo>/../.dev`: a directory belonging to nothing, which exists on some
 * machines and not others.
 *
 * `devDir()` is what specs should call. It fails NAMING the problem, because the
 * failure it replaces is genuinely misleading: a spec reading an absent directory
 * gets an empty listing, and then reports "an export produced no container" —
 * a sentence that accuses the product of a bug in the export path. That cost a
 * real diagnosis session, chasing a defect in code that was working. */
export const DEV_DIR = resolve(process.cwd(), "..", ".dev");

/** DEV_DIR, asserted to be the REAL run dir. Use this anywhere a spec reads it.
 *
 * The check is for `feeder-auth/` — which `make dev-up` always creates — and not
 * for mere existence, because mere existence is exactly what fails to catch this.
 * A wrong-cwd run does not just READ the wrong path: `screens-pairing` calls
 * `mkdirSync(DEV_DIR, {recursive: true})` to drop its handoff file, so the first
 * wrong-cwd run CREATES the bogus directory and every run after it finds one
 * there. An existence check passes from then on, and the misleading empty reads
 * resume. */
export function devDir(): string {
  if (!existsSync(resolve(DEV_DIR, "feeder-auth"))) {
    throw new Error(
      `${DEV_DIR} is not the dev run dir (no feeder-auth/ inside it). Playwright must run ` +
        `with cwd=web/ — \`npm --prefix web run e2e\`, which is what \`make web-e2e\` does — ` +
        `and the stack must be up (\`make dev-up\`). Run from the repo root and this path ` +
        `resolves OUTSIDE the repo, where reads come back empty and specs blame the product ` +
        `for a bug it does not have. If a stray directory is already there, delete it.`,
    );
  }
  return DEV_DIR;
}
const SETUP_CODE_PATH = resolve(DEV_DIR, "feeder-auth", "setup-code.txt");

/** Dev-lab-only credential the suite claims the freshly-installed workspace with
 * (first run) and signs in with (every run). Not a real secret: the stack is a
 * loopback dev instance whose whole auth store is reset with .dev. */
export const OWNER_ID = "e2e-owner";
export const OWNER_PASSWORD = "e2e-owner-password-1";

/** The script-readable half of the double-submit CSRF pair (SEC-024). Name matches
 * the server's own constant (internal/app/auth/middleware.go). */
const CSRF_COOKIE = "csrf_token";
const CSRF_HEADER = "X-CSRF-Token";

// Does the credential the suite signs in with already exist on the box?
//
// This is the ONLY true way to ask. Nothing publishes claim state — the setup
// route's own doc explains why that endpoint deliberately does not exist — so
// the question "has this box been claimed already, by us" has exactly one honest
// probe: present the credential and see. One failed attempt is well inside
// SEC-090's tolerated budget (five consecutive failures per credential/IP class
// before any backoff), and on the path where it fails the claim below is about
// to create the credential anyway.
async function ownerCredentialWorks(api: APIRequestContext): Promise<boolean> {
  const res = await api.post("/api/v1/auth/login", {
    data: { identifier: OWNER_ID, password: OWNER_PASSWORD },
  });
  return res.ok();
}

// Claim the box the way an operator standing at it does: on the console.
//
// This used to POST /api/v1/auth/setup directly, because the console had no
// surface for it — which meant the one step every self-hosted deployment starts
// with was the one step no test drove. It is a real click-through now: open the
// sign-in page, follow the setup link it offers every caller, fill the form with
// the code the feeder printed, and come out the other side already signed in.
//
// # What the code file does and does not mean
//
// It is NOT claim state. Nothing in the claim path touches it: the handler never
// writes disk, and the only removal is EnsureClaimWindow's claimed branch, which
// runs at PROCESS START (internal/app/auth/bootstrap.go). So a successful claim
// leaves the file exactly where it was, and its presence means "the feeder has
// not booted since the claim" — which, against a stack that is already up, is
// true immediately after this function claims the box.
//
// Reading it as "unclaimed" is what made a second `npm run e2e` against a
// still-running feeder fail every test in the suite: the form was driven with a
// spent code, the box answered GRANT_ALREADY_REDEEMED, the page stayed on /setup
// and the Overview assertion timed out. So the real question is asked of the box
// first, and the file only decides whether there is a code to present at all.
async function ensureOwnerCredential(browser: Browser): Promise<void> {
  const api = await pwRequest.newContext({ baseURL: BASE_URL, ignoreHTTPSErrors: true });
  try {
    if (await ownerCredentialWorks(api)) return; // claimed by an earlier run; sign in below
  } finally {
    await api.dispose();
  }
  if (!existsSync(SETUP_CODE_PATH)) return; // no code to redeem; the sign-in below reports why
  const code = readFileSync(SETUP_CODE_PATH, "utf8").trim();
  const context = await browser.newContext({ baseURL: BASE_URL, ignoreHTTPSErrors: true });
  const page = await context.newPage();
  try {
    await page.goto("/login");
    await page.getByRole("link", { name: "Enter your setup code" }).click();
    await expect(page.getByRole("button", { name: "Set up this box" })).toBeVisible();

    await page.getByLabel("Setup code").fill(code);
    await page.getByLabel("Identifier").fill(OWNER_ID);
    // Anchored, not exact. `getByLabel` matches the LABEL ELEMENT'S TEXT, and a
    // required field's label carries the decorative asterisk in that text — so
    // `{ exact: true }` on "Password" matches nothing at all (it is "Password*"),
    // while a bare substring match would also catch "Confirm password*". An
    // anchored pattern picks exactly one and survives either reading.
    await page.getByLabel(/^Password/).fill(OWNER_PASSWORD);
    await page.getByLabel(/^Confirm password/).fill(OWNER_PASSWORD);
    await page.getByRole("button", { name: "Set up this box" }).click();

    // Two outcomes are acceptable, and only two.
    //
    // The Overview means the claim worked: the claim minted the session itself,
    // so the console opens with no second sign-in, which is the whole point of
    // the surface. A setup form that rendered and did nothing lands on /login
    // instead and fails here — the regression this click-through exists to catch
    // is still caught.
    //
    // "Already been used" means the code on disk was spent by someone else
    // (a box claimed outside this suite, or a stale file beside a claimed store).
    // That is a legitimate state of the world, not a defect in the page, and each
    // test's own sign-in is where it gets adjudicated: a credential mismatch
    // fails that test with a sign-in error rather than taking a whole file down
    // with it. Anything else — a wrong-code refusal, a dead button, a page that
    // neither navigates nor complains — matches neither and still fails here.
    const opened = page.getByRole("heading", { level: 1, name: "Overview" });
    const alreadyClaimed = page.getByRole("alert").filter({ hasText: /already been used/i });
    await expect(opened.or(alreadyClaimed)).toBeVisible();
  } finally {
    await context.close();
  }
}

// The claim, memoized for the worker: the probe is idempotent but driving a whole
// browser context through the setup form for every test is waste, and a memoized
// REJECTION means a genuinely broken bootstrap reports the same real error on
// every test instead of five different timeouts.
//
// Playwright discards a worker after a failing test, which resets this — harmless,
// because ensureOwnerCredential asks the box its question again and a claimed box
// answers immediately. Idempotence is the property that carries it, not the cache.
let claim: Promise<void> | undefined;

/**
 * `test` for every spec in this suite: the base runner plus an automatic fixture
 * that guarantees an owner credential exists on the box before the test body runs.
 * No spec declares it and no spec may skip it — the bootstrap is unconditional,
 * and each test that needs it either gets it or fails on its own behalf.
 */
export const test = base.extend<{ ownerCredential: void }>({
  ownerCredential: [
    async ({ browser }, use) => {
      claim ??= ensureOwnerCredential(browser);
      await claim;
      await use();
    },
    { auto: true },
  ],
});

export { expect };

/**
 * Sign in through the REAL login form — no cookie injection — and hand back the
 * page's own request context, whose cookie jar is the session just established.
 * That context is how a spec cross-checks a UI claim against the live API as the
 * very principal the UI is acting as.
 */
export async function signIn(page: Page): Promise<APIRequestContext> {
  await page.goto("/login");
  await page.getByLabel("Identifier").fill(OWNER_ID);
  await page.getByLabel("Password").fill(OWNER_PASSWORD);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("heading", { level: 1, name: "Overview" })).toBeVisible();
  return page.request;
}

/**
 * The double-submit CSRF header a MUTATING request over the browser session has
 * to echo (SEC-024).
 *
 * `page.request` shares the browser context's cookie jar, so the session rides
 * along on its own and a GET needs nothing more. It does NOT run the console's api
 * client, though, and that client is what normally reads the companion
 * script-readable `csrf_token` cookie and echoes it into the header. A
 * cookie-authenticated POST/PATCH/DELETE without it is refused 403 by design, so a
 * spec that mutates over `page.request` performs that echo itself.
 */
export async function csrfHeader(page: Page): Promise<Record<string, string>> {
  const cookies = await page.context().cookies();
  const token = cookies.find((c) => c.name === CSRF_COOKIE)?.value;
  expect(token, `the signed-in session set its companion ${CSRF_COOKIE} cookie`).toBeTruthy();
  return { [CSRF_HEADER]: token! };
}
