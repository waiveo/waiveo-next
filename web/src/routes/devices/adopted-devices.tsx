import { useCallback, useEffect, useMemo, useState } from "react";
import { HardDrive } from "lucide-react";
import {
  Button,
  Checkbox,
  ConfirmModal,
  DataTable,
  EmptyState,
  FormField,
  Modal,
  StatusBadge,
  toast,
  type ColumnDef,
} from "@/components/kit";
import {
  ApiError,
  collectPages,
  etagForRevision,
  type AdoptedDevice,
  type AdoptedDeviceEntity,
  type WaiveoApi,
} from "@/api";

/**
 * Adopted devices — the records, and the policy over them.
 *
 * # Why this is a SEPARATE table from the discovered one
 *
 * The devices page above lists what the relay can SEE. This lists what this
 * deployment has DECIDED about, and the two do not join: a discovered row's id
 * is a discovery id and an adoption record's is its own, sharing no member by
 * design (see the route's module header). Releasing needs the record's id and
 * revision, so it belongs on a page that lists records.
 *
 * # What was missing
 *
 * All of it. The full CRUD has been live and mounted server-side, and the
 * console had no surface at all: after adopting a device an operator could set
 * no poll cadence, disable no noisy entity, hide none, rename none, and release
 * nothing. The adopt dialog told them these were "refined afterwards on the
 * adopted device" and there was no afterwards. Stopping a device being polled
 * meant a raw DELETE with the record's revision ETag.
 *
 * # Every member here is a DECISION
 *
 * That is the whole reason this resource exists apart from the relay's report
 * (REL-063): the relay is authoritative for what EXISTS, and nothing it reports
 * says whether an entity should be polled, shown, or what an operator wants it
 * called. So this panel edits policy and never displays it as though it were
 * discovered fact — including `poll_cadence_seconds`, which is nullable
 * precisely because REL-063 fixes no default and a number shown where none was
 * authored would be a policy nobody set.
 */

/** Which modal, if any, this panel has open. */
type PolicyDialog =
  | { kind: "closed" }
  | { kind: "policy"; record: AdoptedDevice }
  | { kind: "release"; record: AdoptedDevice };

/** The editable slice of a record, held while the modal is open. */
interface PolicyDraft {
  name: string;
  /** The cadence box's raw text — "" means "state no cadence" (null), which is
   * a different decision from any number and must survive the round trip. */
  cadence: string;
  entities: AdoptedDeviceEntity[];
}

function draftFrom(record: AdoptedDevice): PolicyDraft {
  return {
    name: record.name,
    cadence: record.poll_cadence_seconds === null ? "" : String(record.poll_cadence_seconds),
    entities: record.entities.map((e) => ({ ...e })),
  };
}

/** The cadence a draft means, or `invalid` when the box holds something that is
 * not a positive whole number of seconds.
 *
 * Blank is VALID and means null — "this deployment states no cadence" (REL-063).
 * Zero and negatives are not: the schema's minimum is 1, and a 0 sent as a
 * cadence would be refused server-side after the operator had already been told
 * their edit was saved. */
export function cadenceOf(raw: string): number | null | "invalid" {
  const text = raw.trim();
  if (text === "") return null;
  if (!/^\d+$/.test(text)) return "invalid";
  const n = Number(text);
  return Number.isSafeInteger(n) && n >= 1 ? n : "invalid";
}

function problemMessage(err: unknown): string {
  if (err instanceof ApiError) return err.detail ?? err.code;
  return "the service is unreachable.";
}

export function AdoptedDevices({ api }: { api: WaiveoApi }) {
  const [records, setRecords] = useState<AdoptedDevice[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [dialog, setDialog] = useState<PolicyDialog>({ kind: "closed" });
  const [draft, setDraft] = useState<PolicyDraft | null>(null);
  const [busy, setBusy] = useState(false);

  const load = useCallback(async () => {
    try {
      setRecords(await collectPages<AdoptedDevice>((cursor) => api.adoptedDevices.list({ cursor })));
      setLoadError(null);
    } catch (err) {
      setRecords([]);
      setLoadError(problemMessage(err));
    }
  }, [api]);

  useEffect(() => {
    void load();
  }, [load]);

  const openPolicy = useCallback((record: AdoptedDevice) => {
    setDraft(draftFrom(record));
    setDialog({ kind: "policy", record });
  }, []);

  const savePolicy = useCallback(async () => {
    if (dialog.kind !== "policy" || draft === null) return;
    const cadence = cadenceOf(draft.cadence);
    if (cadence === "invalid") {
      // Refused HERE rather than by the server, because the server's refusal
      // would arrive after the modal had closed on what looked like a save.
      toast.error("Poll cadence must be a whole number of seconds, or blank for none.");
      return;
    }
    setBusy(true);
    try {
      await api.adoptedDevices.update(
        dialog.record.id,
        { name: draft.name, poll_cadence_seconds: cadence, entities: draft.entities },
        etagForRevision(dialog.record.revision),
      );
      toast.success("Saved device policy");
      setDialog({ kind: "closed" });
      await load();
    } catch (err) {
      // A 412 means the record moved under this edit. Said in those words: the
      // generic "couldn't save" would send an operator round the same edit again
      // against the same stale revision.
      toast.error(
        err instanceof ApiError && err.status === 412
          ? "This device changed elsewhere. Reopen it to see the current policy."
          : `Couldn't save the policy: ${problemMessage(err)}`,
      );
    } finally {
      setBusy(false);
    }
  }, [api, dialog, draft, load]);

  const confirmRelease = useCallback(async () => {
    if (dialog.kind !== "release") return;
    setBusy(true);
    try {
      await api.adoptedDevices.remove(dialog.record.id, etagForRevision(dialog.record.revision));
      toast.success(`Released ${dialog.record.name}`);
      setDialog({ kind: "closed" });
      await load();
    } catch (err) {
      toast.error(`Couldn't release the device: ${problemMessage(err)}`);
    } finally {
      setBusy(false);
    }
  }, [api, dialog, load]);

  const columns = useMemo<ColumnDef<AdoptedDevice>[]>(
    () => [
      { id: "name", header: "Name", accessorFn: (r) => r.name, meta: { searchable: true } },
      { id: "driver", header: "Driver", accessorFn: (r) => r.driver, meta: { searchable: true, filter: "enum", filterLabel: "Driver" } },
      { id: "native", header: "Native id", accessorFn: (r) => r.native_id, meta: { searchable: true } },
      {
        id: "cadence",
        header: "Poll cadence",
        accessorFn: (r) => r.poll_cadence_seconds ?? 0,
        cell: ({ row }) =>
          row.original.poll_cadence_seconds === null ? (
            // NOT "0s" and not blank: no cadence stated is its own fact, and a
            // number here would be a polling policy nobody authored (REL-063).
            <StatusBadge status="pending">none set</StatusBadge>
          ) : (
            <span>{row.original.poll_cadence_seconds}s</span>
          ),
      },
      {
        id: "entities",
        header: "Entities",
        accessorFn: (r) => r.entities.length,
        cell: ({ row }) => {
          const all = row.original.entities;
          const on = all.filter((e) => e.enabled).length;
          return (
            <span>
              {on} of {all.length} enabled
            </span>
          );
        },
      },
    ],
    [],
  );

  const editing = dialog.kind === "policy" ? dialog.record : null;

  return (
    <section aria-label="Adopted devices" className="flex flex-col gap-3">
      <div>
        <h2 className="text-lg font-semibold">Adopted devices</h2>
        <p className="text-sm text-muted-foreground">
          The devices this deployment is responsible for, and the policy over them. Everything here
          is a decision — the relay reports what exists, not how it should be polled or shown.
        </p>
      </div>
      {loadError ? (
        <EmptyState
          title="Adopted devices could not be read"
          description={loadError}
          icon={HardDrive}
        />
      ) : (
        <DataTable<AdoptedDevice>
          columns={columns}
          data={records ?? []}
          label="Adopted devices"
          loading={records === null}
          search={{ label: "Search adopted devices", placeholder: "Name, driver or native id" }}
          filters
          pagination
          emptyState={
            <EmptyState
              title="No devices adopted yet"
              description="Adopt a discovered device above to take responsibility for it."
              icon={HardDrive}
            />
          }
          rowActions={(record) => (
            <div className="flex justify-end gap-2">
              <Button size="sm" variant="outline" onClick={() => openPolicy(record)}>
                Policy
              </Button>
              <Button size="sm" variant="ghost" onClick={() => setDialog({ kind: "release", record })}>
                Release
              </Button>
            </div>
          )}
        />
      )}

      <Modal
        title={editing ? `Policy — ${editing.name}` : "Device policy"}
        description="How this deployment polls and presents the device. The relay reports what exists; these are the decisions over it."
        open={dialog.kind === "policy"}
        onOpenChange={(open) => {
          if (!open) setDialog({ kind: "closed" });
        }}
        footer={
          <>
            <Button variant="ghost" onClick={() => setDialog({ kind: "closed" })}>
              Cancel
            </Button>
            <Button onClick={() => void savePolicy()} disabled={busy}>
              {busy ? "Saving…" : "Save policy"}
            </Button>
          </>
        }
      >
        {draft !== null && editing !== null ? (
          <div className="flex flex-col gap-4">
            <FormField label="Name">
              {(field) => (
                <input
                  {...field}
                  type="text"
                  className="flex min-h-[44px] w-full min-w-0 rounded-input border border-border bg-transparent px-3 py-1 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
                  value={draft.name}
                  onChange={(e) => setDraft({ ...draft, name: e.target.value })}
                />
              )}
            </FormField>

            <FormField
              label="Poll cadence (seconds)"
              help="Leave blank to state no cadence — the platform fixes no default."
            >
              {(field) => (
                <input
                  {...field}
                  type="text"
                  inputMode="numeric"
                  className="flex min-h-[44px] w-full min-w-0 rounded-input border border-border bg-transparent px-3 py-1 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
                  value={draft.cadence}
                  onChange={(e) => setDraft({ ...draft, cadence: e.target.value })}
                />
              )}
            </FormField>

            <div className="flex flex-col gap-2">
              <h3 className="text-sm font-semibold">Entities</h3>
              <p className="text-xs text-muted-foreground">
                A disabled entity is not polled. A hidden one is still polled and still commandable
                — it is kept out of the operator's way, which is a different decision.
              </p>
              <ul className="flex flex-col gap-3">
                {draft.entities.map((entity, i) => {
                  const patch = (next: Partial<AdoptedDeviceEntity>) =>
                    setDraft({
                      ...draft,
                      entities: draft.entities.map((e, j) => (i === j ? { ...e, ...next } : e)),
                    });
                  return (
                    <li
                      key={entity.entity_id}
                      className="flex flex-col gap-2 rounded-card border border-border p-3"
                    >
                      <div className="flex flex-wrap items-center gap-4">
                        <span className="text-xs text-muted-foreground">{entity.device_class}</span>
                        <label className="flex items-center gap-2 text-sm">
                          <Checkbox
                            aria-label={`${entity.display_name} enabled`}
                            checked={entity.enabled}
                            onCheckedChange={(v) => patch({ enabled: v === true })}
                          />
                          Enabled
                        </label>
                        <label className="flex items-center gap-2 text-sm">
                          <Checkbox
                            aria-label={`${entity.display_name} hidden`}
                            checked={entity.hidden}
                            onCheckedChange={(v) => patch({ hidden: v === true })}
                          />
                          Hidden
                        </label>
                      </div>
                      <FormField label={`Display name — ${entity.device_class}`}>
                        {(field) => (
                          <input
                            {...field}
                            type="text"
                            className="flex min-h-[44px] w-full min-w-0 rounded-input border border-border bg-transparent px-3 py-1 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
                            value={entity.display_name}
                            onChange={(e) => patch({ display_name: e.target.value })}
                          />
                        )}
                      </FormField>
                      <FormField label={`Category — ${entity.device_class}`}>
                        {(field) => (
                          <select
                            {...field}
                            className="flex min-h-[44px] w-full min-w-0 rounded-input border border-border bg-transparent px-3 py-1 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
                            value={entity.category}
                            onChange={(e) =>
                              patch({ category: e.target.value as AdoptedDeviceEntity["category"] })
                            }
                          >
                            <option value="primary">primary</option>
                            <option value="diagnostic">diagnostic</option>
                          </select>
                        )}
                      </FormField>
                    </li>
                  );
                })}
              </ul>
            </div>
          </div>
        ) : null}
      </Modal>

      <ConfirmModal
        title={dialog.kind === "release" ? `Release ${dialog.record.name}?` : "Release device"}
        description="The device stops being polled and commanded, and its adoption record is removed. Discovery will keep finding it, so it can be adopted again."
        open={dialog.kind === "release"}
        onOpenChange={(open) => {
          if (!open) setDialog({ kind: "closed" });
        }}
        confirmLabel={busy ? "Releasing…" : "Release"}
        destructive
        onConfirm={() => void confirmRelease()}
      />
    </section>
  );
}
