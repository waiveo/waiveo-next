import { describe, it, expect, beforeAll, afterAll, afterEach } from "vitest";
import { render, screen, within, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";
import BackupRoute, { formatBytes } from "./backup-route";
import { TRACE_ID, problem } from "@/api/test-support";
import type { Job, WorkspaceArchive } from "@/api";

// The Backup route is parity row 7.5. These tests drive the REAL page against a
// mocked feeder (msw) and hold it to the three things a backup UI can most
// easily get wrong:
//
//   - claiming an async operation finished when it has not;
//   - claiming a RESTORE finished, when what happened is that it was staged and
//     the box has to restart;
//   - offering a download the operator cannot actually take.

const server = setupServer();
beforeAll(() => server.listen({ onUnhandledRequest: "error" }));
afterEach(() => server.resetHandlers());
afterAll(() => server.close());

const JOB_ID = "01J8ZJOB000000000000000001";

function archive(over: Partial<WorkspaceArchive> = {}): WorkspaceArchive {
  const name = over.name ?? "workspace-01J8ZJOB000000000000000001.waiveo-archive";
  return {
    name,
    size_bytes: 4 * 1024 * 1024,
    created_at_ms: 1_752_537_600_000,
    download_path: `/api/v1/workspace/archives/${name}`,
    etag: `"4194304-1752537600000"`,
    ...over,
  };
}

function job(state: Job["state"], over: Partial<Job> = {}): Job {
  return {
    id: JOB_ID,
    state,
    created_by: "principal-a",
    created_at: "2026-07-15T00:00:00Z",
    targets: [{ target_id: "01J8ZORG00000000000000001", state: state === "failed" ? "failed" : "succeeded" }],
    ...over,
  };
}

/** Wire the four operations. `jobStates` is consumed one poll at a time, so a
 * test can prove the page actually POLLS rather than reading once. */
function serve(opts: {
  archives?: WorkspaceArchive[];
  directory?: string;
  jobStates?: Job[];
  onExport?: (body: unknown) => void;
  onRestore?: (body: unknown) => void;
}) {
  const states = [...(opts.jobStates ?? [job("succeeded")])];
  server.use(
    http.get("*/api/v1/workspace/archives", () =>
      HttpResponse.json(
        { items: opts.archives ?? [], directory: opts.directory ?? "/var/lib/waiveo/archive" },
        { headers: { "Trace-Id": TRACE_ID } },
      ),
    ),
    http.post("*/api/v1/workspace/export", async ({ request }) => {
      opts.onExport?.(await request.json());
      return HttpResponse.json(job("pending"), { status: 202, headers: { "Trace-Id": TRACE_ID } });
    }),
    http.post("*/api/v1/workspace/restore", async ({ request }) => {
      opts.onRestore?.(await request.json());
      return HttpResponse.json(job("pending"), { status: 202, headers: { "Trace-Id": TRACE_ID } });
    }),
    http.get("*/api/v1/jobs/:id", () => {
      const next = states.length > 1 ? states.shift()! : states[0]!;
      return HttpResponse.json(next, { headers: { "Trace-Id": TRACE_ID } });
    }),
  );
}

describe("BackupRoute — export", () => {
  it("refuses to arm until the passphrase is long enough and confirmed", async () => {
    serve({});
    render(<BackupRoute />);
    const button = screen.getByRole("button", { name: /export this workspace/i });
    expect(button).toBeDisabled();

    await userEvent.type(screen.getByLabelText(/^Passphrase/), "short");
    expect(button).toBeDisabled();
    // The field marks itself invalid rather than only the button going grey —
    // a disabled button with no explanation is a dead end.
    expect(screen.getByLabelText(/^Passphrase/)).toHaveAttribute("aria-invalid", "true");

    await userEvent.clear(screen.getByLabelText(/^Passphrase/));
    await userEvent.type(screen.getByLabelText(/^Passphrase/), "correct horse battery");
    // Long enough, but not confirmed.
    expect(button).toBeDisabled();

    await userEvent.type(screen.getByLabelText(/^Confirm passphrase/), "correct horse batteryX");
    expect(screen.getByText(/do not match/i)).toBeInTheDocument();
    expect(button).toBeDisabled();

    await userEvent.clear(screen.getByLabelText(/^Confirm passphrase/));
    await userEvent.type(screen.getByLabelText(/^Confirm passphrase/), "correct horse battery");
    await waitFor(() => expect(button).toBeEnabled());
  });

  it("says the box keeps no copy of the passphrase, before anything is exported", async () => {
    // The single most consequential fact on the page: a lost passphrase is a
    // lost backup, and the operator has to know that at the moment they choose
    // one — not after.
    serve({});
    render(<BackupRoute />);
    expect(screen.getByText(/This box keeps no copy of it/)).toBeInTheDocument();
  });

  it("sends the passphrase, POLLS the job to a terminal state, and refreshes the list", async () => {
    const sent: unknown[] = [];
    let listReads = 0;
    server.use(
      http.get("*/api/v1/workspace/archives", () => {
        listReads += 1;
        return HttpResponse.json(
          { items: listReads > 1 ? [archive()] : [], directory: "/var/lib/waiveo/archive" },
          { headers: { "Trace-Id": TRACE_ID } },
        );
      }),
      http.post("*/api/v1/workspace/export", async ({ request }) => {
        sent.push(await request.json());
        return HttpResponse.json(job("pending"), { status: 202, headers: { "Trace-Id": TRACE_ID } });
      }),
    );
    const states = [job("running"), job("succeeded")];
    server.use(
      http.get("*/api/v1/jobs/:id", () =>
        HttpResponse.json(states.length > 1 ? states.shift()! : states[0]!, {
          headers: { "Trace-Id": TRACE_ID },
        }),
      ),
    );

    render(<BackupRoute />);
    await userEvent.type(screen.getByLabelText(/^Passphrase/), "correct horse battery");
    await userEvent.type(screen.getByLabelText(/^Confirm passphrase/), "correct horse battery");
    await userEvent.click(screen.getByRole("button", { name: /export this workspace/i }));

    await waitFor(() => expect(screen.getByText(/Export complete/)).toBeInTheDocument(), {
      timeout: 5000,
    });
    expect(sent).toEqual([{ passphrase: "correct horse battery" }]);
    // The list was re-read, so the container the operator just made is on screen
    // and downloadable without a manual refresh.
    const table = await screen.findByRole("table", { name: /backups on this box/i });
    await userEvent.click(within(table).getByText(/^workspace-/));
    await waitFor(() =>
      expect(screen.getByRole("link", { name: /download workspace-/i })).toBeInTheDocument(),
    );
    // And it tells them the job is not the point — getting the file OFF the box is.
    expect(screen.getByText(/keep it off this box/i)).toBeInTheDocument();
  });

  it("reports a FAILED job's per-target detail rather than a stuck spinner", async () => {
    serve({
      jobStates: [
        job("failed", {
          targets: [
            {
              target_id: "01J8ZORG00000000000000001",
              state: "failed",
              error: { code: "UNAVAILABLE", detail: "This workspace's signing key has been destroyed." },
            },
          ],
        }),
      ],
    });
    render(<BackupRoute />);
    await userEvent.type(screen.getByLabelText(/^Passphrase/), "correct horse battery");
    await userEvent.type(screen.getByLabelText(/^Confirm passphrase/), "correct horse battery");
    await userEvent.click(screen.getByRole("button", { name: /export this workspace/i }));

    await waitFor(() =>
      expect(screen.getByText(/signing key has been destroyed/)).toBeInTheDocument(),
    );
  });

  it("surfaces a synchronous refusal with its trace id", async () => {
    server.use(
      http.get("*/api/v1/workspace/archives", () =>
        HttpResponse.json({ items: [], directory: "" }, { headers: { "Trace-Id": TRACE_ID } }),
      ),
      http.post("*/api/v1/workspace/export", () =>
        problem(403, "FORBIDDEN", "This principal is not an owner of this workspace."),
      ),
    );
    render(<BackupRoute />);
    await userEvent.type(screen.getByLabelText(/^Passphrase/), "correct horse battery");
    await userEvent.type(screen.getByLabelText(/^Confirm passphrase/), "correct horse battery");
    await userEvent.click(screen.getByRole("button", { name: /export this workspace/i }));

    await waitFor(() =>
      expect(screen.getByText(/not an owner of this workspace/)).toBeInTheDocument(),
    );
    expect(screen.getByText(new RegExp(TRACE_ID))).toBeInTheDocument();
  });
});

describe("BackupRoute — the containers on this box", () => {
  it("lists each container with a real download link the server published", async () => {
    const a = archive();
    serve({ archives: [a] });
    render(<BackupRoute />);

    const table = await screen.findByRole("table", { name: /backups on this box/i });
    expect(within(table).getByText(a.name)).toBeInTheDocument();
    expect(within(table).getByText("4.0 MiB")).toBeInTheDocument();

    await userEvent.click(within(table).getByText(a.name));
    const link = await screen.findByRole("link", { name: `Download ${a.name}` });
    // The href is the server's own published path — a client composing it would
    // be re-encoding a file name into a URL, which is the one part of this
    // family with a traversal question attached.
    expect(link).toHaveAttribute("href", a.download_path);
    expect(link).toHaveAttribute("download", a.name);
  });

  it("names the directory, because copying a container BACK is the real recovery path", async () => {
    serve({ archives: [], directory: "/var/lib/waiveo/archive" });
    render(<BackupRoute />);
    await waitFor(() => expect(screen.getByText("/var/lib/waiveo/archive")).toBeInTheDocument());
    expect(screen.getByText(/No backups yet/)).toBeInTheDocument();
  });
});

// ── Delete ───────────────────────────────────────────────────────────────────
//
// The page's only irreversible control. Every case here asserts what the page
// SAYS as well as what it sent, because on an operation nobody can undo the
// sentence is half the product: an operator who dismisses a vague confirmation
// out of habit and destroys the wrong backup has been failed by the page, not by
// themselves.

/** Select a container in the list and return its detail panel's Delete button. */
async function openArchive(name: string) {
  const table = await screen.findByRole("table", { name: /backups on this box/i });
  await userEvent.click(within(table).getByText(name));
  return screen.findByRole("button", { name: /delete this backup/i });
}

describe("BackupRoute — deleting a backup", () => {
  it("names the container, its size and its date in the confirmation, and says the bytes cannot come back", async () => {
    const a = archive();
    serve({ archives: [a] });
    render(<BackupRoute />);

    await userEvent.click(await openArchive(a.name));

    const dialog = await screen.findByRole("dialog");
    // The container is NAMED. A confirmation that describes a class rather than
    // a thing is one an operator agrees to without reading.
    expect(within(dialog).getByText(new RegExp(a.name))).toBeInTheDocument();
    expect(within(dialog).getByText(/4\.0 MiB/)).toBeInTheDocument();
    // And it says plainly that this is not recoverable, and what to do instead.
    expect(within(dialog).getByText(/nobody at all, can bring these bytes back/i)).toBeInTheDocument();
    expect(within(dialog).getByText(/download it first/i)).toBeInTheDocument();
  });

  it("deletes NOTHING when the confirmation is dismissed", async () => {
    const a = archive();
    let deletes = 0;
    serve({ archives: [a] });
    server.use(
      http.delete("*/api/v1/workspace/archives/:name", () => {
        deletes += 1;
        return new HttpResponse(null, { status: 204 });
      }),
    );
    render(<BackupRoute />);

    await userEvent.click(await openArchive(a.name));
    await userEvent.click(await screen.findByRole("button", { name: /keep it/i }));

    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(deletes).toBe(0);
    expect(within(await screen.findByRole("table", { name: /backups on this box/i })).getByText(a.name))
      .toBeInTheDocument();
  });

  it("sends the container's OWN etag as If-Match, and the row is gone afterwards", async () => {
    const a = archive();
    let remaining = [a];
    const seen: { path: string; ifMatch: string | null }[] = [];
    serve({ archives: [] });
    server.use(
      http.get("*/api/v1/workspace/archives", () =>
        HttpResponse.json({ items: remaining, directory: "/var/lib/waiveo/archive" }),
      ),
      http.delete("*/api/v1/workspace/archives/:name", ({ request, params }) => {
        seen.push({ path: String(params.name), ifMatch: request.headers.get("If-Match") });
        remaining = [];
        return new HttpResponse(null, { status: 204 });
      }),
    );
    render(<BackupRoute />);

    await userEvent.click(await openArchive(a.name));
    await userEvent.click(await screen.findByRole("button", { name: /delete for good/i }));

    await waitFor(() => expect(seen).toHaveLength(1));
    expect(seen[0]!.path).toBe(a.name);
    // Verbatim — a client that composed its own validator would be asserting
    // something about bytes it has not seen.
    expect(seen[0]!.ifMatch).toBe(a.etag);
    // And the list is re-read, so the operator sees the box as it now is.
    await waitFor(() => expect(screen.getByText(/No backups yet/)).toBeInTheDocument());
  });

  it("drops the deleted row from the table and keeps the others", async () => {
    // The case the "no backups yet" assertion above cannot make. That empty
    // state is the host's own branch on `archives.length`, so it appears whether
    // or not the RENDERER was given the refreshed rows — and the renderer seeds
    // its store once, at mount. With two containers, a delete that does not
    // remount leaves the destroyed one sitting in the table, still selectable,
    // still offering a Delete button for bytes that are gone.
    const keep = archive({ name: "workspace-KEEP.waiveo-archive" });
    const drop = archive({ name: "workspace-DROP.waiveo-archive" });
    let remaining = [drop, keep];
    serve({ archives: [] });
    server.use(
      http.get("*/api/v1/workspace/archives", () =>
        HttpResponse.json({ items: remaining, directory: "/var/lib/waiveo/archive" }),
      ),
      http.delete("*/api/v1/workspace/archives/:name", ({ params }) => {
        remaining = remaining.filter((a) => a.name !== String(params.name));
        return new HttpResponse(null, { status: 204 });
      }),
    );
    render(<BackupRoute />);

    await userEvent.click(await openArchive(drop.name));
    await userEvent.click(await screen.findByRole("button", { name: /delete for good/i }));

    const table = await screen.findByRole("table", { name: /backups on this box/i });
    await waitFor(() => expect(within(table).queryByText(drop.name)).not.toBeInTheDocument());
    expect(within(screen.getByRole("table", { name: /backups on this box/i })).getByText(keep.name))
      .toBeInTheDocument();
  });

  it("disarms the button while the delete is in flight, so a second click cannot land", async () => {
    const a = archive();
    let deletes = 0;
    let release!: () => void;
    const held = new Promise<void>((r) => {
      release = r;
    });
    serve({ archives: [a] });
    server.use(
      http.delete("*/api/v1/workspace/archives/:name", async () => {
        deletes += 1;
        await held;
        return new HttpResponse(null, { status: 204 });
      }),
    );
    render(<BackupRoute />);

    const button = await openArchive(a.name);
    await userEvent.click(button);
    await userEvent.click(await screen.findByRole("button", { name: /delete for good/i }));

    // In flight: the control is present and un-pressable (UIS-076 bound to the
    // outcome's own `pending`), and it SAYS what is happening rather than going
    // silently grey.
    await waitFor(() => expect(screen.getByRole("button", { name: /delete this backup/i })).toBeDisabled());
    expect(screen.getByText(/Deleting…/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: /delete this backup/i }));
    expect(deletes).toBe(1);

    release();
    await waitFor(() => expect(deletes).toBe(1));
  });

  it("renders a REFUSAL as its own sentence and leaves the backup on the page", async () => {
    const a = archive();
    serve({ archives: [a] });
    server.use(
      http.delete("*/api/v1/workspace/archives/:name", () =>
        problem(
          409,
          "ARCHIVE_IN_USE",
          "A restore accepted as job 01J8ZJOB000000000000000009 is still reading this container.",
        ),
      ),
    );
    render(<BackupRoute />);

    await userEvent.click(await openArchive(a.name));
    await userEvent.click(await screen.findByRole("button", { name: /delete for good/i }));

    // The code's own sentence, not a generic failure — "a job is using it" and
    // "you are not the owner" send an operator to different places.
    const alert = await screen.findByText(/being used by a job that is still running/i);
    expect(alert).toBeInTheDocument();
    expect(alert).toHaveAttribute("role", "alert");
    expect(screen.getByText(/01J8ZJOB000000000000000009/)).toBeInTheDocument();
    expect(screen.getByText(new RegExp(TRACE_ID))).toBeInTheDocument();
    // The backup survived the refusal, and is still on screen with it.
    expect(within(await screen.findByRole("table", { name: /backups on this box/i })).getByText(a.name))
      .toBeInTheDocument();
    expect(screen.getByRole("button", { name: /delete this backup/i })).toBeInTheDocument();
  });

  it("says a stale etag means the bytes changed, not that the delete failed", async () => {
    const a = archive();
    serve({ archives: [a] });
    server.use(
      http.delete("*/api/v1/workspace/archives/:name", () =>
        problem(412, "REVISION_CONFLICT", "mismatch"),
      ),
    );
    render(<BackupRoute />);

    await userEvent.click(await openArchive(a.name));
    await userEvent.click(await screen.findByRole("button", { name: /delete for good/i }));

    expect(await screen.findByText(/may not be the one you were looking at/i)).toBeInTheDocument();
  });

  it("warns when the container is the most recent one on the box", async () => {
    const newest = archive({ name: "workspace-NEW.waiveo-archive" });
    const older = archive({ name: "workspace-OLD.waiveo-archive", created_at_ms: 1_752_000_000_000 });
    serve({ archives: [newest, older] });
    render(<BackupRoute />);

    await openArchive(newest.name);
    expect(screen.getByText(/most recent backup on this box/i)).toBeInTheDocument();

    // …and does NOT cry wolf on the older one. A warning that appears on every
    // row is a warning nobody reads by the third time they see it.
    await openArchive(older.name);
    await waitFor(() =>
      expect(screen.queryByText(/most recent backup on this box/i)).not.toBeInTheDocument(),
    );
  });
});

describe("BackupRoute — restore", () => {
  it("will not arm without both a container and its passphrase", async () => {
    serve({ archives: [archive()] });
    render(<BackupRoute />);
    const button = screen.getByRole("button", { name: /stage a restore/i });
    expect(button).toBeDisabled();

    await screen.findByRole("table", { name: /backups on this box/i });
    await userEvent.selectOptions(screen.getByLabelText(/^Container/), archive().name);
    expect(button).toBeDisabled();

    await userEvent.type(screen.getByLabelText(/^Its passphrase/), "correct horse battery");
    await waitFor(() => expect(button).toBeEnabled());
  });

  it("says STAGED and RESTART REQUIRED — never 'restored'", async () => {
    // The load-bearing honesty claim of this whole page. The swap happens at the
    // next boot; a page that said "restored" would tell an operator their data
    // had moved while this process is still serving the old store.
    const sent: unknown[] = [];
    serve({ archives: [archive()], onRestore: (b) => sent.push(b) });
    render(<BackupRoute />);
    await screen.findByRole("table", { name: /backups on this box/i });
    await userEvent.selectOptions(screen.getByLabelText(/^Container/), archive().name);
    await userEvent.type(screen.getByLabelText(/^Its passphrase/), "correct horse battery");
    await userEvent.click(screen.getByRole("button", { name: /stage a restore/i }));

    const banner = await screen.findByText(/^Staged\./);
    expect(banner).toHaveTextContent(/Restart this box to finish the restore/);
    expect(screen.getByText(/Restart required/i)).toBeInTheDocument();
    // The success message must not claim the restore HAPPENED. Scoped to the
    // banner, because the page legitimately uses the word elsewhere ("can be
    // restored below by name") and an unscoped query would pass or fail for the
    // wrong reason.
    expect(banner).not.toHaveTextContent(/\brestored\b/i);
    expect(banner).not.toHaveTextContent(/\bcomplete\b/i);
    expect(sent).toEqual([{ archive: archive().name, passphrase: "correct horse battery" }]);
  });

  it("warns that a restore replaces the whole workspace before it is run", async () => {
    serve({ archives: [archive()] });
    render(<BackupRoute />);
    expect(screen.getByText(/replaces this entire workspace/)).toBeInTheDocument();
  });

  it("reports a failed restore and says nothing was changed", async () => {
    serve({
      archives: [archive()],
      jobStates: [
        job("failed", {
          targets: [
            {
              target_id: "01J8ZORG00000000000000001",
              state: "failed",
              error: { code: "VALIDATION_FAILED", detail: "DECRYPT_FAILED: the passphrase did not open this container." },
            },
          ],
        }),
      ],
    });
    render(<BackupRoute />);
    await screen.findByRole("table", { name: /backups on this box/i });
    await userEvent.selectOptions(screen.getByLabelText(/^Container/), archive().name);
    await userEvent.type(screen.getByLabelText(/^Its passphrase/), "wrong passphrase");
    await userEvent.click(screen.getByRole("button", { name: /stage a restore/i }));

    await waitFor(() => expect(screen.getByText(/DECRYPT_FAILED/)).toBeInTheDocument());
    expect(screen.queryByText(/Restart required/i)).not.toBeInTheDocument();
  });
});

describe("BackupRoute — formatBytes", () => {
  it("reads in the largest unit that stays legible", () => {
    expect(formatBytes(0)).toBe("0 B");
    expect(formatBytes(4 * 1024 * 1024)).toBe("4.0 MiB");
    expect(formatBytes(12 * 1024 ** 3)).toBe("12 GiB");
    expect(formatBytes(-1)).toBe("—");
  });
});
