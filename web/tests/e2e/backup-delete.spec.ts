import { test, expect, signIn, devDir } from "./support/console-session";
import { existsSync, readdirSync } from "node:fs";
import { resolve } from "node:path";

/**
 * The backup click-through — the destructive one.
 *
 * This gate exists because a delete is the one control on the console that
 * cannot be taken back, and because the shape it closes is a control that
 * RENDERS and does nothing. A render-only test cannot tell a Delete button that
 * unlinks a container from one that paints a confirmation and drops the click on
 * the floor; both look identical in the DOM, and only one of them frees the disk
 * the operator opened this page about.
 *
 * So the spec proves the whole chain on the REAL stack, and it checks the
 * FILESYSTEM at each end rather than the API's own account of it: a container is
 * exported through the console's own Export button, its bytes are confirmed
 * present in the feeder's archive directory, it is deleted through the console's
 * own confirmation dialog, and its bytes are confirmed GONE from that directory.
 *
 * It also drives the refusal that costs nothing to be sure of: dismissing the
 * confirmation must leave the backup exactly where it was.
 *
 * It deliberately does NOT drive the ARCHIVE_IN_USE refusal. That guard fires
 * only while a job holds the container, and on a live box the window is however
 * long a restore takes — so an e2e racing it would pass whether or not the guard
 * exists, which is the one kind of test this repo has shipped too many of. The
 * refusal is pinned deterministically where the window can be held open: the Go
 * suite drives it against a stopped job runner
 * (internal/app/api/workspacearchivedelete_test.go) and the console's rendering
 * of it is driven against a mocked 409 (backup-route.test.tsx).
 */

/** The feeder's archive destination under `make dev-up` (RUNDIR/feeder-archive,
 * cmd/waiveo-feeder's defaultArchiveDir relative to the run dir). Read directly
 * so "gone" means gone from the disk, not merely gone from a listing. */
const ARCHIVE_DIR = () => resolve(devDir(), "feeder-archive");

const PASSPHRASE = "e2e-archive-passphrase-1";

/** The container files actually on the box, straight off the filesystem. */
function containersOnDisk(): string[] {
  const dir = ARCHIVE_DIR();
  // An absent archive dir under a PRESENT run dir is the real "nothing exported
  // yet" — distinct from the run dir itself being missing, which devDir() has
  // already refused above with the reason.
  if (!existsSync(dir)) return [];
  return readdirSync(dir).filter((f) => f.endsWith(".waiveo-archive"));
}

test.describe("Backup — deleting a container", () => {
  // An export runs a memory-hard KDF over the whole workspace, then the delete
  // drives two dialogs. Generous, and still bounded: a page that hangs fails.
  test.setTimeout(120_000);

  test("exports a real container, then deletes it from the console — gone from the list AND from disk", async ({
    page,
  }) => {
    await signIn(page);
    await page.goto("/backup");
    await expect(page.getByRole("heading", { level: 1, name: "Backup" })).toBeVisible();

    const before = containersOnDisk();

    // ── Export, through the page's own control ──────────────────────────────
    await page.getByLabel(/^Passphrase/).fill(PASSPHRASE);
    await page.getByLabel(/^Confirm passphrase/).fill(PASSPHRASE);
    await page.getByRole("button", { name: /export this workspace/i }).click();
    await expect(page.getByText(/Export complete/)).toBeVisible({ timeout: 90_000 });

    // The bytes exist. Asserted against the filesystem, because the whole point
    // of the delete below is what it does to the disk.
    const after = containersOnDisk();
    const created = after.filter((f) => !before.includes(f));
    expect(created, `an export produced no container in ${ARCHIVE_DIR()}`).toHaveLength(1);
    const name = created[0]!;

    const table = page.getByRole("table", { name: /backups on this box/i });
    await expect(table.getByText(name)).toBeVisible();

    // ── The confirmation NAMES what is about to be lost ─────────────────────
    await table.getByText(name).click();
    await page.getByRole("button", { name: /delete this backup/i }).click();
    const dialog = page.getByRole("dialog");
    await expect(dialog).toBeVisible();
    await expect(dialog.getByText(name)).toBeVisible();
    await expect(dialog.getByText(/nobody at all, can bring these bytes back/i)).toBeVisible();

    // ── Dismissing it deletes NOTHING ───────────────────────────────────────
    await dialog.getByRole("button", { name: /keep it/i }).click();
    await expect(dialog).toBeHidden();
    expect(containersOnDisk(), "the container was deleted by a DISMISSED confirmation").toContain(name);
    await expect(table.getByText(name)).toBeVisible();

    // ── Confirming it deletes the container ─────────────────────────────────
    await page.getByRole("button", { name: /delete this backup/i }).click();
    await page.getByRole("dialog").getByRole("button", { name: /delete for good/i }).click();

    // Gone from the BOX. This is the assertion the whole feature is for: a
    // listing that stops mentioning a file has reclaimed no disk at all.
    //
    // Polled rather than read once, because the unlink is a round trip and the
    // click returns before it lands — the first version of this read the
    // directory immediately and passed only when the request happened to win.
    await expect
      .poll(() => containersOnDisk(), {
        timeout: 15_000,
        message: `${name} is still in ${ARCHIVE_DIR()} after a confirmed delete`,
      })
      .not.toContain(name);

    // …and gone from the page. Scoped to the TABLE: the container's name is on
    // screen in three places (the row, the detail pane, and the restore
    // picker's option), so an unscoped locator is a strict-mode violation that
    // throws instead of retrying — which is a test that fails on timing rather
    // than on the thing it is asserting.
    await expect(table.getByText(name)).toBeHidden();
  });
});
