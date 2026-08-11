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

    const link = screen.getByRole("link", { name: `Download ${a.name}` });
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
