import { useCallback, useEffect, useMemo, useState } from "react";
import { Cpu, RefreshCw, Radio } from "lucide-react";
import {
  Button,
  DataTable,
  EmptyState,
  Modal,
  PageHeader,
  StatusBadge,
  Toaster,
  toast,
  type ColumnDef,
} from "@/components/kit";
import {
  ApiError,
  collectPages,
  createApi,
  deviceFacts,
  type Device,
  type Entity,
  type RelayHealth,
  type WaiveoApi,
} from "@/api";
import { formatAge } from "@/lib/format-age";
import { describeDiscovery, type BlindReason } from "./discovery";
import { DiscoveryPanel } from "./discovery-panel";
import { AdoptedDevices } from "./adopted-devices";

/**
 * The Devices route — the device plane as an operator sees it: what the relays
 * have FOUND, what this deployment has ADOPTED, and the entities those devices
 * expose.
 *
 * # The one distinction this page exists to keep straight
 *
 * Discovery and adoption are different facts with different owners, and legacy
 * blurred them into a single "devices" list. Here they stay apart:
 *
 *   - DISCOVERY is a READ MODEL a relay owns. A row means "a relay reported
 *     seeing this on its LAN" (relay/1 REL-110/111) and nothing more. There is
 *     no write path — this console cannot create, rename, or delete one.
 *   - ADOPTION is what THIS deployment decided: a durable record keyed by
 *     REL-153's `(site, driver, native_id)`, compiled into the signed
 *     `device_inventory` the device's relay is sent (REL-063).
 *
 * The consequence worth stating: a device can be adopted while its relay is
 * offline (the record is durable; the discovered row is not), and a device can
 * be discovered forever without being adopted. Both states are normal and both
 * are legible in the status column.
 *
 * # Adopting is one call, and this page does not compose the record
 *
 * `POST /devices/{id}/adopt` is the whole operation. It takes no body: the
 * identity tuple the record is keyed by is NOT on the discovered row — the
 * server derives `device_id` from it one-way (internal/shared/deviceid), and
 * `labels` is authored data discovery never writes into — so the server builds
 * the record from its own durable mirror, which does hold the tuple.
 *
 * That is also why the status column reads `device.adopted` straight off the
 * row rather than joining `/adopted-devices` onto it. The join has no key: the
 * two resources share no member, by design. A console that joined on the tuple
 * would find it absent on every row and report a fully-adopted fleet as
 * entirely un-adopted, which is worse than not showing the column.
 *
 * # Discovery has a STATE, and this page reports which one
 *
 * The list alone cannot say whether discovery is running. An empty table reads
 * identically for "no relay is connected", "a relay swept and the network is
 * empty", "everything found is already adopted", and "this console was refused
 * relay health and does not know" — four situations with four different
 * remedies. So the page reads `/system-health` alongside `/devices` and renders
 * the classification in `./discovery`, which keeps all seven apart.
 *
 * `SystemHealth.relays` is the substrate because a relay that is not connected
 * does not appear in it, and a relay that is not connected is not discovering.
 * That read is OWNER-ONLY, and a 403 is rendered as its own state ("blind")
 * rather than silently collapsed into "found nothing" — stating what is not
 * known is the entire job of this panel.
 *
 * Placement, poll cadence and per-entity policy are NOT asked for here. The
 * server adopts with the device's own reported entities enabled and primary,
 * and those are refined afterwards through the `adopted-devices` family — as is
 * releasing a device, which needs the record's own id and revision and so
 * belongs on a page that lists records, not discovered devices.
 *
 * # There is no control surface here, and that is deliberate
 *
 * This page had a virtual remote and a link to a Roku console. Both are gone
 * (2026-08-19). The owner drew the line first — "there should be no such thing
 * as a Roku console when it comes to devices: Roku is its own extension, and
 * Discovery is its own extension" — and the console strip finished it: a
 * per-driver control surface is a DRIVER PACK's contribution, not something core
 * carries on the discovery page. Until waiveo/roku exists, an adopted Roku is
 * discovered, adopted and unclassified, and nothing on this page drives it. That
 * is the accepted cost of the boundary, not an omission.
 *
 * A command still goes to an ENTITY rather than a device (relay/1 REL-112
 * accepts one resolved entity id and no selector), so when a driver pack does
 * bring a remote back it belongs against the entities table below — which is why
 * that table still lists entities with their device class even though nothing
 * here acts on one.
 */

function problemMessage(err: unknown): string {
  if (err instanceof ApiError) return err.detail ?? err.code;
  return "the service is unreachable.";
}

/** Render one epoch-millisecond instant as an age relative to now, or an em dash
 * when the API omitted it.
 *
 * Absent is a real answer here and must not be faked: `first_seen`/`last_seen`
 * are absent until this deployment has durably mirrored the device, so a
 * fallback of 0 would render every un-mirrored row as "20000d ago" — a
 * confident, wrong statement about a device nobody has a record for. */
function ageCell(atMs: number | undefined): string {
  if (atMs === undefined || atMs <= 0) return "—";
  return formatAge(Math.max(0, Date.now() - atMs));
}

/** Whether a device's `first_seen` is an instant this deployment OBSERVED, and
 * may therefore be drawn as an exact age.
 *
 * Only `planted` is. `adopted` was copied from a pre-ledger column at an upgrade,
 * having been written off the reporting relay's own unattested clock at that
 * relay's last restart; `unrecorded` is a row from a build that did not record
 * which of the two it was, so it cannot be shown not to be the first. Both get
 * the same treatment because the caution is the same. An absent origin belongs to
 * an absent age. */
function isObservedAge(device: Device): boolean {
  return device.first_seen_origin === "planted";
}

/** The `first_seen` cell.
 *
 * This is the whole of #197 as an operator meets it. `first_seen` is served
 * identically whether this deployment WATCHED the device appear or inherited a
 * number from an upgrade, and drawing both as "3d ago" under a column headed
 * "First seen" claims a precision the second does not have. On the one measured
 * deployment every age on the page was inherited and 57 of them were the same
 * instant to the millisecond — a whole inventory reading as three days old, which
 * a reasonable operator takes to mean the devices arrived three days ago.
 *
 * So an inherited age is drawn with a `~` and carries the caveat in its title:
 * approximately, not at least, because the value overstates the age when the
 * reporting relay's clock ran behind and understates it otherwise, and "≥ 3d"
 * would be a second false guarantee in place of the first. The affordance for
 * doing something about it is the row's own Retire action. */
function firstSeenCell(device: Device): React.ReactNode {
  const age = ageCell(device.first_seen);
  if (age === "—" || isObservedAge(device)) return age;
  return (
    <span
      className="text-muted-foreground"
      title={
        "Inherited, not observed. This age was copied from an older build's column, where it had been " +
        "written from the reporting relay's own unattested clock — so it is an estimate rather than an " +
        "instant this deployment watched, it is not comparable with an observed one, and it can overstate " +
        "the device's age as well as understate it. Retire it to have the next report plant a real one."
      }
    >
      ~{age}
    </span>
  );
}

/** A discovered device, flattened with the facts it reported and the entities
 * it exposes. `adopted` is the row's own flag, not a computed join — see the
 * module header. */
interface DeviceRow {
  device: Device;
  adopted: boolean;
  address: string | null;
  model: string | null;
  driver: string | null;
  nativeId: string | null;
  entities: Entity[];
}

/** Which modal, if any, the page has open. */
type Dialog =
  | { kind: "closed" }
  | { kind: "adopt"; row: DeviceRow }
  | { kind: "retire-age"; row: DeviceRow };

export default function DevicesRoute({ api }: { api?: WaiveoApi }) {
  const client = useMemo(() => api ?? createApi(), [api]);
  const [devices, setDevices] = useState<Device[] | null>(null);
  const [entities, setEntities] = useState<Entity[]>([]);
  const [loadError, setLoadError] = useState<string | null>(null);
  // The connected relays, or null when health could not be read at all — the
  // two are DIFFERENT facts and an empty array must never stand in for the
  // unknown one. `blind` names which refusal produced the null.
  const [relays, setRelays] = useState<RelayHealth[] | null>(null);
  const [blind, setBlind] = useState<BlindReason | null>(null);
  const [dialog, setDialog] = useState<Dialog>({ kind: "closed" });
  const [busy, setBusy] = useState(false);
  // The device whose entities the lower table is narrowed to; null shows every
  // entity in the deployment. Selection is a filter, not navigation — the two
  // tables are one page because a device is only interesting through what it
  // exposes.
  const [selectedDeviceId, setSelectedDeviceId] = useState<string | null>(null);

  const load = useCallback(async () => {
    // Relay connectivity is read SEPARATELY from the device plane, and its
    // failure never fails the page: an empty device list is meaningful only
    // once you know whether a relay is connected, but a failure to read THAT
    // must not hide a fleet the caller is entitled to see. This reads the
    // operator-readable /discovery/relays (the live connection set) rather than
    // owner-only /system-health, so a site admin is no longer blind to it — and
    // it does not 404 when the workspace root is missing, the failure that made
    // even the owner blind here.
    void client.diagnostics
      .relays()
      .then((relays) => {
        setRelays(relays);
        setBlind(null);
      })
      .catch((err: unknown) => {
        setRelays(null);
        setBlind(err instanceof ApiError && err.code === "FORBIDDEN" ? "forbidden" : "unreachable");
      });
    try {
      const [deviceRows, entityRows] = await Promise.all([
        collectPages<Device>((cursor) => client.devices.list({ cursor })),
        collectPages<Entity>((cursor) => client.entities.list({ cursor })),
      ]);
      setDevices(deviceRows);
      setEntities(entityRows);
      setLoadError(null);
    } catch (err) {
      setDevices([]);
      setEntities([]);
      setLoadError(problemMessage(err));
    }
  }, [client]);

  useEffect(() => {
    void load();
  }, [load]);

  const rows = useMemo<DeviceRow[]>(() => {
    return (devices ?? []).map((device) => {
      const facts = deviceFacts(device);
      return {
        device,
        adopted: device.adopted,
        address: facts.address,
        model: facts.model,
        driver: facts.driver,
        nativeId: facts.nativeId,
        entities: entities.filter((e) => e.device_id === device.id),
      };
    });
  }, [devices, entities]);

  const discovery = useMemo(
    () => describeDiscovery({ devices, devicesError: loadError, relays, blind }),
    [devices, loadError, relays, blind],
  );

  /** How many devices each relay is currently accounting for — the answer to
   * "which relay found this one", rolled up. Built from the DEVICE rows rather
   * than from health, because health's own `screen_count` counts screens. */
  const devicesByRelay = useMemo(() => {
    const counts = new Map<string, number>();
    for (const device of devices ?? []) {
      counts.set(device.relay_id, (counts.get(device.relay_id) ?? 0) + 1);
    }
    return counts;
  }, [devices]);

  const deviceNames = useMemo(
    () => new Map((devices ?? []).map((d) => [d.id, d.name] as [string, string])),
    [devices],
  );
  const openAdopt = useCallback((row: DeviceRow) => {
    setDialog({ kind: "adopt", row });
  }, []);
  const openRetireAge = useCallback((row: DeviceRow) => {
    setDialog({ kind: "retire-age", row });
  }, []);

  const confirmAdopt = useCallback(async () => {
    if (dialog.kind !== "adopt" || busy) return;
    const { row } = dialog;
    setBusy(true);
    try {
      const updated = await client.devices.adopt(row.device.id);
      toast.success(`Adopted ${updated.name}`);
      setDialog({ kind: "closed" });
      // The answer IS the adopted row, so the table can be corrected from it
      // directly. The reload that follows is for the ENTITIES the adoption
      // enabled, which this response does not carry — but patching the device
      // first means the status column flips immediately rather than after a
      // second round trip, and stays correct even if that reload fails.
      setDevices((current) =>
        (current ?? []).map((d) => (d.id === updated.id ? updated : d)),
      );
      await load();
    } catch (err) {
      toast.error(`Couldn't adopt the device: ${problemMessage(err)}`);
    } finally {
      setBusy(false);
    }
  }, [busy, client, dialog, load]);

  /** Retire one device's inherited age.
   *
   * The other half of #197. Marking an inherited value in the table says "do not
   * read this as exact" and leaves the operator with nothing to do about it —
   * the correction path existed only over `waiveo call` and MCP, so the person
   * who could SEE the wrong number could not act on it and the caller who could
   * act could not see it.
   *
   * It is offered per device and never in bulk, deliberately: the values this
   * repairs are wrong INDIVIDUALLY (an operator has evidence about one device,
   * not a rule about all of them), and retiring a whole inherited inventory
   * would re-date every device as new — which is the exact defect the plant-once
   * rule exists to prevent, wearing a repair's name. */
  const confirmRetireAge = useCallback(async () => {
    if (dialog.kind !== "retire-age" || busy) return;
    const { row } = dialog;
    setBusy(true);
    try {
      const updated = await client.devices.retireFirstSeen(row.device.id);
      toast.success(`Retired the stored age for ${updated.name}`);
      setDialog({ kind: "closed" });
      // The answer is the device as it now reads, so the row corrects itself
      // without a re-list — and without one, the table would go on showing the
      // retired instant until something else happened to reload.
      setDevices((current) =>
        (current ?? []).map((d) => (d.id === updated.id ? updated : d)),
      );
    } catch (err) {
      toast.error(`Couldn't retire the stored age: ${problemMessage(err)}`);
    } finally {
      setBusy(false);
    }
  }, [busy, client, dialog]);

  /* Finding one device in a fleet used to have no mechanism on this page: a
     plain sortable table and nothing else. The four identifying columns are
     marked searchable, and the three closed-vocabulary ones carry their own
     filter — whose options are faceted from the rows actually present, so a new
     device class or a second relay appears in the filter without anyone editing
     a list. Adoption state is a `StatusBadge` cell, so it needs an ACCESSOR too:
     the badge is the presentation, the word is the value a filter reads. */
  const deviceColumns = useMemo<ColumnDef<DeviceRow>[]>(
    () => [
      { id: "name", header: "Device", accessorFn: (r) => r.device.name, meta: { searchable: true } },
      { id: "address", header: "Address", accessorFn: (r) => r.address ?? "—", meta: { searchable: true } },
      { id: "model", header: "Model", accessorFn: (r) => r.model ?? "—", meta: { searchable: true } },
      {
        id: "class",
        header: "Class",
        accessorFn: (r) => r.device.device_class,
        meta: { searchable: true, filter: "enum", filterLabel: "Device class" },
      },
      {
        id: "relay",
        header: "Reported by",
        accessorFn: (r) => r.device.relay_id,
        meta: { filter: "enum", filterLabel: "Reported by" },
      },
      {
        id: "entities",
        header: "Entities",
        accessorFn: (r) => r.entities.length,
        meta: { numeric: true },
      },
      /* The two columns that answer "is this new to my network, or has it been
         here for weeks" — the question this page exists for and the one it could
         not answer at all, because the API served neither field. They are sorted
         on the raw instant and RENDERED as an age: an operator scanning a fleet
         is comparing recency, not reading dates, and "3d ago" is the comparison
         made legible. Absent (the API omits both until a device has been durably
         mirrored) renders as an em dash rather than as the epoch.

         `first_seen` additionally distinguishes an OBSERVED age from an
         INHERITED one (firstSeenCell). The sort deliberately still runs on the
         raw instant — an inherited value is a real number and ranking it among
         the rest is more use than exiling it, and the cell is what says not to
         read the ranking as exact. */
      {
        id: "first_seen",
        header: "First seen",
        accessorFn: (r) => r.device.first_seen ?? 0,
        meta: { numeric: true },
        cell: ({ row }) => firstSeenCell(row.original.device),
      },
      {
        id: "last_seen",
        header: "Last seen",
        accessorFn: (r) => r.device.last_seen ?? 0,
        meta: { numeric: true },
        cell: ({ row }) => ageCell(row.original.device.last_seen),
      },
      {
        id: "status",
        header: "Status",
        accessorFn: (r) => (r.adopted ? "Adopted" : "Discovered"),
        meta: { filter: "enum", filterLabel: "Adoption" },
        cell: ({ row }) =>
          row.original.adopted ? (
            <StatusBadge status="ok">Adopted</StatusBadge>
          ) : (
            <StatusBadge status="pending">Discovered</StatusBadge>
          ),
      },
    ],
    [],
  );

  const entityColumns = useMemo<ColumnDef<Entity>[]>(
    () => [
      { id: "name", header: "Entity", accessorFn: (e) => e.name, meta: { searchable: true } },
      {
        id: "device",
        header: "Device",
        accessorFn: (e) => deviceNames.get(e.device_id) ?? e.device_id,
        meta: { searchable: true },
      },
      {
        id: "class",
        header: "Class",
        accessorFn: (e) => e.device_class,
        meta: { searchable: true, filter: "enum", filterLabel: "Entity class" },
      },
      {
        id: "state",
        header: "State",
        // "unreported" is a real, distinct state and must be filterable as one —
        // an absent `state` is not the same fact as a device reporting "off".
        accessorFn: (e) => e.state ?? "unreported",
        meta: { filter: "enum", filterLabel: "State" },
        cell: ({ row }) =>
          row.original.state ? (
            <StatusBadge status={row.original.state === "off" ? "off" : "ok"}>
              {row.original.state}
            </StatusBadge>
          ) : (
            <StatusBadge status="pending">unreported</StatusBadge>
          ),
      },
    ],
    [deviceNames],
  );

  const shownEntities = useMemo(
    () =>
      selectedDeviceId === null
        ? entities
        : entities.filter((e) => e.device_id === selectedDeviceId),
    [entities, selectedDeviceId],
  );

  const selectedDeviceName = selectedDeviceId ? deviceNames.get(selectedDeviceId) : undefined;

  return (
    <div className="min-h-screen bg-background text-foreground">
      <div className="mx-auto flex max-w-[1200px] flex-col gap-6 px-4 py-6 lg:px-8">
        <PageHeader
          variant="hero"
          title="Discovery"
          description="Everything the relays can see on their networks, and what this deployment has adopted."
          actions={
            <div className="flex flex-wrap gap-2">
              <Button variant="outline" icon={RefreshCw} onClick={() => void load()}>
                Refresh
              </Button>
            </div>
          }
        />

        {loadError ? (
          <p role="alert" className="text-sm text-[color:var(--wv-err)]">
            Couldn't load the device plane — {loadError}
          </p>
        ) : null}

        <DiscoveryPanel
          discovery={discovery}
          relays={relays}
          devicesByRelay={devicesByRelay}
          entityCount={entities.length}
        />

        <section aria-label="Discovered devices" className="flex flex-col gap-3">
          <h2 className="text-lg font-semibold">Discovered devices</h2>
          <DataTable<DeviceRow>
            columns={deviceColumns}
            data={rows}
            label="Discovered devices"
            loading={devices === null}
            search={{ label: "Search devices", placeholder: "Name, address, model or class" }}
            filters
            pagination
            onRowPress={(row) =>
              setSelectedDeviceId((current) =>
                current === row.device.id ? null : row.device.id,
              )
            }
            emptyState={
              /* The empty state IS the classification. "No devices reported"
                 was the same sentence for "no relay is connected" and "a relay
                 swept and found nothing" and "health could not be read" — three
                 different problems wearing one message. It now says which. */
              <EmptyState title={discovery.headline} description={discovery.detail} icon={Radio} />
            }
            rowActions={(row) => {
              /* Retire is offered only on a row whose age is INHERITED, which is
                 the population it exists for: on an observed instant it would
                 discard a fact this deployment actually watched and replace it
                 with a later one, making the device read younger than it is. */
              const canRetire =
                row.device.first_seen !== undefined && !isObservedAge(row.device);
              if (row.adopted && !canRetire) return null;
              return (
                <div className="flex justify-end gap-2">
                  {canRetire ? (
                    <Button size="sm" variant="ghost" onClick={() => openRetireAge(row)}>
                      Retire age
                    </Button>
                  ) : null}
                  {row.adopted ? null : (
                    <Button size="sm" variant="outline" onClick={() => openAdopt(row)}>
                      Adopt
                    </Button>
                  )}
                </div>
              );
            }}
          />
        </section>

        {/* Between discovery and entities deliberately: an operator adopts above,
            refines here, and addresses commands below. The adopt dialog promises
            these are "refined afterwards on the adopted device" — this is that
            afterwards. */}
        <AdoptedDevices api={client} />

        <section aria-label="Entities" className="flex flex-col gap-3">
          <div className="flex flex-wrap items-end justify-between gap-3">
            <div>
              <h2 className="text-lg font-semibold">Entities</h2>
              <p className="text-sm text-muted-foreground">
                {selectedDeviceName
                  ? `The entities ${selectedDeviceName} exposes. Commands are addressed here, never to the device.`
                  : "Every addressable object the fleet's devices expose. Commands are addressed here, never to the device."}
              </p>
            </div>
            {selectedDeviceId ? (
              <Button size="sm" variant="ghost" onClick={() => setSelectedDeviceId(null)}>
                Show all entities
              </Button>
            ) : null}
          </div>
          <DataTable<Entity>
            columns={entityColumns}
            data={shownEntities}
            label="Entities"
            loading={devices === null}
            search={{ label: "Search entities", placeholder: "Entity, device or class" }}
            filters
            pagination
            emptyState={
              <EmptyState
                title="No entities"
                description="A device exposes its entities in the same report that discovers it."
                icon={Cpu}
              />
            }
          />
        </section>
      </div>

      <Modal
        title={dialog.kind === "adopt" ? `Adopt ${dialog.row.device.name}` : "Adopt device"}
        description="Adopting records this device as one this deployment is responsible for, and ships it to its relay to poll."
        open={dialog.kind === "adopt"}
        onOpenChange={(open) => {
          if (!open) setDialog({ kind: "closed" });
        }}
        footer={
          <>
            <Button variant="ghost" onClick={() => setDialog({ kind: "closed" })}>
              Cancel
            </Button>
            <Button onClick={() => void confirmAdopt()} disabled={busy}>
              {busy ? "Adopting…" : "Adopt"}
            </Button>
          </>
        }
      >
        {dialog.kind === "adopt" ? (
          <div className="flex flex-col gap-3 text-sm">
            <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-muted-foreground">
              <dt>Class</dt>
              <dd className="text-foreground">{dialog.row.device.device_class}</dd>
              <dt>Address</dt>
              <dd className="text-foreground">{dialog.row.address ?? "not reported"}</dd>
              <dt>Model</dt>
              <dd className="text-foreground">{dialog.row.model ?? "not reported"}</dd>
              <dt>Reported by</dt>
              <dd className="text-foreground">{dialog.row.device.relay_id}</dd>
            </dl>
            <p className="text-xs text-muted-foreground">
              {dialog.row.entities.length} entit
              {dialog.row.entities.length === 1 ? "y" : "ies"} will be adopted enabled and visible,
              placed where its relay reports it. Poll cadence and per-entity policy are refined
              afterwards on the adopted device.
            </p>
          </div>
        ) : null}
      </Modal>

      <Modal
        title={
          dialog.kind === "retire-age"
            ? `Retire the stored age for ${dialog.row.device.name}`
            : "Retire stored age"
        }
        description="The next report from this device's relay plants a fresh first-seen instant on this deployment's own clock. Nothing else about the device changes."
        open={dialog.kind === "retire-age"}
        onOpenChange={(open) => {
          if (!open) setDialog({ kind: "closed" });
        }}
        footer={
          <>
            <Button variant="ghost" onClick={() => setDialog({ kind: "closed" })}>
              Cancel
            </Button>
            <Button onClick={() => void confirmRetireAge()} disabled={busy}>
              {busy ? "Retiring…" : "Retire age"}
            </Button>
          </>
        }
      >
        {dialog.kind === "retire-age" ? (
          <div className="flex flex-col gap-3 text-sm">
            <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-muted-foreground">
              <dt>Stored age</dt>
              <dd className="text-foreground">{ageCell(dialog.row.device.first_seen)}</dd>
              <dt>Where it came from</dt>
              <dd className="text-foreground">
                {dialog.row.device.first_seen_origin === "adopted"
                  ? "inherited from an older build's column"
                  : "not recorded — this row predates provenance being kept"}
              </dd>
            </dl>
            {/* Said plainly because it is the one thing this cannot do. Retiring
                can only make a device read YOUNGER: the next report plants "now",
                and there is deliberately no way to SET an instant, since nothing
                on this platform can attest an operator's time claim. */}
            <p className="text-xs text-muted-foreground">
              This discards the stored instant and cannot restore or replace it — the device will
              read as newly seen from its next report onward, which is younger than it really is.
              Retire a value you can show is wrong, not a whole inventory: re-dating every device
              at once is the very thing this field's write-once rule exists to prevent. Last-seen is
              untouched.
            </p>
          </div>
        ) : null}
      </Modal>

      <Toaster />
    </div>
  );
}
