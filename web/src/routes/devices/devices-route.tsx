import { useCallback, useEffect, useMemo, useState } from "react";
import { Cpu, MonitorPlay, Network, RefreshCw, Radio } from "lucide-react";
import {
  Button,
  ConfirmModal,
  DataTable,
  EmptyState,
  FormField,
  Modal,
  PageHeader,
  StatCard,
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
  etagForRevision,
  launchableApps,
  type AdoptedDevice,
  type AdoptedDeviceEntity,
  type Device,
  type Entity,
  type ScopeNode,
  type WaiveoApi,
} from "@/api";
import { RokuRemote } from "./roku-remote";

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
 *   - `GET /devices` is a READ MODEL a relay owns. A row means "a relay reported
 *     seeing this on its LAN" (relay/1 REL-110/111) and nothing more. There is
 *     no write path — this console cannot create, rename, or delete one.
 *   - `GET /adopted-devices` is what THIS deployment decided. Adopting is
 *     therefore a CREATE of an adoption record keyed by REL-153's
 *     `(site, driver, native_id)`, and "Adopted" in the table below is a JOIN
 *     this page computes over that tuple — never a flag it wrote on a device.
 *
 * The consequence worth stating: a device can be adopted while its relay is
 * offline (the record is durable; the discovered row is not), and a device can
 * be discovered forever without being adopted. Both states are normal and both
 * are legible in the status column.
 *
 * # Why Adopt can be disabled with a device sitting right there
 *
 * The identity tuple an adoption record needs is `driver` + `native_id`, and the
 * current `Device` representation publishes neither — `device_id` is a one-way
 * derivation of them (internal/shared/deviceid), so it cannot be run backwards
 * in a browser. Where a deployment surfaces the tuple (a top-level member on a
 * widened schema, or the `labels` map today) Adopt is live; where it does not,
 * the button is disabled and says why, because the alternative is a create body
 * built out of guesses that would adopt the wrong device under a plausible name.
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

/** The scope-node kinds an adopted device may be placed under. A device is
 * adopted INTO a place in the tree — its identity is scoped to the site
 * (REL-153) — and sites and groups are the kinds an operator recognises as
 * "where the hardware is". */
const PLACEMENT_SELECTOR = "kind in (site,group)";

function problemMessage(err: unknown): string {
  if (err instanceof ApiError) return err.detail ?? err.code;
  return "the service is unreachable.";
}

/** The adoption-record key REL-153 fixes: `(driver, native_id)` within a site.
 * Length-prefixed rather than joined on a separator for the same reason the
 * server's own derivation is (internal/shared/deviceid): a separator can appear
 * inside a native_id — an SSDP USN is arbitrary text — and a joined key would
 * let two different devices collide onto one entry, showing one of them as
 * adopted because the other is. */
function identityKey(driver: string, nativeId: string): string {
  return `${driver.length}:${driver}${nativeId.length}:${nativeId}`;
}

/** A discovered device paired with the adoption record that claims it, if any. */
interface DeviceRow {
  device: Device;
  adopted: AdoptedDevice | null;
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
  const [adopted, setAdopted] = useState<AdoptedDevice[]>([]);
  const [placements, setPlacements] = useState<ScopeNode[]>([]);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [dialog, setDialog] = useState<Dialog>({ kind: "closed" });
  const [forgetting, setForgetting] = useState<AdoptedDevice | null>(null);
  const [busy, setBusy] = useState(false);
  // The device whose entities the lower table is narrowed to; null shows every
  // entity in the deployment. Selection is a filter, not navigation — the two
  // tables are one page because a device is only interesting through what it
  // exposes.
  const [selectedDeviceId, setSelectedDeviceId] = useState<string | null>(null);
  // Adopt-form state, seeded when the dialog opens.
  const [adoptName, setAdoptName] = useState("");
  const [adoptPlacement, setAdoptPlacement] = useState("");
  const [adoptCadence, setAdoptCadence] = useState("");

  const load = useCallback(async () => {
    try {
      const [deviceRows, entityRows, adoptedRows, placementRows] = await Promise.all([
        collectPages<Device>((cursor) => client.devices.list({ cursor })),
        collectPages<Entity>((cursor) => client.entities.list({ cursor })),
        collectPages<AdoptedDevice>((cursor) => client.adoptedDevices.list({ cursor })),
        collectPages<ScopeNode>((cursor) =>
          client.scopeNodes.list({ selector: PLACEMENT_SELECTOR, cursor }),
        ),
      ]);
      setDevices(deviceRows);
      setEntities(entityRows);
      setAdopted(adoptedRows);
      setPlacements(placementRows);
      setLoadError(null);
    } catch (err) {
      setDevices([]);
      setEntities([]);
      setAdopted([]);
      setPlacements([]);
      setLoadError(problemMessage(err));
    }
  }, [client]);

  useEffect(() => {
    void load();
  }, [load]);

  const rows = useMemo<DeviceRow[]>(() => {
    const byIdentity = new Map<string, AdoptedDevice>();
    for (const record of adopted) {
      byIdentity.set(identityKey(record.driver, record.native_id), record);
    }
    return (devices ?? []).map((device) => {
      const facts = deviceFacts(device);
      const claim =
        facts.driver && facts.nativeId
          ? (byIdentity.get(identityKey(facts.driver, facts.nativeId)) ?? null)
          : null;
      return {
        device,
        adopted: claim,
        address: facts.address,
        model: facts.model,
        driver: facts.driver,
        nativeId: facts.nativeId,
        entities: entities.filter((e) => e.device_id === device.id),
      };
    });
  }, [adopted, devices, entities]);

  const relayCount = useMemo(
    () => new Set((devices ?? []).map((d) => d.relay_id)).size,
    [devices],
  );
  const adoptedCount = useMemo(() => rows.filter((r) => r.adopted !== null).length, [rows]);

  const deviceNames = useMemo(
    () => new Map((devices ?? []).map((d) => [d.id, d.name] as [string, string])),
    [devices],
  );
  const openAdopt = useCallback(
    (row: DeviceRow) => {
      setAdoptName(row.device.name);
      // Default the placement to the device's own scope node — the relay already
      // said where it is, so asking again is asking the operator to re-enter a
      // fact the system has. Only when that node is one the picker OFFERS,
      // though: a value with no matching <option> would leave the control
      // showing the "choose…" placeholder while the state said otherwise, and
      // the operator would submit a placement they never saw.
      const known = placements.some((p) => p.id === row.device.scope_node);
      setAdoptPlacement(known ? row.device.scope_node : "");
      setAdoptCadence("");
      setDialog({ kind: "adopt", row });
    },
    [placements],
  );

  const confirmAdopt = useCallback(async () => {
    if (dialog.kind !== "adopt" || busy) return;
    const { row } = dialog;
    if (!row.driver || !row.nativeId) return;
    const name = adoptName.trim();
    if (name === "") {
      toast.error("Name the device before adopting it.");
      return;
    }
    if (adoptPlacement === "") {
      toast.error("Choose the site or group this device is adopted into.");
      return;
    }
    // A blank cadence is "this deployment states no cadence" — REL-063 fixes no
    // default, so the field is omitted rather than sent as some number the
    // console invented.
    const cadence = adoptCadence.trim() === "" ? null : Number(adoptCadence);
    if (cadence !== null && (!Number.isInteger(cadence) || cadence < 1)) {
      toast.error("Poll cadence must be a whole number of seconds, or blank.");
      return;
    }
    // Every discovered entity is adopted enabled, visible, primary, under the
    // name the device itself reports. These are POLICY (REL-063), which is why
    // they can only be authored here — but an adoption that enabled nothing
    // would ship a device_inventory entry no relay would ever poll.
    const policy: AdoptedDeviceEntity[] = row.entities.map((e) => ({
      entity_id: e.id,
      device_class: e.device_class,
      enabled: true,
      hidden: false,
      display_name: e.name,
      category: "primary",
    }));
    setBusy(true);
    try {
      await client.adoptedDevices.create({
        name,
        scope_node: adoptPlacement,
        driver: row.driver,
        native_id: row.nativeId,
        ...(cadence === null ? {} : { poll_cadence_seconds: cadence }),
        entities: policy,
      });
      toast.success(`Adopted ${name}`);
      setDialog({ kind: "closed" });
      await load();
    } catch (err) {
      toast.error(`Couldn't adopt the device: ${problemMessage(err)}`);
    } finally {
      setBusy(false);
    }
  }, [adoptCadence, adoptName, adoptPlacement, busy, client, dialog, load]);

  const confirmForget = useCallback(async () => {
    if (!forgetting) return;
    try {
      await client.adoptedDevices.remove(forgetting.id, etagForRevision(forgetting.revision));
      toast.success(`Released ${forgetting.name}`);
      await load();
    } catch (err) {
      toast.error(`Couldn't release the device: ${problemMessage(err)}`);
    } finally {
      setForgetting(null);
    }
  }, [client, forgetting, load]);

  const deviceColumns = useMemo<ColumnDef<DeviceRow>[]>(
    () => [
      { id: "name", header: "Device", accessorFn: (r) => r.device.name },
      { id: "address", header: "Address", accessorFn: (r) => r.address ?? "—" },
      { id: "model", header: "Model", accessorFn: (r) => r.model ?? "—" },
      { id: "class", header: "Class", accessorFn: (r) => r.device.device_class },
      { id: "relay", header: "Reported by", accessorFn: (r) => r.device.relay_id },
      {
        id: "entities",
        header: "Entities",
        accessorFn: (r) => r.entities.length,
        meta: { numeric: true },
      },
      {
        id: "status",
        header: "Status",
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
      { id: "name", header: "Entity", accessorFn: (e) => e.name },
      {
        id: "device",
        header: "Device",
        accessorFn: (e) => deviceNames.get(e.device_id) ?? e.device_id,
      },
      { id: "class", header: "Class", accessorFn: (e) => e.device_class },
      {
        id: "state",
        header: "State",
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
            <Button variant="outline" icon={RefreshCw} onClick={() => void load()}>
              Refresh
            </Button>
          }
        />

        {loadError ? (
          <p role="alert" className="text-sm text-[color:var(--wv-err)]">
            Couldn't load the device plane — {loadError}
          </p>
        ) : null}

        <section aria-label="Discovery status" className="flex flex-col gap-3">
          <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
            <StatCard label="Discovered" value={String(devices?.length ?? 0)} icon={Radio} />
            <StatCard label="Adopted" value={String(adoptedCount)} icon={MonitorPlay} />
            <StatCard label="Relays reporting" value={String(relayCount)} icon={Network} />
            <StatCard label="Entities" value={String(entities.length)} icon={Cpu} />
          </div>
          {/* Said plainly because the absence of a "Scan" button is otherwise
              read as a missing feature: the console cannot start a sweep. Each
              relay discovers its own LAN and pushes its full current view
              upward, so Refresh re-reads what the relays have already said. */}
          <p className="text-sm text-muted-foreground">
            Each relay discovers its own network and reports what it finds; this console reads that
            report. Refresh re-reads it — there is no scan to start from here.
          </p>
        </section>

        <section aria-label="Discovered devices" className="flex flex-col gap-3">
          <h2 className="text-lg font-semibold">Discovered devices</h2>
          <DataTable<DeviceRow>
            columns={deviceColumns}
            data={rows}
            label="Discovered devices"
            loading={devices === null}
            onRowPress={(row) =>
              setSelectedDeviceId((current) =>
                current === row.device.id ? null : row.device.id,
              )
            }
            emptyState={
              <EmptyState
                title="No devices reported"
                description="No relay has reported a device on its network yet. A relay reports its full view when it connects."
                icon={Radio}
              />
            }
            rowActions={(row) => (
              <div className="flex justify-end gap-2">
                {row.adopted ? (
                  <Button size="sm" variant="ghost" onClick={() => setForgetting(row.adopted)}>
                    Release
                  </Button>
                ) : row.driver && row.nativeId ? (
                  <Button size="sm" variant="outline" onClick={() => openAdopt(row)}>
                    Adopt
                  </Button>
                ) : (
                  <Button
                    size="sm"
                    variant="outline"
                    disabled
                    title="This relay did not report the device's driver and native id, so there is no identity to adopt against."
                  >
                    Adopt
                  </Button>
                )}
              </div>
            )}
          />
        </section>

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
          <div className="flex flex-col gap-4">
            <FormField label="Name">
              {(field) => (
                <input
                  {...field}
                  className="flex min-h-[44px] w-full min-w-0 rounded-input border border-border bg-transparent px-3 py-1 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
                  value={adoptName}
                  onChange={(e) => setAdoptName(e.target.value)}
                />
              )}
            </FormField>
            <FormField label="Adopt into">
              {(field) => (
                <select
                  {...field}
                  className="flex min-h-[44px] w-full min-w-0 rounded-input border border-border bg-transparent px-3 py-1 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
                  value={adoptPlacement}
                  onChange={(e) => setAdoptPlacement(e.target.value)}
                >
                  <option value="">Choose a site or group…</option>
                  {placements.map((p) => (
                    <option key={p.id} value={p.id}>
                      {p.name}
                    </option>
                  ))}
                </select>
              )}
            </FormField>
            <FormField
              label="Poll cadence (seconds)"
              help="Leave blank to state no cadence — the platform fixes no default."
            >
              {(field) => (
                <input
                  {...field}
                  type="number"
                  min={1}
                  className="flex min-h-[44px] w-full min-w-0 rounded-input border border-border bg-transparent px-3 py-1 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
                  value={adoptCadence}
                  onChange={(e) => setAdoptCadence(e.target.value)}
                />
              )}
            </FormField>
            <p className="text-xs text-muted-foreground">
              Identity: {dialog.row.driver} / {dialog.row.nativeId}. {dialog.row.entities.length}{" "}
              entit{dialog.row.entities.length === 1 ? "y" : "ies"} will be adopted enabled and
              visible.
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

      <ConfirmModal
        title={forgetting ? `Release ${forgetting.name}?` : "Release device?"}
        description="The adoption record is deleted, so the relay stops polling it and it drops out of the delivered device inventory. The device itself stays on the network and will be reported again as discovered."
        open={forgetting !== null}
        onOpenChange={(open) => {
          if (!open) setForgetting(null);
        }}
        confirmLabel="Release"
        destructive
        onConfirm={() => void confirmForget()}
      />

      <Toaster />
    </div>
  );
}
