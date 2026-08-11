import { useCallback, useEffect, useMemo, useState } from "react";
import { Link } from "react-router";
import { MonitorPlay, RefreshCw, Tv } from "lucide-react";
import {
  Button,
  DataTable,
  EmptyState,
  PageHeader,
  StatusBadge,
  Toaster,
  type ColumnDef,
  type Status,
} from "@/components/kit";
import {
  ApiError,
  collectPages,
  createApi,
  launchableApps,
  type AdoptedDevice,
  type Device,
  type Entity,
  type WaiveoApi,
} from "@/api";
import { RokuRemote } from "@/routes/devices/roku-remote";
import {
  commandSummary,
  diagnoseCommandError,
  type CommandDispatch,
} from "@/routes/devices/command-outcome";
import { assessReadiness, type Readiness } from "./control-readiness";
import { extraAttributes, nowShowing, playerFacts } from "./player-state";

/**
 * The Roku console ("/roku") — the surface for OPERATING a media player, as
 * distinct from the Devices page, which is about discovering and adopting one.
 *
 * The Devices page's remote is a dialog: it drives one entity and closes. That
 * is right for "blink the TV to check I adopted the correct one" and wrong for
 * everything an operator actually does with a Roku, which is a SEQUENCE — power
 * on, home, launch the player, four D-pad presses — followed by the question
 * "which of those actually landed". This page keeps the device's live state,
 * the controls, and every dispatch's outcome on screen at once, so that question
 * is answerable without a second surface.
 *
 * ── Why this page is "media players", honestly labelled Roku ────────────────
 *
 * api/1's `Device` carries no driver name. The relay knows it (`roku-ecp`,
 * cmd/waiveo-relay) and it is half of the identity tuple an adoption record is
 * keyed by, so it IS published — on `AdoptedDevice.driver`, reachable only for a
 * device whose adoption record can be joined (see ./control-readiness for why
 * the join is by entity id and why that is sound). So this page lists every
 * `media-player`-class device, shows the real driver where the record can be
 * found, and does not pretend to know it where it cannot.
 *
 * ── Every control is a command the class declares ───────────────────────────
 *
 * The remote is built from `media-player`'s four declared commands and nothing
 * else (REG-066: launch, home, keypress, power). A relay refuses a command its
 * target's class does not declare WITHOUT attempting it (REL-113), so any other
 * button would be a button that can only ever fail. Legacy's Roku remote had
 * fifteen keys; every one of them is one of these four, and the ones that were
 * missing here (Info, Pause) were added to the remote rather than invented.
 *
 * ── Readiness ADVISES, it does not gate ─────────────────────────────────────
 *
 * The page states, before a press, which of the relay's three preconditions a
 * device fails (./control-readiness). It does NOT disable the controls on that
 * basis: a deployment can pin an entity's address on the relay itself, out of
 * band and invisibly to this console, and a page that refused to let an operator
 * try would be wrong about exactly the deployment that needed it most. Predict,
 * then report what actually happened.
 */

/** The class this page operates. A Roku's entity is a `media-player`, and the
 * command vocabulary the remote is built from is that class's (REG-066). */
const PLAYER_CLASS = "media-player";

/** The relay's production driver name for a Roku (cmd/waiveo-relay). Only
 * readable via a joined adoption record — see the header. */
const ROKU_DRIVER = "roku-ecp";

/** How many dispatches the log keeps. Long enough to hold a whole sequence, short
 * enough that the page does not become a scrollback. */
const LOG_LIMIT = 25;

function problemMessage(err: unknown): string {
  if (err instanceof ApiError) return err.detail ?? err.code;
  return "the service is unreachable.";
}

/** Entity state → the kit's status vocabulary. `unavailable` is an error because
 * the relay could not reach the device at all; `off`/`standby` are `off`, which
 * is a condition and not a fault. An unreported state is `pending`, never `off`:
 * "nobody has looked" and "it is off" send an operator to different places. */
function stateStatus(state: string | undefined): Status {
  if (state === undefined || state === "") return "pending";
  if (state === "unavailable") return "error";
  if (state === "off" || state === "standby") return "off";
  return "ok";
}

/** One media player, with everything the page needs about it joined together. */
interface PlayerRow {
  device: Device;
  /** The device's `media-player` entities — what a command is addressed to. */
  entities: Entity[];
  readiness: Readiness;
}

export default function RokuRoute({ api }: { api?: WaiveoApi }) {
  const client = useMemo(() => api ?? createApi(), [api]);
  const [devices, setDevices] = useState<Device[] | null>(null);
  const [entities, setEntities] = useState<Entity[]>([]);
  const [records, setRecords] = useState<AdoptedDevice[]>([]);
  const [recordsError, setRecordsError] = useState<string | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [selectedEntityId, setSelectedEntityId] = useState<string | null>(null);
  const [log, setLog] = useState<CommandDispatch[]>([]);

  const load = useCallback(async () => {
    // The adoption records are read SEPARATELY and their failure never fails the
    // page: they carry policy and the driver name, not the ability to drive. A
    // page that went blank because it could not read policy would deny an
    // operator the remote over a detail.
    void collectPages<AdoptedDevice>((cursor) => client.adoptedDevices.list({ cursor }))
      .then((rows) => {
        setRecords(rows);
        setRecordsError(null);
      })
      .catch((err: unknown) => {
        setRecords([]);
        setRecordsError(problemMessage(err));
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

  const players = useMemo<PlayerRow[]>(() => {
    return (devices ?? [])
      .filter((device) => device.device_class === PLAYER_CLASS)
      .map((device) => {
        const own = entities.filter(
          (e) => e.device_id === device.id && e.device_class === PLAYER_CLASS,
        );
        // Readiness is assessed against the entity that will actually be
        // commanded — the device's first media-player entity. A device with none
        // has nothing to command and is reported as such below.
        const first = own[0];
        return {
          device,
          entities: own,
          readiness: first
            ? assessReadiness(device, first, records)
            : { controllable: false, problems: [], record: null, policy: null },
        };
      });
  }, [devices, entities, records]);

  // Selection defaults to the FIRST player's first entity and is corrected
  // whenever the current selection stops existing — a lab with exactly one Roku
  // must never require a click before the page is useful, and a page that held a
  // dead id would render controls addressed at nothing.
  const selectedEntity = useMemo(
    () => entities.find((e) => e.id === selectedEntityId) ?? null,
    [entities, selectedEntityId],
  );
  useEffect(() => {
    if (selectedEntity !== null) return;
    const first = players.find((p) => p.entities.length > 0)?.entities[0];
    setSelectedEntityId(first?.id ?? null);
  }, [players, selectedEntity]);

  const selected = useMemo(
    () =>
      selectedEntity === null
        ? null
        : (players.find((p) => p.entities.some((e) => e.id === selectedEntity.id)) ?? null),
    [players, selectedEntity],
  );

  const onDispatch = useCallback((dispatch: CommandDispatch) => {
    setLog((current) => [dispatch, ...current].slice(0, LOG_LIMIT));
  }, []);

  const logColumns = useMemo<ColumnDef<CommandDispatch>[]>(
    () => [
      {
        id: "at",
        header: "At",
        accessorFn: (d) => new Date(d.at).toLocaleTimeString(),
      },
      { id: "label", header: "Pressed", accessorFn: (d) => d.label },
      {
        id: "sent",
        header: "Sent",
        accessorFn: (d) => commandSummary(d.command, d.params),
      },
      {
        id: "outcome",
        header: "Outcome",
        cell: ({ row }) => {
          const { outcome } = row.original;
          if (outcome.kind === "ok") return <StatusBadge status="ok">Applied</StatusBadge>;
          if (outcome.kind === "failed") {
            return <StatusBadge status="error">Request failed</StatusBadge>;
          }
          return <StatusBadge status="error">{outcome.code}</StatusBadge>;
        },
      },
      {
        id: "detail",
        header: "What the relay said",
        cell: ({ row }) => {
          const { outcome } = row.original;
          if (outcome.kind === "ok") {
            return <span className="text-muted-foreground">Dispatched to the device.</span>;
          }
          if (outcome.kind === "failed") {
            return <span>{outcome.detail}</span>;
          }
          const diagnosis = diagnoseCommandError(outcome.code);
          return (
            <span className="flex flex-col gap-1">
              <span>{outcome.message}</span>
              {diagnosis ? (
                <span className="text-xs text-muted-foreground">{diagnosis}</span>
              ) : null}
            </span>
          );
        },
      },
    ],
    [],
  );

  const playersLoading = devices === null;
  const cadence = selected?.readiness.record?.poll_cadence_seconds ?? null;

  return (
    <div className="min-h-screen bg-background text-foreground">
      <div className="mx-auto flex max-w-[1200px] flex-col gap-6 px-4 py-6 lg:px-8">
        <PageHeader
          variant="hero"
          title="Roku"
          description="Drive an adopted media player: what it is showing right now, the remote, and what the relay did with every press."
          actions={
            <div className="flex flex-wrap gap-2">
              <Button variant="outline" asChild>
                <Link to="/devices">Devices</Link>
              </Button>
              <Button variant="outline" icon={RefreshCw} onClick={() => void load()}>
                Refresh
              </Button>
            </div>
          }
        />

        {loadError ? (
          <p role="alert" className="text-sm text-[color:var(--wv-err)]">
            Couldn&apos;t read the device plane — {loadError}
          </p>
        ) : null}
        {recordsError ? (
          <p className="text-sm text-[color:var(--wv-warn)]">
            Adoption records could not be read — {recordsError} Poll cadence, per-entity policy and
            the driver name are unavailable, and the readiness checks below are incomplete.
          </p>
        ) : null}

        <section aria-label="Media players" className="flex flex-col gap-3">
          <h2 className="text-lg font-semibold">Media players</h2>
          {playersLoading ? (
            <p className="text-sm text-muted-foreground">Reading the device plane…</p>
          ) : players.length === 0 ? (
            <EmptyState
              title="No media player has been discovered"
              description="A Roku appears here once a relay on its network has reported it. Discovery, and what to do when nothing is found, is on the Devices page."
              icon={Tv}
              action={
                <Button asChild>
                  <Link to="/devices">Open Devices</Link>
                </Button>
              }
            />
          ) : (
            <ul className="grid gap-3 sm:grid-cols-2">
              {players.map((player) => {
                const entity = player.entities[0];
                const active = entity !== undefined && entity.id === selectedEntityId;
                const driver = player.readiness.record?.driver ?? null;
                return (
                  <li key={player.device.id}>
                    <button
                      type="button"
                      aria-pressed={active}
                      disabled={entity === undefined}
                      onClick={() => entity && setSelectedEntityId(entity.id)}
                      data-slot="player-card"
                      className={`wv-touch flex h-auto w-full flex-col items-start gap-1 rounded-card border p-4 text-left outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-60 ${
                        active ? "border-[color:var(--wv-accent)] bg-accent" : "border-border bg-card"
                      }`}
                    >
                      <span className="flex w-full flex-wrap items-center justify-between gap-2">
                        <span className="font-display text-[15px] font-semibold">
                          {player.device.name}
                        </span>
                        <StatusBadge status={player.device.adopted ? "ok" : "pending"}>
                          {player.device.adopted ? "Adopted" : "Discovered"}
                        </StatusBadge>
                      </span>
                      <span className="text-sm text-muted-foreground">
                        {player.device.address ?? "no address reported"}
                        {player.device.model ? ` · ${player.device.model}` : ""}
                      </span>
                      <span className="text-xs text-muted-foreground">
                        {driver === null
                          ? "Driver not published until adopted"
                          : driver === ROKU_DRIVER
                            ? "Roku (ECP)"
                            : `Driver ${driver}`}
                        {entity === undefined ? " · exposes no media-player entity" : ""}
                      </span>
                    </button>
                  </li>
                );
              })}
            </ul>
          )}
        </section>

        {selected !== null && selectedEntity !== null ? (
          <>
            <section
              aria-label="Player state"
              className="flex flex-col gap-4 rounded-card border border-border bg-card p-4"
            >
              <div className="flex flex-wrap items-center justify-between gap-3">
                <div className="flex flex-wrap items-center gap-3">
                  <h2 className="font-display text-[17px] font-semibold">
                    {selected.device.name}
                  </h2>
                  <StatusBadge status={stateStatus(selectedEntity.state)}>
                    {selectedEntity.state ?? "no state"}
                  </StatusBadge>
                </div>
                {/* More than one media-player entity on one device is unusual but
                    legal — the command is addressed to ONE of them, so which is
                    a choice the operator has to be able to make. */}
                {selected.entities.length > 1 ? (
                  <div className="flex flex-wrap gap-2">
                    {selected.entities.map((entity) => (
                      <Button
                        key={entity.id}
                        size="sm"
                        variant={entity.id === selectedEntity.id ? "secondary" : "ghost"}
                        onClick={() => setSelectedEntityId(entity.id)}
                      >
                        {entity.name}
                      </Button>
                    ))}
                  </div>
                ) : null}
              </div>

              <p data-slot="now-showing" className="text-sm">
                {nowShowing(selectedEntity)}
              </p>

              <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-sm">
                <dt className="text-muted-foreground">Address</dt>
                <dd>{selected.device.address ?? "not reported"}</dd>
                <dt className="text-muted-foreground">Model</dt>
                <dd>{selected.device.model ?? "not reported"}</dd>
                <dt className="text-muted-foreground">Serial</dt>
                <dd>{selected.device.serial ?? "not reported"}</dd>
                <dt className="text-muted-foreground">Reported by</dt>
                <dd className="font-mono text-xs break-all">{selected.device.relay_id}</dd>
                <dt className="text-muted-foreground">Entity</dt>
                <dd className="font-mono text-xs break-all">{selectedEntity.id}</dd>
                <dt className="text-muted-foreground">Poll cadence</dt>
                <dd>
                  {cadence === null
                    ? "the relay's own default (none authored)"
                    : `every ${cadence}s`}
                </dd>
              </dl>

              {/* Said once, plainly: this page does not observe the TV, it reads
                  what the relay last polled. An operator who presses Power on and
                  sees the badge stay `off` would otherwise conclude the command
                  failed, when the truth is that the next poll has not happened. */}
              <p className="text-xs text-muted-foreground">
                State and attributes are what the relay last polled, not a live reading — a command
                can take effect up to one poll interval before this page shows it. Press Refresh
                after a moment rather than pressing the button again.
              </p>
            </section>

            <section aria-label="Control readiness" className="flex flex-col gap-2">
              <h2 className="text-lg font-semibold">Can this be driven?</h2>
              {selected.readiness.controllable ? (
                <p className="flex items-center gap-2 text-sm">
                  <StatusBadge status="ok">Ready</StatusBadge>
                  <span>
                    Adopted, this entity is enabled on its record, and the relay holds an address
                    for it — the three things a relay requires before it will dispatch anything.
                  </span>
                </p>
              ) : (
                <div className="flex flex-col gap-2">
                  <p className="flex items-center gap-2 text-sm">
                    <StatusBadge status="warn">Commands will likely be refused</StatusBadge>
                  </p>
                  <ul className="flex flex-col gap-2">
                    {selected.readiness.problems.map((problem) => (
                      <li
                        key={problem.code}
                        data-slot="readiness-problem"
                        data-code={problem.code}
                        className="rounded-input border border-border p-3 text-sm"
                      >
                        <span className="font-medium">{problem.title}</span>
                        <span className="block text-muted-foreground">{problem.detail}</span>
                      </li>
                    ))}
                  </ul>
                  <p className="text-xs text-muted-foreground">
                    The controls stay enabled anyway. A deployment can pin a device&apos;s address
                    on the relay itself, where this console cannot see it, so refusing to let you
                    try would be wrong exactly when it mattered — press, and read the outcome below.
                  </p>
                </div>
              )}
            </section>

            <div className="grid gap-6 lg:grid-cols-[auto_1fr]">
              <section aria-label="Remote" className="flex flex-col gap-3">
                <h2 className="text-lg font-semibold">Remote</h2>
                <RokuRemote
                  api={client}
                  entity={selectedEntity}
                  apps={launchableApps(selected.device)}
                  onDispatch={onDispatch}
                />
              </section>

              <section aria-label="Reported attributes" className="flex flex-col gap-3">
                <h2 className="text-lg font-semibold">Reported attributes</h2>
                <p className="text-sm text-muted-foreground">
                  The six the <code>media-player</code> class declares. A value of &ldquo;not
                  reported&rdquo; means the driver said nothing about it — which is different from
                  it being empty.
                </p>
                <dl className="flex flex-col gap-3">
                  {playerFacts(selectedEntity).map((fact) => (
                    <div
                      key={fact.name}
                      data-slot="player-attribute"
                      data-name={fact.name}
                      className="flex flex-col gap-0.5 border-b border-border pb-2 last:border-0"
                    >
                      <dt className="flex flex-wrap items-center gap-2 text-sm font-medium">
                        {fact.label}
                        <code className="text-xs text-muted-foreground">{fact.name}</code>
                        {fact.emission === "cosmetic" ? (
                          <span className="text-[11px] tracking-wide text-muted-foreground uppercase">
                            cosmetic
                          </span>
                        ) : null}
                      </dt>
                      <dd className={fact.value === null ? "text-sm text-muted-foreground" : "text-sm"}>
                        {fact.value === null ? "not reported" : fact.value}
                      </dd>
                      <dd className="text-xs text-muted-foreground">{fact.help}</dd>
                    </div>
                  ))}
                </dl>
                {extraAttributes(selectedEntity).length > 0 ? (
                  <div className="flex flex-col gap-1">
                    <h3 className="text-sm font-semibold">Also reported</h3>
                    <p className="text-xs text-muted-foreground">
                      Reported by the driver but not declared by the class, so no automation can
                      match on it.
                    </p>
                    <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-sm">
                      {extraAttributes(selectedEntity).map((extra) => (
                        <div key={extra.name} className="contents">
                          <dt className="text-muted-foreground">{extra.name}</dt>
                          <dd>{extra.value}</dd>
                        </div>
                      ))}
                    </dl>
                  </div>
                ) : null}
              </section>
            </div>

            <section aria-label="Command outcomes" className="flex flex-col gap-3">
              <div className="flex flex-wrap items-end justify-between gap-3">
                <div>
                  <h2 className="text-lg font-semibold">Command outcomes</h2>
                  <p className="text-sm text-muted-foreground">
                    Every press this session and what came back. A command the relay refused never
                    reached the device — the code says which of the two happened.
                  </p>
                </div>
                {log.length > 0 ? (
                  <Button size="sm" variant="ghost" onClick={() => setLog([])}>
                    Clear
                  </Button>
                ) : null}
              </div>
              <DataTable<CommandDispatch>
                columns={logColumns}
                data={log}
                label="Command outcomes"
                emptyState={
                  <EmptyState
                    title="Nothing sent yet"
                    description="Press something on the remote. Each dispatch is recorded here with the relay's own answer, so a button that did nothing is visible rather than silent."
                    icon={MonitorPlay}
                  />
                }
              />
            </section>
          </>
        ) : null}
      </div>

      <Toaster />
    </div>
  );
}
