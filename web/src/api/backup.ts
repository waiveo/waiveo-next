// api/1 — the WORKSPACE BACKUP surface: export, the containers that exist,
// downloading one, and restoring from one.
//
// Four operations that are one operator job (parity row 7.5), which is why they
// are one module rather than being scattered across the resource families they
// happen to back up. None of them is a resource: there is no id, no revision, no
// ETag, and the subject of all four is the workspace as a whole.
//
// # Two of them are ASYNC, and this module does not pretend otherwise
//
// `export` and `restore` answer `202 Accepted` with a Job (API-123): neither a
// full workspace export nor a container verification completes inside a
// request. So both return the Job, and a page polls `jobs.get` until the state
// is terminal. A module that resolved only when the work finished would be
// hiding a multi-minute operation behind a promise, and a page built on it
// could not show progress or survive a reload.
//
// # A restore's success does NOT mean "your workspace is now the archived one"
//
// The restore stages the archived store beside the live one; the swap happens
// at the next boot (archive/1's offline swap, which is what makes its rollback
// guarantee hold without a rollback step). So `restore`'s Job succeeding means
// STAGED AND READY, and the console must say "restart to finish" rather than
// "restored". Reporting otherwise would tell an operator their data had moved
// while the process was still serving the old store.
//
// # The download is a browser navigation, not a fetch
//
// `downloadUrl` returns the URL rather than the bytes. A backup is as large as
// the workspace; pulling it through `fetch` into a Blob to hand to an anchor
// would hold the entire thing in the tab's memory for no benefit, when the
// browser can stream it straight to disk from a plain link. The session cookie
// rides a same-origin navigation exactly as it rides a fetch.

import { ApiClient } from "./client";
import type { components } from "../../../api/gen/ts/api";

/** The async Job an export or a restore is tracked by (API-111/112). */
export type Job = components["schemas"]["Job"];
/** One container present in this deployment's archive directory. */
export type WorkspaceArchive = components["schemas"]["WorkspaceArchive"];
/** The archive listing, plus the directory the containers live in. */
export type WorkspaceArchiveList = components["schemas"]["WorkspaceArchiveList"];

/** A Job state that will not change again. A poller stops here. */
export const TERMINAL_JOB_STATES = ["succeeded", "failed", "partial"] as const;

/** Whether a Job has finished, whatever the outcome. */
export function jobIsTerminal(job: Job): boolean {
  return (TERMINAL_JOB_STATES as readonly string[]).includes(job.state);
}

/** The first per-target failure on a Job, as a sentence — what a page shows
 * when an export or a restore did not work. A Job can carry several targets in
 * general; these two carry exactly one (the workspace itself, API-123), so the
 * first failure IS the failure. */
export function jobFailureDetail(job: Job): string | null {
  for (const t of job.targets ?? []) {
    if (t.state === "failed") {
      return t.error?.detail ?? t.error?.code ?? "The operation failed.";
    }
  }
  return null;
}

export interface JobsModule {
  /** Poll one Job (API-112 — the only completion signal an async operation
   * gives). */
  get(jobId: string): Promise<Job>;
}

export function createJobsModule(client: ApiClient): JobsModule {
  return {
    async get(jobId) {
      const { data } = await client.read<Job>(`/jobs/${encodeURIComponent(jobId)}`);
      return data;
    },
  };
}

export interface BackupModule {
  /** Start an export. The passphrase encrypts the container (`archive/1`
   * ARC-010) and is the ONLY way to read it back — the server keeps no copy,
   * deliberately, so a lost passphrase is a lost backup and the UI must say so.
   * Returns the Job to poll. */
  export(passphrase: string): Promise<Job>;
  /** The containers on this box, newest first. */
  archives(): Promise<WorkspaceArchiveList>;
  /** The URL a container's bytes are served from — for an anchor, not a fetch. */
  downloadUrl(archive: WorkspaceArchive): string;
  /** Start a restore from a named container. Returns the Job to poll; its
   * success means STAGED, and the swap happens at the next boot. */
  restore(archive: string, passphrase: string): Promise<Job>;
}

export function createBackupModule(client: ApiClient): BackupModule {
  return {
    export(passphrase) {
      // `action` carries the Idempotency-Key every mutating POST on this surface
      // carries, so a retry-on-timeout replays the original 202 rather than
      // starting a second export of the same workspace under a second Job.
      return client.action<Job>("/workspace/export", { passphrase });
    },
    async archives() {
      const { data } = await client.read<WorkspaceArchiveList>("/workspace/archives");
      return data;
    },
    downloadUrl(archive) {
      // The server publishes the path; the client does not compose one. A client
      // that built it would be re-encoding a file name into a URL path, which is
      // the one part of this family with a traversal question attached.
      return archive.download_path;
    },
    restore(archive, passphrase) {
      return client.action<Job>("/workspace/restore", { archive, passphrase });
    },
  };
}
