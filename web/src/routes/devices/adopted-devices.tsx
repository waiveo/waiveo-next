import { useCallback, useEffect, useMemo, useState } from "react";
import { HardDrive } from "lucide-react";
import { EmptyState, toast } from "@/components/kit";
import { PageRenderer, type ActionHandler } from "@/renderer";
import {
  ApiError,
  collectPages,
  etagForRevision,
  type AdoptedDevice,
  type AdoptedDeviceEntity,
  type WaiveoApi,
} from "@/api";
import adoptedDevicesDoc from "./adopted-devices.uis.json";

/**
 * Adopted devices — the records, and the policy over them.
 *
 * # Authored as `ui-schema/1`, not as React
 *
 * `adopted-devices.uis.json` is the whole panel. This file is the data half:
 * fetch, shape, and the two seams the document's verbs land on.
 *
 * The first version was hand-written TSX with a modal editor, which the loop's
 * standing rule (parity-loop runbook, step 1.5) forbids without naming a
 * renderer limit that forces it. There was none. `device-discovery` and
 * `roku-integration` are two of the nine extensions this programme exists to
 * extract, so this panel is on that list, and a page in this format is portable
 * into a pack while the same page in TSX is thrown away.
 *
 * The conversion also removed the modal. A `list-detail` puts the editor in the
 * detail pane — what every other core page does and what the grammar is shaped
 * for. The modal was invented by the hand-written version, not required.
 *
 * `confirm` on the destructive verb is GRAMMAR, not a dialog this file builds
 * (`system/restart-panel.uis.json` uses the same shape), so Release still asks
 * before it acts and the asking is declared.
 *
 * # Every member here is a DECISION
 *
 * That is why this resource exists apart from the relay's report (REL-063). The
 * relay is authoritative for what EXISTS; nothing it reports says whether an
 * entity should be polled, shown, or what an operator wants it called. So
 * `poll_cadence_seconds` being null renders as "none set" in the table rather
 * than 0s or blank: REL-063 fixes no default, and a number shown where none was
 * authored is a polling policy nobody set.
 *
 * # The renderer limit this hit, recorded rather than worked around
 *
 * `number-input` has NO null. A cadence is `integer | null` on the wire and
 * "state no cadence" is a real, distinct policy — but an emptied number input
 * yields an empty value, not null, and the grammar has no clearable-number
 * shape. The coercion therefore lives in `submit` below, at the boundary where
 * the value is on its way to the server and can be read once, honestly. A
 * grammar that could say this would not need the hop.
 */

/** The `msg:` catalog the document's references resolve against. Copy lives here
 * so the document stays layout; a pack shipping this page supplies its own. */
const MESSAGES: Record<string, string> = {
  "msg:adopted.col.name": "Name",
  "msg:adopted.col.driver": "Driver",
  "msg:adopted.col.native": "Native id",
  "msg:adopted.col.cadence": "Poll cadence",
  "msg:adopted.col.entities": "Entities",
  "msg:adopted.detail.title": "Policy",
  "msg:adopted.detail.empty": "Select an adopted device to set its policy.",
  "msg:adopted.detail.lead":
    "How this deployment polls and presents the device. The relay reports what exists; these are the decisions over it.",
  "msg:adopted.field.name": "Name",
  "msg:adopted.field.cadence": "Poll cadence (seconds)",
  "msg:adopted.field.cadenceHelp": "Leave empty to state no cadence — the platform fixes no default.",
  "msg:adopted.entities.one": "Entity",
  "msg:adopted.entities.empty": "This device exposes no entities.",
  "msg:adopted.entity.enabled": "Enabled",
  "msg:adopted.entity.hidden": "Hidden",
  "msg:adopted.entity.displayName": "Display name",
  "msg:adopted.entity.category": "Category",
  "msg:adopted.category.primary": "primary",
  "msg:adopted.category.diagnostic": "diagnostic",
  "msg:adopted.save": "Save policy",
  "msg:adopted.release": "Release",
  "msg:adopted.release.confirm.title": "Release this device?",
  "msg:adopted.release.confirm.body":
    "The device stops being polled and commanded, and its adoption record is removed. Discovery will keep finding it, so it can be adopted again.",
  "msg:adopted.release.confirm.ok": "Release",
  "msg:adopted.release.confirm.cancel": "Keep it",
};

function problemMessage(err: unknown): string {
  if (err instanceof ApiError) return err.detail ?? err.code;
  return "the service is unreachable.";
}

/** The cadence a submitted value means.
 *
 * Empty is VALID and means null — "this deployment states no cadence"
 * (REL-063). Zero and negatives are not: the schema's minimum is 1, and a 0 sent
 * as a cadence is refused server-side after the operator was told it saved.
 *
 * Exported because it is the whole of the number-input-has-no-null workaround,
 * and a workaround nobody can test is one nobody can remove when the grammar
 * grows a clearable number. */
export function cadenceOf(raw: unknown): number | null | "invalid" {
  if (raw === null || raw === undefined) return null;
  const text = typeof raw === "number" ? String(raw) : String(raw).trim();
  if (text === "") return null;
  const n = Number(text);
  if (!Number.isFinite(n)) return "invalid";
  return Number.isSafeInteger(n) && n >= 1 ? n : "invalid";
}

/** One record as the document binds it: the wire row plus the two display
 * strings a `cell` cannot compute (the catalog has no concatenation). */
interface AdoptedRow extends AdoptedDevice {
  cadence_display: string;
  entities_display: string;
}

export function toRow(record: AdoptedDevice): AdoptedRow {
  const on = record.entities.filter((e) => e.enabled).length;
  return {
    ...record,
    // NOT "0s" and not blank: no cadence stated is its own fact, and a number
    // here would be a polling policy nobody authored (REL-063).
    cadence_display: record.poll_cadence_seconds === null ? "none set" : `${record.poll_cadence_seconds}s`,
    entities_display: `${on} of ${record.entities.length} enabled`,
  };
}

export function AdoptedDevices({ api }: { api: WaiveoApi }) {
  const [records, setRecords] = useState<AdoptedDevice[] | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  // PageRenderer seeds its editable store ONCE from the initial resource, so a
  // panel that fetches after first paint shows its empty state forever without a
  // remount. Bumping a key is how the pack page route solves the same thing.
  //
  // Consequence worth knowing before writing a test against this: the remount
  // REPLACES the table element, so a node captured before the data lands is
  // detached, and every `within(thatTable)` query then fails while the screen is
  // visibly correct. Query the document after the rows arrive, not the table
  // before them. That cost a whole iteration to find.
  const [generation, setGeneration] = useState(0);

  const load = useCallback(async () => {
    try {
      setRecords(await collectPages<AdoptedDevice>((cursor) => api.adoptedDevices.list({ cursor })));
      setLoadError(null);
    } catch (err) {
      setRecords([]);
      setLoadError(problemMessage(err));
    } finally {
      setGeneration((g) => g + 1);
    }
  }, [api]);

  useEffect(() => {
    void load();
  }, [load]);

  const data = useMemo(() => ({ adopted: (records ?? []).map(toRow) }), [records]);

  const handler: ActionHandler = useMemo(
    () => ({
      submit: async (_target, resource) => {
        const row = resource as AdoptedRow | null;
        if (!row?.id) return;
        const cadence = cadenceOf(row.poll_cadence_seconds);
        if (cadence === "invalid") {
          // Refused HERE rather than by the server, whose 422 would arrive after
          // the operator had been told the save went through.
          toast.error("Poll cadence must be a whole number of seconds, or empty for none.");
          return;
        }
        try {
          await api.adoptedDevices.update(
            row.id,
            {
              name: row.name,
              poll_cadence_seconds: cadence,
              // Stripped of the two display strings this panel added: they are
              // view state, and the resource's schema closes additionalProperties.
              entities: row.entities.map((e) => ({ ...e }) as AdoptedDeviceEntity),
            },
            etagForRevision(row.revision),
          );
          toast.success("Saved device policy");
          await load();
        } catch (err) {
          // A 412 gets its own words: the generic "couldn't save" sends an
          // operator round the same edit against the same stale revision.
          toast.error(
            err instanceof ApiError && err.status === 412
              ? "This device changed elsewhere. Reselect it to see the current policy."
              : `Couldn't save the policy: ${problemMessage(err)}`,
          );
        }
      },
      // The confirmation is DECLARED on the verb, so by the time this runs the
      // operator has already been asked.
      remove: async (_target, resource) => {
        const row = resource as AdoptedRow | null;
        if (!row?.id) return;
        try {
          await api.adoptedDevices.remove(row.id, etagForRevision(row.revision));
          toast.success(`Released ${row.name}`);
          await load();
        } catch (err) {
          toast.error(`Couldn't release the device: ${problemMessage(err)}`);
        }
      },
    }),
    [api, load],
  );

  if (loadError) {
    return (
      <section aria-label="Adopted devices" className="flex flex-col gap-3">
        <EmptyState title="Adopted devices could not be read" description={loadError} icon={HardDrive} />
      </section>
    );
  }

  return (
    <section aria-label="Adopted devices" className="flex flex-col gap-3">
      <div>
        <h2 className="text-lg font-semibold">Adopted devices</h2>
        <p className="text-sm text-muted-foreground">
          The devices this deployment is responsible for, and the policy over them. Everything here
          is a decision — the relay reports what exists, not how it should be polled or shown.
        </p>
      </div>
      <PageRenderer key={generation} doc={adoptedDevicesDoc} data={data} messages={MESSAGES} handler={handler} />
    </section>
  );
}
