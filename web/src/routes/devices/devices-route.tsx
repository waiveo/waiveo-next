import { useCallback, useEffect, useMemo, useState } from "react";
import { Link } from "react-router";
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
  launchableApps,
  type Device,
  type Entity,
  type RelayHealth,
  type WaiveoApi,
} from "@/api";
import { RokuRemote } from "./roku-remote";
import { describeDiscovery, type BlindReason } from "./discovery";
import { DiscoveryPanel } from "./discovery-panel";
import { AdoptedDevices } from "./adopted-devices";

/**
 * The Devices route — the device plane as an operator sees it: what the relays
 * have FOUND, what this deployment has ADOPTED, the entities those devices
 * expose, and a virtual remote that drives one of them.
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
 * # Control is entity-addressed
 *
 * A command goes to an ENTITY, never to a device (relay/1 REL-112 accepts one
 * resolved entity id and no selector). So the remote opens from the entities
 * table, not the devices table, even though "remote-control the TV" is how an
 * operator thinks about it.
 */

/** The device class the virtual remote knows how to drive. The remote is built
 * from `media-player`'s declared command vocabulary (REG-066) and would emit
 * `COMMAND_UNRESOLVED` against anything else, so it is offered for this class
 * only rather than for every entity with a state. */
const REMOTE_CLASS = "media-player";

function problemMessage(err: unknown): string {
  if (err instanceof ApiError) return err.detail ?? err.code;
  return "the service is unreachable.";
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
  | { kind: "remote"; entity: Entity; device: Device | null };

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
    // Health is read SEPARATELY from the device plane, and its failure never
    // fails the page: it is owner-only (a 403 for a site admin is routine), and
    // folding it into the same try would turn "you may not read relay health"
    // into "the device plane could not be read" — hiding a fleet the caller is
    // perfectly entitled to see.
    void client.diagnostics
      .health()
      .then((health) => {
        setRelays(health.relays);
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

  /** Whether any media player has been DISCOVERED — adopted or not. The link is
   * offered for an unadopted one too, because the Roku console is where the
   * reason it cannot be driven yet is spelled out. It is withheld only when the
   * page has nothing of that class at all, since a link to an empty surface is
   * the same dead-end this work exists to remove. */
  const hasMediaPlayer = useMemo(
    () => (devices ?? []).some((d) => d.device_class === REMOTE_CLASS),
    [devices],
  );

  const deviceNames = useMemo(
    () => new Map((devices ?? []).map((d) => [d.id, d.name] as [string, string])),
    [devices],
  );
  const openAdopt = useCallback((row: DeviceRow) => {
    setDialog({ kind: "adopt", row });
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
          title="Devices"
          description="Everything the relays can see on their networks, what this deployment has adopted, and a remote for the ones it can drive."
          actions={
            <div className="flex flex-wrap gap-2">
              {/* The dialog remote below drives ONE entity and closes. The Roku
                  console is the surface for actually operating a media player:
                  its live attributes, whether it is drivable at all, and every
                  dispatch's outcome kept on screen. Linked from here because
                  this is where an operator arrives first. */}
              {hasMediaPlayer ? (
                <Button variant="outline" asChild>
                  <Link to="/roku">Roku console</Link>
                </Button>
              ) : null}
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
            rowActions={(row) =>
              row.adopted ? null : (
                <div className="flex justify-end gap-2">
                  <Button size="sm" variant="outline" onClick={() => openAdopt(row)}>
                    Adopt
                  </Button>
                </div>
              )
            }
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
            rowActions={(entity) =>
              entity.device_class === REMOTE_CLASS ? (
                <div className="flex justify-end">
                  <Button
                    size="sm"
                    variant="outline"
                    onClick={() =>
                      setDialog({
                        kind: "remote",
                        entity,
                        device: (devices ?? []).find((d) => d.id === entity.device_id) ?? null,
                      })
                    }
                  >
                    Remote
                  </Button>
                </div>
              ) : null
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
        title={dialog.kind === "remote" ? `Remote — ${dialog.entity.name}` : "Remote"}
        open={dialog.kind === "remote"}
        onOpenChange={(open) => {
          if (!open) setDialog({ kind: "closed" });
        }}
        size="sm"
      >
        {dialog.kind === "remote" ? (
          <div className="flex justify-center">
            <RokuRemote
              api={client}
              entity={dialog.entity}
              apps={dialog.device ? launchableApps(dialog.device) : []}
            />
          </div>
        ) : null}
      </Modal>

      <Toaster />
    </div>
  );
}
