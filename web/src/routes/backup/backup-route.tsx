import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Download, Upload } from "lucide-react";
import {
  Button,
  DownloadLink,
  FormField,
  PageHeader,
  StatusBadge,
  toast,
} from "@/components/kit";
import { PageRenderer, type ActionHandler } from "@/renderer";
import {
  ApiError,
  createApi,
  jobFailureDetail,
  jobIsTerminal,
  type Job,
  type WaiveoApi,
  type WorkspaceArchive,
} from "@/api";
import archivesDoc from "./archives.uis.json";

/**
 * The Backup route ("/backup") — parity row 7.5. A daily driver with no backup
 * is an operational risk, and until this page the platform's backup existed only
 * as three API operations nobody could reach: a console could not start an
 * export, could not learn the name of a container to restore, and could not get
 * a single byte off the box.
 *
 * ── What this page is careful about ─────────────────────────────────────────
 *
 * THE PASSPHRASE IS THE BACKUP. The container is encrypted under it (archive/1
 * ARC-010) and the server keeps no copy — deliberately, since a server-held
 * passphrase makes the encryption decorative. So a lost passphrase is a lost
 * backup, the page says exactly that before the export runs, and it makes the
 * operator type it twice.
 *
 * A RESTORE DOES NOT FINISH HERE. The archived store is STAGED beside the live
 * one and the swap happens at the next boot — that is what makes archive/1's
 * rollback guarantee hold without a rollback step. So the success message is
 * "staged — restart the box to finish", never "restored". Saying otherwise would
 * tell an operator their data had moved while this process was still serving the
 * old store from memory, which is the precise shape of a surface that reports
 * work it has not performed.
 *
 * A RESTORE IS DESTRUCTIVE AND IS CONFIRMED AS SUCH. It replaces the whole
 * workspace at the next boot. The button is not armed until the operator has
 * chosen a container AND typed its passphrase, and it says what it is about to
 * replace.
 *
 * BOTH OPERATIONS ARE ASYNC AND ARE POLLED. Each answers 202 with a Job; the
 * page polls until the state is terminal and reports the per-target failure
 * detail when there is one, rather than showing a spinner that never resolves.
 *
 * A DELETE IS FOREVER, AND IT IS CONFIRMED BY NAME. A container is encrypted
 * under a passphrase this box does not keep, so deleting one is not recoverable
 * by anybody — not by support, not by the author. The confirmation therefore
 * names the container, its size and its date, and says in as many words that the
 * bytes cannot be got back. A failed delete says the backup is still there,
 * using the server's own refusal code.
 *
 * ── The archive list is a ui-schema/1 DOCUMENT, not React ───────────────────
 *
 * `archives.uis.json` is the table, the detail panel, the destructive button,
 * its confirmation, and every sentence a refusal can produce — rendered through
 * the same PageRenderer an extension page goes through (parity-loop Step 1.5).
 * What stays here is the seam: the rows are PROJECTED into the fields the
 * document binds, and the `delete` verb resolves onto the typed api/1 client.
 *
 * Two things the host does that the grammar cannot, both recorded as gaps rather
 * than worked around silently:
 *
 *   • A ConfirmSpec's labels are plain `msg:` references with no argument list
 *     (UIS-165), so a confirmation cannot interpolate the record it is about to
 *     destroy. Naming the container is not optional on an irreversible act, so
 *     the HOST recomputes the message catalog per selection — the same class of
 *     host projection the Variables page uses for a polymorphic scalar.
 *   • The catalog has no link widget, so "Download" is a host `slot` (UIS-185)
 *     holding a real anchor — the kit's `DownloadLink`, which exists because
 *     this page and the cast library had each hand-rolled the same one. A
 *     `button` + `navigate` would have been in-grammar and would have thrown
 *     away what an anchor is for: a URL the browser streams straight to disk,
 *     right-clickable, needing no script.
 */

/** How often an in-flight Job is polled. An export of a real workspace takes
 * seconds to minutes (a memory-hard KDF, then the whole store and every asset);
 * a second is responsive without hammering. */
const JOB_POLL_MS = 1_000;

/** The floor the server enforces on an export passphrase (openapi
 * WorkspaceExportRequest `minLength`). Mirrored so the page can refuse before a
 * round trip — and only as a courtesy: the server is the authority and its 422
 * is surfaced verbatim if the two ever disagree. */
const MIN_PASSPHRASE = 12;

type JobState =
  | { status: "idle" }
  | { status: "running"; jobId: string }
  | { status: "done"; message: string }
  | { status: "failed"; detail: string; traceId: string | null };

function apiFailure(err: unknown): JobState {
  if (err instanceof ApiError) {
    return { status: "failed", detail: err.detail ?? err.title ?? err.code, traceId: err.traceId };
  }
  return { status: "failed", detail: "The service is unreachable.", traceId: null };
}

/** One container projected into the fields `archives.uis.json` binds.
 *
 * The document renders three columns and a facts line; a table cell binds ONE
 * path and the grammar has no formatter for bytes, so the rendered forms are
 * computed here rather than bent into the document. `is_newest` is the same
 * story from the other side — see the note on it below. */
type ArchiveViewRow = WorkspaceArchive & {
  size_display: string;
  created_display: string;
  /** Whether this is the most recent container on the box.
   *
   * A HOST projection because the grammar has no ordinal or comparison compute
   * (UIS-140's list is eq/not/and/or/count/isEmpty/join/label/msg/format*), so
   * "is this the first row of a newest-first list" cannot be expressed.
   *
   * It exists to WARN, never to refuse. Deleting your newest backup is a real
   * mistake and the confirmation says so; making it impossible would be worse,
   * because an operator who has copied the container off the box (the thing this
   * page tells them to do) would then be unable to reclaim its space, and a
   * container exported under a mistyped passphrase — useless bytes — would be
   * undeletable forever. The operator owns the workspace; the page's job is to
   * make sure they know what they are about to lose, not to keep it from them. */
  is_newest: boolean;
};

export function formatBytes(n: number): string {
  if (!Number.isFinite(n) || n < 0) return "—";
  const units = ["B", "KiB", "MiB", "GiB"];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i += 1;
  }
  return `${v >= 10 || i === 0 ? Math.round(v) : v.toFixed(1)} ${units[i]}`;
}

function JobBanner({ state }: { state: JobState }) {
  if (state.status === "idle") return null;
  if (state.status === "running") {
    return (
      <p className="text-sm text-muted-foreground" role="status">
        Working… (job {state.jobId})
      </p>
    );
  }
  if (state.status === "done") {
    return (
      <p className="text-sm text-[color:var(--wv-ok)]" role="status">
        {state.message}
      </p>
    );
  }
  return (
    <p className="text-sm text-[color:var(--wv-err)]" role="alert">
      {state.detail}
      {state.traceId ? (
        <span className="mt-0.5 block font-mono text-[11px] text-muted-foreground">
          trace {state.traceId}
        </span>
      ) : null}
    </p>
  );
}

/** The sentences `archives.uis.json` resolves, with the two that name a specific
 * container filled in from the CURRENT selection.
 *
 * The interpolation is the host's because UIS-165's ConfirmSpec carries plain
 * `msg:` references and no argument list, so the dialog cannot say "delete
 * workspace-01J8….waiveo-archive (4 MiB)" from the document alone. On an
 * irreversible act that is not a cosmetic loss: a confirmation that describes a
 * class rather than a thing is one an operator dismisses out of habit and later
 * discovers destroyed the wrong backup. */
function archiveMessages(selected: ArchiveViewRow | null): Record<string, string> {
  const named = selected
    ? `“${selected.name}” — ${selected.size_display}, taken ${selected.created_display}`
    : "this backup";
  return {
    "msg:backup.col.name": "Container",
    "msg:backup.col.created": "Created",
    "msg:backup.col.size": "Size",
    "msg:backup.detail.title": "Backup",
    "msg:backup.detail.empty": "Choose a backup to download or delete it.",
    "msg:backup.detail.facts": "{0}, taken {1}.",
    "msg:backup.detail.newest":
      "This is the most recent backup on this box. If you delete it, the newest one you have is whatever is left.",
    "msg:backup.detail.delete": "Delete this backup",
    "msg:backup.delete.confirm.title": "Delete this backup for good?",
    "msg:backup.delete.confirm.body":
      `This permanently deletes ${named} from this box. It is encrypted with the passphrase you chose when ` +
      "you exported it, and this box keeps no copy of that passphrase — so nothing here, and nobody at all, " +
      "can bring these bytes back. If you have not already downloaded this container somewhere else, " +
      "cancel and download it first.",
    "msg:backup.delete.confirm.ok": "Delete for good",
    "msg:backup.delete.confirm.cancel": "Keep it",
    "msg:backup.delete.pending": "Deleting…",
    "msg:backup.delete.inUse":
      "Not now — this backup is being used by a job that is still running, so it was not deleted. {0}",
    "msg:backup.delete.changed":
      "This backup changed on the box since this page read it, so nothing was deleted. Refresh the list and " +
      "look again before deleting — the container under that name may not be the one you were looking at.",
    "msg:backup.delete.gone": "That backup is no longer on this box. Refresh the list.",
    "msg:backup.delete.forbidden":
      "Only the workspace owner can delete a backup. Nothing was deleted. Ask an owner.",
    "msg:backup.delete.failed": "The backup was not deleted. {0}",
    "msg:backup.delete.trace": "trace {0}",
  };
}

export default function BackupRoute({ api }: { api?: WaiveoApi }) {
  const client = useMemo(() => api ?? createApi(), [api]);

  const [archives, setArchives] = useState<WorkspaceArchive[]>([]);
  const [directory, setDirectory] = useState("");
  const [listError, setListError] = useState<string | null>(null);

  const [passphrase, setPassphrase] = useState("");
  const [confirm, setConfirm] = useState("");
  const [exportState, setExportState] = useState<JobState>({ status: "idle" });

  const [selected, setSelected] = useState("");
  const [restorePassphrase, setRestorePassphrase] = useState("");
  const [restoreState, setRestoreState] = useState<JobState>({ status: "idle" });

  // Which container the ARCHIVE PANEL has selected, learned from the renderer's
  // own `$ui` (onUiChange) rather than kept in parallel with it — the document
  // owns the selection and this is a read of it.
  const [inspecting, setInspecting] = useState<string | null>(null);
  // Bumped to REMOUNT the renderer after a successful delete: the store seeds
  // from `data` once, at mount, so a refreshed list is only visible across a
  // remount. Deliberately NOT bumped on a failure — a remount would clear the
  // ActionOutcome carrying the refusal, and the operator would be left with a
  // backup that is still there and no sentence saying why.
  const [archiveEpoch, setArchiveEpoch] = useState(0);

  // Every in-flight poll is cancelled on unmount, so a page left during a long
  // export does not keep a timer (and a setState) alive behind it.
  const alive = useRef(true);
  useEffect(() => {
    alive.current = true;
    return () => {
      alive.current = false;
    };
  }, []);

  const refreshArchives = useCallback(async () => {
    try {
      const list = await client.backup.archives();
      if (!alive.current) return;
      setArchives(list.items);
      setDirectory(list.directory);
      setListError(null);
    } catch (err: unknown) {
      if (!alive.current) return;
      setListError(
        err instanceof ApiError
          ? (err.detail ?? err.title ?? err.code)
          : "The service is unreachable.",
      );
    }
  }, [client]);

  useEffect(() => {
    void refreshArchives();
  }, [refreshArchives]);

  /** Poll one Job to a terminal state. Resolves with the finished Job, so each
   * caller words its OWN success — an export and a restore mean different
   * things by "succeeded" and must not share a sentence. */
  const pollJob = useCallback(
    async (jobId: string): Promise<Job | null> => {
      for (;;) {
        const job = await client.jobs.get(jobId);
        if (!alive.current) return null;
        if (jobIsTerminal(job)) return job;
        await new Promise((r) => setTimeout(r, JOB_POLL_MS));
        if (!alive.current) return null;
      }
    },
    [client],
  );

  const runExport = useCallback(async () => {
    try {
      const job = await client.backup.export(passphrase);
      if (!alive.current) return;
      setExportState({ status: "running", jobId: job.id });
      const done = await pollJob(job.id);
      if (!done || !alive.current) return;
      const failure = jobFailureDetail(done);
      if (done.state !== "succeeded" || failure) {
        setExportState({ status: "failed", detail: failure ?? "The export did not complete.", traceId: null });
        return;
      }
      setExportState({
        status: "done",
        message: "Export complete. Download it and keep it off this box — a backup that only lives here is not a backup.",
      });
      setPassphrase("");
      setConfirm("");
      await refreshArchives();
      toast.success("Workspace exported");
    } catch (err: unknown) {
      if (alive.current) setExportState(apiFailure(err));
    }
  }, [client, passphrase, pollJob, refreshArchives]);

  const runRestore = useCallback(async () => {
    try {
      const job = await client.backup.restore(selected, restorePassphrase);
      if (!alive.current) return;
      setRestoreState({ status: "running", jobId: job.id });
      const done = await pollJob(job.id);
      if (!done || !alive.current) return;
      const failure = jobFailureDetail(done);
      if (done.state !== "succeeded" || failure) {
        setRestoreState({
          status: "failed",
          detail: failure ?? "The restore did not complete. Nothing was changed.",
          traceId: null,
        });
        return;
      }
      // NOT "restored". The swap happens at the next boot; this process is
      // still serving the old store.
      setRestoreState({
        status: "done",
        message:
          "Staged. Restart this box to finish the restore — until then it is still serving the current workspace, and nothing has been replaced.",
      });
      setRestorePassphrase("");
    } catch (err: unknown) {
      if (alive.current) setRestoreState(apiFailure(err));
    }
  }, [client, selected, restorePassphrase, pollJob]);

  const exportReady =
    passphrase.length >= MIN_PASSPHRASE && passphrase === confirm && exportState.status !== "running";
  const restoreReady = selected !== "" && restorePassphrase !== "" && restoreState.status !== "running";

  // The rows the document binds. `is_newest` reads off the FIRST row because the
  // server sorts newest-first (workspacearchives.go) — the one ordering fact this
  // page depends on, taken from the one place that decides it rather than
  // re-derived here from timestamps that would disagree on a tie.
  const viewRows: ArchiveViewRow[] = useMemo(
    () =>
      archives.map((a, i) => ({
        ...a,
        size_display: formatBytes(a.size_bytes),
        created_display: new Date(a.created_at_ms).toLocaleString(),
        is_newest: i === 0,
      })),
    [archives],
  );
  const inspected = useMemo(
    () => viewRows.find((a) => a.name === inspecting) ?? null,
    [viewRows, inspecting],
  );
  const archiveCatalog = useMemo(() => archiveMessages(inspected), [inspected]);

  const onArchiveUiChange = useCallback((ui: Record<string, unknown>) => {
    setInspecting(typeof ui.selected === "string" ? ui.selected : null);
  }, []);

  /**
   * The delete seam — the ONE verb the archive document dispatches.
   *
   * It RETHROWS a refusal rather than swallowing it into a toast. That is the
   * whole point of the outcome binding: the document renders each published code
   * as its own sentence ("a job is using it", "it changed on the box", "you are
   * not the owner"), and a handler that caught the error would leave the page
   * saying nothing while the backup quietly survived.
   *
   * The list is refreshed only on SUCCESS, and the remount that makes the
   * refreshed rows visible is the same event — so a failure leaves the operator
   * looking at the container that is still there, with the refusal beside it.
   */
  const archiveHandler = useMemo<ActionHandler>(
    () => ({
      remove: async (_target, resource) => {
        const row = resource as ArchiveViewRow | null;
        if (!row?.name) return;
        await client.backup.remove(row);
        toast.success(`Deleted ${row.name}`);
        setInspecting(null);
        await refreshArchives();
        setArchiveEpoch((n) => n + 1);
      },
    }),
    [client, refreshArchives],
  );

  return (
    <div className="min-h-screen bg-background text-foreground">
      <div className="mx-auto flex max-w-[1200px] flex-col gap-6 px-4 py-6 lg:px-8">
        <PageHeader
          variant="hero"
          title="Backup"
          description="Export this workspace to one encrypted, portable file — every cast, schedule, screen and image — and restore it onto this box or a new one."
        />

        <section aria-labelledby="export-heading" className="flex flex-col gap-3">
          <h2 id="export-heading" className="text-lg font-semibold">
            Export
          </h2>
          <p className="text-sm text-muted-foreground">
            The export is encrypted with the passphrase you choose here.{" "}
            <strong>This box keeps no copy of it</strong> — that is what makes the encryption worth
            anything, and it means a lost passphrase is a lost backup. Write it down somewhere that
            is not this box.
          </p>
          <div className="flex flex-wrap items-end gap-3">
            <FormField
              label="Passphrase"
              help={`At least ${MIN_PASSPHRASE} characters.`}
              {...(passphrase && passphrase.length < MIN_PASSPHRASE
                ? { error: `Use at least ${MIN_PASSPHRASE} characters.` }
                : {})}
            >
              {(control) => (
                <input
                  {...control}
                  type="password"
                  autoComplete="new-password"
                  className="h-9 rounded-md border border-border bg-[color:var(--wv-surface-2)] px-2 text-sm"
                  value={passphrase}
                  onChange={(e) => setPassphrase(e.target.value)}
                />
              )}
            </FormField>
            <FormField
              label="Confirm passphrase"
              {...(confirm && confirm !== passphrase ? { error: "The two do not match." } : {})}
            >
              {(control) => (
                <input
                  {...control}
                  type="password"
                  autoComplete="new-password"
                  className="h-9 rounded-md border border-border bg-[color:var(--wv-surface-2)] px-2 text-sm"
                  value={confirm}
                  onChange={(e) => setConfirm(e.target.value)}
                />
              )}
            </FormField>
            <Button
              onClick={() => void runExport()}
              disabled={!exportReady}
              aria-label="Export this workspace"
            >
              <Upload aria-hidden className="size-4" />
              Export workspace
            </Button>
          </div>
          <JobBanner state={exportState} />
        </section>

        <section aria-labelledby="archives-heading" className="flex flex-col gap-3">
          <div className="flex flex-wrap items-center justify-between gap-2">
            <h2 id="archives-heading" className="text-lg font-semibold">
              Backups on this box
            </h2>
            <Button
              variant="secondary"
              onClick={() => void refreshArchives()}
              aria-label="Refresh the backup list"
            >
              Refresh
            </Button>
          </div>
          {directory ? (
            <p className="text-sm text-muted-foreground">
              Stored in <span className="font-mono text-xs">{directory}</span>. A container copied
              back into that directory can be restored below by name — that is the disaster-recovery
              path when the box itself is gone.
            </p>
          ) : null}
          {listError ? (
            <p className="text-sm text-[color:var(--wv-err)]" role="alert">
              {listError}
            </p>
          ) : archives.length === 0 ? (
            <p className="text-sm text-muted-foreground">
              No backups yet. Export the workspace above, then download the container and keep it
              somewhere other than this box.
            </p>
          ) : (
            <PageRenderer
              key={archiveEpoch}
              doc={archivesDoc}
              data={{ archives: viewRows }}
              initialUi={{ selected: inspecting }}
              messages={archiveCatalog}
              handler={archiveHandler}
              onUiChange={onArchiveUiChange}
              slots={{
                // The `download` slot the document declares (UIS-185): a real
                // anchor, because a download is a URL the browser streams to
                // disk and a scripted click would only take that away.
                download: inspected ? (
                  <DownloadLink
                    href={client.backup.downloadUrl(inspected)}
                    download={inspected.name}
                    icon={Download}
                    aria-label={`Download ${inspected.name}`}
                  >
                    Download
                  </DownloadLink>
                ) : null,
              }}
            />
          )}
        </section>

        <section aria-labelledby="restore-heading" className="flex flex-col gap-3">
          <h2 id="restore-heading" className="text-lg font-semibold">
            Restore
          </h2>
          <p className="text-sm text-muted-foreground">
            A restore <strong>replaces this entire workspace</strong> with the container's — every
            cast, schedule and screen. It is verified and staged now, and takes effect{" "}
            <strong>when this box next restarts</strong>; until then nothing changes and you can
            simply not restart.
          </p>
          <div className="flex flex-wrap items-end gap-3">
            <FormField label="Container">
              {(control) => (
                <select
                  {...control}
                  className="h-9 rounded-md border border-border bg-[color:var(--wv-surface-2)] px-2 text-sm"
                  value={selected}
                  onChange={(e) => setSelected(e.target.value)}
                >
                  <option value="">Choose a backup…</option>
                  {archives.map((a) => (
                    <option key={a.name} value={a.name}>
                      {a.name}
                    </option>
                  ))}
                </select>
              )}
            </FormField>
            <FormField label="Its passphrase" help="The one used when that container was exported.">
              {(control) => (
                <input
                  {...control}
                  type="password"
                  autoComplete="off"
                  className="h-9 rounded-md border border-border bg-[color:var(--wv-surface-2)] px-2 text-sm"
                  value={restorePassphrase}
                  onChange={(e) => setRestorePassphrase(e.target.value)}
                />
              )}
            </FormField>
            <Button
              variant="destructive"
              onClick={() => void runRestore()}
              disabled={!restoreReady}
              aria-label="Stage a restore from this backup"
            >
              Stage restore
            </Button>
          </div>
          <JobBanner state={restoreState} />
          {restoreState.status === "done" ? (
            <StatusBadge status="warn">Restart required</StatusBadge>
          ) : null}
        </section>
      </div>
    </div>
  );
}
